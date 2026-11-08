// addaccount.go 建号接口。
//
// 两条路径：
//   - 粘贴型（trae / qoder / deepseek）：POST /admin/api/accounts/add，
//     服务端归一凭证 → 写 auth.json → 生成专属密钥 → 同步账号池，一次请求完成。
//   - 设备流（codebuddy / codebuddy_intl）：POST oauth/start 拿授权链接，
//     后台 goroutine 轮询上游；前端用 GET oauth/status?ticket=… 每 3s 问一次。
//
// 设备流必须拆两段：拿到授权链接后用户要去浏览器登录，HTTP 请求不能一直挂着等。
package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// addAccount 粘贴型建号。
func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Deps.AddAccount == nil {
		writeErr(w, http.StatusNotImplemented, "该部署未启用建号能力")
		return
	}
	var body AddAccountReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	body.Provider = strings.ToLower(strings.TrimSpace(body.Provider))
	body.Nickname = strings.TrimSpace(body.Nickname)
	if body.Provider == "" {
		writeErr(w, http.StatusBadRequest, "缺少 provider")
		return
	}
	res, err := s.cfg.Deps.AddAccount(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"label":       res.Label,
		"uid":         res.UID,
		"provider":    res.Provider,
		"account_key": res.AccountKey,
		"in_pool":     res.InPool,
	})
}

// oauthStart 申请 CodeBuddy 授权链接，并起后台轮询。
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Deps.StartOAuth == nil {
		writeErr(w, http.StatusNotImplemented, "该部署未启用建号能力")
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	if provider == "" {
		provider = "codebuddy"
	}
	flow, err := s.cfg.Deps.StartOAuth(provider, strings.TrimSpace(body.Nickname))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ticket := randomKey()
	if ticket == "" {
		writeErr(w, http.StatusInternalServerError, "生成授权任务标识失败")
		return
	}
	s.putOAuth(ticket, &oauthJob{status: "pending", expire: time.Now().Add(oauthTimeout + oauthKeep)})
	go s.runOAuth(ticket, flow)
	writeJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "auth_url": flow.AuthURL()})
}

// oauthStatus 前端轮询当前授权任务。
func (s *Server) oauthStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getOAuth(r.URL.Query().Get("ticket"))
	if !ok {
		writeErr(w, http.StatusNotFound, "授权任务不存在或已过期，请重新生成链接")
		return
	}
	out := OAuthJob{Status: job.status, Error: job.errMsg}
	if job.res != nil {
		out.Label = job.res.Label
		out.UID = job.res.UID
		out.AccountKey = job.res.AccountKey
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── oauth 任务表 ───────────────────────────────────────────────────────────

func (s *Server) putOAuth(ticket string, job *oauthJob) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	// 顺手清理过期任务，避免 ticket 表随使用无限增长
	now := time.Now()
	for k, v := range s.oauth {
		if now.After(v.expire) {
			delete(s.oauth, k)
		}
	}
	s.oauth[ticket] = job
}

func (s *Server) getOAuth(ticket string) (*oauthJob, bool) {
	if ticket == "" {
		return nil, false
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	job, ok := s.oauth[ticket]
	if !ok {
		return nil, false
	}
	if time.Now().After(job.expire) {
		delete(s.oauth, ticket)
		return nil, false
	}
	cp := *job // 复制一份，调用方在锁外读不会踩到并发写
	return &cp, true
}

func (s *Server) finishOAuth(ticket string, job *oauthJob) {
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	// 终态保留 oauthKeep：前端可能两次轮询才渲染完，立刻删会出现「刚拿到 ok、刷新变 404」
	job.expire = time.Now().Add(oauthKeep)
	s.oauth[ticket] = job
}

// runOAuth 后台轮询设备流直到完成/失败/超时。
func (s *Server) runOAuth(ticket string, flow OAuthFlow) {
	deadline := time.Now().Add(oauthTimeout)
	for {
		res, pending, err := flow.Poll()
		if err != nil {
			s.finishOAuth(ticket, &oauthJob{status: "error", errMsg: err.Error()})
			return
		}
		if res != nil {
			s.finishOAuth(ticket, &oauthJob{status: "ok", res: res})
			return
		}
		_ = pending // pending 与「本轮查询失败」同义：继续等下一轮
		if time.Now().Add(oauthPollInterval).After(deadline) {
			s.finishOAuth(ticket, &oauthJob{
				status: "error",
				errMsg: "等待授权超时（120 秒）：确认浏览器里已完成登录后再生成一次链接",
			})
			return
		}
		time.Sleep(oauthPollInterval)
	}
}
