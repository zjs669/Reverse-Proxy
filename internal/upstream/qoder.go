// qoder.go Qoder 供应商实现（国内 / 国际双区域）。
//
// 协议：COSY 私有协议（RSA 包裹 AES 会话密钥 + MD5 签名）+ 换表编码 SSE。
//
// 两条链路别混——签名要求完全不同：
//
//	/algo 面：需 COSY 签名，cosy-user 必须是真实 uid
//	  对话 POST /algo/api/v2/service/pro/sse/agent_chat_generation?...
//	  目录 GET  /algo/api/v2/model/list?Encode=1
//	/sash 面：只要 Bearer，无签名，token 类型无关（dt-/jt- 等价）
//	  签到 GET  /sash/api/v1/me/campaigns → POST .../{id}/claim
//
// 宿主按区域不同（qoderRegion / qoderGateway / qoderOpenAPI）：
//   /algo    国内 gateway.qoder.com.cn  国际 api3.qoder.sh
//   额度     国内 gateway.qoder.com.cn  国际 center.qoder.sh（api3 上 404）
//   OpenAPI  国内 openapi.qoder.com.cn  国际 center.qoder.sh
//
// 凭证：accessToken = dt- 或 PAT 换来的 jt-；refreshToken = drt-/jrt-；
//       extra.machineID/machineToken/machineType = COSY 持久指纹；extra.pat 用于自动续期。
//
// 关键陷阱：cosy-user 用网关账号标识 → 403 code 105（看着像凭证过期，实为身份不符）；
//           签名错是 403 code 101。先看 code 区分，别一上来就换 token。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"reverse-proxy/internal/auth"
)

const (
	qoderGatewayCN     = "https://gateway.qoder.com.cn"
	qoderOpenAPIBaseCN = "https://openapi.qoder.com.cn"
	// 国际版：center 承载 userinfo / jobToken / deviceToken 等 OpenAPI；
	// inference 承载 /algo（COSY 签名）与额度查询。与国内不同域，必须分开。
	qoderGatewayIntl     = "https://api3.qoder.sh"
	qoderOpenAPIBaseIntl = "https://center.qoder.sh"
	qoderChatPath      = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	qoderModelsPath    = "/algo/api/v2/model/list?Encode=1"
	// qoderUsagePath 账号额度接口：只认 Authorization: Bearer dt-（CLI 设备令牌），
	// 不需要网页 qoder_session_cookie，网关因此可以随时自动对账。
	// 返回里的 add_on_quota.detail_url 就是网页 qoder.cn/account/usage 的明细页。
	qoderUsagePath = "/api/v1/me/usage"
)

// QoderQuota 单项额度（用户套餐额度 / 加油包额度）。
type QoderQuota struct {
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	Unit      string  `json:"unit"`
}

// QoderUsage 账号额度快照（实扣口径，和网页 Usage 页同源）。
type QoderUsage struct {
	UserID           string     `json:"user_id"`
	PlanTier         string     `json:"plan_tier"`
	PlanTierName     string     `json:"plan_tier_name"`
	TotalUsagePct    float64    `json:"total_usage_percentage"`
	UserQuota        QoderQuota `json:"user_quota"`
	AddOnQuota       QoderQuota `json:"add_on_quota"`
}

// qoderRegion 返回该账号的区域："intl" 或 "cn"。
//
// 判定顺序有讲究：**必须先判 .cn 再判 .com**。
// 国内网关是 gateway.qoder.com.cn，字符串里包含 "qoder.com"，
// 若先判 .com 会把国内账号误判成国际版，请求直接打到 center.qoder.sh。
func qoderRegion(a *auth.Auth) string {
	if a == nil {
		return "cn"
	}
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	// 显式 region 优先（导入时按用户选择写入，比域名推断可靠）。
	if r := strings.ToLower(strings.TrimSpace(a.ExtraVal("region"))); r == "intl" || r == "cn" {
		return r
	}
	switch {
	case strings.Contains(d, "qoder.com.cn"), strings.Contains(d, "qoder.cn"):
		return "cn"
	case strings.Contains(d, "qoder.sh"), strings.Contains(d, "qoder.com"):
		return "intl"
	}
	// provider 名兜底：qoder_intl 账号即使 domain 为空也按国际版走。
	if a.ProviderOrDefault() == "qoder_intl" {
		return "intl"
	}
	return "cn"
}

// qoderGateway 返回该账号的 inference 基址（/algo 与额度都走它）。
func qoderGateway(a *auth.Auth) string {
	if qoderRegion(a) == "intl" {
		return qoderGatewayIntl
	}
	return qoderGatewayCN
}

// qoderOpenAPI 返回该账号的 OpenAPI 基址（userinfo / 令牌轮换）。
func qoderOpenAPI(a *auth.Auth) string {
	if qoderRegion(a) == "intl" {
		return qoderOpenAPIBaseIntl
	}
	return qoderOpenAPIBaseCN
}

// qoderProvider 实现 Provider 接口（Qoder 国内 / 国际双区域）。
type qoderProvider struct {
	c *Client

	mu sync.RWMutex
	// modelMap 按区域分表：国内与国际的模型目录**不是同一份**（国内有 qwen3.8-*，
	// 国际没有；反之亦然）。共用一张表时，谁后 FetchModels 谁覆盖，
	// 另一个区域的号就会拿到对方的 model_key 去请求，表现为「模型不存在」。
	// 键是 qoderRegion 的返回值（"cn" / "intl"）。
	modelMap map[string]map[string]string

	// refreshMu 串行化凭证刷新：drt- 一次性轮换，并发刷新会互相踩。
	refreshMu sync.Mutex
}

// Name 返回 provider 标识。
//
// 这里恒为 "qoder"：provider 名是**账号属性**（ProviderOrDefault），不是实现属性。
// 同一个 qoderProvider 实例要同时服务国内号与国际号，若按实例返回区域名，
// 注册表里两个键就会互相矛盾（qoder_intl 槽位却报 qoder）。
// 区域一律由账号侧决定：日志里看 a.ProviderOrDefault() 或 qoderRegion(a)。
func (p *qoderProvider) Name() string { return "qoder" }

// StaticModels 动态目录拉取失败时的回退表。
// 只放「展示名」（与 FetchModels 的 client 名一致）：早期版本混进了 qmodel_preview/qfmodel
// 这类上游原始 key，会让客户端把内部 key 当成模型名直接调用。
func (p *qoderProvider) StaticModels() []map[string]any {
	ids := []string{"auto", "fast", "qwen3.8-max", "qwen3.8-flash", "deepseek-v4-pro", "deepseek-flash",
		"glm-5.3", "glm-5.3-flash", "kimi-k3.1-pro", "kimi-k2.8-preview", "minimax-m3.1-flash"}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{
			"id":             id,
			"object":         "model",
			"created":        1753600000,
			"owned_by":       "qoder",
			"context_length": 131072,
		})
	}
	return out
}

// Classify Qoder 错误分类。
//   - 401 → ErrClient（dt- 过期：换号重试，不直接禁用；下次选到该号前会走 NeedsRefresh 刷新）
//   - 403 → ErrSessionDead（凭证彻底失效，禁用账号、需重新 bootstrap）
//   - 402/额度关键词 → ErrHardCredit；429/限流关键词 → ErrSoftRate
func (p *qoderProvider) Classify(status int, body string) ErrKind {
	switch status {
	case http.StatusPaymentRequired:
		return ErrHardCredit
	case http.StatusUnauthorized:
		return ErrClient
	case http.StatusForbidden:
		return ErrSessionDead
	case http.StatusTooManyRequests:
		return ErrSoftRate
	case http.StatusNotFound:
		return ErrNotFound
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

// resolveModel 把客户端模型名解析为上游 model_key；未知则原样回退（best effort）。
//
// region 必须传账号区域：国内外模型目录不同，查错表会把对方的 key 发上去。
func (p *qoderProvider) resolveModel(clientModel, region string) string {
	clientModel = strings.TrimSpace(clientModel)
	if clientModel == "" {
		clientModel = "qmodel_preview"
	}
	p.mu.RLock()
	var mapped string
	var ok bool
	if m := p.modelMap[region]; m != nil {
		mapped, ok = m[clientModel]
	}
	p.mu.RUnlock()
	if ok {
		return mapped
	}
	return clientModel
}

// RefreshToken 刷新 Qoder 凭证，两条路线：
//
//  1. 有 extra.pat（PAT pt-）：走 POST openapi/api/v1/jobToken/exchange 换新的 jt-。
//     这是推荐路线——PAT 长期有效，jobToken 24h 过期，网关可在每次临期时自动续。
//  2. 无 PAT 但有 refreshToken(drt-)：走 POST openapi/api/v1/deviceToken/refresh 轮换 dt-。
//
// 成功时原地更新 a 的 AccessToken/RefreshToken/ExpiresAt；调用方负责 SaveAtomic。
func (p *qoderProvider) RefreshToken(c *Client, a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if pat := strings.TrimSpace(a.ExtraVal("pat")); pat != "" {
		if err := p.exchangePATLocked(c, a, pat); err == nil {
			return nil
		} else if strings.TrimSpace(a.RefreshToken) == "" {
			return err
		}
		// PAT 兑换失败但仍有 drt-：退到设备令牌刷新（PAT 可能被吊销，drt 仍有效）。
	}
	return p.refreshDeviceLocked(c, a)
}

// exchangePATLocked 用 PAT 换 jobToken（jt-）。调用方必须已持有 a.mu。
// 端点/头/体与 qoder 官方 CLI 对齐：body {"personal_token": "pt-..."}，
// 响应 {"token": "jt-...", "refresh_token": ..., "expires_at"/"expires_in": ...}。
func (p *qoderProvider) exchangePATLocked(c *Client, a *auth.Auth, pat string) error {
	bodyB, _ := json.Marshal(map[string]string{"personal_token": pat})
	req, err := http.NewRequest(http.MethodPost, qoderOpenAPI(a)+"/api/v1/jobToken/exchange", bytes.NewReader(bodyB))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "reverse-proxy")
	req.Header.Set("Cosy-Version", "1.0.1")
	req.Header.Set("Cosy-ClientType", "5")
	resp, err := c.QoderHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("qoder: PAT rejected (http %d) — 重新生成 PAT", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qoder PAT exchange http %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var out struct {
		Token        string `json:"token"`
		JobToken     string `json:"job_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return fmt.Errorf("qoder PAT exchange parse: %w", err)
	}
	jt := out.Token
	if jt == "" {
		jt = out.JobToken
	}
	if jt == "" {
		return fmt.Errorf("qoder PAT exchange: empty token")
	}
	a.AccessToken = jt
	if out.RefreshToken != "" {
		a.RefreshToken = out.RefreshToken
	}
	now := time.Now()
	switch {
	case out.ExpiresAt != "":
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			a.ExpiresAt = t.Unix()
		}
	case out.ExpiresIn > 0:
		if out.ExpiresIn > 1_000_000_000_000 { // 毫秒
			a.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Millisecond).Unix()
		} else {
			a.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Second).Unix()
		}
	}
	if a.ExpiresAt <= now.Unix() {
		a.ExpiresAt = now.Add(24 * time.Hour).Unix() // jobToken 观测寿命 24h
	}
	return nil
}

// refreshDeviceLocked 用 drt- 轮换 dt-（POST /api/v1/deviceToken/refresh）。
// 调用方必须已持有 a.mu。
func (p *qoderProvider) refreshDeviceLocked(c *Client, a *auth.Auth) error {
	drt := strings.TrimSpace(a.RefreshToken)
	if drt == "" {
		return fmt.Errorf("qoder: no refresh token (drt-) available (set auth.refreshToken)")
	}
	url := qoderOpenAPI(a) + "/api/v1/deviceToken/refresh"
	bodyB, _ := json.Marshal(map[string]string{"refresh_token": drt})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyB))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if dt := strings.TrimSpace(a.AccessToken); dt != "" {
		req.Header.Set("Authorization", "Bearer "+dt)
	}
	resp, err := c.QoderHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("qoder: credential dead (http %d) — re-bootstrap with PAT", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qoder refresh http %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"` // ms
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return fmt.Errorf("qoder refresh parse: %w", err)
	}
	dt := out.Token
	if dt == "" {
		dt = out.DeviceToken
	}
	if dt == "" || out.RefreshToken == "" {
		return fmt.Errorf("qoder refresh: incomplete token pair")
	}
	a.AccessToken = dt
	a.RefreshToken = out.RefreshToken
	now := time.Now()
	if out.ExpiresIn > 0 {
		a.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Millisecond).Unix()
	} else if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			a.ExpiresAt = t.Unix()
		}
	}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = now.Add(30 * 24 * time.Hour).Unix() // dt 观测寿命 30d
	}
	return nil
}

// FetchModels 拉取 Qoder 模型目录，更新 modelMap，并返回 OpenAI 面模型列表。
func (p *qoderProvider) FetchModels(c *Client, a *auth.Auth) ([]ModelInfo, error) {
	dt := strings.TrimSpace(a.AccessToken)
	if dt == "" {
		return nil, fmt.Errorf("qoder: no dt- available")
	}
	rawURL := qoderGateway(a) + qoderModelsPath
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	drt := strings.TrimSpace(a.RefreshToken)
	sess, err := NewCosySession(a.ExtraVal("machineID"), a.ExtraVal("machineToken"), a.ExtraVal("machineType"), qoderCosyUID(a), a.Nickname, dt, drt)
	if err != nil {
		return nil, fmt.Errorf("qoder: cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, "", rawURL, qoderCosyUID(a), false, ""); err != nil {
		return nil, fmt.Errorf("qoder: cosy headers: %w", err)
	}
	resp, err := c.QoderHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("qoder models http %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var apiResp map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("qoder models parse: %w", err)
	}
	chatRaw, ok := apiResp["chat"]
	if !ok {
		return nil, fmt.Errorf("qoder models: no chat scene in response")
	}
	var dyn []struct {
		Key         string `json:"key"`
		DisplayName string `json:"display_name"`
		Enable      bool   `json:"enable"`
	}
	if err := json.Unmarshal(chatRaw, &dyn); err != nil {
		return nil, fmt.Errorf("qoder chat scene parse: %w", err)
	}
	mm := make(map[string]string)
	out := make([]ModelInfo, 0, len(dyn))
	for _, m := range dyn {
		if !m.Enable || m.Key == "" {
			continue
		}
		name := m.Key
		if m.DisplayName != "" {
			name = normalizeQoderModelName(m.DisplayName)
		}
		mm[name] = m.Key
		mm[m.Key] = m.Key // 两种写法都能命中
		// qoder 的模型清单接口只给 key/display_name/enable，不带思考档位信息；
		// 但协议层对所有 key 都按 is_reasoning=true 发（见 IsReasoningModel），
		// 档位取值由 NormalizeReasoningEffort 限定为 low/medium/xhigh；此处仅记录能力，
		// /v1/models 只列裸模型名。
		out = append(out, ModelInfo{ID: name, Name: m.DisplayName, ContextWindow: 0, Efforts: qoderEfforts})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("qoder models: no enabled chat models")
	}
	// 诊断日志：打一行汇总（客户端名 → 上游真实 key 的差异条数），
	// 单条映射明细用 1.py 复现即可，避免每次拉取刷 11 行。
	aliased := 0
	for _, mi := range out {
		if mm[mi.ID] != mi.ID {
			aliased++
		}
	}
	log.Printf("[qoder] FetchModels: %d 个模型（其中 %d 个需要名字→上游key 翻译）", len(out), aliased)
	region := qoderRegion(a)
	p.mu.Lock()
	if p.modelMap == nil {
		p.modelMap = map[string]map[string]string{}
	}
	p.modelMap[region] = mm
	p.mu.Unlock()
	return out, nil
}

// ChatStream 发起 Qoder 对话，返回原生嵌套 SSE 流（由 StreamTo/Aggregate 翻译）。
//
// 鉴权失败自愈：上游对过期凭证不下发 HTTP 4xx，而是在 200 的 SSE 里塞一帧
// {"body":"{\"code\":\"105\",\"message\":\"Login expired\"}","statusCodeValue":403}。
// 这里先偷看第一帧：若是 105/403，就用 drt-/PAT 刷新凭证后重试一次，
// 避免「凭证一过期该号就永久不可用」（expiresAt 不可信时尤其致命）。
func (p *qoderProvider) ChatStream(c *Client, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return nil, 0, nil, fmt.Errorf("qoder: parse request: %w", err)
	}
	clientModel, _ := reqBody["model"].(string)
	region := qoderRegion(a)
	modelKey := p.resolveModel(clientModel, region)
	// 诊断日志：响应里的 model 是网关按客户端请求名回显的（上游恒为 auto），
	// 真正决定用哪个模型的是这里解析出发往上游的 model_key，故一并打出。
	// 若 upstreamKey 与 client 相同且非已知 key，多半是 modelMap 未命中（冷/拉取失败）。
	log.Printf("[qoder] chat model: region=%s client=%q -> upstreamKey=%q", region, clientModel, modelKey)

	msgs, _ := reqBody["messages"].([]any)
	openaiMessages := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			openaiMessages = append(openaiMessages, mm)
		}
	}
	clientTools, _ := reqBody["tools"].([]any)
	reasoningEffort := ""
	if re, ok := reqBody["reasoning_effort"].(string); ok && re != "" {
		reasoningEffort = re
	} else if re, ok := reqBody["reasoningEffort"].(string); ok && re != "" {
		reasoningEffort = re
	}
	return p.chatStreamOnce(c, a, modelKey, openaiMessages, clientTools, reasoningEffort, false)
}

// chatStreamOnce 执行一次对话请求；retried=true 表示这是刷新凭证后的重试，
// 再失败就直接把错误交给上层（避免无限刷新循环）。
func (p *qoderProvider) chatStreamOnce(c *Client, a *auth.Auth, modelKey string, openaiMessages []map[string]any,
	clientTools []any, reasoningEffort string, retried bool) (io.ReadCloser, int, []byte, error) {

	rawBody, err := BuildAgentBody(openaiMessages, modelKey, clientTools, reasoningEffort)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("qoder: build body: %w", err)
	}
	encoded := QoderEncode(rawBody)

	dt := strings.TrimSpace(a.AccessToken)
	if dt == "" {
		return nil, 0, nil, fmt.Errorf("qoder: no dt- (accessToken) available")
	}
	drt := strings.TrimSpace(a.RefreshToken)
	sess, err := NewCosySession(a.ExtraVal("machineID"), a.ExtraVal("machineToken"), a.ExtraVal("machineType"), qoderCosyUID(a), a.Nickname, dt, drt)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("qoder: cosy session: %w", err)
	}
	url := qoderGateway(a) + qoderChatPath
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	if err := sess.ApplyHeaders(req, encoded, url, qoderCosyUID(a), true, modelKey); err != nil {
		return nil, 0, nil, fmt.Errorf("qoder: cosy headers: %w", err)
	}
	resp, err := c.QoderHTTP.Do(req)
	if err != nil {
		log.Printf("ERR: [qoder] chat uid=%s: transport error: %v", logUID(a.UID), err)
		return nil, 0, nil, transportErrHint(err)
	}
	if resp.StatusCode >= 400 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := p.Classify(resp.StatusCode, string(rb))
		log.Printf("WARN: [qoder] chat uid=%s: upstream %d %s body=%s", logUID(a.UID), resp.StatusCode, kind, truncate(string(rb), 200))
		// HTTP 403 也可能是凭证过期（部分链路直接回 403 而不是 200+105）。
		if !retried && resp.StatusCode == http.StatusForbidden && p.tryRefreshCredential(c, a, string(rb)) {
			return p.chatStreamOnce(c, a, modelKey, openaiMessages, clientTools, reasoningEffort, true)
		}
		return nil, resp.StatusCode, rb, nil
	}
	// 上游鉴权失败走的是「HTTP 200 + SSE 里一帧 105」，必须先看首帧。
	//
	// 注意不能用 br.Peek(4096)：Peek(n) 会阻塞到读满 n 字节或 EOF，
	// 而错误帧只有 ~150 字节且上游不关连接 → 直接挂死到超时。
	// 这里只 ReadString('\n') 读第一行（读到换行就返回），再把这一行
	// 拼回流的头部交给下游，语义等价且不会阻塞。
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	firstLine, ferr := br.ReadString('\n')
	if ferr != nil && ferr != io.EOF {
		resp.Body.Close()
		return nil, 0, nil, fmt.Errorf("qoder: read first frame: %w", ferr)
	}
	if be := qoderPeekBizError([]byte(firstLine)); be != nil {
		resp.Body.Close()
		log.Printf("WARN: [qoder] chat uid=%s: 流内鉴权失败 code=%s %s", logUID(a.UID), be.Code, be.Message)
		if !retried && p.tryRefreshCredential(c, a, be.Message) {
			return p.chatStreamOnce(c, a, modelKey, openaiMessages, clientTools, reasoningEffort, true)
		}
		// 刷新也没救：转成 403 交给上层按 session_dead 处理（禁用该号并提示重登）。
		return nil, http.StatusForbidden, []byte(be.Error()), nil
	}
	return monitorBody(&qoderPrefixedBody{prefix: []byte(firstLine), br: br, rc: resp.Body}, c.IdleTimeout, nil), resp.StatusCode, nil, nil
}

// qoderPrefixedBody 让「已 Peek 过的 bufio.Reader」继续充当流：
// Peek 不消费字节，但 resp.Body 已经被 bufio 读走了一部分到内部缓冲，
// 必须从 br 读（而不是 resp.Body），否则会丢开头。
type qoderPrefixedBody struct {
	// prefix 已从 br 读走、需要重新拼回流的字节（首帧探活用）。
	prefix []byte
	br     *bufio.Reader
	rc     io.ReadCloser
}

func (b *qoderPrefixedBody) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	return b.br.Read(p)
}

func (b *qoderPrefixedBody) Close() error { return b.rc.Close() }

// qoderPeekBizError 从流首部字节里判断是否为鉴权/业务错误帧（不消费流）。
func qoderPeekBizError(peek []byte) *QoderBizError {
	for _, raw := range strings.Split(string(peek), "\n") {
		line := strings.TrimRight(raw, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var top map[string]any
		if json.Unmarshal([]byte(payload), &top) != nil {
			continue // 首帧不完整（被 Peek 截断），交给正常解析路径
		}
		if be := qoderFrameBizError(top); be != nil {
			return be
		}
	}
	return nil
}

// tryRefreshCredential 尝试刷新凭证（drt- 轮换或 PAT 兑换），成功返回 true。
// 刷新后原地更新 a 并落盘，后续请求即用新凭证。
// tryRefreshCredential 尝试刷新凭证（drt- 轮换或 PAT 兑换），成功返回 true。
//
// 并发安全：drt- 是**一次性轮换**的。若多个请求同时撞 105 各刷一次，
// 后到者拿旧 drt 必然失败，还可能把已换到的新 token 覆盖回旧值。
// 因此用 refreshMu 单飞，并在拿到锁后比对 failedToken 判断是否已被别人换过。
func (p *qoderProvider) tryRefreshCredential(c *Client, a *auth.Auth, reason string) bool {
	if strings.TrimSpace(a.RefreshToken) == "" && strings.TrimSpace(a.ExtraVal("pat")) == "" {
		log.Printf("WARN: [qoder] uid=%s 凭证失效（%s）且无 drt-/pat，无法自动续期", logUID(a.UID), truncate(reason, 80))
		return false
	}
	// 触发本次刷新的「失败 token」：等锁后若 a.AccessToken 已变，说明别的 goroutine
	// 刚刷过，直接用新 token 重试即可。
	failedToken := strings.TrimSpace(a.AccessToken)

	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	if cur := strings.TrimSpace(a.AccessToken); cur != "" && cur != failedToken {
		log.Printf("INFO: [qoder] uid=%s 凭证已由并发请求续期，直接重试", logUID(a.UID))
		return true
	}
	if err := p.RefreshToken(c, a); err != nil {
		log.Printf("WARN: [qoder] uid=%s 自动续期失败: %v", logUID(a.UID), err)
		return false
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("ERR: [qoder] uid=%s 续期后落盘失败: %v", logUID(a.UID), err)
	}
	log.Printf("INFO: [qoder] uid=%s 凭证已自动续期（触发原因：%s）", logUID(a.UID), truncate(reason, 60))
	return true
}

// StreamTo 把 Qoder 原生嵌套 SSE 翻译为 OpenAI SSE 写入 w。
func (p *qoderProvider) StreamTo(w http.ResponseWriter, rc io.Reader, model string) error {
	return qoderWrapBizErr(qoderStreamTo(w, rc, model))
}

// Aggregate 把 Qoder 原生嵌套 SSE 聚合成单个 OpenAI chat.completion 响应对象。
func (p *qoderProvider) Aggregate(rc io.Reader, model string) (map[string]any, error) {
	res, err := qoderAggregate(rc, model)
	return res, qoderWrapBizErr(err)
}

// qoderWrapBizErr 把上游业务错误码翻译成带业务语义的可读错误，
// 便于日志定位（105 = Login expired，即凭证不被该接口接受）。
func qoderWrapBizErr(err error) error {
	if err == nil {
		return nil
	}
	var be *QoderBizError
	if errors.As(err, &be) {
		switch be.Code {
		case "105", "401":
			return fmt.Errorf("qoder: 凭证被上游拒绝（code=%s %s）：check accessToken 是否为 dt- 设备令牌，jt-（PAT 换来的 jobToken）不被对话接口接受", be.Code, be.Message)
		case "403":
			return fmt.Errorf("qoder: 无权限/会话失效（code=%s %s）", be.Code, be.Message)
		case "429":
			return fmt.Errorf("qoder: 限流（code=%s %s）", be.Code, be.Message)
		}
	}
	return err
}

// normalizeQoderModelName 把 display_name 转成 OpenAI 风格客户端名（与 trae 同级处理）。
// "Qwen3.8-Max-Preview" → "qwen3.8-max-preview"；中文等字符原样保留。
func normalizeQoderModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}

// IsReasoningModel 判断上游模型 key 是否开启思考模式。
// Qoder 现网对所有模型统一开启（source="system" 触发思考链路），故非空即开。
func IsReasoningModel(modelKey string) bool {
	return modelKey != ""
}

// qoderEfforts qoder 上游可用于 reasoning_effort 的全部档位（见 NormalizeReasoningEffort）。
var qoderEfforts = []string{"low", "medium", "xhigh"}

// NormalizeReasoningEffort 校验思考档位（upstream 支持 low/medium/xhigh）。
// 非法值返回空串 = 不注入 parameters，走上游默认（medium）。
func NormalizeReasoningEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "xhigh":
		return "xhigh"
	default:
		return ""
	}
}

// BuildAgentBody 构造 agent_chat_generation 请求体（纯透传模式）。
//   - openaiMessages：客户端原始消息列表（可含 system/assistant/tool 多轮）
//   - modelKey：上游模型 key（如 qmodel_preview）
//   - clientTools：客户端传来的 OpenAI tools 数组；为空则不注入 tools 字段
//   - reasoningEffort：客户端可选思考档位（low/medium/xhigh）；空 = 上游默认
func BuildAgentBody(openaiMessages []map[string]any, modelKey string, clientTools []any, reasoningEffort string) ([]byte, error) {
	// 最后一条 user 消息文本（chat_context.text 上游协议要求必填）
	prompt := ""
	for i := len(openaiMessages) - 1; i >= 0; i-- {
		if openaiMessages[i]["role"] == "user" {
			if c, ok := openaiMessages[i]["content"].(string); ok && c != "" {
				prompt = c
				break
			}
		}
	}

	now := time.Now()
	newUUID := uuid4()

	// 思考模式：所有模型统一开启；source="system" 是上游触发思考的真正开关。
	isReasoning := IsReasoningModel(modelKey)
	modelConfig := map[string]any{"key": modelKey, "is_reasoning": isReasoning}
	if isReasoning {
		modelConfig["source"] = "system"
	}

	base := map[string]any{
		"request_id":       newUUID,
		"chat_record_id":   newUUID,
		"request_set_id":   uuid4(),
		"session_id":       uuid4(),
		"stream":           true,
		"aliyun_user_type": "personal_professional_trial",
		"agent_id":         "agent_common",
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"image_urls":       nil,
		"session_type":     "qodercli",
		"model_config":     modelConfig,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"text":       map[string]any{"type": "text", "text": prompt},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": modelKey, "is_reasoning": isReasoning},
				"originalContent": map[string]any{"type": "text", "text": prompt},
			},
			"features":  []any{},
			"imageUrls": nil,
		},
		"messages": openaiMessages,
		"business": map[string]any{
			"id":       uuid4(),
			"begin_at": now.UnixMilli(),
			"name":     qoderTruncateRunes(prompt, 30),
		},
	}

	// tools：客户端传了才注入
	if len(clientTools) > 0 {
		base["tools"] = clientTools
	}

	// 思考档位：客户端显式传了合法值时注入 parameters；不传 = 上游默认。
	if isReasoning && reasoningEffort != "" {
		base["parameters"] = map[string]any{
			"enable_thinking":  true,
			"reasoning_effort": reasoningEffort,
		}
	}

	return json.Marshal(base)
}

// Usage 拉取账号额度（自动对账）。只依赖 auth 里的 dt- 设备令牌，
// 不需要网页登录态/cookie；返回口径与 qoder.cn/account/usage 页一致（实扣）。
func (p *qoderProvider) Usage(c *Client, a *auth.Auth) (any, error) {
	dt := strings.TrimSpace(a.AccessToken)
	if dt == "" {
		return nil, fmt.Errorf("qoder usage: empty accessToken (dt-)")
	}
	// 额度接口的宿主**按区域不同**（2026-11-06 实测）：
	//   国内 gateway.qoder.com.cn  → /api/v1/me/usage 200（已长期验证）
	//   国际 center.qoder.sh       → /api/v1/me/usage 200
	//   国际 api3.qoder.sh         → 同路径 404（inference 主机上没有额度接口）
	//
	// 国内不能跟着改成 openapi.qoder.com.cn：那条路径从未验证过 200，
	// 而 gateway 是长期在用的（管理台「查积分」一直走它）。
	// 所以只有国际版切到 center，国内保持 gateway。
	usageBase := qoderGateway(a)
	if qoderRegion(a) == "intl" {
		usageBase = qoderOpenAPI(a)
	}
	req, err := http.NewRequest(http.MethodGet, usageBase+qoderUsagePath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Authorization", "Bearer "+dt)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qoder usage http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out QoderUsage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("qoder usage parse: %w", err)
	}
	return &out, nil
}

// qoderTruncateRunes 截断到 n 个 rune（与 client.go 的截断字节版 truncate 区分）。
func qoderTruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ─── 签到与身份自检 ─────────────────────────────────────────────────────────

const (
	// qoderCampaignsPath 列出活动。claimable 为 false 表示当前无任何可领项。
	qoderCampaignsPath = "/sash/api/v1/me/campaigns"
	// qoderNotificationsPath 通知面，其中的 hasClaimableCampaign 与 claimable 同口径，
	// 客户端靠它决定头像是否显示红点。诊断时一起打，便于交叉确认。
	qoderNotificationsPath = "/sash/api/v1/me/notifications"

	// claimStatusClaimable campaigns[].claimStatus 的可领取取值。
	claimStatusClaimable = "CLAIMABLE"
)

// qoderCampaign 活动项。只取判定与领取必需的字段。
type qoderCampaign struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ClaimStatus string `json:"claimStatus"` // CLAIMABLE / CLAIMED / EXPIRED / INELIGIBLE
	StartAt     int64  `json:"startAt"`
	EndAt      int64  `json:"endAt"`
	ActionType  string `json:"actionType"`
}

// qoderCampaignsResp 列表响应。
type qoderCampaignsResp struct {
	UID          string          `json:"uid"`
	ShowCampaign bool            `json:"showCampaign"`
	Claimable    bool            `json:"claimable"`
	CampaignURL  string          `json:"campaignUrl"`
	Campaigns    []qoderCampaign `json:"campaigns"`
}

// qoderSashCall 发一次 /sash 请求。
//
// 与 qoderAlgoCall 的区别：/sash 面**不需要 COSY 签名**，只要 Bearer token。
func (p *qoderProvider) qoderSashCall(c *Client, a *auth.Auth, method, path string) (int, []byte, error) {
	rawURL := qoderOpenAPI(a) + path
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(a.AccessToken))
	req.Header.Set("User-Agent", "Qoder/0.5.3")
	if method == http.MethodPost {
		// 领取是空 body，但必须显式给 Content-Length: 0，
		// 否则部分网关会把 POST 当成无体请求拒绝。
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.ContentLength = 0
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, rb, nil
}

// Checkin 领取 Qoder 每日 100 Credits。
//
// 判据见文件头注释：claimable → campaignId → claim → replayed。
func (p *qoderProvider) Checkin(c *Client, a *auth.Auth) (CheckinResult, error) {
	if a == nil {
		return CheckinResult{State: CheckinFailed, Msg: "账号为空"}, nil
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return CheckinResult{State: CheckinFailed, Msg: "账号没有 accessToken"}, nil
	}

	status, raw, err := p.qoderSashCall(c, a, http.MethodGet, qoderCampaignsPath)
	if err != nil {
		return CheckinResult{State: CheckinFailed, Msg: err.Error()}, err
	}
	body := string(raw)
	if status >= 400 {
		return CheckinResult{
			State: CheckinFailed,
			Msg:   fmt.Sprintf("活动列表 http %d: %s", status, qoderTruncateRunes(body, 200)),
		}, nil
	}
	var list qoderCampaignsResp
	if err := json.Unmarshal(raw, &list); err != nil {
		return CheckinResult{State: CheckinFailed, Msg: "活动列表解析失败: " + err.Error()}, nil
	}

	// claimable=false：零可领项。
	//
	// 这**不代表「今天已领」**——也可能该账号压根没有活动（showCampaign=false）。
	// 两者对调度器处置一致（今天不用再试），所以都归 Already，
	// 但文案要说清是「没有可领」，不谎称「已签到」。
	if !list.Claimable || len(list.Campaigns) == 0 {
		msg := "当前没有可领取的活动"
		if !list.ShowCampaign {
			msg = "该账号当前无活动（showCampaign=false）"
		}
		return CheckinResult{State: CheckinAlready, Msg: msg}, nil
	}

	// 优先挑 claimStatus==CLAIMABLE 的。
	var target *qoderCampaign
	for i := range list.Campaigns {
		if strings.EqualFold(list.Campaigns[i].ClaimStatus, claimStatusClaimable) {
			target = &list.Campaigns[i]
			break
		}
	}
	if target == nil {
		return CheckinResult{
			State: CheckinAlready,
			Msg:   fmt.Sprintf("有 %d 个活动但均不可领", len(list.Campaigns)),
		}, nil
	}
	id := strings.TrimSpace(target.CampaignID)
	if id == "" {
		return CheckinResult{State: CheckinFailed, Msg: "活动缺少 campaignId"}, nil
	}

	status, raw, err = p.qoderSashCall(c, a, http.MethodPost, qoderCampaignsPath+"/"+id+"/claim")
	if err != nil {
		return CheckinResult{State: CheckinFailed, Msg: err.Error()}, err
	}
	body = string(raw)
	if status >= 400 {
		return CheckinResult{
			State: CheckinFailed,
			Msg:   fmt.Sprintf("领取 http %d: %s", status, qoderTruncateRunes(body, 200)),
		}, nil
	}
	var cres struct {
		GrantID     string `json:"grantId"`
		Status      string `json:"status"`
		Replayed    bool   `json:"replayed"`
		CampaignKey string `json:"campaignKey"`
		ExpiresAt   string `json:"expiresAt"`
		Benefit     struct {
			Kind     string  `json:"kind"`
			Amount   float64 `json:"amount"`
			Validity struct {
				Mode string `json:"mode"`
				Days int    `json:"days"`
			} `json:"validity"`
		} `json:"benefit"`
	}
	if err := json.Unmarshal(raw, &cres); err != nil {
		return CheckinResult{State: CheckinFailed, Msg: "领取响应解析失败: " + err.Error()}, nil
	}

	if !strings.EqualFold(cres.Status, "CLAIMED") {
		return CheckinResult{
			State: CheckinFailed,
			Msg:   fmt.Sprintf("领取未成功 status=%s: %s", cres.Status, qoderTruncateRunes(body, 160)),
		}, nil
	}

	credit := int64(cres.Benefit.Amount)
	if cres.Replayed {
		// 之前已领过，本次是重放。
		return CheckinResult{
			State:  CheckinAlready,
			Credit: credit,
			Msg:    fmt.Sprintf("今天已领取（replayed）%s +%d，有效期至 %s", cres.CampaignKey, credit, cres.ExpiresAt),
		}, nil
	}
	return CheckinResult{
		State:  CheckinOK,
		Credit: credit,
		Msg:    fmt.Sprintf("领取成功 %s +%d（%d 天有效）", cres.CampaignKey, credit, cres.Benefit.Validity.Days),
	}, nil
}
// qoderCosyUID 返回 COSY 身份与 cosy-user 头要用的 uid。
//
// 必须是**真实 Qoder uid**（userinfo.id）。a.UID 是网关侧账号标识，
// 历史账号（PAT 导入时 userinfo 解析漏字段）退化成 16 位随机 hex，
// 拿它当 cosy-user 会被上游判 403 105。cosyUID 为空时回退 a.UID，
// 保证「导入即拿到真实 uid」的新账号无需额外字段。
func qoderCosyUID(a *auth.Auth) string {
	if v := strings.TrimSpace(a.ExtraVal("cosyUID")); v != "" {
		return v
	}
	return strings.TrimSpace(a.UID)
}

// qoderAlgoCall 发一次 /algo 请求并自动注入 COSY 签名，返回状态码与响应体。
// body 为空串时用无 body 请求（签名按空 body 计算，与上游口径一致）。
func (p *qoderProvider) qoderAlgoCall(c *Client, a *auth.Auth, method, path, rawQuery, body string) (int, []byte, error) {
	rawURL := qoderGateway(a) + path
	if rawQuery != "" {
		rawURL += "?" + rawQuery
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return 0, nil, err
	}
	uid := qoderCosyUID(a)
	sess, err := NewCosySession(a.ExtraVal("machineID"), a.ExtraVal("machineToken"), a.ExtraVal("machineType"),
		uid, a.Nickname, strings.TrimSpace(a.AccessToken), strings.TrimSpace(a.RefreshToken))
	if err != nil {
		return 0, nil, fmt.Errorf("qoder: cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, body, rawURL, uid, false, ""); err != nil {
		return 0, nil, fmt.Errorf("qoder: cosy headers: %w", err)
	}
	// ApplyHeaders 固定写 text/event-stream（对话面用）；这里是 JSON 面，改回来。
	req.Header.Set("Accept", "application/json")
	resp, err := c.QoderHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, rb, nil
}


// qoderQuotaSnapshot 读当前可用总额度（套餐剩余 + 加油包剩余）。
//
// 返回 -1 表示读不到（凭证失效/网络），调用方据此决定「无法验证」而非「没到账」。
// 用 remaining 而不是 total：领取是往 total 加、往 remaining 加，
// 但 used 也可能同时变（上游有时把「已用」重算），只有 remaining 是净增量。
func (p *qoderProvider) qoderQuotaSnapshot(c *Client, a *auth.Auth) (float64, error) {
	raw, err := p.Usage(c, a)
	if err != nil {
		return -1, err
	}
	u, ok := raw.(*QoderUsage)
	if !ok || u == nil {
		return -1, fmt.Errorf("qoder usage 类型不符")
	}
	return u.UserQuota.Remaining + u.AddOnQuota.Remaining, nil
}
// qoderRealUID 用当前凭证向上游问「我是谁」，返回真实 Qoder uid。
//
// 存在的理由：uid 在网关侧是账号主键（auth.json 的键、account_keys 的键、
// 池的 byUID 都挂在它上面），一旦写错就不能就地改——改了等于换了一个账号。
// 而它同时又是 COSY 身份与 cosy-user 头，写错就全量 403。
// 所以需要一条**只读**的探测路径：拿它诊断存量账号，而不是贸然迁移。
//
// 走 /api/v1/userinfo（只认 Bearer dt-/jt-，不需要 COSY 签名），
// 与导入时的 qoderUserinfo 同一口径，返回顶层 id。
func (p *qoderProvider) qoderRealUID(c *Client, a *auth.Auth) (string, error) {
	tok := strings.TrimSpace(a.AccessToken)
	if tok == "" {
		return "", fmt.Errorf("qoder: 账号没有 accessToken，无法确认身份")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qoderOpenAPI(a)+"/api/v1/userinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "reverse-proxy")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("qoder userinfo http %d: %s", resp.StatusCode, qoderTruncateRunes(string(rb), 160))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("qoder userinfo parse: %w", err)
	}
	if strings.TrimSpace(out.ID) == "" {
		return "", fmt.Errorf("qoder userinfo 未返回 id")
	}
	return strings.TrimSpace(out.ID), nil
}

// QoderIdentity 一次身份自检的结果，供管理台展示。
type QoderIdentity struct {
	// AccountUID 网关侧账号标识（池主键）。
	AccountUID string `json:"account_uid"`
	// CosyUID 当前 COSY 请求实际使用的 uid（extra.cosyUID，缺省回落到账号标识）。
	CosyUID string `json:"cosy_uid"`
	// RealUID 上游确认的真实 uid。
	RealUID string `json:"real_uid,omitempty"`
	// Match 三者是否一致——不一致就说明该号 /algo 链路必然 403 105。
	Match bool `json:"match"`
	// Err 探测失败原因（网络/凭证失效）。
	Err string `json:"err,omitempty"`
}

// CheckIdentity 探测某号 COSY 身份是否指向真实 uid。
//
// 这是**只读**诊断：只比对，不修改任何字段。
// 为什么不自动修正：账号标识是池主键，静默改写会让 auth.json、account_keys、
// state.json 三处失去关联，风险远大于收益。修不修、怎么修留给运维决定。
func (p *qoderProvider) CheckIdentity(c *Client, a *auth.Auth) QoderIdentity {
	out := QoderIdentity{AccountUID: strings.TrimSpace(a.UID), CosyUID: qoderCosyUID(a)}
	real, err := p.qoderRealUID(c, a)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	out.RealUID = real
	out.Match = real == out.CosyUID
	return out
}

// qoderCosyAlias 把真实 uid 写进 extra.cosyUID（唯一改身份的入口）。
//
// 不写在 a.UID 上：a.UID 是池主键与 auth.json 的键，动了它整个账号关联都会断。
// extra 是 provider 私有字段，SaveAtomic 原样透传，正好承载这份别名。
// 用法：账号身份写错时（历史 PAT 导入触发随机 uid 兜底）用它一次性纠正。
func (p *qoderProvider) qoderCosyAlias(c *Client, a *auth.Auth, realUID string) error {
	realUID = strings.TrimSpace(realUID)
	if realUID == "" {
		return fmt.Errorf("qoder: 真实 uid 为空")
	}
	a.Lock()
	if a.Extra == nil {
		a.Extra = map[string]string{}
	}
	prev := a.Extra["cosyUID"]
	a.Extra["cosyUID"] = realUID
	a.Unlock()
	if err := a.SaveAtomic(); err != nil {
		// 落盘失败必须回滚内存，否则重启后两边不一致，排查时更迷惑。
		a.Lock()
		if prev == "" {
			delete(a.Extra, "cosyUID")
		} else {
			a.Extra["cosyUID"] = prev
		}
		a.Unlock()
		return fmt.Errorf("写入 extra.cosyUID 失败: %w", err)
	}
	return nil
}

// CheckIdentity 诊断某号的 COSY 身份是否指向真实 uid（只读）。
//
// 非 qoder 供应商返回 unsupported——身份别名是 Qoder COSY 协议独有的概念，
// 别家没有 cosy-user 头，不适用。
func (c *Client) CheckIdentity(a *auth.Auth) (*QoderIdentity, error) {
	if a == nil {
		return nil, fmt.Errorf("账号为空")
	}
	p, ok := c.providerFor(a).(*qoderProvider)
	if !ok {
		return nil, fmt.Errorf("该供应商没有 COSY 身份概念")
	}
	out := p.CheckIdentity(c, a)
	return &out, nil
}

// SetCosyUID 用上游确认的真实 uid 纠正该号的 COSY 身份（写 extra.cosyUID 并落盘）。
//
// 传入空 realUID 时先自行探测，避免调用方把「探测」和「写回」拼错。
func (c *Client) SetCosyUID(a *auth.Auth, realUID string) (*QoderIdentity, error) {
	if a == nil {
		return nil, fmt.Errorf("账号为空")
	}
	p, ok := c.providerFor(a).(*qoderProvider)
	if !ok {
		return nil, fmt.Errorf("该供应商没有 COSY 身份概念")
	}
	if strings.TrimSpace(realUID) == "" {
		real, err := p.qoderRealUID(c, a)
		if err != nil {
			return nil, err
		}
		realUID = real
	}
	if err := p.qoderCosyAlias(c, a, realUID); err != nil {
		return nil, err
	}
	out := p.CheckIdentity(c, a)
	return &out, nil
}

// ─── COSY 签名协议 ──────────────────────────────────────────────────────────

// qoderServerPubKeyPEM Qoder RSA 公钥（桌面客户端硬编码）。
const qoderServerPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var qoderServerPubKey *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(qoderServerPubKeyPEM))
	if block == nil {
		panic("qoder: bad server pubkey PEM")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	qoderServerPubKey = k.(*rsa.PublicKey)
}

// CosySession 每账号的签名状态。
type CosySession struct {
	MachineID    string
	MachineToken string
	MachineType  string
	TempKey      []byte // 16 字节
	CosyKey      string // base64(RSA(tempKey))
	Info         string // base64(AES-CBC(identity))
}

// NewCosySession 用持久指纹 + 当前 dt/drt 构建签名会话。
// dt/drt 变化（refresh）时需要重建（identity 内嵌这两个 token）。
// tempKey 是 16 个 ASCII 字符（与桌面端一致），不是随机字节。
func NewCosySession(machineID, machineToken, machineType, uid, nickname, dt, drt string) (*CosySession, error) {
	if machineID == "" || machineToken == "" || machineType == "" {
		return nil, fmt.Errorf("qoder: missing machine fingerprint (need machineId/machineToken/machineType in extra)")
	}
	tempKey := []byte(hexShort(16)) // 16 ASCII chars, AES-128 key
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, qoderServerPubKey, tempKey)
	if err != nil {
		return nil, err
	}
	cosyKey := base64.StdEncoding.EncodeToString(wrapped)
	identity := map[string]string{
		"name":                 nickname,
		"aid":                  uid,
		"uid":                  uid,
		"yx_uid":               "",
		"organization_id":      "",
		"organization_name":    "",
		"staffId":              "",
		"avatar_url":           "",
		"user_type":            "personal_standard",
		"security_oauth_token": dt,
		"refresh_token":        drt,
		"key":                  "",
		"encrypt_user_info":    "",
		"user_source_channel":  "",
		"account_source":       "qodercn_2_0",
		"login_method":         "",
		"email":                "",
	}
	infoPlain := jsonSortedCompact(identity)
	infoCipher, err := aesCBCEncrypt(infoPlain, tempKey)
	if err != nil {
		return nil, err
	}
	return &CosySession{
		MachineID:    machineID,
		MachineToken: machineToken,
		MachineType:  machineType,
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         base64.StdEncoding.EncodeToString(infoCipher),
	}, nil
}

// jsonSortedCompact 按 key 排序、无空白序列化。
func jsonSortedCompact(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return []byte(sb.String())
}

// aesCBCEncrypt AES-128-CBC，key=iv=tempKey（16 字节），PKCS7 padding。
func aesCBCEncrypt(plain, tempKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(tempKey)
	if err != nil {
		return nil, err
	}
	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+padLen)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, tempKey).CryptBlocks(out, padded)
	return out, nil
}

// AuthHeader 计算单次请求的 Authorization 头。
func (s *CosySession) AuthHeader(body, rawURL, uid string) (string, error) {
	payload := map[string]string{
		"cosyVersion": "2026.1021.1",
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   uuid4(),
		"version":     "v1",
	}
	payloadB64 := base64.StdEncoding.EncodeToString(jsonSortedCompact(payload))

	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	pathSig := strings.TrimPrefix(u.Path, "/algo")
	date := fmt.Sprintf("%d", time.Now().Unix())
	sigInput := payloadB64 + "\n" + s.CosyKey + "\n" + date + "\n" + body + "\n" + pathSig
	sum := md5.Sum([]byte(sigInput))
	sig := hex.EncodeToString(sum[:])
	return "Bearer COSY." + payloadB64 + "." + sig, nil
}

// ApplyHeaders 把 15 个必带头 + 推理附加头全部设置到 req。
func (s *CosySession) ApplyHeaders(req *http.Request, body, rawURL, uid string, sse bool, modelKey string) error {
	auth, err := s.AuthHeader(body, rawURL, uid)
	if err != nil {
		return err
	}
	h := req.Header
	h.Set("cosy-data-policy", "AGREE")
	h.Set("content-type", "application/json")
	h.Set("cosy-machinetype", s.MachineType)
	h.Set("cosy-clienttype", "5")
	h.Set("cosy-date", fmt.Sprintf("%d", time.Now().Unix()))
	h.Set("cosy-user", uid)
	h.Set("cosy-key", s.CosyKey)
	h.Set("accept", "text/event-stream")
	if sse {
		h.Set("cache-control", "no-cache")
	}
	h.Set("cosy-clientip", "169.254.198.161")
	h.Set("authorization", auth)
	h.Set("accept-encoding", "identity")
	h.Set("cosy-version", "2026.1021.1")
	h.Set("cosy-machineid", s.MachineID)
	h.Set("cosy-machinetoken", s.MachineToken)
	h.Set("login-version", "v2")
	h.Set("user-agent", "Go-http-client/2.0")
	if modelKey != "" {
		h.Set("x-model-key", modelKey)
		h.Set("x-model-source", "system")
	}
	return nil
}

// uuid4 简单 UUIDv4。
func uuid4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// hexShort 生成 n 字符的随机 hex（用于 tempKey，与桌面端 uuid.hex[:16] 对齐）。
func hexShort(n int) string {
	b := make([]byte, (n+1)/2)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// ─── 请求体换表编码 ─────────────────────────────────────────────────────────

const (
	qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCustomPad      = '$'
)

var (
	qoderStdToCustom [128]byte
	qoderCustomToStd [256]byte
)

func init() {
	for i := range qoderStdToCustom {
		qoderStdToCustom[i] = 0xFF
	}
	for i := range qoderCustomToStd {
		qoderCustomToStd[i] = 0xFF
	}
	for i := 0; i < len(qoderStdAlphabet); i++ {
		qoderStdToCustom[qoderStdAlphabet[i]] = qoderCustomAlphabet[i]
		qoderCustomToStd[qoderCustomAlphabet[i]] = qoderStdAlphabet[i]
	}
	qoderCustomToStd[qoderCustomPad] = '='
}

// QoderEncode 编码：base64 → 三段重排 → 字符映射（'=' → '$'）。
func QoderEncode(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	if n == 0 {
		return ""
	}
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if c == '=' {
			sb.WriteByte(qoderCustomPad)
		} else {
			sb.WriteByte(qoderStdToCustom[c])
		}
	}
	return sb.String()
}

// QoderDecode 逆编码：字符映射（'$' → '='）→ 三段逆重排 → base64 解码。
func QoderDecode(enc string) ([]byte, error) {
	n := len(enc)
	if n == 0 {
		return nil, nil
	}
	mapped := make([]byte, n)
	for i := 0; i < n; i++ {
		c := enc[i]
		s := qoderCustomToStd[c]
		if s == 0xFF {
			return nil, fmt.Errorf("invalid char %q at %d", c, i)
		}
		mapped[i] = s
	}
	a := n / 3
	std := string(mapped[n-a:]) + string(mapped[a:n-a]) + string(mapped[:a])
	return base64.StdEncoding.DecodeString(std)
}

// ─── 嵌套 SSE 解析 ──────────────────────────────────────────────────────────

// QoderBizError 上游在 HTTP 200 的 SSE 里下发的业务错误，例如：
//
//	data: {"code":"105","message":"Login expired","model":"qmodel_preview"}
//
// 这类帧没有 body 字段，必须显式识别并立刻中断，否则连接会一直被上游挂着
// 直到 HTTP 超时（实测会挂满 120s，客户端只看到空响应）。
type QoderBizError struct {
	Code    string
	Message string
}

func (e *QoderBizError) Error() string {
	if e.Code == "" {
		return "qoder upstream error: " + e.Message
	}
	return "qoder upstream code=" + e.Code + ": " + e.Message
}

// qoderCodeOK 判断业务 code 是否代表成功（缺省/0/200 均视为成功）。
func qoderCodeOK(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case float64:
		return t == 0 || t == 200
	case string:
		s := strings.TrimSpace(t)
		return s == "" || s == "0" || s == "200"
	case bool:
		return t
	}
	return true
}

// ParseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
//   - body == "[DONE]" → 正常结束
//   - body 二次 unmarshal 失败 → 跳过该行（不致命）
//   - 顶层帧带非成功 code（无 body）→ 返回 *QoderBizError，立即中断
func ParseNestedSSE(r io.Reader, onChunk func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var top map[string]any
			if json.Unmarshal([]byte(payload), &top) == nil {
				// 错误可能出现在两处（都必须判）：
				//   1) 顶层：{"code":"105","message":"Login expired"}
				//   2) 包在 body 里：{"body":"{\"code\":\"105\"}","statusCodeValue":403}
				// 实测上游走第 2 种。只判顶层会把错误帧当正常 chunk 透传，
				// 客户端永远等不到 [DONE]（表现为请求挂死到超时）。
				if be := qoderFrameBizError(top); be != nil {
					return be
				}
				if body, _ := top["body"].(string); body != "" {
					if body == "[DONE]" {
						return nil
					}
					var chunk map[string]any
					if json.Unmarshal([]byte(body), &chunk) == nil {
						if err := onChunk(chunk); err != nil {
							return err
						}
					}
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// qoderFrameBizError 判断一帧是否携带业务错误，是则返回 *QoderBizError。
//
// 覆盖三种形态：
//   - 顶层 code 非成功：{"code":"105","message":"..."}
//   - body 内嵌错误体：{"body":"{\"code\":\"105\"}","statusCodeValue":403}（当前线上形态）
//   - HTTP 语义失败：statusCodeValue>=400 或 statusCode=="FORBIDDEN"/"UNAUTHORIZED"
//
// 返回 nil 表示这一帧是正常数据帧（正常 chunk 必有 choices 字段）。
func qoderFrameBizError(top map[string]any) *QoderBizError {
	if code, has := top["code"]; has && !qoderCodeOK(code) {
		msg, _ := top["message"].(string)
		if msg == "" {
			msg, _ = top["msg"].(string)
		}
		return &QoderBizError{Code: fmt.Sprint(code), Message: msg}
	}
	body, _ := top["body"].(string)
	if body == "" || body == "[DONE]" {
		return nil
	}
	var inner map[string]any
	if json.Unmarshal([]byte(body), &inner) != nil {
		return nil
	}
	if _, hasChoices := inner["choices"]; hasChoices {
		return nil
	}
	if code, has := inner["code"]; has && !qoderCodeOK(code) {
		msg, _ := inner["message"].(string)
		if msg == "" {
			msg, _ = inner["msg"].(string)
		}
		return &QoderBizError{Code: fmt.Sprint(code), Message: msg}
	}
	if v, ok := top["statusCodeValue"].(float64); ok && v >= 400 {
		msg, _ := inner["message"].(string)
		return &QoderBizError{Code: fmt.Sprint(int(v)), Message: msg}
	}
	if s, ok := top["statusCode"].(string); ok {
		sw := strings.ToUpper(strings.TrimSpace(s))
		if sw == "FORBIDDEN" || sw == "UNAUTHORIZED" {
			msg, _ := inner["message"].(string)
			return &QoderBizError{Code: sw, Message: msg}
		}
	}
	return nil
}

// qoderAggregate 聚合完整嵌套 SSE 为单个 OpenAI chat.completion 响应。
// model 用于覆盖响应中的 model 字段（上游恒为 "auto"，客户端要看到自己的模型名）。
func qoderAggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
	)
	err := ParseNestedSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		ch, _ := chunk["choices"].([]any)
		for _, ci := range ch {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if delta, ok := c["delta"].(map[string]any); ok {
				if r2, ok := delta["role"].(string); ok && r2 != "" {
					role = r2
				}
				if txt, ok := delta["content"].(string); ok {
					content.WriteString(txt)
				}
				if rc, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(rc)
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
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
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// qoderStreamTo 把嵌套 SSE 流边读边转写为标准 OpenAI SSE 给客户端（带 usage 终帧）。
func qoderStreamTo(w http.ResponseWriter, r io.Reader, model string) error {
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
	sawFinish := false
	err := ParseNestedSSE(r, func(chunk map[string]any) error {
		chunk["model"] = model
		normalizeDeltaKeys(chunk)
		// 提取 usage（嵌套 chunk 偶尔把 usage 放在顶层）
		if u, ok := chunk["usage"].(map[string]any); ok {
			lastUsage = u
		}
		// 上游自己会发一帧 finish_reason，记下来避免末尾再补一帧（客户端会收到两个 stop）。
		if chs, ok := chunk["choices"].([]any); ok {
			for _, ci := range chs {
				if c, ok := ci.(map[string]any); ok {
					if fr, ok := c["finish_reason"].(string); ok && fr != "" {
						sawFinish = true
					}
				}
			}
		}
		raw, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 终帧：上游没发 finish 才补（否则客户端会看到两个 finish_reason:"stop"）。
	if !sawFinish {
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

// normalizeDeltaKeys 删除空的 content/reasoning_content 键（就地修改 chunk）。
func normalizeDeltaKeys(chunk map[string]any) {
	chs, _ := chunk["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		d, _ := c["delta"].(map[string]any)
		if d == nil {
			continue
		}
		if v, ok := d["content"].(string); ok && v == "" {
			delete(d, "content")
		}
		if v, ok := d["reasoning_content"].(string); ok && v == "" {
			delete(d, "reasoning_content")
		}
	}
}
