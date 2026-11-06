// handlers.go 各接口实现。约定：
//   - 所有返回的账号数据都不含凭证字段（那是 AccountDTO 的职责）
//   - 写操作返回 200 + {ok:true}，失败返回 {error:"..."}，HTTP 码表意
package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// overview 概览页数据。
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	d := s.cfg.Deps
	total, healthy, cooling, disabled, inFlight := d.PoolCounts()

	var up time.Duration
	if d.Uptime != nil {
		up = d.Uptime()
	}
	sticky := 0
	if d.StickyCount != nil {
		sticky = d.StickyCount()
	}
	redis := "noop"
	if d.RedisMode != nil {
		redis = d.RedisMode()
	}

	// 汇总各 provider 的账号数与状态，给前端画分布用
	type pstat struct {
		Provider string `json:"provider"`
		Total    int    `json:"total"`
		Healthy  int    `json:"healthy"`
		Expiring int    `json:"expiring"` // 24h 内到期
		Expired  int    `json:"expired"`
	}
	byProv := map[string]*pstat{}
	var success, errs int64
	now := time.Now().Unix()
	for _, a := range d.ListAccounts() {
		p := a.Provider
		if p == "" {
			p = "codebuddy"
		}
		st := byProv[p]
		if st == nil {
			st = &pstat{Provider: p}
			byProv[p] = st
		}
		st.Total++
		if !a.Cooling && !a.Disabled {
			st.Healthy++
		}
		if a.ExpiresAt > 0 {
			if a.ExpiresAt <= now {
				st.Expired++
			} else if a.ExpiresAt-now <= 86400 {
				st.Expiring++
			}
		}
		success += a.SuccessCount
		errs += a.ErrTotal
	}
	provs := make([]pstat, 0, len(byProv))
	for _, st := range byProv {
		provs = append(provs, *st)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_sec":     int64(up.Seconds()),
		"redis_mode":     redis,
		"sticky_sessions": sticky,
		"pool": map[string]any{
			"total": total, "healthy": healthy, "cooling": cooling,
			"disabled": disabled, "in_flight_full": inFlight,
		},
		"success_total": success,
		"error_total":   errs,
		"providers":     provs,
	})
}

// accounts 账号列表。默认脱敏（不含专属密钥明文）。
func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	list := s.cfg.Deps.ListAccounts()
	// 列表不返回密钥，避免一次拉全表就把所有 key 暴露给 XSS
	for i := range list {
		list[i].AccountKey = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": list})
}

// accountDetail 单账号详情，含专属密钥明文（前端默认掩码，点「显示」才渲染）。
func (s *Server) accountDetail(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a, ok := s.cfg.Deps.AccountByUID(uid)
	if !ok {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// renew 立即续期单个账号。
func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	exp, err := s.cfg.Deps.RenewOne(uid)
	if err != nil {
		// 区分「凭证真死了」和「临时网络错误」：前者要提示重新登录
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": err.Error(),
			"hint":  "若提示凭证失效，请重新登录该账号并更新 auth.json",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": exp})
}

// revive 解除冷却/禁用。
func (s *Server) revive(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Deps.ReviveOne(r.PathValue("uid")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resetKey 重置该账号的专属密钥，返回新密钥（唯一一次明文回传）。
func (s *Server) resetKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.cfg.Deps.ResetKey(r.PathValue("uid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account_key": key})
}

// deleteAccount 从 auth.json 移除该账号。
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Deps.DeleteAcct(r.PathValue("uid")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// models 该号可用模型（走 upstream.FetchModels，按 uid 取凭证）。
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	ids, err := s.cfg.Deps.ModelsOf(r.PathValue("uid"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ids, "count": len(ids)})
}

// accountUsage 查该号剩余额度。
//
// 上游只有 Qoder 提供额度接口（/api/v1/me/usage，只认 dt- 设备令牌）；
// 其余供应商返回 supported=false + hint，前端据此提示「这家查不了」而不是报错。
func (s *Server) accountUsage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Deps.UsageOf == nil {
		writeErr(w, http.StatusNotImplemented, "该部署未启用额度查询")
		return
	}
	u, err := s.cfg.Deps.UsageOf(r.PathValue("uid"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// checkinOne 对单号执行签到（账号详情页「签到」按钮）。
//
// 签到不是错误语义的操作：「今天已签」「没有签到体系」都是正常结果，
// 所以一律 200 + state 字段，由前端按 state 着色，不用 HTTP 状态码表达业务结果。
func (s *Server) checkinOne(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Deps.CheckinOne == nil {
		writeErr(w, http.StatusNotImplemented, "该部署未启用签到")
		return
	}
	res, err := s.cfg.Deps.CheckinOne(r.PathValue("uid"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// checkinAll 全池一键签到（账号页「一键签到」按钮）。
//
// 逐号返回结果而不是只回 ok：用户需要看到哪个号签上了、哪个号没有签到体系、
// 哪个号还在接入中，否则一个「成功」等于什么都没说。
func (s *Server) checkinAll(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Deps.CheckinAll == nil {
		writeErr(w, http.StatusNotImplemented, "该部署未启用签到")
		return
	}
	results := s.cfg.Deps.CheckinAll()
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// identity 身份自检：确认该号 COSY 请求带的 uid 是否就是上游认定的真实 uid。
//
// 为什么单独开一个接口而不是塞进 accountDetail：探测要打上游（有网络代价与
// 频次），而详情页是高频操作。分开后前端点击才触发，不打扰常规浏览。
func (s *Server) identity(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Deps.IdentityOf == nil {
		writeErr(w, http.StatusNotImplemented, "该部署未启用身份自检")
		return
	}
	res, err := s.cfg.Deps.IdentityOf(r.PathValue("uid"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// adoptIdentity 采纳上游确认真实 uid，纠正该号的 COSY 身份。
//
// 只写 extra.cosyUID，**不动 uid 本身**：uid 是池主键（auth.json 键、
// account_keys 键、state.json 键都挂在它上面），改了会让账号关联整体断裂。
// 请求体 {real_uid} 为空时由上游自动探测。
func (s *Server) adoptIdentity(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Deps.AdoptIdentity == nil {
		writeErr(w, http.StatusNotImplemented, "该部署未启用身份纠正")
		return
	}
	var body struct {
		RealUID string `json:"real_uid"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	res, err := s.cfg.Deps.AdoptIdentity(r.PathValue("uid"), body.RealUID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// renewAll 全部巡检续期（= scheduler.RunTokenWatchNow）。
func (s *Server) renewAll(w http.ResponseWriter, r *http.Request) {
	if err := s.cfg.Deps.RenewAll(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// readKeys 读四类密钥。账号密钥默认掩码，?reveal=1 才回明文。
func (s *Server) readKeys(w http.ResponseWriter, r *http.Request) {
	k, err := s.cfg.Deps.ReadKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("reveal") != "1" {
		k.APIKey = maskSecret(k.APIKey)
		for key := range k.APIKeys {
			k.APIKeys[key] = "" // 键本身就是密钥，掩码时只保留"有哪些"
		}
		for key := range k.ProviderKeys {
			k.ProviderKeys[key] = ""
		}
		for label, m := range k.AccountKeys {
			for uid := range m {
				m[uid] = maskSecret(m[uid])
			}
			k.AccountKeys[label] = m
		}
	}
	writeJSON(w, http.StatusOK, k)
}

// writeKeys 写密钥。必须走完整校验（重复/互斥/合法性），
// 否则会存进一份网关启动即 fatal 的配置。
func (s *Server) writeKeys(w http.ResponseWriter, r *http.Request) {
	var body KeysDTO
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if err := s.cfg.Deps.WriteKeys(body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// readSettings 读配置（已脱敏：密钥类字段不在这里暴露）。
func (s *Server) readSettings(w http.ResponseWriter, r *http.Request) {
	m, err := s.cfg.Deps.ReadSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// writeSettings 写配置。不能只 patch 传入字段 —— 必须
// 「读全量 → 合并 → normalize → 写」，否则用户改一处、其它被默认值覆盖。
func (s *Server) writeSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if err := s.cfg.Deps.WriteSettings(body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// logs 增量拉取日志。?since=<seq> 取更新的；?tail=<n> 取最后 n 行。
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if s.cfg.LogRing == nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []LogLineDTO{}, "max_seq": 0})
		return
	}
	if t := r.URL.Query().Get("tail"); t != "" {
		n, _ := strconv.Atoi(t)
		lines := s.cfg.LogRing.Tail(n)
		var max int64
		if len(lines) > 0 {
			max = lines[len(lines)-1].Seq
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "max_seq": max})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	lines, max := s.cfg.LogRing.Since(since)
	if lines == nil {
		lines = []LogLineDTO{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "max_seq": max})
}

// maskSecret 保留首 6 尾 4，中间打码。短于 12 位整个打码。
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 12 {
		return strings.Repeat("*", len(s))
	}
	return s[:6] + "…" + s[len(s)-4:]
}

// 编译期断言：Deps 的字段用到了 fmt/strings，避免未使用 import 报错
var _ = fmt.Sprintf
