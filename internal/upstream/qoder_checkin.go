// qoder_checkin.go Qoder 每日签到（「每日领取 100 Credits」）。
//
// 接口（2026-11-06 抓包 + 实测闭环）：
//
//	GET  /sash/api/v1/me/campaigns                 列出活动（权威判据 claimable）
//	POST /sash/api/v1/me/campaigns/{id}/claim       领取（空 body，无 COSY 签名）
//
// **主机是 openapi 那一台，不是 gateway**：
//   国内 openapi.qoder.com.cn   国际 openapi.qoder.sh
//
// 重要教训：之前用的是 /algo/api/v2/activity，那个端点**永远返回空列表**，
// 无论今天领没领过——它根本不是签到接口。用它当判据的后果是
// 空列表被当成「今天已领」，每天谎报签到成功。真正的接口在活动 iframe 页里。
//
// /sash 面**不要求 COSY 签名**，只要 Bearer token；且 token 类型无关
// （dt- / jt- 实测结果一致），所以不需要为签到专门搞设备令牌。
//
// 判据（顺序不能改）：
//   1. campaigns.claimable == false  → 无可领（今天已领 / 无活动）
//   2. 取 campaigns[] 里 claimStatus == CLAIMABLE 的 campaignId
//   3. POST claim
//      replayed == false → OK（本次真领到）
//      replayed == true  → Already（之前领过，幂等）
//
// 「replayed」是区分「本次领到」和「之前领过」的唯一可靠依据；
// 额度增量可以交叉验证，但不能当主判据（领取生效有秒级延迟）。
//
// 额度落点：+100 计入 Add-on Credits（资源包），30 天有效。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"reverse-proxy/internal/auth"
)

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
