// trae.go Trae CN 供应商实现（chat_v3 协议）。
//
// 上游：https://trae-api-cn.mchost.guru
//   POST /api/agent/v3/llm_utils_chat                       对话（SSE）
//   GET  /api/ide/v1/get_detail_param?function=chat_v3      模型目录（实机验证）
//     注意是 GET + query，不是 POST body({})；body 形式永远返回空 config_info_list，
//     function 也必须是 chat_v3（agent_chat/chat/fusion_chat 均为空）。
//     返回 config_info_list[]：config_name + display_config.display_name + model_detail_list[].model_name
//   令牌刷新：POST https://api.trae.cn/cloudide/api/v3/common/GetUserToken（已实机验证）
//     头必须用 X-Cloudide-Token: <当前JWT> + X-Uid（放 Authorization: Cloud-IDE-JWT 会 401 Token 无效）
//     响应 Result.Token / Result.ExpiredAt，无需 refreshToken；JWT 寿命约 8h，到期前换新可无限续
//   旧路径（未验证，仅作兜底）：https://api.trae.cn/cloudide/api/v3/trae/oauth/ExchangeToken
//
// 凭证来源（auth JSON）：
//   accessToken  = Cloud-IDE-JWT（Authorization: Cloud-IDE-JWT <jwt> 与 X-Cloudide-Token 同源）
//   uid          = X-Uid
//   extra.appId / extra.appVersion：可选覆盖默认（见 traeAppID / traeAppVersionCode）
//
// 协议要点（详见上游 PROTOCOL.md）：
//   - messages[].content 必须是 parts 数组 [{type:"text",text:"..."}]，传字符串会被拒
//   - model / model_name 必须是目录里带 __dev 后缀的 model_name
//   - SSE 事件：output（content/response/reasoning_content/reasoning 均为增量）、
//     token_usage、done、error、[DONE]
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	traeChatPath     = "/api/agent/v3/llm_utils_chat"
	traeModelsPath   = "/api/ide/v1/get_detail_param"
	traeModelFunc    = "chat_v3"
	traeRefreshBase  = "https://api.trae.cn"
	traeAppID        = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeAppVersion   = "20260901"
	traeChatUpstream = "https://trae-api-cn.mchost.guru"
	// traeUserAgent 桌面客户端形态 UA。实测 Go 默认 UA 会被上游判为非法客户端并
	// 返回 4011 伪限流；换成浏览器/桌面形态即正常。
	traeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// traeStaticModel 旧的写死回退表已删除：模型清单只认上游动态目录，
// 失败时由 lastModels（最近一次成功快照）兜底，见 StaticModels。

// traeProvider 实现 Provider 接口（Trae CN）。
type traeProvider struct {
	c *Client

	mu       sync.RWMutex
	modelMap map[string]string // client 模型名(去 __dev) -> 上游 model_name(带 __dev)
	// lastModels 最近一次成功拉取的目录快照（仅内存）：FetchModels 写入，
	// 动态拉取失败时 StaticModels 用它兜底。不写死任何模型名——上游一改，
	// 写死的表就会像旧版那样变成错误的清单。
	lastModels []ModelInfo
}

func (p *traeProvider) Name() string { return "trae" }

// StaticModels 用最近一次成功拉取的目录快照兜底（含思考档位变体）；
// 从没成功拉过则返回 nil，不写死任何模型名。
func (p *traeProvider) StaticModels() []map[string]any {
	p.mu.RLock()
	list := append([]ModelInfo(nil), p.lastModels...)
	p.mu.RUnlock()
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		if len(m.Efforts) == 0 {
			out = append(out, traeModelEntry(m.ID, m))
			continue
		}
		for _, e := range m.Efforts {
			out = append(out, traeModelEntry(m.ID+":"+e, m))
		}
	}
	return out
}

func traeModelEntry(id string, m ModelInfo) map[string]any {
	ctx := m.ContextWindow
	if ctx == 0 {
		ctx = 131072
	}
	return map[string]any{
		"id":             id,
		"object":         "model",
		"created":        1753600000,
		"owned_by":       "trae",
		"context_length": ctx,
	}
}

// Classify Trae 错误分类（复用通用分类，补充常见失败码）。
func (p *traeProvider) Classify(status int, body string) ErrKind {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrSessionDead
	case http.StatusTooManyRequests:
		return ErrSoftRate
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusPaymentRequired:
		return ErrHardCredit
	}
	if status >= 500 {
		return ErrServer
	}
	lower := strings.ToLower(body)
	for _, m := range []string{"rate limit", "too many requests", "请求过于频繁", "限流"} {
		if strings.Contains(lower, m) {
			return ErrSoftRate
		}
	}
	for _, m := range []string{"quota", "credit", "额度", "余额"} {
		if strings.Contains(lower, m) {
			return ErrHardCredit
		}
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// modelsEmpty 目录（modelMap/lastModels）是否还没拉取成功过。
func (p *traeProvider) modelsEmpty() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.modelMap) == 0
}

// resolveModel 把客户端模型名解析为 (configName 干净名, modelName 带__dev 上游名)。
func (p *traeProvider) resolveModel(clientModel string) (string, string) {
	clientModel = strings.TrimSpace(clientModel)
	if clientModel == "" {
		clientModel = "GLM-5.3-Flash"
	}
	if strings.HasSuffix(clientModel, "__dev") || strings.HasSuffix(clientModel, "__max") {
		return strings.TrimSuffix(strings.TrimSuffix(clientModel, "__dev"), "__max"), clientModel
	}
	p.mu.RLock()
	mapped, ok := p.modelMap[clientModel]
	if !ok {
		// 大小写不敏感匹配展示名（快照与 modelMap 同步写入，这里只做别名兜底）。
		for _, m := range p.lastModels {
			if strings.EqualFold(m.ID, clientModel) {
				mapped, ok = p.modelMap[m.ID], true
				break
			}
		}
	}
	p.mu.RUnlock()
	if ok {
		return strings.TrimSuffix(strings.TrimSuffix(mapped, "__dev"), "__max"), mapped
	}
	// 目录尚未拉取成功：按上游命名约定追加 __dev（ChatStream 会先尝试懒拉取再走到这里）。
	return clientModel, clientModel + "__dev"
}

// RefreshToken 用当前 Cloud-IDE-JWT 换一枚新的 Cloud-IDE-JWT。
//
// 主路径 GetUserToken：凭证里只要有 accessToken（JWT 本身未过期或刚过期）即可自助续期，
// 不需要 refreshToken；这是网页/客户端加载时走的同一接口，JWT 约 8h 一轮，续期成功即无限可用。
// 兜底路径 ExchangeToken：仅在配了 refreshToken 时尝试（协议细节未经实机验证）。
func (p *traeProvider) RefreshToken(c *Client, a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	errGet := p.refreshViaGetUserToken(c, a)
	if errGet == nil {
		return nil
	}
	rt := strings.TrimSpace(a.RefreshToken)
	if rt == "" {
		if rt = strings.TrimSpace(a.ExtraVal("refreshToken")); rt == "" {
			return errGet
		}
	}
	url := traeRefreshBase + "/cloudide/api/v3/trae/oauth/ExchangeToken"
	body, _ := json.Marshal(map[string]string{"refresh_token": rt})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("trae refresh http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		JWT        string `json:"cloud_ide_jwt"`
		Token      string `json:"token"`
		ExpiresIn  int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("trae refresh parse: %w", err)
	}
	jwt := out.AccessToken
	if jwt == "" {
		jwt = out.JWT
	}
	if jwt == "" {
		jwt = out.Token
	}
	if jwt == "" {
		return fmt.Errorf("trae refresh: no access_token in response")
	}
	a.AccessToken = jwt
	if out.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// refreshViaGetUserToken 调 GetUserToken 换新 JWT。头是实机验证的关键点：
// 必须 X-Cloudide-Token（同值放 Authorization 会被 401 拒绝），X-Uid 用于定位账号。
func (p *traeProvider) refreshViaGetUserToken(c *Client, a *auth.Auth) error {
	jwt := strings.TrimSpace(a.AccessToken)
	if jwt == "" {
		return fmt.Errorf("trae refresh: empty accessToken")
	}
	url := traeRefreshBase + "/cloudide/api/v3/common/GetUserToken"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Cloudide-Token", jwt)
	if uid := strings.TrimSpace(a.UID); uid != "" {
		req.Header.Set("X-Uid", uid)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("trae GetUserToken http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Result struct {
			Token     string `json:"Token"`
			ExpiredAt string `json:"ExpiredAt"`
		} `json:"Result"`
		ResponseMetadata struct {
			Error struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("trae GetUserToken parse: %w", err)
	}
	if out.Result.Token == "" {
		return fmt.Errorf("trae GetUserToken: no token in response (code=%s %s)",
			out.ResponseMetadata.Error.Code, truncate(out.ResponseMetadata.Error.Message, 120))
	}
	a.AccessToken = out.Result.Token
	if t, err := time.Parse(time.RFC3339, out.Result.ExpiredAt); err == nil {
		a.ExpiresAt = t.Unix()
	}
	log.Printf("[trae] refresh ok uid=%s expired=%s", logUID(a.UID), out.Result.ExpiredAt)
	return nil
}

// FetchModels 拉取 Trae 模型目录，更新 modelMap，并返回 OpenAI 面模型列表。
// 必须 GET + ?function=chat_v3（POST body 形式永远返回空 config_info_list）。
func (p *traeProvider) FetchModels(c *Client, a *auth.Auth) ([]ModelInfo, error) {
	url := traeChatUpstream + traeModelsPath + "?function=" + traeModelFunc
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	p.applyChatHeaders(req, a, "")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae models http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		ConfigInfoList []struct {
			ConfigName   string `json:"config_name"`
			ConfigSource int    `json:"config_source"`
			ConfigSwitch bool   `json:"config_switch"`
			Invisible    *bool  `json:"is_invisible_to_user"`
			Context      struct {
				Dev int64 `json:"dev"`
			} `json:"context_window_tokens"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
			ModelDetailList []struct {
				ModelName string `json:"model_name"`
			} `json:"model_detail_list"`
			// reasoning_effort_config 每个模型自己的思考档位声明：
			//   support_thinking=false → 不支持思考，无档位；
			//   options 例：["light","high"] 或 ["light","high","extra_high"]，default_level 为默认档。
			ReasoningEffortConfig *struct {
				DefaultLevel    string   `json:"default_level"`
				Options         []string `json:"options"`
				SupportThinking bool     `json:"support_thinking"`
			} `json:"reasoning_effort_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("trae models parse: %w", err)
	}
	if len(env.ConfigInfoList) == 0 {
		return nil, fmt.Errorf("trae models: empty config_info_list")
	}
	mm := make(map[string]string)
	out := make([]ModelInfo, 0, len(env.ConfigInfoList))
	for _, cfg := range env.ConfigInfoList {
		displayName := strings.TrimSpace(cfg.DisplayConfig.DisplayName)
		// 只保留用户可见的用户级模型：跳过关闭的、隐藏的、无展示名的、
		// 非 config_source=1（自定义/内部配置）的条目。
		if !cfg.ConfigSwitch || cfg.Invisible != nil && *cfg.Invisible || displayName == "" || cfg.ConfigSource != 1 {
			continue
		}
		// 同一 config 可挂多个档位（__dev 默认、__max 长上下文），OpenAI 面用 __dev。
		modelName := ""
		for _, md := range cfg.ModelDetailList {
			if strings.HasSuffix(md.ModelName, "__dev") {
				modelName = md.ModelName
				break
			}
		}
		if modelName == "" && len(cfg.ModelDetailList) > 0 {
			modelName = cfg.ModelDetailList[0].ModelName
		}
		if strings.TrimSpace(modelName) == "" {
			continue
		}
		mm[displayName] = modelName
		mm[cfg.ConfigName] = modelName
		mm[strings.TrimSuffix(modelName, "__dev")] = modelName
		mi := ModelInfo{ID: displayName, Name: displayName, ContextWindow: cfg.Context.Dev}
		// 思考档位：由该模型自己的 reasoning_effort_config 决定（有的 2 档、有的 3 档、
		// 有的不支持思考）。/v1/models 会按 Efforts 逐档展开成「模型:档位」。
		if rc := cfg.ReasoningEffortConfig; rc != nil && rc.SupportThinking {
			for _, o := range rc.Options {
				if o = strings.TrimSpace(o); o != "" {
					mi.Efforts = append(mi.Efforts, o)
				}
			}
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("trae models: no usable model parsed")
	}
	p.mu.Lock()
	p.modelMap = mm
	p.lastModels = out
	p.mu.Unlock()
	return out, nil
}

// normalizeTraeTools 把 tools[].function.parameters 从对象转成 JSON 字符串。
// 已是字符串或结构异常的原样保留，避免误伤上游本来就能接受的形态。
func normalizeTraeTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			out = append(out, t)
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			out = append(out, t)
			continue
		}
		p, ok := fn["parameters"]
		if !ok {
			out = append(out, t)
			continue
		}
		if _, isStr := p.(string); isStr {
			out = append(out, t)
			continue
		}
		b, err := json.Marshal(p)
		if err != nil {
			out = append(out, t)
			continue
		}
		fnCopy := make(map[string]any, len(fn))
		for k, v := range fn {
			fnCopy[k] = v
		}
		fnCopy["parameters"] = string(b)
		tCopy := make(map[string]any, len(tm))
		for k, v := range tm {
			tCopy[k] = v
		}
		tCopy["function"] = fnCopy
		out = append(out, tCopy)
	}
	return out
}

// applyChatHeaders 设置 Trae chat 必需请求头。
func (p *traeProvider) applyChatHeaders(req *http.Request, a *auth.Auth, modelName string) {
	jwt := strings.TrimSpace(a.AccessToken)
	uid := strings.TrimSpace(a.UID)
	appID := a.ExtraVal("appId")
	if appID == "" {
		appID = traeAppID
	}
	appVer := a.ExtraVal("appVersion")
	if appVer == "" {
		appVer = traeAppVersion
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// User-Agent 必须伪装成桌面客户端：上游按 UA 识别调用方，Go 默认的
	// "Go-http-client/1.1" 会被判为非官方客户端并返回 code 4011
	// 「exceeded the rate limit」（HTTP 200，纯属伪装成限流，不是真限流）。
	req.Header.Set("User-Agent", traeUserAgent)
	if jwt != "" {
		req.Header.Set("Authorization", "Cloud-IDE-JWT "+jwt)
		req.Header.Set("X-Cloudide-Token", jwt)
	}
	if uid != "" {
		req.Header.Set("X-Uid", uid)
	}
	req.Header.Set("x-app-id", appID)
	req.Header.Set("x-app-version-code", appVer)
	req.Header.Set("x-ide-version-code", appVer)
	// X-Cloudide-Session：JWT payload.data.source_id（用户登录会话标识），
	// 缺失时部分上游接口会拒绝或限流，故始终携带（值为空则跳过）。
	if sid := strings.TrimSpace(a.ExtraVal("sourceId")); sid != "" {
		req.Header.Set("X-Cloudide-Session", sid)
	}
}

// traeTransformMessages 把 OpenAI 消息转 Trae 格式：content 必须是 parts 数组。
// 同时把历史里的 tool_calls 转成上游形态（function → function_call），并剔除因丢弃
// 无名工具调用而变成“孤儿”的 tool 消息——否则上游会报 param invalid。
func traeTransformMessages(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	keptIDs := map[string]bool{}
	for _, m := range msgs {
		nm := map[string]any{}
		for k, v := range m {
			nm[k] = v
		}
		// content -> parts 数组
		switch c := m["content"].(type) {
		case nil:
			nm["content"] = []any{}
		case string:
			nm["content"] = []any{map[string]any{"type": "text", "text": c}}
		case []any:
			// 已是数组：规范化每个元素为 {type,text}
			parts := make([]any, 0, len(c))
			for _, item := range c {
				if mm, ok := item.(map[string]any); ok {
					parts = append(parts, mm)
				}
			}
			if len(parts) == 0 {
				parts = []any{map[string]any{"type": "text", "text": ""}}
			}
			nm["content"] = parts
		default:
			// 其他类型原样保留（不阻断转发）
		}
		had, kept := normalizeTraeToolCalls(nm)
		// 原本有工具调用、但全被丢弃，且正文为空：这条 assistant 消息已无意义，直接剔除。
		if had && !kept && traeContentEmpty(nm) {
			continue
		}
		if tcs, ok := nm["tool_calls"].([]any); ok {
			for _, it := range tcs {
				if call, ok := it.(map[string]any); ok {
					if id, _ := call["id"].(string); id != "" {
						keptIDs[id] = true
					}
				}
			}
		}
		out = append(out, nm)
	}
	// 过滤孤儿 tool 消息：tool_call_id 指向的消息已不存在，保留会被上游拒。
	filtered := make([]map[string]any, 0, len(out))
	for _, nm := range out {
		if role, _ := nm["role"].(string); role == "tool" {
			if id, _ := nm["tool_call_id"].(string); id != "" && !keptIDs[id] {
				continue
			}
		}
		filtered = append(filtered, nm)
	}
	return filtered
}

// traeContentEmpty 判断消息 content（parts 数组）是否为空。
func traeContentEmpty(nm map[string]any) bool {
	if parts, ok := nm["content"].([]any); ok {
		return len(parts) == 0
	}
	return true
}

// normalizeTraeToolCalls 把 OpenAI 形态的 tool_calls 转成 Trae 的上游形态。
// Trae 的 ToolCall 第 4 个字段叫 FunctionCall（JSON key `function_call`），
// 而 OpenAI 客户端历史里是 `function`；原样透传会被上游当成空 FunctionCall，
// 报 `ToolCall read field 4 'FunctionCall' error: required field Name is not set` 并中断会话。
// 同时：Name 必填，丢掉无名条目；arguments 缺失时补 "{}"。
// 返回 (原本是否有 tool_calls, 是否至少保留了一条)。
func normalizeTraeToolCalls(nm map[string]any) (had, keptAny bool) {
	raw, ok := nm["tool_calls"].([]any)
	if !ok {
		return false, false
	}
	kept := make([]any, 0, len(raw))
	for _, it := range raw {
		call, ok := it.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			fn, _ = call["function_call"].(map[string]any)
		}
		if fn == nil {
			continue
		}
		if name, _ := fn["name"].(string); strings.TrimSpace(name) == "" {
			continue
		}
		if args, _ := fn["arguments"].(string); args == "" {
			fn["arguments"] = "{}"
		}
		delete(call, "function")
		call["function_call"] = fn
		kept = append(kept, call)
	}
	if len(kept) == 0 {
		delete(nm, "tool_calls")
		return true, false
	}
	nm["tool_calls"] = kept
	return true, true
}

// ChatStream 发起 Trae 对话，返回原生 SSE 流（由 StreamTo/Aggregate 翻译）。
func (p *traeProvider) ChatStream(c *Client, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return nil, 0, nil, fmt.Errorf("trae: parse request: %w", err)
	}
	clientModel, _ := reqBody["model"].(string)
	// 目录懒拉取：客户端没先调 /v1/models 时 modelMap 为空，先拉一次目录再翻译，
	// 否则展示名会被直接拼成 "__dev"（如 Seed-2.1-Turbo 的真实 model_name 是
	// Doubao-Seed-2.1-Turbo__dev，拼错即 4001）。失败则走 resolveModel 的兜底。
	if p.modelsEmpty() {
		if _, err := p.FetchModels(c, a); err != nil {
			log.Printf("[trae] lazy model fetch: %v", err)
		}
	}
	configName, modelName := p.resolveModel(clientModel)
	// 诊断日志：响应里的 model 字段是网关按客户端请求名回显的，真正发往上游的是 model_name，
	// 与 qoder 同格式，便于 `1.py` 这类脚本直接读日志核对名字翻译。
	log.Printf("[trae] chat model: client=%q -> upstreamKey=%q", clientModel, modelName)

	msgs, _ := reqBody["messages"].([]any)
	traeMsgs := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			traeMsgs = append(traeMsgs, mm)
		}
	}
	traeMsgs = traeTransformMessages(traeMsgs)

	payload := map[string]any{
		"messages":        traeMsgs,
		"function":        "chat_v3",
		"stream":          true,
		"config_name":     configName,
		"model":           modelName,
		"model_name":      modelName,
		"config_source":   1,
		"is_custom_model": false,
		"provider":        "",
	}
	// 透传 tools。注意 Trae 上游的 FunctionDefinition.parameters 是 **string** 类型
	// （JSON 序列化后的 schema 字符串），OpenAI 客户端传的是对象，直接透传会被上游
	// 4001 拒绝：cannot unmarshal object into Go struct field ... of type string。
	if tools, ok := reqBody["tools"].([]any); ok && len(tools) > 0 {
		payload["tools"] = normalizeTraeTools(tools)
	}
	// 思考档位（reasoning_effort / reasoningEffort）
	if re, ok := reqBody["reasoning_effort"].(string); ok && re != "" {
		payload["reasoning_effort"] = re
	} else if re, ok := reqBody["reasoningEffort"].(string); ok && re != "" {
		payload["reasoning_effort"] = re
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("trae: marshal body: %w", err)
	}

	url := traeChatUpstream + traeChatPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, nil, err
	}
	p.applyChatHeaders(req, a, modelName)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		log.Printf("ERR: [trae] chat uid=%s: transport error: %v", logUID(a.UID), err)
		return nil, 0, nil, transportErrHint(err)
	}
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := p.Classify(resp.StatusCode, string(rb))
		log.Printf("WARN: [trae] chat uid=%s: upstream %d %s body=%s", logUID(a.UID), resp.StatusCode, kind, truncate(string(rb), 200))
		return nil, resp.StatusCode, rb, nil
	}
	return monitorBody(resp.Body, c.IdleTimeout, nil), resp.StatusCode, nil, nil
}

// StreamTo 把 Trae 原生 SSE 翻译为 OpenAI SSE 写入 w。
func (p *traeProvider) StreamTo(w http.ResponseWriter, rc io.Reader, model string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := float64(time.Now().Unix())
	var lastUsage map[string]any
	writeChunk := func(delta map[string]any, finish string, usage map[string]any) error {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		} else {
			choice["finish_reason"] = nil
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": int64(created),
			"model":   model,
			"choices": []any{choice},
		}
		if usage != nil {
			chunk["usage"] = usage
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	leak := &traeLeakFilter{}
	hasTools := false
	finished := false
	err := traeWalk(rc, model, id, created,
		func(content, reasoning string, toolCalls []any, finish string) error {
			if finish != "" {
				// 先补发过滤器里残留的正文，再发结束帧。
				if rest := leak.flush(); rest != "" {
					if err := writeChunk(map[string]any{"content": rest}, "", nil); err != nil {
						return err
					}
				}
				// 有工具调用时按 OpenAI 语义把 stop 改成 tool_calls，
				// 否则客户端不会进入工具执行流程。
				if hasTools && finish == "stop" {
					finish = "tool_calls"
				}
				finished = true
				return writeChunk(map[string]any{}, finish, lastUsage)
			}
			if len(toolCalls) > 0 {
				hasTools = true
				if err := writeChunk(map[string]any{"tool_calls": toolCalls}, "", nil); err != nil {
					return err
				}
			}
			// 思考内容不对外输出（客户端会把 reasoning_content 直接渲染成正文，
			// 看起来像模型在自言自语）；正文里被模型写成 {"reasoning_content":"..."}
			// 的文本同样过滤掉。
			if out := leak.write(content); out != "" {
				return writeChunk(map[string]any{"content": out}, "", nil)
			}
			return nil
		},
		func(usage map[string]any) {
			lastUsage = usage
		})
	if err != nil {
		return err
	}
	// 兜底终帧（traeWalk 未发 done 时保证恰好一个结束帧）
	if !finished {
		fbFinish := "stop"
		if hasTools {
			fbFinish = "tool_calls"
		}
		_ = writeChunk(map[string]any{}, fbFinish, lastUsage)
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	return nil
}

// Aggregate 把 Trae 原生 SSE 聚合成单个 OpenAI 响应对象。
func (p *traeProvider) Aggregate(rc io.Reader, model string) (map[string]any, error) {
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := float64(time.Now().Unix())
	var (
		content    strings.Builder
		finish     = "stop"
		usage      map[string]any
		toolCalls  = map[int]map[string]any{}
		toolOrder  []int
	)
	err := traeWalk(rc, model, id, created, func(c, r string, tcs []any, f string) error {
		content.WriteString(c) // 思考内容(r)丢弃，不对外输出
		for _, it := range tcs {
			call, ok := it.(map[string]any)
			if !ok {
				continue
			}
			idx := 0
			if v, ok := call["index"].(float64); ok {
				idx = int(v)
			}
			merged, seen := toolCalls[idx]
			if !seen {
				merged = map[string]any{"index": idx}
				toolCalls[idx] = merged
				toolOrder = append(toolOrder, idx)
			}
			mergeToolCallDelta(merged, call)
		}
		if f != "" {
			finish = f
		}
		return nil
	}, func(u map[string]any) {
		usage = u
	})
	if err != nil {
		return nil, err
	}
	message := map[string]any{
		"role":    "assistant",
		"content": stripTraeLeak(content.String()),
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
		if finish == "stop" {
			finish = "tool_calls"
		}
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// traeLeakMarker 部分 Trae 模型（实测 GLM-5.3-Flash 裸名）不走独立的 reasoning 通道，
// 而是把思考内容按 {"reasoning_content":"..."} 的 JSON 文本直接写进正文 content，
// 客户端会把它当正文渲染出来。这里在输出侧做清理。
const traeLeakMarker = `{"reasoning_content"`

// stripTraeLeak 去掉正文里从泄漏标记起的全部内容（实测泄漏总出现在回答末尾）。
func stripTraeLeak(s string) string {
	if i := strings.Index(s, traeLeakMarker); i >= 0 {
		return s[:i]
	}
	return s
}

// traeLeakFilter 流式版 stripTraeLeak：标记可能被拆到多个 chunk，所以要保留末尾
// 可能是标记前缀的字符，检测到标记后丢弃其后所有正文。
type traeLeakFilter struct {
	hold     string
	dropping bool
}

func (f *traeLeakFilter) write(s string) string {
	if f.dropping || s == "" {
		return ""
	}
	f.hold += s
	if i := strings.Index(f.hold, traeLeakMarker); i >= 0 {
		out := f.hold[:i]
		f.hold = ""
		f.dropping = true
		return out
	}
	max := len(f.hold)
	if max > len(traeLeakMarker)-1 {
		max = len(traeLeakMarker) - 1
	}
	keep := 0
	for n := max; n > 0; n-- {
		if strings.HasPrefix(traeLeakMarker, f.hold[len(f.hold)-n:]) {
			keep = n
			break
		}
	}
	out := f.hold[:len(f.hold)-keep]
	f.hold = f.hold[len(f.hold)-keep:]
	return out
}

// flush 流结束时补发 hold 里未输出的正文。
func (f *traeLeakFilter) flush() string {
	out := f.hold
	f.hold = ""
	return out
}

// traeWalk 逐帧解析 Trae 原生 SSE，回调 onDelta(正文, 思考, 工具调用增量, finish) 与 onUsage。
// finish 非空时表示这是结束帧（已含 finish_reason）。
// 工具调用上游用 function_call 形态下发（首帧带 id/name，后续帧只带 arguments 片段），
// 这里统一转成 OpenAI 形态（function），并按 index 保留给调用方拼接。
func traeWalk(r io.Reader, model, id string, created float64, onDelta func(content, reasoning string, toolCalls []any, finish string) error, onUsage func(map[string]any)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	evName := "" // 当前 SSE 事件名（event: 行）；data 里不带 type 时靠它识别 done/error
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "event:") {
			evName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			if err == io.EOF {
				return nil
			}
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				return nil
			}
			var env map[string]any
			if json.Unmarshal([]byte(payload), &env) != nil {
				if err == io.EOF {
					return nil
				}
				continue
			}
			t, _ := env["type"].(string)
			if t == "" {
				t = evName
			}
			// 结束 / 错误
			if t == "error" {
				msg, _ := env["message"].(string)
				if msg == "" {
					msg, _ = env["error"].(string)
				}
				return fmt.Errorf("trae upstream error: %s", msg)
			}
			if t == "done" {
				fr, _ := env["finish_reason"].(string)
				if fr == "" {
					fr = "stop"
				}
				if fr == "length" || fr == "max_tokens" {
					fr = "length"
				}
				_ = onDelta("", "", nil, fr)
				if err == io.EOF {
					return nil
				}
				continue
			}
			// 用量
			if t == "token_usage" || hasUsageKeys(env) {
				onUsage(traeUsage(env))
				if err == io.EOF {
					return nil
				}
				continue
			}
			// 工具调用增量
			if tcs := traeToolCallDeltas(env); len(tcs) > 0 {
				if err := onDelta("", "", tcs, ""); err != nil {
					return err
				}
			}
			// 正文 / 思考增量
			content := pickStr(env, "content", "response", "message")
			reasoning := pickStr(env, "reasoning_content", "reasoning")
			if content == "" && reasoning == "" {
				if err == io.EOF {
					return nil
				}
				continue
			}
			if err := onDelta(content, reasoning, nil, ""); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// traeToolCallDeltas 把上游 tool_calls 的 function_call 形态转成 OpenAI 的 function 形态。
func traeToolCallDeltas(env map[string]any) []any {
	raw, _ := env["tool_calls"].([]any)
	out := make([]any, 0, len(raw))
	for _, it := range raw {
		call, ok := it.(map[string]any)
		if !ok {
			continue
		}
		d := map[string]any{}
		if v, ok := call["index"].(float64); ok {
			d["index"] = v
		}
		if v, _ := call["id"].(string); v != "" {
			d["id"] = v
		}
		if v, _ := call["type"].(string); v != "" {
			d["type"] = v
		}
		fc, _ := call["function_call"].(map[string]any)
		if fc == nil {
			fc, _ = call["function"].(map[string]any)
		}
		fn := map[string]any{}
		if fc != nil {
			if v, _ := fc["name"].(string); v != "" {
				fn["name"] = v
			}
			if v, _ := fc["arguments"].(string); v != "" {
				fn["arguments"] = v
			}
		}
		if len(fn) > 0 {
			d["function"] = fn
		}
		if len(d) == 0 {
			continue
		}
		out = append(out, d)
	}
	return out
}

// hasUsageKeys 判断事件是否像 usage（含 prompt_tokens / completion_tokens 等）。
func hasUsageKeys(env map[string]any) bool {
	for _, k := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "usage", "token_usage"} {
		if _, ok := env[k]; ok {
			return true
		}
	}
	return false
}

// traeUsage 从事件里提取 OpenAI 风格 usage。
func traeUsage(env map[string]any) map[string]any {
	u := map[string]any{}
	if v, ok := num(env, "prompt_tokens"); ok {
		u["prompt_tokens"] = v
	}
	if v, ok := num(env, "completion_tokens"); ok {
		u["completion_tokens"] = v
	}
	if v, ok := num(env, "total_tokens"); ok {
		u["total_tokens"] = v
	}
	if v, ok := num(env, "cache_read_input_tokens"); ok {
		if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
			d["cached_tokens"] = v
		} else {
			u["prompt_tokens_details"] = map[string]any{"cached_tokens": v}
		}
	}
	if v, ok := num(env, "reasoning_tokens"); ok {
		if d, ok := u["completion_tokens_details"].(map[string]any); ok {
			d["reasoning_tokens"] = v
		} else {
			u["completion_tokens_details"] = map[string]any{"reasoning_tokens": v}
		}
	}
	if len(u) == 0 {
		return nil
	}
	return u
}

// pickStr 从 env 取第一个存在的字符串键。
func pickStr(env map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := env[k].(string); ok {
			return v
		}
	}
	return ""
}

// num 从 env 取数值（int/float64 统一转 float64）。
func num(env map[string]any, key string) (float64, bool) {
	switch v := env[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// logUID 简短日志用（避免直接打全 UID）。
func logUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8] + "***"
	}
	return uid
}
