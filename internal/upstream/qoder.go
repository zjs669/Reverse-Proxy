// qoder.go Qoder CN 供应商实现（COSY 私有协议 + 换表编码 SSE）。
//
// 上游：
//   Gateway  https://gateway.qoder.com.cn
//     POST /algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1  对话（嵌套 SSE）
//     GET  /algo/api/v2/model/list?Encode=1                                                                                 模型目录
//   OpenAPI https://openapi.qoder.com.cn
//     POST /api/v1/deviceToken/refresh                                                                                      dt-/drt- 轮换
//
// 凭证来源（auth JSON，provider="qoder"）：
//   accessToken  = dt-（COSY identity.security_oauth_token，Bearer 形态，可为 PAT 换来的 jobToken jt-）
//   refreshToken = drt-（deviceToken refresh 用的 refresh_token）
//   extra.machineID / extra.machineToken / extra.machineType = COSY 持久机器指纹
//
// 协议要点（移植自 qoderwork2api）：
//   - 请求体先做 BuildAgentBody → base64 换表编码（QoderEncode）→ 放在 POST body 明文发送
//   - 头由 NewCosySession + ApplyHeaders 以 RSA 包裹 AES 会话密钥 + MD5 签名方式注入
//   - 响应是「嵌套 SSE」：每行 data:{...,"body":"<json-string>"}，body 二次解析得标准 OpenAI chunk
//   - 模型目录取 chat scene 的 key 列表；客户端名 = display_name 规范化（无则直接用 key）
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	qoderGatewayCN     = "https://gateway.qoder.com.cn"
	qoderOpenAPIBaseCN = "https://openapi.qoder.com.cn"
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

// qoderProvider 实现 Provider 接口（Qoder CN）。
type qoderProvider struct {
	c *Client

	mu       sync.RWMutex
	modelMap map[string]string // 规范化客户端名(或上游 key) -> 上游 model_key

	// refreshMu 串行化凭证刷新：drt- 一次性轮换，并发刷新会互相踩。
	refreshMu sync.Mutex
}

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
func (p *qoderProvider) resolveModel(clientModel string) string {
	clientModel = strings.TrimSpace(clientModel)
	if clientModel == "" {
		clientModel = "qmodel_preview"
	}
	p.mu.RLock()
	mapped, ok := p.modelMap[clientModel]
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
	req, err := http.NewRequest(http.MethodPost, qoderOpenAPIBaseCN+"/api/v1/jobToken/exchange", bytes.NewReader(bodyB))
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
	url := qoderOpenAPIBaseCN + "/api/v1/deviceToken/refresh"
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
	rawURL := qoderGatewayCN + qoderModelsPath
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	drt := strings.TrimSpace(a.RefreshToken)
	sess, err := NewCosySession(a.ExtraVal("machineID"), a.ExtraVal("machineToken"), a.ExtraVal("machineType"), a.UID, a.Nickname, dt, drt)
	if err != nil {
		return nil, fmt.Errorf("qoder: cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, "", rawURL, a.UID, false, ""); err != nil {
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
		// 档位取值由 NormalizeReasoningEffort 限定为 low/medium/xhigh，故统一按这三档展开。
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
	p.mu.Lock()
	p.modelMap = mm
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
	modelKey := p.resolveModel(clientModel)
	// 诊断日志：响应里的 model 是网关按客户端请求名回显的（上游恒为 auto），
	// 真正决定用哪个模型的是这里解析出发往上游的 model_key，故一并打出。
	// 若 upstreamKey 与 client 相同且非已知 key，多半是 modelMap 未命中（冷/拉取失败）。
	log.Printf("[qoder] chat model: client=%q -> upstreamKey=%q", clientModel, modelKey)

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
	sess, err := NewCosySession(a.ExtraVal("machineID"), a.ExtraVal("machineToken"), a.ExtraVal("machineType"), a.UID, a.Nickname, dt, drt)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("qoder: cosy session: %w", err)
	}
	url := qoderGatewayCN + qoderChatPath
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	if err := sess.ApplyHeaders(req, encoded, url, a.UID, true, modelKey); err != nil {
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
	req, err := http.NewRequest(http.MethodGet, qoderGatewayCN+qoderUsagePath, nil)
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
