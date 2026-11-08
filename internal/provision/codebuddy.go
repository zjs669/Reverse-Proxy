// codebuddy.go CodeBuddy/WorkBuddy 的 OAuth 设备流 + 登录后签到。
//
// 拆成 Start/Poll 两段式是管理台的需要：网页要先拿到授权链接让用户去浏览器登录，
// 再周期性问上游「授权完成了吗」。CLI 是同一套调用的阻塞版。
package provision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

const clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// Region 一组区域端点（仅 codebuddy 使用）。
type Region struct {
	Name   string
	Base   string
	Origin string
}

var regions = map[string]Region{
	"cn": {
		Name:   "CN",
		Base:   "https://copilot.tencent.com",
		Origin: "https://www.codebuddy.cn",
	},
	"intl": {
		Name:   "Intl",
		Base:   "https://www.codebuddy.ai",
		Origin: "https://www.codebuddy.ai",
	},
}

// PickRegion 归一区域参数（空 = cn）；非法值返回 error。
func PickRegion(arg string) (string, Region, error) {
	name := strings.ToLower(strings.TrimSpace(arg))
	if name == "" {
		name = "cn"
	}
	r, ok := regions[name]
	if !ok {
		return "", Region{}, fmt.Errorf("未知区域 %q（只支持 cn / intl）", name)
	}
	return name, r, nil
}

// OAuthSession 一次设备流授权的上下文。
//
// client 必须与 Start 是同一个（内部 cookie jar 会记住 auth/state 阶段种下的会话），
// 所以不能把 Start/Poll 拆成两个无状态函数——重新 new 一个 client 会丢 cookie。
type OAuthSession struct {
	RegionName string
	Region     Region
	State      string
	AuthURL    string
	client     *http.Client
}

// StartCodeBuddy 申请一次性授权链接。
func StartCodeBuddy(region string) (*OAuthSession, error) {
	name, rg, err := PickRegion(region)
	if err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	hdrs := commonHeaders(rg)

	data, _, err := doJSON(client, http.MethodPost, rg.Base+"/v2/plugin/auth/state?platform=CLI", hdrs, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth state: missing state or authUrl")
	}
	return &OAuthSession{
		RegionName: name,
		Region:     rg,
		State:      st.State,
		AuthURL:    st.AuthURL,
		client:     client,
	}, nil
}

// Poll 查一次授权结果：
//   - (cred, false, nil)：登录已完成，凭证可用；
//   - (nil, true, nil) ：用户还没在浏览器里完成登录，调用方隔几秒再查；
//   - (nil, false, err)：这次查询本身失败（网络/上游报错），调用方决定是否重试。
//
// 「没拿到 token」与「查询失败」都可能是用户手慢，保持 pending 语义与 CLI 一致——
// CLI 那边也是无限重试到超时，不会因为一次 5xx 就把整个登录判死。
func (s *OAuthSession) Poll() (*Credential, bool, error) {
	hdrs := commonHeaders(s.Region)
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	tokRaw, _, errTok := doJSON(s.client, http.MethodGet, s.Region.Base+"/v2/plugin/auth/token?state="+s.State, hdrs, nil)
	if errTok != nil {
		return nil, true, nil
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return nil, true, nil
	}
	if tok.Domain == "" {
		tok.Domain = s.Region.Origin
	}

	// uid / enterpriseId 必须再问一次 account 接口：令牌本身不含这些字段，
	// 而池按 uid 建 entry，没有 uid 的凭证入池会被当成同一个空 uid 互相覆盖。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		hdrs(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(s.client, http.MethodGet, s.Region.Base+"/v2/plugin/login/account?state="+s.State, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	if strings.TrimSpace(acct.UID) == "" {
		return nil, false, fmt.Errorf("无法获取 uid，登录态可能已失效，请重试")
	}

	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	if expiresAt <= 0 || tok.ExpiresIn <= 0 {
		expiresAt = farFuture
	}
	return &Credential{
		Provider:     "codebuddy",
		Region:       s.RegionName,
		UID:          acct.UID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt,
		Domain:       tok.Domain,
		EnterpriseID: acct.EnterpriseID,
	}, false, nil
}

// ─── 内部 HTTP 公共件 ───────────────────────────────────────────────────────

func commonHeaders(r Region) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", r.Origin)
		req.Header.Set("Referer", r.Origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(regions["cn"])(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// Checkin 登录后立刻打一次每日签到（幂等）。返回是否成功与原始回执，
// 供 CLI 打印；管理台建号后可忽略结果（定时任务会补）。
func Checkin(domain, token, uid, entID string) (bool, string) {
	host := domain
	if host == "" {
		host = "https://www.codebuddy.cn"
	}
	req, err := http.NewRequest(http.MethodPost, host+"/v2/billing/meter/daily-checkin", bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-Id", uid)
	if entID != "" {
		req.Header.Set("X-Enterprise-Id", entID)
		req.Header.Set("X-Tenant-Id", entID)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	var env apiEnvelope
	if json.Unmarshal(rb, &env) == nil && env.Code == 0 {
		return true, ""
	}
	return false, truncate(string(rb), 120)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
