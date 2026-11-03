// deepseek.go DeepSeek 网页版（chat.deepseek.com）供应商实现。
//
// 上游端点（2026-11-02 浏览器抓包 MCP 实测）：
//   POST /api/v0/chat_session/create          新建会话 → chat_session.id（UUID）+ ttl_seconds=259200
//   POST /api/v0/chat/create_pow_challenge    申请 PoW（求解见 deepseek_pow.go）
//   POST /api/v0/chat/completion              对话（SSE）
//   GET  /api/v0/chat/history_messages        取会话消息树（对齐 parent_message_id）
//   GET  /api/v0/client/settings?scope=model  模型档位表（model_configs）
//   GET  /api/v0/users/current                会话存活校验（RefreshToken 复用）
//
// 凭证来源（auth JSON，provider="deepseek"）：
//   accessToken = userToken（浏览器 localStorage 的 userToken.value，或 Network 里
//                 "Authorization: Bearer xxx" 后面那串）
//   extra.device_id   可选，缺省用进程内随机值
//
// 协议要点（全部经实测确认，不是推测）：
//   - 鉴权只需 Authorization: Bearer <userToken>；Cookie 与 x-device-id 均可缺省
//   - 每个 /chat/completion 请求都要新解一次 PoW：同一个 x-ds-pow-response
//     复用第二次返回 40301 INVALID_POW_RESPONSE
//   - chat_session_id 必须是 UUID：空串 → 422 "UUID parsing failed"，null → 422
//   - parent_message_id 首轮必须 null；之后传上轮的 response_message_id（2,4,6…）。
//     传 0 或不存在 id → biz_code 26 "invalid message id"
//   - HTTP 状态恒 200，错误在 body：code 是全局码（40003 token 失效 / 40300 缺 PoW /
//     40301 PoW 无效 / 40029 IP 限制 / 40012 封号 / 50006 禁言），
//     data.biz_code 是业务码（1=会话不存在，26=消息 id 无效，15=思考额度超限…）
//   - SSE 首帧 {"v":{"response":{...}}} 是全量快照，之后
//     {"p":"response/fragments/-1/content","o":"APPEND","v":"..."} 是增量
//   - 模型档位 model_type ∈ default / expert / vision；思考开关是独立布尔 thinking_enabled
//   - 网关**默认开启**深度思考与联网搜索；上游错误码表里没有「搜索与思考互斥」，
//     只有 search/thinking 与**文件**互斥（本实现不传 ref_file_ids，故安全）
//
// 上下文策略：
//   - 请求体带会话键（metadata.conversation_id 等）→ 复用 chat_session_id +
//     parent_message_id，prompt 只放本轮问题；上游按会话树自动带全部前文。
//   - 无会话键 → 每次新建会话，prompt 拼全部历史（"每次导入前面所有输入"）。
//
// 不支持 Function Calling：网页版无 tools 透传，请求体里的 tools 会被忽略。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/dsconv"
	"reverse-proxy/internal/session"
)

const (
	deepseekBase          = "https://chat.deepseek.com"
	deepseekCompletion    = "/api/v0/chat/completion"
	deepseekPowChallenge  = "/api/v0/chat/create_pow_challenge"
	deepseekSessionCreate = "/api/v0/chat_session/create"
	deepseekHistoryPath   = "/api/v0/chat/history_messages"
	deepseekSettingsPath  = "/api/v0/client/settings"
	deepseekUsersCurrent  = "/api/v0/users/current"

	// 浏览器实测的客户端标识（缺任一项上游仍可用，保持与真实前端一致更稳）。
	deepseekClientBundle   = "com.deepseek.chat"
	deepseekClientPlatform = "web"
	deepseekClientVersion  = "2.7.0"
	deepseekUA             = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
)

// DeepSeek 业务码（data.biz_code）。
const (
	dsBizInvalidSession   = 1  // invalid chat session id
	dsBizThinkingWithFile = 13 // THINKING_CANNOT_BE_WITH_FILE
	dsBizThinkingQuota    = 15 // THINKING_QUOTA_EXCEEDED
	dsBizInvalidParent    = 26 // invalid message id
)

// DeepSeek 全局码（顶层 code）。
const (
	dsCodeInvalidToken = 40003
	dsCodeBanned       = 40012
	dsCodeIpRestricted = 40029
	dsCodePowMissing   = 40300
	dsCodePowInvalid   = 40301
	dsCodeMuted        = 50006
)

// deepseekProvider 实现 Provider 接口（DeepSeek 网页版）。
// 用指针注册：会话状态需在多次请求间共享（见 Client.deepSeekStore）。
type deepseekProvider struct {
	c *Client
}

func (p *deepseekProvider) Name() string { return "deepseek" }

// StaticModels 动态目录拉取失败时的回退表。
// 取自浏览器 settings?scope=model 的 model_configs（2026-11-02）。
func (p *deepseekProvider) StaticModels() []map[string]any {
	list := []struct {
		id, name string
		ctx      int64
	}{
		{"deepseek-instant", "Instant", 2621440},
		{"deepseek-expert", "Expert", 2621440},
		{"deepseek-vision", "Vision", 2621440},
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		out = append(out, map[string]any{
			"id":             e.id,
			"object":         "model",
			"created":        1753600000,
			"owned_by":       "deepseek",
			"context_length": e.ctx,
		})
	}
	return out
}

// deepseekModelType 把客户端模型名还原成上游 model_type。
// 兼容别名（deepseek-instant）、裸档位（default/expert/vision）、口语写法（instant）。
func deepseekModelType(clientModel string) string {
	m := strings.ToLower(strings.TrimSpace(clientModel))
	if m == "" {
		return "default"
	}
	switch m {
	case "deepseek-instant", "instant", "default", "deepseek-default", "deepseek-chat", "chat":
		return "default"
	case "deepseek-expert", "expert":
		return "expert"
	case "deepseek-vision", "vision":
		return "vision"
	}
	if bare := strings.TrimPrefix(m, "deepseek-"); bare != m {
		switch bare {
		case "instant", "default":
			return "default"
		case "expert":
			return "expert"
		case "vision":
			return "vision"
		}
	}
	return "default"
}

// deepseekModelID 把上游 model_type 转成对外模型 ID。
func deepseekModelID(modelType string) string {
	switch strings.ToLower(strings.TrimSpace(modelType)) {
	case "expert":
		return "deepseek-expert"
	case "vision":
		return "deepseek-vision"
	default:
		return "deepseek-instant"
	}
}

// deepseekThinking 决定 thinking_enabled（深度思考）。
//
// **默认开启**：DeepSeek 网页版的思考模式对推理题提升明显，网关按「默认开」
// 发请求。客户端要关可显式传：
//   - {"thinking": false}            最直观
//   - {"thinking_enabled": false}    同义（与上游字段同名）
//   - {"reasoning_effort": "none"}   也认（兼容 OpenAI 风格）
// 其余情况（含 low/medium/high/未传）一律开启——DeepSeek 只有开/关两档，
// 没有中间档位，把 low 映射成关闭会让客户端「要更强的推理反而更弱」。
func deepseekThinking(reqBody map[string]any) bool {
	if v, ok := reqBody["thinking"].(bool); ok {
		return v
	}
	if v, ok := reqBody["thinking_enabled"].(bool); ok {
		return v
	}
	if raw, ok := reqBody["reasoning_effort"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "none", "off", "false", "disabled":
			return false
		}
	}
	return true
}

// deepseekSearch 决定 search_enabled（联网搜索）。
//
// **默认开启**：网页版前端默认就是开着的（settings 里 search_state_on_launch="on"），
// 且错误码表里没有「搜索与思考互斥」，只有 search 与**文件**互斥
// （SEARCH_CANNOT_WITH_FILE）——本实现不传文件，因此可安全同时开启。
// 客户端要关可传 {"search": false} 或 {"search_enabled": false}。
func deepseekSearch(reqBody map[string]any) bool {
	if v, ok := reqBody["search"].(bool); ok {
		return v
	}
	if v, ok := reqBody["search_enabled"].(bool); ok {
		return v
	}
	return true
}

// Classify DeepSeek 错误分类。
//
// 关键点：DeepSeek 的业务错误**恒为 HTTP 200 + JSON 信封**，所以必须先按信封判，
// 再按 HTTP 状态码兜底。顺序反过来（先判 status>=500）会让调用方为了让 handler
// 走错误分支而改写的 502 被误判成 ErrServer，从而丢掉 biz_code 的精确语义
// （例如 biz_code=1 会话失效本该重建会话，却被当成上游故障）。
func (p *deepseekProvider) Classify(status int, body string) ErrKind {
	if kind, ok := deepseekClassifyEnvelope(body); ok {
		return kind
	}
	// 信封解析不出来 = 非本协议响应（WAF 拦截页 / 网关错误页 / 空 body）。
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrSessionDead
	case http.StatusTooManyRequests:
		return ErrSoftRate
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
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// deepseekClassifyEnvelope 按上游 JSON 信封判定错误类别。
// 第二个返回值 false 表示「body 不是本协议的合法信封」，调用方应改用状态码兜底。
func deepseekClassifyEnvelope(body string) (ErrKind, bool) {
	var env struct {
		Code int `json:"code"`
		Data struct {
			BizCode int `json:"biz_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return ErrNone, false
	}
	// 纯 "{}" 之类也解析成功但语义为空：视为无效信封，交给状态码判断。
	if env.Code == 0 && env.Data.BizCode == 0 && !strings.Contains(body, "\"code\"") {
		return ErrNone, false
	}
	switch env.Code {
	case 0:
		// 全局码为 0，看业务码
	case dsCodeInvalidToken, dsCodeBanned:
		return ErrSessionDead, true
	case dsCodeIpRestricted, dsCodeMuted:
		return ErrSoftRate, true
	case dsCodePowMissing, dsCodePowInvalid:
		// PoW 失效是网关侧求解问题，与账号健康无关。
		return ErrServer, true
	default:
		return ErrClient, true
	}
	switch env.Data.BizCode {
	case 0:
		return ErrNone, true
	case dsBizThinkingQuota:
		// 思考额度用完，但普通对话仍可用。归 ErrBadParams（不罚号、只轮转）；
		// 真正的降级（关掉 thinking 同号重试）由 ChatStream 内部先做一次。
		return ErrBadParams, true
	case dsBizThinkingWithFile:
		return ErrBadParams, true
	case dsBizInvalidSession, dsBizInvalidParent:
		// 会话/消息 id 失效：不是账号问题，调用方重建会话后重试。
		return ErrBadParams, true
	default:
		return ErrClient, true
	}
}

// deepseekHeaders 设置 DeepSeek 出站请求头（与浏览器抓包一致）。
func (p *deepseekProvider) deepseekHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(a.AccessToken))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", deepseekBase)
	req.Header.Set("Referer", deepseekBase+"/")
	req.Header.Set("User-Agent", deepseekUA)
	req.Header.Set("x-client-bundle-id", deepseekClientBundle)
	req.Header.Set("x-client-platform", deepseekClientPlatform)
	req.Header.Set("x-client-version", deepseekClientVersion)
	req.Header.Set("x-client-locale", "zh_CN")
	req.Header.Set("x-client-timezone-offset", "28800")
	req.Header.Set("x-device-id", p.deviceID(a))
	req.Header.Set("x-device-model", "")
}

// deepseekFallbackDevice 进程级兜底设备 ID。
//
// 不用硬编码的真实设备指纹：那会把使用者自己的指纹写进公开仓库（隐私泄漏），
// 而且多个部署共用同一个 id 反而更可疑。这里每个进程随机生成一个，
// 需要长期稳定的话在 auth 的 extra.device_id 里显式指定。
// 实测上游不校验该头（缺失也能正常对话），因此随机值完全够用。
var deepseekFallbackDevice = newUUID()

// deviceID 取设备 ID（extra 优先，否则进程内随机兜底）。
func (p *deepseekProvider) deviceID(a *auth.Auth) string {
	if v := strings.TrimSpace(a.ExtraVal("device_id")); v != "" {
		return v
	}
	return deepseekFallbackDevice
}

// dsEnvelope 上游统一信封。
type dsEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		BizCode int             `json:"biz_code"`
		BizMsg  string          `json:"biz_msg"`
		BizData json.RawMessage `json:"biz_data"`
	} `json:"data"`
}

// dsParseEnvelope 解析信封并判定成功与否：成功返回 bizData，失败返回已分类的 *Error。
func dsParseEnvelope(status int, raw []byte) (json.RawMessage, error) {
	var env dsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &Error{Kind: Classify(status, string(raw)), Status: status,
			Msg: "deepseek: 响应不是合法 JSON: " + truncate(string(raw), 160)}
	}
	// status 由调用方保证 >= 400（上游错误信封恒 HTTP 200，dsDoJSON 会改写），
	// 这里统一用 502 兜底：调用方若拿 Status=0 去判 status>=400 会漏判成「成功」。
	if env.Code != 0 {
		st := status
		if st < 400 {
			st = http.StatusBadGateway
		}
		return nil, &Error{Kind: Classify(status, string(raw)), Status: st,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 120))}
	}
	if env.Data.BizCode != 0 {
		st := status
		if st < 400 {
			st = http.StatusBadGateway
		}
		return nil, &Error{Kind: Classify(status, string(raw)), Status: st,
			Msg: fmt.Sprintf("biz_code=%d biz_msg=%s", env.Data.BizCode, truncate(env.Data.BizMsg, 120))}
	}
	return env.Data.BizData, nil
}

// dsDoJSON 发一个普通 JSON 请求并解析信封。
func (p *deepseekProvider) dsDoJSON(c *Client, a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("deepseek: marshal %s: %w", path, err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, deepseekBase+path, rdr)
	if err != nil {
		return nil, err
	}
	p.deepseekHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, transportErrHint(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
	}
	return dsParseEnvelope(resp.StatusCode, raw)
}

// RefreshToken DeepSeek 无 refresh 机制（前端收到 40003 直接跳登录页），
// 这里只做会话存活校验：token 有效则不改任何字段，失效则返回错误让网关禁用该号。
func (p *deepseekProvider) RefreshToken(c *Client, a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("deepseek: empty userToken")
	}
	req, err := http.NewRequest(http.MethodGet, deepseekBase+deepseekUsersCurrent, nil)
	if err != nil {
		return err
	}
	p.deepseekHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportErrHint(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("deepseek session check http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env dsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("deepseek session check parse: %w", err)
	}
	if env.Code == dsCodeInvalidToken || env.Code == dsCodeBanned {
		return fmt.Errorf("deepseek session invalid (code=%d): %s — 需重新执行 ./login deepseek <昵称>",
			env.Code, truncate(env.Msg, 120))
	}
	if env.Code != 0 {
		return fmt.Errorf("deepseek session check code=%d: %s", env.Code, truncate(env.Msg, 120))
	}
	// 存活：userToken 即凭证，上游不回传新 token，无需更新字段。
	return nil
}

// FetchModels 拉取模型档位表。
//
// 数据源：GET /api/v0/client/settings?did=<device>&scope=model 的
// biz_data.settings.model_configs.value（浏览器实测）。
// 只返回 enabled && switchable 的档位（当前仅 default 可切换）；
// 若全部不可切换，返回错误由调用方回退静态表，保证 /v1/models 不为空。
func (p *deepseekProvider) FetchModels(c *Client, a *auth.Auth) ([]ModelInfo, error) {
	q := url.Values{}
	q.Set("did", p.deviceID(a))
	q.Set("scope", "model")
	data, err := p.dsDoJSON(c, a, http.MethodGet, deepseekSettingsPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Settings struct {
			ModelConfigs struct {
				Value []struct {
					ModelType           string `json:"model_type"`
					Name                string `json:"name"`
					Enabled             bool   `json:"enabled"`
					Switchable          bool   `json:"switchable"`
					InputCharacterLimit int64  `json:"input_character_limit"`
				} `json:"value"`
			} `json:"model_configs"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("deepseek: parse model_configs: %w", err)
	}
	cfgs := payload.Settings.ModelConfigs.Value
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("deepseek: model_configs 为空")
	}
	out := make([]ModelInfo, 0, len(cfgs))
	seen := make(map[string]bool, len(cfgs))
	for _, cf := range cfgs {
		// 只暴露账号实际能切换的档位：不可切换的档位请求必然失败，列出来只会误导客户端。
		if !cf.Enabled || !cf.Switchable {
			continue
		}
		id := deepseekModelID(cf.ModelType)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(cf.Name)
		if name == "" {
			name = id
		}
		out = append(out, ModelInfo{ID: id, Name: name, ContextWindow: cf.InputCharacterLimit})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("deepseek: 没有可切换的模型档位")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 会话管理
// ---------------------------------------------------------------------------

// dsCreateSession 新建上游会话，返回 chat_session_id 与上游 TTL（秒）。
func (p *deepseekProvider) dsCreateSession(c *Client, a *auth.Auth) (string, int64, error) {
	data, err := p.dsDoJSON(c, a, http.MethodPost, deepseekSessionCreate, struct{}{})
	if err != nil {
		return "", 0, err
	}
	var payload struct {
		ChatSession struct {
			ID string `json:"id"`
		} `json:"chat_session"`
		TTLSeconds int64 `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", 0, fmt.Errorf("deepseek: parse chat_session: %w", err)
	}
	if strings.TrimSpace(payload.ChatSession.ID) == "" {
		return "", 0, fmt.Errorf("deepseek: chat_session/create 未返回 id")
	}
	return payload.ChatSession.ID, payload.TTLSeconds, nil
}

// dsFetchCurrentMessageID 取会话当前叶子消息 id（对齐 parent_message_id 用）。
func (p *deepseekProvider) dsFetchCurrentMessageID(c *Client, a *auth.Auth, sessionID string) (int64, error) {
	q := url.Values{}
	q.Set("chat_session_id", sessionID)
	data, err := p.dsDoJSON(c, a, http.MethodGet, deepseekHistoryPath+"?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	var payload struct {
		ChatSession struct {
			CurrentMessageID *int64 `json:"current_message_id"`
		} `json:"chat_session"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("deepseek: parse history_messages: %w", err)
	}
	if payload.ChatSession.CurrentMessageID == nil {
		return 0, nil
	}
	return *payload.ChatSession.CurrentMessageID, nil
}

// dsPowHeader 申请并求解一次 PoW，返回 x-ds-pow-response 头值（一次性，不可复用）。
func (p *deepseekProvider) dsPowHeader(c *Client, a *auth.Auth) (string, error) {
	data, err := p.dsDoJSON(c, a, http.MethodPost, deepseekPowChallenge,
		map[string]string{"target_path": deepseekCompletion})
	if err != nil {
		return "", err
	}
	var payload struct {
		Challenge DeepSeekPowChallenge `json:"challenge"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("deepseek: parse pow challenge: %w", err)
	}
	return BuildDeepSeekPowHeader(payload.Challenge)
}

// ---------------------------------------------------------------------------
// ChatStream
// ---------------------------------------------------------------------------

// ChatStream 发起 DeepSeek 对话。
//
// 会话键从请求体自身提取（metadata.conversation_id / conversation_id 等，
// 复用 session.ExtractKey 的口径），因此 handler 侧无需任何额外传参：
//   - 有键 → 复用 chat_session_id + parent_message_id（上游按会话树带全部前文）
//   - 无键 → 每次新建会话，prompt 拼全部历史（无状态 replay）
//
// 返回的 rc 已包一层会话观察器：流读完（或关闭）时把本轮的 response_message_id
// 写回会话存储，供下一轮当 parent_message_id。
func (p *deepseekProvider) ChatStream(c *Client, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return nil, 0, nil, fmt.Errorf("deepseek: parse request: %w", err)
	}
	sessKey := session.ExtractKey(body)
	store := c.deepSeekStore()

	// 命中已有会话（同账号 + 未过期）才复用；否则本轮新建。
	var entry dsconv.Entry
	if sessKey != "" {
		if e, ok := store.Get(sessKey, a.UID); ok {
			entry = e
		}
	}

	// 两类「可自愈」失败各自最多重试一次：
	//   a) 会话坐标失效（biz_code=1 会话过期 / 26 parent 无效）→ 重建会话
	//   b) 思考额度用完（biz_code=15）→ 同号关掉 thinking 重试
	// 两者都**不罚账号**：a 是坐标问题；b 是账号思考额度用完但普通对话仍可用。
	var lastStatus int
	var lastBody []byte
	rebuiltSession := false
	degradedThinking := false
	for attempt := 0; attempt < 3; attempt++ {
		rc, status, respBody, err := p.chatStreamOnce(c, a, reqBody, sessKey, &entry)
		if err != nil {
			return nil, status, respBody, err
		}
		if status < 400 {
			return rc, status, respBody, nil
		}
		lastStatus, lastBody = status, respBody
		if p.Classify(status, string(respBody)) != ErrBadParams {
			return nil, status, respBody, nil
		}
		// b) 思考额度超限：关掉思考、同号重试一次（账号健康，不该被换掉）。
		if dsIsThinkingQuota(respBody) {
			if degradedThinking {
				return nil, status, respBody, nil
			}
			degradedThinking = true
			reqBody["thinking"] = false
			reqBody["thinking_enabled"] = false
			log.Printf("INFO: [deepseek] uid=%s 思考额度已用完，关闭深度思考后重试（同号）", logUID(a.UID))
			continue
		}
		// a) 坐标失效：清本地记录并重建（biz_code=26 也可以只重置 parent，
		// 但统一重建更简单，代价是一次多余的 create）。
		if rebuiltSession {
			return nil, status, respBody, nil
		}
		rebuiltSession = true
		store.Delete(sessKey)
		entry = dsconv.Entry{}
		log.Printf("INFO: [deepseek] 会话坐标失效 uid=%s key=%q，重建会话后重试",
			logUID(a.UID), truncate(sessKey, 32))
	}
	return nil, lastStatus, lastBody, nil
}

// dsIsThinkingQuota 判定响应是否表示「思考额度用完」（biz_code=15）。
func dsIsThinkingQuota(body []byte) bool {
	var env dsEnvelope
	if json.Unmarshal(body, &env) != nil {
		return false
	}
	return env.Data.BizCode == dsBizThinkingQuota
}

// chatStreamOnce 执行一次对话请求（不含重试）。
func (p *deepseekProvider) chatStreamOnce(c *Client, a *auth.Auth, reqBody map[string]any,
	sessKey string, entry *dsconv.Entry) (io.ReadCloser, int, []byte, error) {

	clientModel, _ := reqBody["model"].(string)
	modelType := deepseekModelType(clientModel)
	// 深度思考 + 联网搜索默认都开（见 deepseekThinking / deepseekSearch 注释）。
	thinking := deepseekThinking(reqBody)
	search := deepseekSearch(reqBody)

	// 有会话键 → 只发本轮问题（历史由上游会话树带）；
	// 无会话键 → 拼全部历史（上游无状态）。
	var prompt string
	if sessKey != "" {
		prompt = deepseekLastUserText(reqBody)
	} else {
		prompt = deepseekPromptFromMessages(reqBody)
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, 0, nil, fmt.Errorf("deepseek: 请求里没有可发送的 user 文本")
	}

	// 会话坐标：命中则复用，否则新建。
	sessionID := strings.TrimSpace(entry.ChatSessionID)
	var parentID *int64
	if sessionID != "" && entry.ParentMessageID > 0 {
		pid := entry.ParentMessageID
		parentID = &pid
	}
	if sessionID == "" {
		id, _, err := p.dsCreateSession(c, a)
		if err != nil {
			log.Printf("WARN: [deepseek] create session uid=%s: %v", logUID(a.UID), err)
			var ue *Error
			if asError(err, &ue) {
				return nil, ue.Status, []byte(ue.Msg), nil
			}
			return nil, 0, nil, err
		}
		sessionID = id
		parentID = nil
		entry.ChatSessionID = id
		entry.ParentMessageID = 0
	}

	payload := map[string]any{
		"chat_session_id":   sessionID,
		"parent_message_id": parentID, // nil → JSON null（首轮）
		"model_type":        modelType,
		"prompt":            prompt,
		"ref_file_ids":      []any{},
		"thinking_enabled":  thinking,
		"search_enabled":    search,
		"action":            nil,
		"preempt":           false,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("deepseek: marshal body: %w", err)
	}

	// PoW 每个请求都必须新解（复用会 40301）。
	powHeader, err := p.dsPowHeader(c, a)
	if err != nil {
		// PoW 申请本身也可能撞 40003（token 失效）——必须保留分类，
		// 否则会被当成传输层错误（terr != nil）直接换号，账号永不被禁用。
		var ue *Error
		if asError(err, &ue) {
			log.Printf("WARN: [deepseek] pow uid=%s: %v", logUID(a.UID), err)
			return nil, ue.Status, []byte(ue.Msg), nil
		}
		log.Printf("ERR: [deepseek] pow uid=%s: %v", logUID(a.UID), err)
		return nil, 0, nil, fmt.Errorf("deepseek: 求解 PoW 失败: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, deepseekBase+deepseekCompletion, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, nil, err
	}
	p.deepseekHeaders(req, a)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Referer", deepseekBase+"/a/chat/s/"+sessionID)
	req.Header.Set("x-ds-pow-response", powHeader)

	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		log.Printf("ERR: [deepseek] chat uid=%s: transport error: %v", logUID(a.UID), err)
		return nil, 0, nil, transportErrHint(err)
	}
	// 非 SSE = 错误信封（上游错误恒 HTTP 200 + JSON），读全量 body 交给上层 Classify。
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := p.Classify(resp.StatusCode, string(rb))
		log.Printf("WARN: [deepseek] chat uid=%s: upstream %d %s body=%s",
			logUID(a.UID), resp.StatusCode, kind, truncate(string(rb), 200))
		status := resp.StatusCode
		if status < 400 {
			// 上游错误信封 HTTP 也是 200，必须改成 5xx，否则 handler 会当成功。
			// 注意：Classify 优先解析信封（见 deepseekClassifyEnvelope），
			// 因此这里的 502 不会掩盖 biz_code=1/26 的「会话失效」语义。
			status = http.StatusBadGateway
		}
		return nil, status, rb, nil
	}

	stream := monitorBody(resp.Body, c.IdleTimeout, nil)
	if sessKey == "" {
		return stream, resp.StatusCode, nil, nil
	}
	return &deepseekSessionBody{
		rc:      stream,
		store:   c.deepSeekStore(),
		key:     sessKey,
		uid:     a.UID,
		session: sessionID,
	}, resp.StatusCode, nil, nil
}

// asError 是 errors.As 的薄封装，避免在多处重复导入 errors。
func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// ---------------------------------------------------------------------------
// 会话观察器：边透传 SSE 边记录 message id
// ---------------------------------------------------------------------------

// deepseekSessionBody 包在 SSE 流外层：原始字节原样透传给 StreamTo/Aggregate，
// 同时旁路扫描 ready 帧里的 response_message_id，在 EOF/Close 时提交会话状态。
//
// 为什么用 tee 而不是让 handler 回调：Provider 的 StreamTo/Aggregate 是通用签名，
// 改它们会波及 codebuddy/trae/qoder/doubao 四家；在 provider 内部包装流是最小侵入。
type deepseekSessionBody struct {
	rc  io.ReadCloser
	key string
	uid string

	store   *dsconv.Store
	session string

	// 旁路扫描状态。Read 与 Close 可能在不同 goroutine（handler 在 StreamTo
	// 返回后 Close，空闲监控也可能提前掐断），必须加锁。
	mu        sync.Mutex
	scanBuf   []byte
	respID    int64
	committed bool
}

func (b *deepseekSessionBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.scan(p[:n])
	}
	if err != nil {
		b.commit()
	}
	return n, err
}

// scan 从透传的字节里旁路找 "response_message_id":N。
// 它只出现在流的开头（ready 帧），因此拿到即可停止扫描。
func (b *deepseekSessionBody) scan(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.respID != 0 {
		return
	}
	b.scanBuf = append(b.scanBuf, chunk...)
	const anchor = "\"response_message_id\":"
	if idx := bytes.Index(b.scanBuf, []byte(anchor)); idx >= 0 {
		rest := b.scanBuf[idx+len(anchor):]
		var id int64
		i := 0
		if len(rest) > 0 && rest[0] == '-' {
			i = 1 // 负 id 无意义，跳过符号位后仍按 0 处理
		}
		for ; i < len(rest); i++ {
			if rest[i] < '0' || rest[i] > '9' {
				break
			}
			id = id*10 + int64(rest[i]-'0')
		}
		if id > 0 {
			b.respID = id
			b.scanBuf = nil
			return
		}
	}
	// 只保留尾部，防止无限增长（anchor + 20 位数字足够）。
	if len(b.scanBuf) > 4096 {
		b.scanBuf = b.scanBuf[len(b.scanBuf)-64:]
	}
}

// commit 把本轮 assistant message id 写回会话存储（幂等，只在首次生效）。
func (b *deepseekSessionBody) commit() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.committed {
		return
	}
	b.committed = true
	if b.respID <= 0 || b.key == "" || b.store == nil {
		return
	}
	b.store.Put(b.key, dsconv.Entry{
		AccountUID:      b.uid,
		ChatSessionID:   b.session,
		ParentMessageID: b.respID,
	})
}

func (b *deepseekSessionBody) Close() error {
	// 提前关闭（客户端断开）也要提交：ready 帧早已到达，本轮 assistant 消息
	// 在上游已落库，指针同样有效。
	b.commit()
	return b.rc.Close()
}

// ---------------------------------------------------------------------------
// prompt 构造
// ---------------------------------------------------------------------------

// deepseekLastUserText 取最后一条 user 消息的文本（有会话键时用，历史由上游带）。
func deepseekLastUserText(reqBody map[string]any) string {
	msgs, _ := reqBody["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		if text := strings.TrimSpace(openAIText(m)); text != "" {
			return text
		}
	}
	return ""
}

// deepseekPromptFromMessages 把 OpenAI messages 压成 DeepSeek 的单条 prompt（无会话键时用）。
//
// DeepSeek 网页版 /chat/completion 只接受一条 prompt 字符串，没有 role 数组。映射：
//   - system/developer：折叠成前缀，放在最前
//   - 历史 user/assistant：按「用户: … / 助手: …」逐行拼接
//   - 最后一条 user：作为本轮真正的问题，放在末尾
//
// 只有一条 user 且无 system 时原样返回，不加任何包装。
func deepseekPromptFromMessages(reqBody map[string]any) string {
	msgs, _ := reqBody["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	var system []string
	type turn struct{ role, text string }
	var turns []turn
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		text := strings.TrimSpace(openAIText(mm))
		if text == "" {
			continue
		}
		switch role {
		case "system", "developer":
			system = append(system, text)
		case "tool":
			// DeepSeek 网页无工具调用，丢弃工具结果
		default:
			turns = append(turns, turn{role: role, text: text})
		}
	}
	if len(turns) == 1 && len(system) == 0 {
		return turns[0].text
	}
	var sb strings.Builder
	for _, s := range system {
		sb.WriteString(s)
		sb.WriteString("\n\n")
	}
	if len(turns) > 1 {
		for _, t := range turns[:len(turns)-1] {
			if t.role == "assistant" {
				sb.WriteString("助手: ")
			} else {
				sb.WriteString("用户: ")
			}
			sb.WriteString(t.text)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	if len(turns) > 0 {
		sb.WriteString(turns[len(turns)-1].text)
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// SSE 解析
// ---------------------------------------------------------------------------

// deepseekWalk 逐帧解析 DeepSeek SSE，回调 onDelta(正文, 思考, finish) 与 onUsage。
//
// 帧形态（浏览器实测）：
//
//	event: ready
//	data: {"request_message_id":3,"response_message_id":4,"model_type":"default"}
//
//	data: {"v":{"response":{... "fragments":[{"type":"RESPONSE","content":"864"}...]}}}
//	data: {"p":"response/fragments/-1/content","o":"APPEND","v":"2"}
//	data: {"v":" How"}                       ← 裸增量帧（无 p 字段，同样是正文）
//	data: {"p":"response/fragments","o":"APPEND","v":[{"type":"THINK"|"RESPONSE",...}]}
//	data: {"p":"response","o":"BATCH","v":[{"p":"accumulated_token_usage","v":72},...]}
//	data: {"p":"response/status","o":"SET","v":"FINISHED"}
//
// 快照语义（实测）：response.fragments[].content 是「截至目前的完整前缀」，
// 后续 APPEND 在该前缀之后追加。例如回答 "8642" 的流是：
//   快照 content="864"  →  APPEND "2"
// 因此必须把快照内容补发出去，否则会丢掉前缀（只输出 "2"）。
// 两者衔接规则见 emitSnapshot：只补发比已产出部分多出来的后缀，绝不重复。
func deepseekWalk(r io.Reader, onDelta func(content, reasoning, finish string) error,
	onUsage func(map[string]any), onIDs func(requestID, responseID int64)) error {

	br := bufio.NewReaderSize(r, 64*1024)
	// 已产出的文本（按片段类型分开记）：快照是「截至目前的完整前缀」，
	// APPEND 是在其后追加的增量。二者必须衔接成一条连续的流，
	// 既不能重复（快照 + APPEND 全量重发），也不能丢失（只发 APPEND 会丢掉前缀）。
	var emittedText, emittedThink strings.Builder
	curThinking := false
	finished := false
	// sawResponseFrame 是否已见过 response 相关帧，用于区分正文裸帧与搜索状态裸帧。
	sawResponseFrame := false
	flushFinal := func() {
		if finished {
			return
		}
		finished = true
		_ = onDelta("", "", "stop")
	}
	// emitSnapshot 把快照里比已产出部分多出来的后缀补发出去。
	// 正常情况下快照只在流首出现一次，此时 emitted* 为空 → 整段补发；
	// 若上游中途再发一次全量快照，也只会补发增量部分，不会重复。
	emitSnapshot := func(typ, content string) error {
		if content == "" {
			return nil
		}
		var already *strings.Builder
		if typ == "THINK" {
			already = &emittedThink
		} else {
			already = &emittedText
		}
		prev := already.String()
		if len(content) <= len(prev) {
			return nil // 快照不比已产出的长：无需补发
		}
		if !strings.HasPrefix(content, prev) {
			// 内容被改写（极少见）：以快照为准补发差额，保证不丢字。
			prev = ""
			already.Reset()
		}
		delta := content[len(prev):]
		already.WriteString(delta)
		if typ == "THINK" {
			return onDelta("", delta, "")
		}
		return onDelta(delta, "", "")
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(trimmed, "data:") {
			if err == io.EOF {
				flushFinal()
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
		var env map[string]any
		if json.Unmarshal([]byte(payload), &env) != nil {
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		// 1) ready 帧：message id
		if onIDs != nil {
			reqID, hasReq := num(env, "request_message_id")
			resID, hasRes := num(env, "response_message_id")
			if hasReq || hasRes {
				onIDs(int64(reqID), int64(resID))
			}
		}
		// 2) 全量快照：补发尚未产出的前缀
		if v, ok := env["v"].(map[string]any); ok {
			if resp, ok := v["response"].(map[string]any); ok {
				sawResponseFrame = true
				for _, fr := range deepseekFragments(resp) {
					typ, _ := fr["type"].(string)
					content, _ := fr["content"].(string)
					switch typ {
					case "THINK":
						curThinking = true
						if e := emitSnapshot("THINK", content); e != nil {
							return e
						}
					case "RESPONSE":
						curThinking = false
						if e := emitSnapshot("RESPONSE", content); e != nil {
							return e
						}
					}
				}
			}
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		p, _ := env["p"].(string)
		v := env["v"]
		// 3) 片段类型切换（新片段从空开始）
		if p == "response/fragments" {
			sawResponseFrame = true
			if arr, ok := v.([]any); ok {
				for _, item := range arr {
					fr, ok := item.(map[string]any)
					if !ok {
						continue
					}
					typ, _ := fr["type"].(string)
					content, _ := fr["content"].(string)
					switch typ {
					case "THINK":
						curThinking = true
						emittedThink.Reset()
						if e := emitSnapshot("THINK", content); e != nil {
							return e
						}
					case "RESPONSE":
						curThinking = false
						emittedText.Reset()
						if e := emitSnapshot("RESPONSE", content); e != nil {
							return e
						}
					}
				}
			}
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		// 4) 正文增量。上游有两种形态，**都必须处理**（实测同一流里混用）：
		//   a) 带路径的显式增量：{"p":"response/fragments/-1/content","o":"APPEND","v":"!"}
		//   b) 裸增量帧：         {"v":" How"}          ← 没有 p 字段
		// 只认 a) 会丢掉 b) 的全部内容（回答会被截断成快照前缀）。
		//
		// 但裸帧必须收紧：开联网搜索后，同一条流里还会混入别的裸字符串帧
		// （搜索进度/状态文案）。它们都出现在首个 response 快照**之前**，
		// 若一律当正文输出，用户会在回答里看到搜索提示语。
		// 故裸帧额外要求 sawResponseFrame（已见过 response 快照或 fragments 切换）。
		if s, ok := v.(string); ok && s != "" {
			isDelta := p == "response/fragments/-1/content" || (p == "" && sawResponseFrame)
			if isDelta {
				if curThinking {
					emittedThink.WriteString(s)
					if e := onDelta("", s, ""); e != nil {
						return e
					}
				} else {
					emittedText.WriteString(s)
					if e := onDelta(s, "", ""); e != nil {
						return e
					}
				}
				if err == io.EOF {
					flushFinal()
					return nil
				}
				continue
			}
		}
		// 5) 用量
		if p == "response" {
			if arr, ok := v.([]any); ok && onUsage != nil {
				for _, item := range arr {
					im, ok := item.(map[string]any)
					if !ok {
						continue
					}
					if k, _ := im["p"].(string); k == "accumulated_token_usage" {
						if n, ok := num(im, "v"); ok {
							total := int64(n)
							onUsage(map[string]any{
								"prompt_tokens":     0,
								"completion_tokens": total,
								"total_tokens":      total,
							})
						}
					}
				}
			}
			if err == io.EOF {
				flushFinal()
				return nil
			}
			continue
		}
		// 6) 结束
		if p == "response/status" {
			if s, _ := v.(string); s == "FINISHED" {
				flushFinal()
				if err == io.EOF {
					return nil
				}
				continue
			}
		}
		if err == io.EOF {
			flushFinal()
			return nil
		}
	}
}

// deepseekFragments 取 response.fragments 数组（容错 nil）。
func deepseekFragments(resp map[string]any) []map[string]any {
	arr, _ := resp["fragments"].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// StreamTo / Aggregate
// ---------------------------------------------------------------------------

// StreamTo 把 DeepSeek 原生 SSE 翻译为 OpenAI SSE 写入 w。
func (p *deepseekProvider) StreamTo(w http.ResponseWriter, rc io.Reader, model string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := int64(time.Now().Unix())
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
			"created": created,
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
	err := deepseekWalk(rc,
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
		func(u map[string]any) { lastUsage = u },
		nil,
	)
	if err != nil {
		return err
	}
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

// Aggregate 把 DeepSeek 原生 SSE 聚合成单个 OpenAI chat.completion 响应对象。
func (p *deepseekProvider) Aggregate(rc io.Reader, model string) (map[string]any, error) {
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := int64(time.Now().Unix())
	var (
		content   strings.Builder
		reasoning strings.Builder
		finish    = "stop"
		usage     map[string]any
	)
	err := deepseekWalk(rc,
		func(c, r, f string) error {
			content.WriteString(c)
			reasoning.WriteString(r)
			if f != "" {
				finish = f
			}
			return nil
		},
		func(u map[string]any) { usage = u },
		nil,
	)
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
		"created": created,
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
