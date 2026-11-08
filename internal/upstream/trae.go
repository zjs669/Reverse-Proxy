// trae.go Trae 供应商实现（国内 trae.cn / 国际 trae.ai）。
//
// 上游（国内）：https://trae-api-cn.mchost.guru
//   POST /api/agent/v3/llm_utils_chat                     对话（SSE）
//   GET  /api/ide/v1/get_detail_param?function=chat_v3    模型目录
//     必须是 GET + query，且 function=chat_v3；用 POST body 或别的 function 都返回空列表。
//   刷新：POST https://api.trae.cn/cloudide/api/v3/common/GetUserToken
//     认 X-Cloudide-Token(JWT) 或 X-Cloudide-Session(source_id)。
//     **source_id 是长期登录会话标识**：即使 JWT 过期也能换到新令牌（JWT 寿命约 8h）。
//     只有 X-Uid 会被拒（401/20101）；头名不能错。
//
// 凭证：accessToken = Cloud-IDE-JWT；uid = X-Uid；extra.sourceId = X-Cloudide-Session；
//       extra.region = cn|intl（决定基址，见 isIntl）。
//
// 协议要点：
//   - messages[].content 必须是 parts 数组 [{type:"text",text:"..."}]，传字符串会被拒
//   - model 必须是目录里带 __dev 后缀的 model_name
//   - SSE 事件：output（content/response/reasoning 均为增量）、token_usage、done、error、[DONE]
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

	"reverse-proxy/internal/auth"
)

const (
	traeChatPath   = "/api/agent/v3/llm_utils_chat"
	traeModelsPath = "/api/ide/v1/get_detail_param"
	traeModelFunc  = "chat_v3"
	// 国内版（trae.cn）
	traeRefreshBase  = "https://api.trae.cn"
	traeAppID        = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeAppVersion   = "20260901"
	traeChatUpstream = "https://trae-api-cn.mchost.guru"
	// 国际版（trae.ai）—— 与国内版是独立体系，域名、认证头、积分接口全不同。
	// 实测：换 token 认 X-Cloudide-Token（客户端 JWT）或 X-Cloudide-Session（source_id），
	// 两者都返回 200；国内版只认后者。详见 docs/trae-intl.md。
	traeIntlRefreshBase = "https://api.trae.ai"
	// traeIntlChatUpstream 国际版对话/模型上游（2026-11-05 实测）。
	// 命名规律与积分域 ug-normal.trae.ai 同构；国内版是 trae-api-cn.mchost.guru。
	// 模型目录与对话都挂在这个域下的 /api/... 路径，与国内版路径完全一致。
	traeIntlChatUpstream = "https://coresg-normal.trae.ai"
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

// isIntl 判断该账号是否国际版（trae.ai）。
//
// 判据优先级：
//  1. extra.region 显式写了 "intl"/"cn"（最可靠，用户可强制指定）
//  2. auth.domain 含 trae.ai（login 时若写入了 domain）
//
// 不能复用 auth.Auth.Region()：那个只认 codebuddy.ai，对 trae 恒返回 "cn"。
func (p *traeProvider) isIntl(a *auth.Auth) bool {
	if a == nil {
		return false
	}
	if r := strings.ToLower(strings.TrimSpace(a.ExtraVal("region"))); r != "" {
		return r == "intl"
	}
	return strings.Contains(strings.ToLower(a.Domain), "trae.ai")
}

// refreshBase 按区域返回换 token 基址。
func (p *traeProvider) refreshBase(a *auth.Auth) string {
	if p.isIntl(a) {
		return traeIntlRefreshBase
	}
	return traeRefreshBase
}

// chatUpstream 按区域返回对话/模型上游基址。
//
// 两家路径相同（/api/ide/v1/get_detail_param、/api/agent/v3/llm_utils_chat），
// 只有域名不同，所以调用方拿到基址后拼路径即可。
func (p *traeProvider) chatUpstream(a *auth.Auth) string {
	if p.isIntl(a) {
		return traeIntlChatUpstream
	}
	return traeChatUpstream
}

// StaticModels 用最近一次成功拉取的目录快照兜底（只列裸模型名）；
// 从没成功拉过则返回 nil，不写死任何模型名。
func (p *traeProvider) StaticModels() []map[string]any {
	p.mu.RLock()
	list := append([]ModelInfo(nil), p.lastModels...)
	p.mu.RUnlock()
	out := make([]map[string]any, 0, len(list))
	// 与 /v1/models 动态路径一致：只列裸模型名，不展开「模型:档位」变体。
	for _, m := range list {
		out = append(out, traeModelEntry(m.ID, m))
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

// RefreshToken 换一枚新的 Cloud-IDE-JWT。
//
// 主路径 GetUserToken：不需要 refreshToken，也不需要手上有一枚有效的 JWT——
// 只要带上登录会话标识 X-Cloudide-Session（data.source_id，长期有效）就能换新。
// 这是“浏览器好几天不用也免验证”的同一个道理，所以 Trae 账号可以完全自动养护。
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
	url := p.refreshBase(a) + "/cloudide/api/v3/trae/oauth/ExchangeToken"
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
	} else if exp := auth.JWTExp(jwt); exp > 0 {
		// 无 expires_in 时按新 JWT 的 exp 定寿命（与 refreshViaGetUserToken 同一兜底口径）。
		a.ExpiresAt = exp
	}
	return nil
}

// resolveSourceID 取登录会话标识（X-Cloudide-Session）：extra.sourceId（显式覆盖）→ 当前 JWT 的 data.source_id。
// 实测同一登录会话铸出的多枚 JWT 里 source_id 相同（不随 8h 轮换而变），
// 所以它既是聊天请求头，也是 JWT 过期后唯一能自救的长期凭证。
func (p *traeProvider) resolveSourceID(a *auth.Auth) string {
	if sid := strings.TrimSpace(a.ExtraVal("sourceId")); sid != "" {
		return sid
	}
	return auth.JWTSourceID(a.AccessToken)
}

// applyRefreshHeaders 设置 GetUserToken 的鉴权头：登录会话（X-Cloudide-Session）必带，
// 手上那枚 JWT（哪怕已过期）作为补充一并带上。头名不能弄错：source_id 放 X-Cloudide-Token
// 会被判 401/20101，只给 X-Uid 同样被拒。accessToken 为空时不设令牌头（靠会话换新）。
func (p *traeProvider) applyRefreshHeaders(req *http.Request, a *auth.Auth, sid string) {
	if jwt := strings.TrimSpace(a.AccessToken); jwt != "" {
		req.Header.Set("X-Cloudide-Token", jwt)
	}
	if sid != "" {
		req.Header.Set("X-Cloudide-Session", sid)
	}
	if uid := strings.TrimSpace(a.UID); uid != "" {
		req.Header.Set("X-Uid", uid)
	}
}

// refreshViaGetUserToken 调 GetUserToken 换新 JWT。头是实机验证的关键点：
// 未过期时用 X-Cloudide-Token；**令牌已过期时靠 X-Cloudide-Session（登录会话，长期有效）照样能换**。
// 两个都带最稳（会话优先，令牌作为补充）；只有 X-Uid 会被上游判 401/20101 Token 无效。
func (p *traeProvider) refreshViaGetUserToken(c *Client, a *auth.Auth) error {
	jwt := strings.TrimSpace(a.AccessToken)
	sid := p.resolveSourceID(a)
	if jwt == "" && sid == "" {
		return fmt.Errorf("trae refresh: 既无 accessToken 也无 source_id，无法换新令牌")
	}
	url := p.refreshBase(a) + "/cloudide/api/v3/common/GetUserToken"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	p.applyRefreshHeaders(req, a, sid)
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
	} else if exp := auth.JWTExp(out.Result.Token); exp > 0 {
		// ExpiredAt 缺失/改格式不能把新令牌的寿命丢掉：改从新 JWT 的 exp 取。
		// 否则 ExpiresAt 还留着上一次的旧值（甚至是 farFuture 占位值），看护按它判定
		// “还剩很久”，新令牌再也不会被续期，8h 后账号又一次静默死亡。
		a.ExpiresAt = exp
	}
	log.Printf("[trae] refresh ok uid=%s expired=%s", logUID(a.UID), out.Result.ExpiredAt)
	return nil
}

// FetchModels 拉取 Trae 模型目录，更新 modelMap，并返回 OpenAI 面模型列表。
// 必须 GET + ?function=chat_v3（POST body 形式永远返回空 config_info_list）。
func (p *traeProvider) FetchModels(c *Client, a *auth.Auth) ([]ModelInfo, error) {
	url := p.chatUpstream(a) + traeModelsPath + "?function=" + traeModelFunc
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
		// 同一 config 可挂多个变体：__dev 默认、__max 长上下文，以及国际版特有的
		// __dollar__dev（美元计费档）。
		//
		// __dollar__ 变体必须以额外参数调用，否则上游直接 4001「param is invalid」；
		// 网关目前不带那些参数，所以**优先选非 dollar 的 __dev**，选不到才退 dollar 档。
		// 不这样挑的话，模型名以 __dev 结尾的 dollar 变体会被先命中，整条链路必失败。
		modelName := ""
		pickDev := func(allowDollar bool) string {
			for _, md := range cfg.ModelDetailList {
				if !strings.HasSuffix(md.ModelName, "__dev") {
					continue
				}
				if !allowDollar && strings.Contains(md.ModelName, "__dollar") {
					continue
				}
				return md.ModelName
			}
			return ""
		}
		modelName = pickDev(false)
		if modelName == "" {
			modelName = pickDev(true)
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
		// 思考档位元数据：由该模型自己的 reasoning_effort_config 决定（有的 2 档、
		// 有的 3 档、有的不支持思考）。仅作能力记录，/v1/models 只列裸模型名。
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
	// 缺失时部分上游接口会拒绝或限流，故始终携带。
	// 取值优先级：extra.sourceId（用户显式覆盖）→ 从当前 JWT 自动解析。
	sid := p.resolveSourceID(a)
	if sid != "" {
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

// traeContentToParts 把 OpenAI 风格的字符串 content 转成国际版要求的数组形态：
//
//	"hi"  →  [{"type":"text","text":"hi"}]
//
// 已经是数组/非字符串的原样保留（多模态消息客户端本来就传数组）。
// 空 content 不塞空数组——上游对空数组同样报参数错，直接删掉该字段。
func traeContentToParts(msgs []map[string]any) []map[string]any {
	for _, m := range msgs {
		s, ok := m["content"].(string)
		if !ok {
			continue
		}
		if s == "" {
			delete(m, "content")
			continue
		}
		m["content"] = []map[string]any{{"type": "text", "text": s}}
	}
	return msgs
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
	// 国际版目录里很多模型只提供 __dollar__dev 变体（美元计费档），
	// 直接拿它调对话会被上游判 4001 "param is invalid"。
	// 实测把 __dollar 去掉（gpt-5.2__dollar__dev → gpt-5.2__dev）就能正常返回，
	// 计费口径不受影响（仍按该账号的 basic_usage 扣）。所以国际版统一剥掉。
	if p.isIntl(a) {
		modelName = strings.ReplaceAll(modelName, "__dollar", "")
		configName = strings.ReplaceAll(configName, "__dollar", "")
	}
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
	// 国际版要求 messages[].content 是**数组** [{type:"text",text:"..."}]，
	// 传字符串会被上游判 4001 拒绝：
	//   bad request: json: cannot unmarshal string into Go struct field
	//   LLMRawMessage.messages.content of type []*idecopilot.LLMRawMessageContent
	// 国内版接受字符串，所以只在国际版做这层转换（国内版改了反而不稳）。
	if p.isIntl(a) {
		traeMsgs = traeContentToParts(traeMsgs)
	}

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


	url := p.chatUpstream(a) + traeChatPath
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
	// writeErr 记录向客户端写帧是否失败（失败=客户端已断开）：据此决定异常收尾时还要不要兜底补帧。
	var writeErr error
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
			writeErr = err
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
		// 已提交 200 且可能已下发部分 SSE：此处区分两类错误——
		//   - writeErr != nil：客户端已断开，补写无意义，直接返回；
		//   - 其余（上游截断 / 上游 error 事件帧）：客户端仍连着等待收尾，若不补
		//     finish_reason 帧，客户端会因「Stream ended without finish_reason」无限退避重试。
		//     兜底补一个结束帧 + [DONE]，把异常截断转为「正常说完」（内容可能缺失，属已知取舍）。
		if writeErr == nil {
			if !finished {
				fbFinish := "stop"
				if hasTools {
					fbFinish = "tool_calls"
				}
				_ = writeChunk(map[string]any{}, fbFinish, lastUsage)
			}
			if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr == nil && fl != nil {
				fl.Flush()
			}
		}
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

// traeLeakMarkers 上游把思考内容当正文 content 下发的几种已知形态
// （客户端会把它们原样渲染成正文，看起来像模型在念乱码）：
//
//	{"reasoning_content":"..."}
//	    国内版部分模型（实测 GLM-5.3-Flash 裸名）不走独立 reasoning 通道，
//	    直接把思考塞进 content。
//	[{"id":"rs_...","type":"reasoning","encrypted_content":"..."}]
//	    国际版 GPT 系列（实测 GPT-5.2）：上游把 Responses API 的 reasoning
//	    条目整个 JSON 数组当正文追加在回答后面。正文只有「你好」，content
//	    却有 1728 字符，多出来的全是这坨密文。
//
// 实测两种泄漏都出现在回答末尾，所以从标记处截断即可。
var traeLeakMarkers = []string{
	`{"reasoning_content"`,
	`[{"id":"rs_`,
	`[{"type":"reasoning"`,
	`{"type":"reasoning"`,
}

// traeLeakMaxLen 最长标记长度，流式过滤时据此决定要保留多少尾部字符。
var traeLeakMaxLen = func() int {
	m := 0
	for _, s := range traeLeakMarkers {
		if len(s) > m {
			m = len(s)
		}
	}
	return m
}()

// stripTraeLeak 去掉正文里从最早出现的泄漏标记起的全部内容。
func stripTraeLeak(s string) string {
	cut := -1
	for _, mk := range traeLeakMarkers {
		if i := strings.Index(s, mk); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut >= 0 {
		return s[:cut]
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
	// 命中任一标记就丢弃其后全部正文（泄漏总在末尾）。
	if out := stripTraeLeak(f.hold); len(out) != len(f.hold) {
		f.hold = ""
		f.dropping = true
		return out
	}
	max := len(f.hold)
	if max > traeLeakMaxLen-1 {
		max = traeLeakMaxLen - 1
	}
	keep := 0
	for n := max; n > 0; n-- {
		tail := f.hold[len(f.hold)-n:]
		for _, mk := range traeLeakMarkers {
			if strings.HasPrefix(mk, tail) {
				keep = n
				break
			}
		}
		if keep > 0 {
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

// ─── 每日签到 ───────────────────────────────────────────────────────────────

const (
	traeCheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	traeCheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"
)

// traeCheckinReqSource IDE=1 / Lite=2。网关按 IDE 口径发。
const traeCheckinReqSource = 1

// traeCheckinDeviceID claim 必需的 x-device-id。
//
// 实测（2026-11-06）：**必须是纯数字**，不能是 UUID。
//   空串 / 缺失        → 9004 "The submitted order parameters are incorrect"
//   UUID（带横线）      → 9074 "当前参与用户太多"（看起来像限流，其实是格式不对）
//   纯数字 498551588155017 → {"code":0,"message":"success"}
//
// 这个值在客户端里是设备指纹，服务端似乎不校验与账号的绑定关系
// （同一个值在多个账号上都能签成功），所以固定一个即可。
const traeCheckinDeviceID = "498551588155017"

// traeCheckinSoftBusyCode 上游「当前参与用户太多」的码。
//
// 修正（2026-11-06 实测）：这个码**不一定是真限流**。
//   x-device-id 用 UUID 格式（带横线）→ 9074
//   x-device-id 用纯数字          → code:0 success
// 所以 9074 更像是「设备标识不合规」，真限流只是它的一种成因。
const traeCheckinSoftBusyCode = 9074

// traeCheckinResp status 与 claim 共用同一形状。
type traeCheckinResp struct {
	Code          int    `json:"code"`
	Message       string `json:"message"`
	Enable        bool   `json:"enable"`
	CheckedIn     bool   `json:"checked_in"`
	DidCheckedIn  bool   `json:"did_checked_in"`
	Credits       int64  `json:"credits"`
	ExtraCredits  int64  `json:"extra_credits"`
}

// Checkin 实现 Trae 签到。国际版无签到体系（额度列表只有 Free plan，无 group_type=1 签到包）。
func (p *traeProvider) Checkin(c *Client, a *auth.Auth) (CheckinResult, error) {
	if p.isIntl(a) {
		return CheckinResult{State: CheckinUnsupported, Msg: "国际版没有签到包"}, nil
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return CheckinResult{State: CheckinFailed, Msg: "账号没有 accessToken"}, nil
	}

	// 先探状态：已签就直接返回 Already，不再打 claim（避免无谓的风控暴露）。
	st, err := p.checkinCall(c, a, traeCheckinStatusPath)
	if err != nil {
		return CheckinResult{State: CheckinFailed, Msg: err.Error()}, err
	}
	if st.Code != 0 {
		return CheckinResult{State: CheckinFailed, Msg: st.Message}, nil
	}
	if st.CheckedIn {
		return CheckinResult{State: CheckinAlready, Msg: "今天已签到", Credit: st.Credits}, nil
	}
	if !st.Enable {
		return CheckinResult{State: CheckinUnsupported, Msg: "签到未开启"}, nil
	}

	cl, err := p.checkinCall(c, a, traeCheckinClaimPath)
	if err != nil {
		return CheckinResult{State: CheckinFailed, Msg: err.Error()}, err
	}
	switch {
	case cl.Code == 0:
		credit := cl.ExtraCredits
		if credit == 0 {
			credit = cl.Credits
		}
		return CheckinResult{State: CheckinOK, Credit: credit, Msg: cl.Message}, nil
	case cl.Code == traeCheckinSoftBusyCode:
		// 9074 实测多为 x-device-id 格式不对（UUID 带横线）而非真限流。
		// 修好头之后仍可能出现，那时才是上游瞬时抖动 —— 归 Failed 让日志能看见。
		return CheckinResult{State: CheckinFailed, Msg: "x-device-id 可能不合规或上游限流：" + cl.Message}, nil
	default:
		return CheckinResult{State: CheckinFailed, Msg: cl.Message}, nil
	}
}

// checkinCall 发一次签到相关请求并解包。HTTP 非 2xx 视为错误，业务 code 交给调用方判断。
func (p *traeProvider) checkinCall(c *Client, a *auth.Auth, path string) (traeCheckinResp, error) {
	var out traeCheckinResp
	body := map[string]any{"req_source": traeCheckinReqSource}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	// 签到接口挂在 api.trae.cn（= refreshBase），与对话域 trae-api-cn.mchost.guru 不同。
	// 实测确认：POST api.trae.cn/trae/api/v2/ug/checkin_credits/status 返回 enable/checked_in。
	req, err := http.NewRequest(http.MethodPost, p.refreshBase(a)+path, bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	p.applyChatHeaders(req, a, "")
	// x-device-id 是 claim 的硬性要求，缺失/格式不对会返回 9004 或伪装成限流的 9074。
	req.Header.Set("x-device-id", traeCheckinDeviceID)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return out, &Error{Kind: Classify(resp.StatusCode, string(rawResp)), Status: resp.StatusCode, Msg: truncate(string(rawResp), 200)}
	}
	if err := json.Unmarshal(rawResp, &out); err != nil {
		return out, fmt.Errorf("签到响应解析失败: %w (body: %s)", err, truncate(string(rawResp), 120))
	}
	return out, nil
}

// ─── 额度查询 ───────────────────────────────────────────────────────────────

// traeEntitlementPath 额度接口路径（挂在 api.trae.cn，与 GetUserToken 同域）。
const traeEntitlementPath = "/trae/api/v2/pay/user_current_entitlement_list"

// traeIntlEntitlementBase / traeIntlEntitlementPath 国际版额度接口。
// 与国内版三处差异：独立域名（ug-normal）、v1 而非 v2、不需要 X-Cloudide-Session。
const traeIntlEntitlementBase = "https://ug-normal.trae.ai"
const traeIntlEntitlementPath = "/trae/api/v1/pay/user_current_entitlement_list"

// TraeUsage 账号积分快照。
//
// 口径说明：Remain/Used/Total 直接取上游 usage_summary，
// 不要把 user_entitlement_pack_list 自己累加——包里混有过期与已用完的项，
// 自行聚合会与页面数字对不上（页面的「总可用积分」用的就是 usage_summary）。
type TraeUsage struct {
	Remain   float64 // 总可用积分
	Used     float64 // 已消耗
	Total    float64 // 总额
	Ratio    float64 // 已用比例（0~1）
	PlanName string  // 当前方案（如「免费」），取不到为空
	Packages int     // 权益包数量
	// Unit 计量单位：国内版是「积分」；国际版按 is_dollar_usage_billing 可能是「USD」。
	// 不写死，否则管理台会把 $1.00 的美元额度显示成「积分」，看的人会误判成 1 积分。
	Unit string
}

// Usage 实现 Provider 的可选额度接口（管理台「查积分」用）。
//
// 请求头复用 applyChatHeaders：它已包含 Authorization（Cloud-IDE-JWT）、
// X-Cloudide-Session、X-Uid、x-app-id 与桌面端 UA，正是该接口要求的组合；
// 仅把 Accept 从 SSE 覆盖成 JSON。
func (p *traeProvider) Usage(c *Client, a *auth.Auth) (any, error) {
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("trae usage: 账号没有 accessToken，请先重新登录")
	}
	if p.isIntl(a) {
		return p.usageIntl(c, a)
	}
	body := []byte(`{"require_usage":true,"full_data":true}`)
	req, err := http.NewRequest(http.MethodPost, traeRefreshBase+traeEntitlementPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	p.applyChatHeaders(req, a, "")
	req.Header.Set("Accept", "application/json") // 覆盖 SSE 头，否则上游可能按流式返回

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae usage http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var out struct {
		UsageSummary struct {
			ConsumedAmount float64 `json:"consumed_amount"`
			TotalAmount    float64 `json:"total_amount"`
			Ratio          float64 `json:"consumption_ratio"`
		} `json:"usage_summary"`
		Packs []struct {
			DisplayDesc string `json:"display_desc"`
			Base        struct {
				ProductID   int `json:"product_id"`
				ProductType int `json:"product_type"`
			} `json:"entitlement_base_info"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("trae usage parse: %w", err)
	}
	remain := out.UsageSummary.TotalAmount - out.UsageSummary.ConsumedAmount
	if remain < 0 {
		remain = 0
	}
	// 方案名：订阅型包（entitlement_base_info.product_id=0 且 product_type=0，如「免费」）。
	// 不能直接取第一个包——列表按业务优先级排序，首项往往是「大学生认证奖励」这类积分包，
	// 不是订阅方案（已踩过一次：管理台把方案名错显成奖励名）。
	plan := ""
	for _, pk := range out.Packs {
		if pk.Base.ProductID == 0 && pk.Base.ProductType == 0 && pk.DisplayDesc != "" {
			plan = pk.DisplayDesc
			break
		}
	}
	return &TraeUsage{
		Remain:   remain,
		Used:     out.UsageSummary.ConsumedAmount,
		Total:    out.UsageSummary.TotalAmount,
		Ratio:    out.UsageSummary.Ratio,
		PlanName: plan,
		Packages: len(out.Packs),
		Unit:     "积分",
	}, nil
}

// usageIntl 国际版额度查询（trae.ai）。
//
// 与国内版的差异（2026-11-05 实测）：
//   - 域名独立：ug-normal.trae.ai，不是 api.trae.ai
//   - 路径是 v1（国内版 v2）
//   - 只认 Authorization: Cloud-IDE-JWT <jwt>，**不要**带 X-Cloudide-Session
//     （带上也不会错，但没必要；实测最小参数就是这一个头）
//   - 响应结构完全不同：没有 usage_summary，额度在
//     entitlement_base_info.quota.basic_usage_limit（上限）
//     与 usage.basic_usage_amount（已用），都是美元口径
//
// 返回同样的 *TraeUsage，让上层（/usage 接口、管理台）不用分区域处理。
func (p *traeProvider) usageIntl(c *Client, a *auth.Auth) (any, error) {
	jwt := strings.TrimSpace(a.AccessToken)
	body := []byte(`{"require_usage":true}`)
	req, err := http.NewRequest(http.MethodPost, traeIntlEntitlementBase+traeIntlEntitlementPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+jwt)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae intl usage http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var out struct {
		IsDollarBilling bool `json:"is_dollar_usage_billing"`
		Packs           []struct {
			DisplayDesc string `json:"display_desc"`
			Status      int    `json:"status"`
			Base        struct {
				ProductID   int `json:"product_id"`
				ProductType int `json:"product_type"`
				Quota       struct {
					BasicUsageLimit float64 `json:"basic_usage_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				BasicUsageAmount float64 `json:"basic_usage_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("trae intl usage parse: %w", err)
	}

	// 取第一条 status==1（有效）的套餐；没有就退回第一条。
	// 不能盲目累加所有包：列表里混着已过期/已用完的项，自行聚合会与页面数字对不上
	pick := -1
	for i := range out.Packs {
		if out.Packs[i].Status == 1 {
			pick = i
			break
		}
	}
	if pick < 0 && len(out.Packs) > 0 {
		pick = 0
	}
	if pick < 0 {
		return &TraeUsage{}, nil
	}
	pk := out.Packs[pick]

	// 单位：is_dollar_usage_billing=true 表示额度以美元计（如 $1.00），
	// 否则按积分口径展示。写死「积分」会把 $0.01/$1.00 显示成 0.01/1 积分。
	unit := "积分"
	if out.IsDollarBilling {
		unit = "USD"
	}

	remain := pk.Base.Quota.BasicUsageLimit - pk.Usage.BasicUsageAmount
	if remain < 0 {
		remain = 0
	}
	ratio := 0.0
	if pk.Base.Quota.BasicUsageLimit > 0 {
		ratio = pk.Usage.BasicUsageAmount / pk.Base.Quota.BasicUsageLimit
	}

	// 方案名：订阅型包（product_id=0 且 product_type=0）才是方案本身，
	// 首项往往是奖励包（同国内版的坑）。
	plan := ""
	for _, q := range out.Packs {
		if q.Base.ProductID == 0 && q.Base.ProductType == 0 && q.DisplayDesc != "" {
			plan = q.DisplayDesc
			break
		}
	}

	return &TraeUsage{
		Remain:   remain,
		Used:     pk.Usage.BasicUsageAmount,
		Total:    pk.Base.Quota.BasicUsageLimit,
		Ratio:    ratio,
		PlanName: plan,
		Packages: len(out.Packs),
		Unit:     unit,
	}, nil
}
