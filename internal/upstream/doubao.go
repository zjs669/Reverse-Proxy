// doubao.go 豆包（Doubao，字节跳动）网页版供应商实现。
//
// 上游：https://www.doubao.com
//   POST /chat/completion            对话（SSE，免签名新版端点）
//   POST /passport/account/info/v2   会话存活校验（RefreshToken 复用）
//
// 凭证来源（auth JSON）：
//   accessToken = sessionid（Cookie 鉴权，浏览器登录态）
//   extra.ttwid / extra.passport_csrf_token：可选配套 Cookie
//   extra.device_id / extra.web_id / extra.fp：可选设备指纹覆盖（缺省用内置常量）
//
// 协议要点（逆向所得，仅供个人学习研究）：
//   - 鉴权走 Cookie（sessionid 必需；ttwid / passport_csrf_token 配套）
//   - 请求体为 client_meta / messages[].content_block(block_type=10000) / option / ext 结构
//   - 必须带 Agw-Js-Conv: str 头 / Referer: https://www.doubao.com/chat
//   - SSE 事件名如 SSE_ACK / json / message / done / gateway-error；
//     内容在 data 的 content 字段（常是嵌套 JSON 字符串），含 block_type=10040 思考标记
//
// 注意：豆包网页模型不支持 Function Calling / Tool Use（无法作为编程智能体后端），
// 故本实现不实现工具调用透传——请求体里的 tools 会被忽略。
package upstream

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"reverse-proxy/internal/auth"
)

const (
	doubaoChatBase   = "https://www.doubao.com"
	doubaoChatPath   = "/chat/completion"
	doubaoSessionAPI = "/passport/account/info/v2"
	doubaoBotID      = "7338286299411103781"

	// 内置默认设备指纹（单个浏览器会话视角，保持稳定比每次随机更逼真）。
	doubaoDefDeviceID = "714003710229497"
	doubaoDefWebID    = "7604137868021548590"
)

// doubaoProvider 实现 Provider 接口（豆包网页版）。
type doubaoProvider struct {
	c *Client

	mu       sync.RWMutex
	modelMap map[string]string // client 模型名 -> 上游模型名（豆包多为直通，留作扩展）
}

func (p doubaoProvider) Name() string { return "doubao" }

// doubaoItemAlias 上游 model_item_key → 对外的稳定模型 ID。
// key 来自 /chat/ 页面下发的 menu_item_list（豆包官方模型菜单，随前端版本变化）：
//
//	"0"            快速
//	"3"            专家
//	"9"            工作任务 Auto
//	"seed-lite-7b" 豆包 2.1 Lite
//	"5"            工作任务 Pro
//
// 出现未知 key 时按 "doubao-" + key 兜底，保证不因官方新增模型而丢项。
var doubaoItemAlias = map[string]string{
	"0":           "doubao-fast",
	"3":           "doubao-expert",
	"9":           "doubao-auto",
	"seed-lite-7b": "doubao-2.1-lite",
	"5":           "doubao-2.1-pro",
}

// doubaoDefaultItemKey 官方默认模型（menu_item_list 的 default_select_model_item_key）。
const doubaoDefaultItemKey = "0"

// doubaoModelID 把上游 model_item_key 转成对外模型 ID。
func doubaoModelID(key string) string {
	key = strings.TrimSpace(key)
	if id, ok := doubaoItemAlias[key]; ok {
		return id
	}
	if key == "" {
		return ""
	}
	if strings.HasPrefix(key, "doubao-") {
		return key
	}
	return "doubao-" + key
}

// doubaoModelKey 把客户端模型 ID 还原成上游 model_item_key。
// 兼容三种写法：别名（doubao-2.1-lite）、裸 key（seed-lite-7b / 5）、以及旧版 seed 命名。
func doubaoModelKey(clientModel string) string {
	m := strings.ToLower(strings.TrimSpace(clientModel))
	if m == "" {
		return doubaoDefaultItemKey
	}
	// 1) 别名精确命中
	for key, id := range doubaoItemAlias {
		if id == m {
			return key
		}
	}
	// 2) 裸 key（含 "doubao-" 前缀时剥掉再判）
	if _, ok := doubaoItemAlias[m]; ok {
		return m
	}
	if bare := strings.TrimPrefix(m, "doubao-"); bare != m {
		if _, ok := doubaoItemAlias[bare]; ok {
			return bare
		}
		return bare // 官方新增模型：id 就是 "doubao-"+key，原样还原
	}
	// 3) 旧版命名兜底映射到当前等价档位
	switch {
	case strings.Contains(m, "lite"), strings.Contains(m, "1.5"), strings.Contains(m, "seed-2.0"):
		return "seed-lite-7b"
	case strings.Contains(m, "thinking"), strings.Contains(m, "expert"):
		return "3"
	case strings.Contains(m, "pro"), strings.Contains(m, "vision"):
		return "5"
	}
	return doubaoDefaultItemKey
}

// StaticModels 动态目录拉取失败时的回退表（对齐官方 menu_item_list 当前 5 个档位）。
func (p doubaoProvider) StaticModels() []map[string]any {
	ids := []string{
		"doubao-fast", "doubao-expert", "doubao-auto",
		"doubao-2.1-lite", "doubao-2.1-pro",
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{
			"id":             id,
			"object":         "model",
			"created":        1753600000,
			"owned_by":       "doubao",
			"context_length": 131072,
		})
	}
	return out
}

// Classify 豆包错误分类（复用通用分类，补充豆包限流码 710022002/710022004）。
func (p doubaoProvider) Classify(status int, body string) ErrKind {
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
	// 豆包私有限流码（伪造 msToken 等会触发 710022002）。
	if strings.Contains(body, "710022002") || strings.Contains(body, "710022004") {
		return ErrSoftRate
	}
	lower := strings.ToLower(body)
	for _, m := range []string{"rate limit", "too many requests", "请求过于频繁", "限流"} {
		if strings.Contains(lower, m) {
			return ErrSoftRate
		}
	}
	for _, m := range []string{"quota", "credit", "额度", "余额", "登录", "login", "session"} {
		if strings.Contains(lower, m) {
			return ErrHardCredit
		}
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// resolveModel 把客户端模型名解析为上游模型名（豆包基本直通）。
func (p *doubaoProvider) resolveModel(clientModel string) string {
	clientModel = strings.TrimSpace(clientModel)
	if clientModel == "" {
		clientModel = "doubao-seed-2.0"
	}
	p.mu.RLock()
	mapped, ok := p.modelMap[clientModel]
	p.mu.RUnlock()
	if ok {
		return mapped
	}
	return clientModel
}

// RefreshToken 豆包用 Cookie(sessionid) 鉴权，无 refresh token 机制；此处做会话存活校验。
// 会话失效（需重新抓 sessionid）返回错误，由网关禁用该账号；存活则无需更新 token。
func (p doubaoProvider) RefreshToken(c *Client, a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("doubao: empty sessionid")
	}
	u, _ := url.Parse(doubaoChatBase + doubaoSessionAPI)
	q := u.Query()
	q.Set("account_sdk_source", "web")
	u.RawQuery = q.Encode()
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://www.doubao.com/chat")
	req.Header.Set("Cookie", "sessionid="+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("doubao session check http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			UserID string `json:"user_id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil {
		// user_id 缺失且业务码非 0 → 会话失效
		if env.Data.UserID == "" && env.Code != 0 {
			return fmt.Errorf("doubao session invalid: %s", env.Message)
		}
	}
	// 存活：sessionid 即凭证，无需更新字段（保持 ExpiresAt 不清零，避免被误判过期）
	return nil
}

// FetchModels 动态拉取豆包官方模型目录。
//
// 豆包没有「模型列表」OpenAPI，但 /chat/ 页面首屏 HTML 里内联了下发的
// `"menu_item_list":[...]`（前端模型下拉菜单的数据源），字段含
// item_id / menu_type / model_item_key / name / agent_mode。
// 这里直接带登录 Cookie 取该页面，定位该键后用 json.Decoder 解析出数组，
// 从而让 /v1/models 随官方模型更新自动变化，不必改代码。
func (p doubaoProvider) FetchModels(c *Client, a *auth.Auth) ([]ModelInfo, error) {
	infos, err := p.fetchMenuModels(c, a)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("doubao: menu_item_list 为空")
	}
	return infos, nil
}

// doubaoMenuItem /chat/ 页面 menu_item_list 的单项。
type doubaoMenuItem struct {
	ItemID       int    `json:"item_id"`
	MenuType     int    `json:"menu_type"`
	ModelItemKey string `json:"model_item_key"`
	Name         string `json:"name"`
	AgentMode    int    `json:"agent_mode"`
	ExtraParams  struct {
		TotalWindowSize string `json:"total_window_size"`
	} `json:"model_extra_params"`
}

// fetchMenuModels 抓 /chat/ 页面并解析 menu_item_list。
func (p doubaoProvider) fetchMenuModels(c *Client, a *auth.Auth) ([]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, doubaoChatBase+"/chat/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", p.cookie(a))
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", "https://www.doubao.com/chat/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("doubao: GET /chat/ http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	const anchor = `"menu_item_list":`
	idx := bytes.Index(body, []byte(anchor))
	if idx < 0 {
		return nil, fmt.Errorf("doubao: 页面里没有 menu_item_list（未登录或前端改版）")
	}
	dec := json.NewDecoder(bytes.NewReader(body[idx+len(anchor):]))
	var items []doubaoMenuItem
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("doubao: 解析 menu_item_list: %w", err)
	}
	out := make([]ModelInfo, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		id := doubaoModelID(it.ModelItemKey)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(it.Name)
		if name == "" {
			name = id
		}
		ctxWin := int64(0)
		if n, err := strconv.ParseInt(it.ExtraParams.TotalWindowSize, 10, 64); err == nil {
			ctxWin = n
		}
		out = append(out, ModelInfo{ID: id, Name: name, ContextWindow: ctxWin})
	}
	return out, nil
}

// doubaoCookie 由 auth 组装 Cookie 头。
func (p doubaoProvider) cookie(a *auth.Auth) string {
	parts := []string{"sessionid=" + strings.TrimSpace(a.AccessToken)}
	if v := strings.TrimSpace(a.ExtraVal("ttwid")); v != "" {
		parts = append(parts, "ttwid="+v)
	}
	if v := strings.TrimSpace(a.ExtraVal("passport_csrf_token")); v != "" {
		parts = append(parts, "passport_csrf_token="+v)
	}
	return strings.Join(parts, "; ")
}

// doubaoDeviceID 取设备 ID（extra 优先，否则内置常量）。
func (p doubaoProvider) deviceID(a *auth.Auth) string {
	if v := strings.TrimSpace(a.ExtraVal("device_id")); v != "" {
		return v
	}
	return doubaoDefDeviceID
}

func (p doubaoProvider) webID(a *auth.Auth) string {
	if v := strings.TrimSpace(a.ExtraVal("web_id")); v != "" {
		return v
	}
	return doubaoDefWebID
}

// ChatStream 发起豆包对话，返回原生 SSE 流（由 StreamTo/Aggregate 翻译）。
func (p doubaoProvider) ChatStream(c *Client, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return nil, 0, nil, fmt.Errorf("doubao: parse request: %w", err)
	}
	clientModel, _ := reqBody["model"].(string)
	// 模型档位：把客户端模型名还原成 /chat/ 页面 menu_item_list 里的 model_item_key。
	// ⚠️ 实测（2026-11-02）上游 /chat/completion 并不认这个字段：报文里无论传
	// doubao-fast / doubao-2.1-lite / doubao-2.1-pro，响应回显的 model_id 恒为 38、
	// bot_id 恒为 doubaoBotID，即实际仍跑账号默认档。此处保留注入作为后续定位
	// 「模型档位 → 报文」的挂点，真实切换字段待抓包确认（可能是 menu_type / agent_mode）。
	modelKey := doubaoModelKey(clientModel)
	_ = p.resolveModel(clientModel)

	msgs, _ := reqBody["messages"].([]any)
	doubaoMsgs := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			doubaoMsgs = append(doubaoMsgs, mm)
		}
	}
	doubaoMsgs = p.buildMessages(doubaoMsgs)

	// 思考档位：need_deep_think 0=快思 1=深思 2=自动 3=专家
	needDeep := 0
	if re, ok := reqBody["reasoning_effort"].(string); ok && re != "" {
		switch strings.ToLower(re) {
		case "low", "fast":
			needDeep = 0
		case "medium", "think", "deep":
			needDeep = 1
		case "high", "expert":
			needDeep = 3
		case "auto":
			needDeep = 2
		}
	}

	payload := map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": "local_" + doubaoHex(16),
			"conversation_id":       "",
			"bot_id":                doubaoBotID,
			"last_section_id":       "",
			"last_message_index":    0,
		},
		"messages": doubaoMsgs,
		"option": map[string]any{
			"model_item_key":            modelKey,
			"send_message_scene":        "",
			"create_time_ms":            0,
			"collect_id":                "",
			"is_audio":                  false,
			"answer_with_suggest":       true,
			"tts_switch":                false,
			"need_deep_think":           needDeep,
			"click_clear_context":       false,
			"from_suggest":              false,
			"is_regen":                  false,
			"is_replace":                false,
			"disable_sse_cache":         false,
			"select_text_action":        "",
			"resend_for_regen":          false,
			"scene_type":                0,
			"unique_key":                doubaoUUID(),
			"start_seq":                 0,
			"need_create_conversation":  true,
			"conversation_init_option":  map[string]any{"need_ack_conversation": true},
			"regen_query_id":            []any{},
			"edit_query_id":             []any{},
			"regen_instruction":         "",
			"no_replace_for_regen":      false,
			"message_from":              0,
			"shared_app_name":           "",
			"action_bar_skill_id":       0,
			"sse_recv_event_options":    map[string]any{"support_chunk_delta": true},
			"is_ai_playground":          false,
		},
		"chat_ability": map[string]any{},
		"ext": map[string]any{
			"use_deep_think":               fmt.Sprintf("%d", needDeep),
			"use_submit_pipeline":          "1",
			"commerce_credit_config_enable": "0",
			"sub_conv_firstmet_type":       "1",
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("doubao: marshal body: %w", err)
	}

	u, _ := url.Parse(doubaoChatBase + doubaoChatPath)
	q := u.Query()
	q.Set("aid", "582478")
	q.Set("real_aid", "582478")
	q.Set("device_id", p.deviceID(a))
	q.Set("tea_uuid", p.deviceID(a))
	q.Set("web_id", p.webID(a))
	q.Set("device_platform", "web")
	q.Set("language", "zh")
	q.Set("region", "CN")
	q.Set("sys_region", "CN")
	q.Set("pkg_type", "release_version")
	q.Set("version_code", "20800")
	q.Set("pc_version", "2.1.7")
	q.Set("client_platform", "pc_client")
	q.Set("runtime", "web")
	q.Set("samantha_web", "1")
	q.Set("use-olympus-account", "1")
	q.Set("web_tab_id", doubaoUUID())
	if fp := strings.TrimSpace(a.ExtraVal("fp")); fp != "" {
		q.Set("fp", fp)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", "https://www.doubao.com")
	req.Header.Set("Referer", "https://www.doubao.com/chat")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	req.Header.Set("Agw-Js-Conv", "str") // 新版 /chat/completion 需要
	req.Header.Set("Cookie", p.cookie(a))

	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		log.Printf("ERR: [doubao] chat uid=%s: transport error: %v", logUID(a.UID), err)
		return nil, 0, nil, transportErrHint(err)
	}
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := p.Classify(resp.StatusCode, string(rb))
		log.Printf("WARN: [doubao] chat uid=%s: upstream %d %s body=%s", logUID(a.UID), resp.StatusCode, kind, truncate(string(rb), 200))
		return nil, resp.StatusCode, rb, nil
	}
	return monitorBody(resp.Body, c.IdleTimeout, nil), resp.StatusCode, nil, nil
}

// buildMessages 把 OpenAI 消息映射为豆包 messages[]（每个非工具消息一个元素，单文本块）。
// 豆包网页协议无显式 role 字段，按出现顺序即对话时序；system 内容折叠进首条文本。
func (p doubaoProvider) buildMessages(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	systemPrefix := ""
	for _, m := range msgs {
		role, _ := m["role"].(string)
		if role == "tool" {
			continue // 豆包不支持工具调用，丢弃工具结果
		}
		text := openAIText(m)
		if role == "system" {
			systemPrefix += text + "\n\n"
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if systemPrefix != "" {
			text = systemPrefix + text
			systemPrefix = ""
		}
		out = append(out, map[string]any{
			"local_message_id": doubaoUUID(),
			"content_block": []map[string]any{
				doubaoTextBlock(text),
			},
			"message_status": 0,
		})
	}
	if systemPrefix != "" && len(out) > 0 {
		// system 出现在末尾（少见）：追加到首个消息
		if blocks, ok := out[0]["content_block"].([]map[string]any); ok && len(blocks) > 0 {
			if tb, ok := blocks[0]["content"].(map[string]any); ok {
				if tblk, ok := tb["text_block"].(map[string]any); ok {
					if t, ok := tblk["text"].(string); ok {
						tblk["text"] = systemPrefix + t
					}
				}
			}
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{
			"local_message_id": doubaoUUID(),
			"content_block":    []map[string]any{doubaoTextBlock("")},
			"message_status":   0,
		})
	}
	return out
}

// doubaoTextBlock 构造一个文本内容块（block_type=10000）。
func doubaoTextBlock(text string) map[string]any {
	return map[string]any{
		"block_type": 10000,
		"content": map[string]any{
			"text_block": map[string]any{
				"text":        text,
				"icon_url":    "",
				"icon_url_dark": "",
				"summary":     "",
			},
			"pc_event_block": "",
		},
		"block_id":     doubaoUUID(),
		"parent_id":    "",
		"meta_info":    []any{},
		"append_fields": []any{},
		"is_finish":    true,
		"patch_type":   2,
	}
}

// openAIText 从 OpenAI 消息里提取纯文本（string / parts 数组 / 多模态 content）。
func openAIText(m map[string]any) string {
	switch c := m["content"].(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, item := range c {
			if mm, ok := item.(map[string]any); ok {
				if t, ok := mm["text"].(string); ok {
					sb.WriteString(t)
				} else if t, ok := mm["content"].(string); ok {
					sb.WriteString(t)
				}
			} else if s, ok := item.(string); ok {
				sb.WriteString(s)
			}
		}
		return sb.String()
	}
	return ""
}

// StreamTo 把豆包原生 SSE 翻译为 OpenAI SSE 写入 w。
func (p doubaoProvider) StreamTo(w http.ResponseWriter, rc io.Reader, model string) error {
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
	stopped := false
	err := doubaoWalk(rc, model, id, created,
		func(content, reasoning, finish string) error {
			if finish != "" {
				stopped = true
				return writeChunk(map[string]any{}, finish, lastUsage)
			}
			delta := map[string]any{}
			if content != "" {
				delta["content"] = content
			}
			if reasoning != "" {
				delta["reasoning_content"] = reasoning
			}
			return writeChunk(delta, "", nil)
		},
		func(usage map[string]any) {
			lastUsage = usage
		})
	if err != nil {
		return err
	}
	// doubaoWalk 正常收尾时 flushFinal 已发过 stop（stopped=true），此处不再重复，
	// 否则客户端会收到两个 finish_reason:"stop"。只有异常路径（没走到 flushFinal）才补一次。
	if !stopped {
		_ = writeChunk(map[string]any{}, "stop", lastUsage)
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	return nil
}

// Aggregate 把豆包原生 SSE 聚合成单个 OpenAI 响应对象。
func (p doubaoProvider) Aggregate(rc io.Reader, model string) (map[string]any, error) {
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := float64(time.Now().Unix())
	var (
		content   strings.Builder
		reasoning strings.Builder
		finish    = "stop"
		usage     map[string]any
	)
	err := doubaoWalk(rc, model, id, created, func(c, r, f string) error {
		content.WriteString(c)
		reasoning.WriteString(r)
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
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
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

// doubaoWalk 逐帧解析豆包原生 SSE，回调 onDelta(正文, 思考, finish) 与 onUsage。
func doubaoWalk(r io.Reader, model, id string, created float64, onDelta func(content, reasoning, finish string) error, onUsage func(map[string]any)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var inThinking bool
	var finalText string
	var ttsText string
	var finished bool
	// flushFinal 幂等发送 stop（正文已在前面的 patch 循环中逐块 emit）。
	// 若 content_block 全程未产出正文（极罕见：该会话仅走 tts_content 流），用兜底 ttsText 一次性补发。
	flushFinal := func() {
		if !finished {
			finished = true
			if finalText == "" && ttsText != "" {
				_ = onDelta(ttsText, "", "")
			}
			_ = onDelta("", "", "stop")
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(trimmed, "data:") {
			if err == io.EOF {
				return nil
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" {
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		if payload == "[DONE]" {
			flushFinal()
			return nil
		}
		var env map[string]any
		if json.Unmarshal([]byte(payload), &env) != nil {
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		// 错误事件
		if ev, _ := env["event"].(string); ev == "gateway-error" || ev == "STREAM_ERROR" || ev == "GATEWAY_ERROR" {
			msg, _ := env["message"].(string)
			if msg == "" {
				msg, _ = env["error"].(string)
			}
			if msg == "" {
				msg = fmt.Sprintf("%v", env["code"])
			}
			return fmt.Errorf("doubao upstream error: %s", msg)
		}
		// Samantha 风格 event_type
		if et, ok := num(env, "event_type"); ok {
			switch int(et) {
			case 2003:
				flushFinal()
				if err == io.EOF {
					return nil
				}
				continue
			case 2005:
				return fmt.Errorf("doubao upstream error: %v", env["message"])
			}
		}
		// 结束事件（type=done/finish 或 end_type=1 回答完成）
		if t, _ := env["type"].(string); t == "done" || t == "finish" {
			flushFinal()
			if err == io.EOF {
				return nil
			}
			continue
		}
		if et, ok := num(env, "end_type"); ok && int(et) == 1 {
			// 若前面 patch 循环未收集到正文（极少数情况），用 brief 兜底。
			if finalText == "" {
				if b, ok := env["msg_finish_attr"].(map[string]any); ok {
					if s, ok := b["brief"].(string); ok && s != "" {
						_ = onDelta(s, "", "")
					}
				}
			}
			flushFinal()
			if err == io.EOF {
				return nil
			}
			continue
		}
		// 用量
		if hasUsageKeys(env) {
			onUsage(traeUsage(env))
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		// 豆包正文以 STREAM_CHUNK 的 patch_op 增量投递，且存在两条并行流：
		//   - patch_object=1 帧：patch_value.content_block[].content.text_block.text（真实可见正文，权威来源）
		//   - patch_object=111 帧：patch_value.tts_content（同源 TTS 语音文本，与正文逐字相同）
		// 二者分别在不同帧到达、内容完全一致。若同时 emit 会造成整段重复，
		// 故以 content_block 为正文唯一来源，tts_content 仅静默收集、作为无正文时的兜底。
		if pops, ok := env["patch_op"].([]any); ok {
			var chunks []string
			for _, pop := range pops {
				pm, ok := pop.(map[string]any)
				if !ok {
					continue
				}
				pv, ok := pm["patch_value"].(map[string]any)
				if !ok {
					continue
				}
				if s := doubaoBlockText(pv); s != "" {
					chunks = append(chunks, s)
				} else if s, ok := pv["tts_content"].(string); ok && s != "" {
					ttsText += s
				}
			}
			for _, ch := range chunks {
				finalText += ch
				if e := onDelta(ch, "", ""); e != nil {
					return e
				}
			}
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		// 内容提取
		inner, _ := doubaoInner(env)
		bt := blockTypeOf(inner)
		if bt == 0 {
			bt = blockTypeOf(env)
		}
		toggle := bt == 10040
		text := doubaoTextFrom(inner)
		if text == "" {
			text = doubaoTextFrom(env)
		}
		reasoning := pickStr(inner, "think", "reasoning", "reasoning_content")
		if reasoning == "" {
			reasoning = pickStr(env, "think", "reasoning", "reasoning_content")
		}
		content := text
		if toggle {
			inThinking = !inThinking
		}
		if inThinking && content != "" && reasoning == "" {
			reasoning = content
			content = ""
		}
		if content == "" && reasoning == "" {
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		if e := onDelta(content, reasoning, ""); e != nil {
			return e
		}
		if err == io.EOF {
			flushFinal()
			return nil
		}
	}
}

// doubaoInner 把 data.content（可能是 JSON 字符串或对象）归一为 map。
func doubaoInner(env map[string]any) (map[string]any, bool) {
	if c, ok := env["content"].(string); ok && c != "" {
		var inner map[string]any
		if json.Unmarshal([]byte(c), &inner) == nil && inner != nil {
			return inner, true
		}
	}
	if c, ok := env["content"].(map[string]any); ok {
		return c, true
	}
	return env, false
}

// blockTypeOf 取 block_type / content_type（二者其一标记块类型）。
func blockTypeOf(obj map[string]any) float64 {
	if v, ok := num(obj, "block_type"); ok {
		return v
	}
	if v, ok := num(obj, "content_type"); ok {
		return v
	}
	return 0
}

// doubaoTextFrom 从对象里取正文文本。
//
// 兼容多形态：
//   - text / content(string) / message(string) / content.text
//   - tts_content：豆包 STREAM_MSG_NOTIFY 帧携带的「完整 TTS 文本」，即答案全文
//   - content_block[]：豆包正文块数组，逐块取 content.text_block.text 拼接
func doubaoTextFrom(obj map[string]any) string {
	if v, ok := obj["text"].(string); ok && v != "" {
		return v
	}
	if v, ok := obj["content"].(string); ok && v != "" {
		return v
	}
	if v, ok := obj["message"].(string); ok && v != "" {
		return v
	}
	if v, ok := obj["tts_content"].(string); ok && v != "" {
		return v
	}
	if c, ok := obj["content"].(map[string]any); ok {
		if t, ok := c["text"].(string); ok && t != "" {
			return t
		}
	}
	if blocks, ok := obj["content_block"].([]any); ok {
		var sb strings.Builder
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			inner, ok := bm["content"].(map[string]any)
			if !ok {
				continue
			}
			tb, ok := inner["text_block"].(map[string]any)
			if !ok {
				continue
			}
			if t, ok := tb["text"].(string); ok && t != "" {
				sb.WriteString(t)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
}

// doubaoBlockText 仅从 content_block[] 取正文（不回退到 tts_content），
// 专供 patch_op 正文流使用，避免与并行的 tts_content 语音流重复发射。
func doubaoBlockText(obj map[string]any) string {
	if blocks, ok := obj["content_block"].([]any); ok {
		var sb strings.Builder
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			inner, ok := bm["content"].(map[string]any)
			if !ok {
				continue
			}
			tb, ok := inner["text_block"].(map[string]any)
			if !ok {
				continue
			}
			if t, ok := tb["text"].(string); ok && t != "" {
				sb.WriteString(t)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	return ""
}

// doubaoUUID 生成标准 UUID v4 字符串。
func doubaoUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// doubaoHex 返回 n 字节的十六进制串（用于 local_conversation_id 等）。
func doubaoHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, n*2)
	for _, by := range b {
		out = append(out, hexd[by>>4], hexd[by&0x0f])
	}
	return string(out)
}
