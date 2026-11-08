// deepseek.go DeepSeek 网页版（chat.deepseek.com）供应商实现。
//
// 上游端点（2026-11-02 抓包实测）：
//   POST /api/v0/chat_session/create          建会话 → chat_session.id(UUID)
//   POST /api/v0/chat/create_pow_challenge    申请 PoW（求解见 deepseek_pow 部分）
//   POST /api/v0/chat/completion              对话（SSE）
//   GET  /api/v0/chat/history_messages        消息树（对齐 parent_message_id）
//   GET  /api/v0/client/settings?scope=model  模型档位表
//   GET  /api/v0/users/current                会话存活校验
//
// 凭证：accessToken = userToken（localStorage 的 userToken.value）；extra.device_id 可选。
//
// 协议陷阱（实测，改代码前务必看）：
//   - 鉴权只需 Bearer userToken，Cookie 与 x-device-id 可缺省
//   - **每次** completion 都要新解 PoW；复用同一 x-ds-pow-response → 40301
//   - chat_session_id 必须是 UUID：空串/null → 422 UUID parsing failed
//   - parent_message_id 首轮必须 null，之后传上轮 response_message_id；传 0 → biz_code 26
//   - HTTP 恒 200，错误在 body：code 全局码（40003 token 失效 / 40300 缺 PoW /
//     40301 PoW 无效 / 40029 IP 限制 / 40012 封号 / 50006 禁言），
//     data.biz_code 业务码（1 会话不存在 / 26 消息 id 无效 / 15 思考额度超限）
//   - SSE 首帧 {"v":{"response":{...}}} 是全量快照，之后 {"p":...,"o":"APPEND","v":"..."} 是增量
//   - 模型档位 model_type ∈ default/expert/vision；思考是独立布尔 thinking_enabled
//   - 网关默认开深度思考与联网搜索；搜索/思考只与**文件**互斥（本实现不传 ref_file_ids，安全）
//   - 无 Function Calling：tools 会被忽略
//
// 上下文策略：请求体带会话键 → 复用 chat_session_id + parent_message_id，prompt 只放本轮；
// 无会话键 → 每次新建会话，prompt 拼全部历史。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
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

// dsPowHeader 申请并求解一次 completion 用的 PoW（一次性，不可复用）。
func (p *deepseekProvider) dsPowHeader(c *Client, a *auth.Auth) (string, error) {
	return p.dsPowHeaderFor(c, a, deepseekCompletion)
}

// dsPowHeaderFor 按指定 target_path 申请并求解 PoW。
//
// 上传文件与对话用的是**两个不同的 PoW**：target_path 必须与目标接口一致
// （/api/v0/chat/completion 或 /api/v0/file/upload_file），否则上游返回 40301。
func (p *deepseekProvider) dsPowHeaderFor(c *Client, a *auth.Auth, targetPath string) (string, error) {
	data, err := p.dsDoJSON(c, a, http.MethodPost, deepseekPowChallenge,
		map[string]string{"target_path": targetPath})
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

	// 图片：OpenAI 的 image_url(data URL) → 先上传拿 file id → ref_file_ids。
	// 上传有独立的 PoW，且要轮询等审核通过，因此放在会话创建之前做：
	// 上传失败时不必浪费一次 chat_session create。
	atts, err := dsExtractImages(reqBody)
	if err != nil {
		return nil, http.StatusBadRequest, []byte(err.Error()), nil
	}
	var refFileIDs []string
	if len(atts) > 0 {
		refFileIDs, err = p.dsUploadAll(c, a, atts)
		if err != nil {
			log.Printf("WARN: [deepseek] upload uid=%s: %v", logUID(a.UID), err)
			var ue *Error
			if asError(err, &ue) {
				return nil, ue.Status, []byte(ue.Msg), nil
			}
			return nil, http.StatusBadGateway, []byte(err.Error()), nil
		}
		// 上游有 SEARCH_CANNOT_WITH_FILE：带文件时必须关搜索，否则 biz_code=17。
		if search {
			log.Printf("[deepseek] uid=%s 带文件对话，自动关闭联网搜索（上游互斥）", logUID(a.UID))
			search = false
		}
		// 视觉档位：带图时默认走 vision（账号未开放该档时上游会忽略，不影响）
		if modelType == "default" {
			modelType = "vision"
		}
	}

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
		"ref_file_ids":      refFileIDs,
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
// 改它们会波及 codebuddy/trae/qoder 三家；在 provider 内部包装流是最小侵入。
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

// ─── 文件上传 ───────────────────────────────────────────────────────────────

// deepseekUploadPath 文件上传端点（也是上传 PoW 的 target_path）。
const deepseekUploadPath = "/api/v0/file/upload_file"

// deepseekFileFetchPath 文件状态查询端点。
const deepseekFileFetchPath = "/api/v0/file/fetch_files"

// dsFileReadyTimeout 等待文件就绪的总时长。
// 实测图片要 20~40s（PENDING → PARSING → SUCCESS），文本几乎立刻 SUCCESS。
const dsFileReadyTimeout = 60 * time.Second

// dsFilePollInterval 轮询间隔。
const dsFilePollInterval = 1500 * time.Millisecond

// dsMaxInlineImageBytes 单张内联图片的大小上限（解码后）。
// 上游单文件上限 100MB，但内联 base64 会先占满网关内存，这里收在 20MB。
const dsMaxInlineImageBytes = 20 << 20

// dsAttachment 一张待上传的图片。
type dsAttachment struct {
	Name string // 文件名（上游按扩展名判类型，必须带正确后缀）
	MIME string // 内容类型
	Data []byte // 原始字节
}

// dsExtractImages 从 OpenAI messages 里提取最后一条 user 消息的图片。
//
// 支持 OpenAI 规范的 content 数组写法：
//
//	{"type":"image_url","image_url":{"url":"data:image/png;base64,xxx"}}
//
// 只认 data URL（见 dsParseDataURL 的 SSRF 说明）。
// 返回 nil 表示本轮没有图片。
func dsExtractImages(reqBody map[string]any) ([]dsAttachment, error) {
	msgs, _ := reqBody["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			return nil, nil // 纯文本 user 消息，无图片
		}
		var out []dsAttachment
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["type"].(string); t != "image_url" {
				continue
			}
			img, _ := pm["image_url"].(map[string]any)
			if img == nil {
				continue
			}
			raw, _ := img["url"].(string)
			att, err := dsParseDataURL(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, att)
		}
		return out, nil
	}
	return nil, nil
}

// dsParseDataURL 解析 data:image/png;base64,xxx 形式的图片。
//
// 只支持 data URL：远程 http(s) 图片需要网关先去下载，那会引入 SSRF 面
// （客户端可让网关访问任意内网地址），故明确拒绝并提示改用 base64。
func dsParseDataURL(raw string) (dsAttachment, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return dsAttachment{}, fmt.Errorf("deepseek: image_url 为空")
	}
	if !strings.HasPrefix(raw, "data:") {
		return dsAttachment{}, fmt.Errorf(
			"deepseek: 只支持 base64 data URL（data:image/png;base64,...），"+
				"不支持远程地址 %q——请让客户端内联 base64", truncate(raw, 60))
	}
	comma := strings.Index(raw, ",")
	if comma < 0 {
		return dsAttachment{}, fmt.Errorf("deepseek: data URL 缺少逗号分隔")
	}
	meta := raw[5:comma]
	if !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return dsAttachment{}, fmt.Errorf("deepseek: data URL 必须是 base64 编码（缺 ;base64）")
	}
	mime := strings.TrimSpace(meta[:len(meta)-len(";base64")])
	if mime == "" {
		mime = "application/octet-stream"
	}
	// base64 可能被客户端按行折断，先清掉空白再解
	payload := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, raw[comma+1:])
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(payload) // URL-safe 变体
		if err != nil {
			return dsAttachment{}, fmt.Errorf("deepseek: base64 解码失败: %w", err)
		}
	}
	if len(data) == 0 {
		return dsAttachment{}, fmt.Errorf("deepseek: 图片内容为空")
	}
	if len(data) > dsMaxInlineImageBytes {
		return dsAttachment{}, fmt.Errorf("deepseek: 图片过大 %d MB（上限 %d MB）",
			len(data)>>20, dsMaxInlineImageBytes>>20)
	}
	return dsAttachment{Name: "image" + dsMimeExt(mime), MIME: mime, Data: data}, nil
}

// dsMimeExt 给 MIME 类型配扩展名（上游按扩展名判文件类型）。
func dsMimeExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	}
	return ".png" // 含 image/png 与未知类型兜底
}

// dsUploadFile 上传一个文件，返回 file id（形如 file-<uuid>）。
//
// 上传有独立的 PoW：target_path 必须是 /api/v0/file/upload_file。
func (p *deepseekProvider) dsUploadFile(c *Client, a *auth.Auth, att dsAttachment) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", att.Name)
	if err != nil {
		return "", fmt.Errorf("deepseek: build multipart: %w", err)
	}
	if _, err := fw.Write(att.Data); err != nil {
		return "", fmt.Errorf("deepseek: write multipart: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("deepseek: close multipart: %w", err)
	}

	powHeader, err := p.dsPowHeaderFor(c, a, deepseekUploadPath)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, deepseekBase+deepseekUploadPath, &buf)
	if err != nil {
		return "", err
	}
	p.deepseekHeaders(req, a)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("x-ds-pow-response", powHeader)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", transportErrHint(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
	}
	data, err := dsParseEnvelope(resp.StatusCode, raw)
	if err != nil {
		return "", err
	}
	var payload struct {
		ID       string `json:"id"`
		FileName string `json:"file_name"`
		IsImage  bool   `json:"is_image"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("deepseek: parse upload: %w", err)
	}
	if strings.TrimSpace(payload.ID) == "" {
		return "", fmt.Errorf("deepseek: upload_file 未返回 id")
	}
	log.Printf("[deepseek] upload uid=%s file=%s name=%s image=%v size=%d",
		logUID(a.UID), payload.ID, payload.FileName, payload.IsImage, len(att.Data))
	return payload.ID, nil
}

// dsWaitFileReady 轮询到文件可用。
//
// 图片要 audit_result=pass（PENDING/unknown 阶段引用会 biz_code=9），
// 非图片 audit_result 为 null，SUCCESS 即可。
func (p *deepseekProvider) dsWaitFileReady(c *Client, a *auth.Auth, fileID string) error {
	deadline := time.Now().Add(dsFileReadyTimeout)
	for {
		q := url.Values{}
		q.Set("file_ids", fileID)
		req, err := http.NewRequest(http.MethodGet,
			deepseekBase+deepseekFileFetchPath+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		p.deepseekHeaders(req, a)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return transportErrHint(err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode < 400 {
			if data, perr := dsParseEnvelope(resp.StatusCode, raw); perr == nil {
				var payload struct {
					Files []struct {
						Status      string `json:"status"`
						AuditResult string `json:"audit_result"`
						IsImage     bool   `json:"is_image"`
					} `json:"files"`
				}
				if json.Unmarshal(data, &payload) == nil && len(payload.Files) > 0 {
					f := payload.Files[0]
					if f.AuditResult == "reject" {
						return fmt.Errorf("deepseek: 图片未通过审核（audit_result=reject）")
					}
					if f.Status == "SUCCESS" && (!f.IsImage || f.AuditResult == "pass") {
						return nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deepseek: 等待文件就绪超时（%s）", dsFileReadyTimeout)
		}
		time.Sleep(dsFilePollInterval)
	}
}

// dsUploadAll 上传全部图片并等可用，返回 file id 列表。
func (p *deepseekProvider) dsUploadAll(c *Client, a *auth.Auth, atts []dsAttachment) ([]string, error) {
	ids := make([]string, 0, len(atts))
	for _, att := range atts {
		id, err := p.dsUploadFile(c, a, att)
		if err != nil {
			return nil, err
		}
		if err := p.dsWaitFileReady(c, a, id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ─── PoW 工作量证明（wasm）─────────────────────────────────────────────────

//go:embed sha3_wasm_bg.wasm
var deepseekPowWasm []byte

// deepseekPowRuntime 全局唯一的 wazero 运行时与编译产物。
//
// 为什么全局单例：CompileModule 是重活（几十毫秒级），而 Instantiate 很轻。
// 但 instantiate 不能完全省掉——wasm 的 __wbindgen_export_0 只分配不释放，
// 复用同一个实例会让线性内存随请求数线性增长，最终 OOM。
var deepseekPowRuntime struct {
	once sync.Once
	rt   wazero.Runtime
	mod  wazero.CompiledModule
	err  error
}

// deepseekPowInit 幂等初始化运行时（首次调用编译 wasm）。
func deepseekPowInit() error {
	deepseekPowRuntime.once.Do(func() {
		ctx := context.Background()
		rt := wazero.NewRuntime(ctx)
		mod, err := rt.CompileModule(ctx, deepseekPowWasm)
		if err != nil {
			_ = rt.Close(ctx)
			deepseekPowRuntime.err = fmt.Errorf("deepseek: compile pow wasm: %w", err)
			return
		}
		deepseekPowRuntime.rt = rt
		deepseekPowRuntime.mod = mod
	})
	return deepseekPowRuntime.err
}

// DeepSeekPowAnswer 求解结果。OK=false 表示 wasm 未解出（status=0）。
type DeepSeekPowAnswer struct {
	Answer int64
	OK     bool
}

// deepseekPowInstance 一个已实例化的 wasm 实例 + 它缓存的导出函数句柄。
//
// 为什么复用而不是每次 instantiate：InstantiateModule 会重建整个线性内存
// （wazero 默认 4MB 起），每个请求都做一次纯属浪费——PoW 求解本身只要 ~40ms，
// 实例化开销在同一量级。这里改成池化复用。
//
// 复用的前提是「wasm 的 bump allocator 只增不减」：
//   - 每次求解只分配 ~200 字节（challenge + prefix 两个字符串），
//   - 但 __wbindgen_export_0 没有对应的 free，指针会一直往上走。
// 因此给每个实例设分配水位上限（deepseekPowRecycleBytes），超过就丢弃重建，
// 避免线性内存无限增长。
type deepseekPowInstance struct {
	mod   api.Module
	alloc api.Function
	stack api.Function
	solve api.Function
	mem   api.Memory
	// used 本实例累计分配字节数（粗略：只算我们主动 put 的字符串）。
	used int
}

// deepseekPowRecycleBytes 单个实例的分配水位；超过即回收重建。
// 26KB 的模块、4MB 初始内存，1MB 水位足够跑几千次求解。
const deepseekPowRecycleBytes = 1 << 20

// deepseekPowPool 实例池（复用实例，避免每请求 instantiate）。
var deepseekPowPool struct {
	mu   sync.Mutex
	idle []*deepseekPowInstance
}

// deepseekAcquireInstance 取一个可用实例（池空则新建）。
func deepseekAcquireInstance() (*deepseekPowInstance, error) {
	if err := deepseekPowInit(); err != nil {
		return nil, err
	}
	deepseekPowPool.mu.Lock()
	if n := len(deepseekPowPool.idle); n > 0 {
		inst := deepseekPowPool.idle[n-1]
		deepseekPowPool.idle = deepseekPowPool.idle[:n-1]
		deepseekPowPool.mu.Unlock()
		return inst, nil
	}
	deepseekPowPool.mu.Unlock()
	return deepseekNewInstance()
}

// deepseekReleaseInstance 归还实例；超过分配水位则直接丢弃（Close 掉）。
func deepseekReleaseInstance(inst *deepseekPowInstance) {
	if inst == nil {
		return
	}
	ctx := context.Background()
	if inst.used > deepseekPowRecycleBytes {
		_ = inst.mod.Close(ctx)
		return
	}
	deepseekPowPool.mu.Lock()
	// 池子只留少量实例（并发上限很低，多了纯占内存）。
	if len(deepseekPowPool.idle) >= 8 {
		deepseekPowPool.mu.Unlock()
		_ = inst.mod.Close(ctx)
		return
	}
	deepseekPowPool.idle = append(deepseekPowPool.idle, inst)
	deepseekPowPool.mu.Unlock()
}

// deepseekNewInstance 实例化一个新 wasm 模块并缓存导出句柄。
func deepseekNewInstance() (*deepseekPowInstance, error) {
	ctx := context.Background()
	mod, err := deepseekPowRuntime.rt.InstantiateModule(ctx, deepseekPowRuntime.mod,
		wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("deepseek: instantiate pow wasm: %w", err)
	}
	inst := &deepseekPowInstance{
		mod:   mod,
		mem:   mod.Memory(),
		alloc: mod.ExportedFunction("__wbindgen_export_0"),
		stack: mod.ExportedFunction("__wbindgen_add_to_stack_pointer"),
		solve: mod.ExportedFunction("wasm_solve"),
	}
	if inst.mem == nil || inst.alloc == nil || inst.stack == nil || inst.solve == nil {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("deepseek: pow wasm 缺少必要导出（memory/__wbindgen_export_0/__wbindgen_add_to_stack_pointer/wasm_solve）")
	}
	return inst, nil
}

// SolveDeepSeekPow 用官方 wasm 求解一个 challenge。
// 返回 answer 与是否解出；wasm 加载失败或调用异常时返回 error。
func SolveDeepSeekPow(challenge, salt string, expireAt int64, difficulty float64) (DeepSeekPowAnswer, error) {
	inst, err := deepseekAcquireInstance()
	if err != nil {
		return DeepSeekPowAnswer{}, err
	}
	defer deepseekReleaseInstance(inst)

	ctx := context.Background()
	put := func(s string) (uint32, uint32, error) {
		b := []byte(s)
		res, err := inst.alloc.Call(ctx, uint64(len(b)), 1)
		if err != nil {
			return 0, 0, fmt.Errorf("deepseek: pow alloc: %w", err)
		}
		ptr := uint32(res[0])
		if !inst.mem.Write(ptr, b) {
			return 0, 0, fmt.Errorf("deepseek: pow 写入 wasm 内存越界 (ptr=%d len=%d)", ptr, len(b))
		}
		inst.used += len(b)
		return ptr, uint32(len(b)), nil
	}

	// 返回值暂存区 16 字节（wbindgen 约定：i32 status + f64 answer）。
	sp, err := inst.stack.Call(ctx, deepseekNeg16())
	if err != nil {
		return DeepSeekPowAnswer{}, fmt.Errorf("deepseek: pow stack: %w", err)
	}
	retPtr := uint32(sp[0])
	defer func() { _, _ = inst.stack.Call(ctx, 16) }()

	chalPtr, chalLen, err := put(challenge)
	if err != nil {
		return DeepSeekPowAnswer{}, err
	}
	// 前缀固定为 "<salt>_<expire_at>_"，answer 由 wasm 自己拼。
	prefixPtr, prefixLen, err := put(fmt.Sprintf("%s_%d_", salt, expireAt))
	if err != nil {
		return DeepSeekPowAnswer{}, err
	}

	if _, err := inst.solve.Call(ctx, uint64(retPtr), uint64(chalPtr), uint64(chalLen),
		uint64(prefixPtr), uint64(prefixLen), math.Float64bits(difficulty)); err != nil {
		return DeepSeekPowAnswer{}, fmt.Errorf("deepseek: wasm_solve: %w", err)
	}

	status, ok := inst.mem.ReadUint32Le(retPtr)
	if !ok {
		return DeepSeekPowAnswer{}, fmt.Errorf("deepseek: 读取 pow status 失败")
	}
	bits, ok := inst.mem.ReadUint64Le(retPtr + 8)
	if !ok {
		return DeepSeekPowAnswer{}, fmt.Errorf("deepseek: 读取 pow answer 失败")
	}
	if status == 0 {
		return DeepSeekPowAnswer{OK: false}, nil
	}
	return DeepSeekPowAnswer{Answer: int64(math.Float64frombits(bits)), OK: true}, nil
}

// DeepSeekPowChallenge 上游下发的 challenge（字段名与接口一致）。
type DeepSeekPowChallenge struct {
	Algorithm   string  `json:"algorithm"`
	Challenge   string  `json:"challenge"`
	Salt        string  `json:"salt"`
	Signature   string  `json:"signature"`
	Difficulty  float64 `json:"difficulty"`
	ExpireAt    int64   `json:"expire_at"`
	ExpireAfter int64   `json:"expire_after"`
	TargetPath  string  `json:"target_path"`
}

// BuildDeepSeekPowHeader 求解 challenge 并组装 x-ds-pow-response 头的值。
// JSON 必须紧凑（无空格）且字段顺序固定——上游按整串校验（实测 tampering 返回 40301）。
func BuildDeepSeekPowHeader(ch DeepSeekPowChallenge) (string, error) {
	ans, err := SolveDeepSeekPow(ch.Challenge, ch.Salt, ch.ExpireAt, ch.Difficulty)
	if err != nil {
		return "", err
	}
	if !ans.OK {
		return "", fmt.Errorf("deepseek: wasm_solve 未解出 challenge（difficulty=%v）", ch.Difficulty)
	}
	// 用 map 会打乱字段顺序，这里用固定顺序的结构体保证与浏览器一致。
	payload := struct {
		Algorithm  string `json:"algorithm"`
		Challenge  string `json:"challenge"`
		Salt       string `json:"salt"`
		Answer     int64  `json:"answer"`
		Signature  string `json:"signature"`
		TargetPath string `json:"target_path"`
	}{
		Algorithm:  ch.Algorithm,
		Challenge:  ch.Challenge,
		Salt:       ch.Salt,
		Answer:     ans.Answer,
		Signature:  ch.Signature,
		TargetPath: ch.TargetPath,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("deepseek: marshal pow payload: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// deepseekNeg16 返回 -16 的 uint64 表示（wasm i32 参数按无符号传）。
// 不能直接写 uint64(uint32(int32(-16)))：常量表达式在编译期就溢出。
func deepseekNeg16() uint64 {
	v := int32(-16)
	return uint64(uint32(v))
}

// deepseekHashString 用 wasm 计算 H(s)，仅供测试断言 PoW 正确性。
func deepseekHashString(s string) (string, error) {
	inst, err := deepseekAcquireInstance()
	if err != nil {
		return "", err
	}
	defer deepseekReleaseInstance(inst)

	ctx := context.Background()
	hashFn := inst.mod.ExportedFunction("wasm_deepseek_hash_v1")
	if hashFn == nil {
		return "", fmt.Errorf("deepseek: pow wasm 缺少 hash 导出")
	}
	b := []byte(s)
	res, err := inst.alloc.Call(ctx, uint64(len(b)), 1)
	if err != nil {
		return "", err
	}
	ptr := uint32(res[0])
	if !inst.mem.Write(ptr, b) {
		return "", fmt.Errorf("deepseek: hash 写入 wasm 内存越界")
	}
	inst.used += len(b)

	sp, err := inst.stack.Call(ctx, deepseekNeg16())
	if err != nil {
		return "", err
	}
	retPtr := uint32(sp[0])
	defer func() { _, _ = inst.stack.Call(ctx, 16) }()
	if _, err := hashFn.Call(ctx, uint64(retPtr), uint64(ptr), uint64(len(b))); err != nil {
		return "", err
	}
	outPtr, ok := inst.mem.ReadUint32Le(retPtr)
	if !ok {
		return "", fmt.Errorf("deepseek: 读取 hash ptr 失败")
	}
	outLen, ok := inst.mem.ReadUint32Le(retPtr + 4)
	if !ok {
		return "", fmt.Errorf("deepseek: 读取 hash len 失败")
	}
	out, ok := inst.mem.Read(outPtr, outLen)
	if !ok {
		return "", fmt.Errorf("deepseek: 读取 hash 内容失败")
	}
	return string(out), nil
}

// 编译期断言：api.Memory 被上面的 mem 变量使用（避免 import 未使用）。
var _ api.Memory = nil
