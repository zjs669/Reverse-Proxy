// checkin.go 网关侧签到接口（对外暴露给 App / 脚本用）。
//
// 为什么不复用 /admin/api/*/checkin：管理台那套是「口令 + Cookie + CSRF」，
// 浏览器专用。APK 和命令行客户端只有 API key，打不进来。
// 但签到本身跟「谁能调」是两件事——所以这里复用同一套四类密钥鉴权
// （主密钥 / 区域密钥 / 账号级密钥 / 供应商密钥），语义与 /usage 完全一致：
//
//	主密钥          → 全池签到
//	区域密钥（cn）   → 只签国内号
//	账号级密钥       → 只签绑定的那一个号
//	供应商密钥       → 只签该供应商的号
//
// 幂等：已签到返回 state=already，不是错误。
package server

import (
	"net/http"
	"sort"
	"sync"

	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/pool"
	"reverse-proxy/internal/upstream"
)

// checkinConcurrency 签到并发度上限。
//
// 各账号是完全独立的上游请求，全并发最快，但同一供应商多号同时打同一个
// 签到接口很容易触发风控（trae 的 claim 已实测会返回「当前参与用户太多」）。
// 4 是折中：10 个号也能从 ~10×RTT 降到 ~3×RTT，又不至于被上游限流。
const checkinConcurrency = 4

// checkinItem 单个账号的签到结果（JSON 字段供 App / 脚本渲染）。
type checkinItem struct {
	UID        string `json:"uid"`
	Provider   string `json:"provider"`
	Nickname   string `json:"nickname,omitempty"`
	State      string `json:"state"`
	Credit     int64  `json:"credit,omitempty"`
	StreakDays int    `json:"streak_days,omitempty"`
	Msg        string `json:"msg,omitempty"`
}

// checkin POST /checkin：对当前密钥可见的账号执行一次每日签到。
//
// 用 POST 而非 GET：签到有副作用（消耗当日名额）。GET 会被浏览器/爬虫
// 预取，等于白签——必须是不被预取的方法。
func (h *Handler) checkin(w http.ResponseWriter, r *http.Request) {
	region := regionFromCtx(r)
	uid := uidFromCtx(r)
	provider := providerFromCtx(r)

	// 可见范围与 /status、/usage 同口径：账号级密钥只见到绑定的那一个号，
	// 区域/供应商密钥按各自维度过滤，主密钥全池。
	var accounts []pool.Status
	if uid != "" {
		accounts = h.conf().Pool.ListByUID(uid)
	} else {
		accounts = h.conf().Pool.ListIn(region, provider)
	}

	jobs := make([]struct {
		st pool.Status
		a  *auth.Auth
	}, 0, len(accounts))
	for _, st := range accounts {
		a := h.conf().Pool.AccountByUID(st.UID)
		if a == nil {
			continue
		}
		jobs = append(jobs, struct {
			st pool.Status
			a  *auth.Auth
		}{st: st, a: a})
	}

	// 并发签到，结果按下标写回，最后排序保证输出顺序稳定
	// （否则每次响应里账号顺序都不一样，App 列表会闪）。
	out := make([]checkinItem, len(jobs))
	limit := checkinConcurrency
	if limit <= 0 || limit > len(jobs) {
		limit = len(jobs)
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			jb := jobs[i]
			res, _ := h.conf().Upstream.Checkin(jb.a)
			it := checkinItem{
				UID:        jb.st.UID,
				Provider:   jb.st.Provider,
				Nickname:   jb.st.Nickname,
				State:      res.State.String(),
				Credit:     res.Credit,
				StreakDays: res.StreakDays,
				Msg:        res.Msg,
			}
			// 签到成功且拿到了到账额度时回写池内 credits，
			// 否则 /status 显示的还是签前的旧余额。
			if res.State == upstream.CheckinOK && res.Credit > 0 {
				h.conf().Pool.SetCredits(jb.st.UID, res.Credit)
			}
			out[i] = it
		}(i)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Nickname != out[j].Nickname {
			return out[i].Nickname < out[j].Nickname
		}
		return out[i].UID < out[j].UID
	})

	// 汇总成状态计数，调用方不用自己遍历就能给出一句人话。
	sum := map[string]int{}
	for _, it := range out {
		sum[it.State]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": out,
		"summary":  sum,
		"total":    len(out),
	})
}
