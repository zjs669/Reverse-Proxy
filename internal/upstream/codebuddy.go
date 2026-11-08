// codebuddy.go 把既有 CodeBuddy 逻辑包装成 Provider 接口实现，并实现额度查询。
//
// 既有 Client 的 chat/refresh/models 方法已重命名为 chatStreamCodeBuddy /
// refreshTokenCodeBuddy / fetchModelsCodeBuddy（保留全部硬编码基址与请求头逻辑），
// 此处 codebuddyProvider 仅做委派，不重复实现。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/logfmt"
)

// codebuddyProvider 包装既有 CodeBuddy 实现。
type codebuddyProvider struct {
	c *Client
}

func (p codebuddyProvider) Name() string { return codebuddyName }

func (p codebuddyProvider) ChatStream(c *Client, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return c.chatStreamCodeBuddy(a, body)
}

func (p codebuddyProvider) RefreshToken(c *Client, a *auth.Auth) error {
	return c.refreshTokenCodeBuddy(a)
}

func (p codebuddyProvider) FetchModels(c *Client, a *auth.Auth) ([]ModelInfo, error) {
	return c.fetchModelsCodeBuddy(a)
}

// StaticModels 返回 nil：CodeBuddy 的静态回退表由 handler 按区域（cn/intl）维护，
// 不走 Provider 接口（见 handler.staticModels / staticModelsIntl）。
func (p codebuddyProvider) StaticModels() []map[string]any { return nil }

// Classify 复用既有 CodeBuddy 错误分类。
func (p codebuddyProvider) Classify(status int, body string) ErrKind {
	return Classify(status, body)
}

// StreamTo CodeBuddy 上游已是 OpenAI 兼容 SSE，直接规范透传。
func (p codebuddyProvider) StreamTo(w http.ResponseWriter, rc io.Reader, model string) error {
	return Stream(w, rc)
}

// Aggregate CodeBuddy 上游已是 OpenAI 兼容 SSE，直接聚合。
func (p codebuddyProvider) Aggregate(rc io.Reader, model string) (map[string]any, error) {
	return Aggregate(rc)
}

// ─── 额度查询 ───────────────────────────────────────────────────────────────

// CodeBuddyUsage 账号积分快照（billing meter 聚合口径）。
type CodeBuddyUsage struct {
	Remain   int64 // 所有套餐剩余之和
	Used     int64
	Total    int64
	Packages int // 有效期内的套餐数
}

// Usage 实现 Provider 的可选额度接口（管理台「查积分」用）。
//
// 数据源与 cmd/credit 完全相同：POST {billingBase}/v2/billing/meter/get-user-resource。
// 国内/国际域由 billingBase 按账号 domain 自动选择（codebuddy.cn / codebuddy.ai）。
func (p codebuddyProvider) Usage(c *Client, a *auth.Auth) (any, error) {
	st, err := c.UserResourceDetail(a)
	if err != nil {
		return nil, err
	}
	return &CodeBuddyUsage{
		Remain:   st.Remain,
		Used:     st.Used,
		Total:    st.Total,
		Packages: st.Packages,
	}, nil
}

// ─── 对话 / 模型目录 / 额度 ─────────────────────────────────────────────────
//
func (c *Client) billingBase(a *auth.Auth) string {
	if a != nil && isIntlDomain(a.Domain) && c.BillingBaseIntl != "" {
		return c.BillingBaseIntl
	}
	return c.BillingBaseCN
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath = "/v2/billing/meter/get-user-resource"
	dailyCheckinPath = "/v2/billing/meter/daily-checkin"
)

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// refreshTokenCodeBuddy 刷新 CodeBuddy access token（codebuddyProvider 委派入口）。
// 成功时更新 a 的字段（缺省值保留旧值），调用方负责 SaveAtomic。
// 全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) refreshTokenCodeBuddy(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	c.RefreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// transportErrHint 给传输层错误补一句排查线索。EOF / TLS 握手失败这类多半不是网关代码问题，
// 而是本机代理或 DNS 劫持把上游域名解析成假 IP（见交接文档 §5.1）：把提示写进错误串，
// 客户端和 /status 都能直接看到，不用再翻容器日志。
func transportErrHint(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	for _, k := range []string{"EOF", "tls:", "TLS", "handshake", "certificate", "connection reset"} {
		if strings.Contains(s, k) {
			return fmt.Errorf("%w（上游连接被中断：多为本机代理/DNS 劫持导致 TLS 握手失败，先查代理分流与 DNS）", err)
		}
	}
	return err
}

// chatStreamCodeBuddy 发 CodeBuddy chat 请求并返回原始 SSE body 流（codebuddyProvider 委派入口）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) chatStreamCodeBuddy(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(c.prepareBody(a, body)))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] chat_stream uid=%s: transport error: %v", logfmt.UID8(a.UID), err)
		return nil, 0, nil, transportErrHint(err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] chat_stream uid=%s: upstream %d %s body=%s",
			logfmt.UID8(a.UID), resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
	// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
	// ctx 无 deadline 无 goroutine，连接由 resp.Body.Close 正常清理。
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64    // = maxInputTokens
	MaxTokens     int64    // = maxOutputTokens
	Efforts       []string // reasoning.supportedEfforts（空=未知/固定档）
}

// fetchModelsCodeBuddy 调 CodeBuddy 动态模型接口（codebuddyProvider 委派入口）。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
// 国际版（codebuddy.ai）：scoped 模型接口 500（2026-10-25 实测），改走 GET /v3/config。
func (c *Client) fetchModelsCodeBuddy(a *auth.Auth) ([]ModelInfo, error) {
	if isIntlDomain(a.Domain) {
		return c.fetchModelsIntl(a)
	}
	url := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // 复用共享请求头（Origin/Referer/UA/Accept/Content-Type）
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]struct {
		ID              string
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		Disabled        bool
		Efforts         []string
	}, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = struct {
			ID              string
			Name            string
			MaxInputTokens  int64
			MaxOutputTokens int64
			Disabled        bool
			Efforts         []string
		}{m.ID, m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled, m.Reasoning.SupportedEfforts}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		out = append(out, ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Efforts,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	// 刷新 effort 能力缓存（供请求体降级；无 supportedEfforts 的模型不入缓存）。
	c.storeEfforts(out)
	return out, nil
}

// fetchModelsIntl 国际版模型目录：GET /v3/config。
// 结构：data.agents[]（tags 含 "cli" 的 agents.models = 可用模型 id 列表，含别名）
// + data.models[]（明细：id/name/maxInputTokens/maxOutputTokens/reasoning.supportedEfforts）。
// 字段名与 scoped 接口一致，解析后复用 ModelInfo。
func (c *Client) fetchModelsIntl(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + "/v3/config"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("intl models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Agents []struct {
				Tags   []string `json:"tags"`
				Models []string `json:"models"`
			} `json:"agents"`
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Reasoning       struct {
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("intl models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("intl models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		cli := false
		for _, t := range ag.Tags {
			if t == "cli" {
				cli = true
				break
			}
		}
		if cli {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("intl models: no cli agent found")
	}
	type intlModel = struct {
		ID              string
		Name            string
		MaxInputTokens  int64
		MaxOutputTokens int64
		SupportedEfforts []string
	}
	dynMap := make(map[string]intlModel, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = intlModel{m.ID, m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Reasoning.SupportedEfforts}
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok {
			continue
		}
		out = append(out, ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.SupportedEfforts,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("intl models api returned empty list")
	}
	c.storeEfforts(out)
	return out, nil
}

// ResourceStat 账号积分明细（billing meter 聚合口径，与 cmd/credit 一致）。
type ResourceStat struct {
	Remain   int64
	Used     int64
	Total    int64
	Packages int
}

// PackageUsage billing meter 返回的单个套餐用量字段。
type PackageUsage struct {
	PackageName         string `json:"PackageName"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
// 保留原签名供 scheduler 签到解冻 / signin 使用；要明细请用 UserResourceDetail。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	st, err := c.UserResourceDetail(a)
	return st.Remain, err
}

// UserResourceDetail 查询账号积分明细。
//
// 聚合口径与 cmd/credit 一致（就是同一个接口 /v2/billing/meter/get-user-resource）：
// 有 Cycle* 的套餐按周期量算（remain/used/size），否则用容量量；
// TotalDosage 作 Total 下限；已用至少取 Total-Remain（上游有时不给 CapacityUsed）。
func (c *Client) UserResourceDetail(a *auth.Auth) (ResourceStat, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingJSON(a, http.MethodPost, billingMeterPath, body)
	if err != nil {
		return ResourceStat{}, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalDosage int64            `json:"TotalDosage"`
				Accounts    []PackageUsage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return ResourceStat{}, fmt.Errorf("resource parse: %w", err)
	}
	st := ResourceStat{Packages: len(resp.Response.Data.Accounts)}
	for _, acct := range resp.Response.Data.Accounts {
		r, u, sz := PackageStat(acct)
		st.Remain += r
		st.Used += u
		st.Total += sz
	}
	if st.Total > 0 {
		if d := st.Total - st.Remain; d > st.Used {
			st.Used = d
		}
	}
	if d := resp.Response.Data.TotalDosage; d > st.Total {
		st.Total = d
		if x := st.Total - st.Remain; x > st.Used {
			st.Used = x
		}
	}
	return st, nil
}

// PackageStat 单套餐的 remain/used/size。有周期额度时按周期算，否则回落总容量字段。
// 导出供 cmd/credit 复用，避免两处实现漂移。
func PackageStat(a PackageUsage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		r := a.CycleCapacityRemain
		if r < 0 {
			r = 0
		}
		if r > a.CycleCapacitySize {
			r = a.CycleCapacitySize
		}
		u := a.CycleCapacitySize - r
		if a.CycleCapacityUsed > u {
			u = a.CycleCapacityUsed
			if a.CycleCapacitySize >= u {
				r = a.CycleCapacitySize - u
			}
		}
		return r, u, a.CycleCapacitySize
	}
	r := a.CapacityRemain
	if r < 0 {
		r = 0
	}
	u := a.CapacityUsed
	if u == 0 && a.CapacitySize > r {
		u = a.CapacitySize - r
	}
	return r, u, a.CapacitySize
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.billingJSON(a, http.MethodPost, dailyCheckinPath, map[string]any{})
	return err
}
