// token.go token 自动续期看护（token watchdog）。
//
// 为什么需要它：不同 provider 的凭证寿命差得很远——trae 的 Cloud-IDE-JWT 只有 8h，
// 而 keepalive_hours 默认只在 22 点刷一次。8h < 24h，意味着每天有 16 小时令牌是死的。
// 更麻烦的是 trae 的续期接口（GetUserToken）要求传入的 JWT **尚未过期**：实测过期
// 12.5h 后带着旧 token 调它返回 401 / 20101「Token 无效」，也就是说一旦错过窗口，
// 就只能人工重新登录。
//
// 所以这里做的是「**提前**续期」：按固定间隔（token_watch_interval，默认 10m）巡检，
// 剩余有效期一旦进入窗口（refresh_window，默认 30m；trae 覆盖为 2h）就立刻换新。
// 窗口必须显著小于凭证寿命，才能保证在过期之前把新令牌拿到手。
//
// 与 keepalive 的分工：
//   - keepalive_hours 是「按点」的兜底巡检（顺带做 12153 session 死亡判定）；
//   - 本看护是「按剩余寿命」的连续看护，与钟点无关，二者共用 provider 的 RefreshToken。
//
// 额外职责：续期成功即解除「凭证类禁用」。原因是 RunKeepaliveNow 会跳过 disabled 账号，
// 若不在这里回血，「一次误判禁用 → 再也不会被巡检 → 永久死号」就是死锁。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/logfmt"
	"reverse-proxy/internal/upstream"
)

const (
	// defaultTokenWatchInterval 未配置时的巡检间隔。
	defaultTokenWatchInterval = 10 * time.Minute

	// defaultRefreshWindow 未配置时的提前续期窗口。
	defaultRefreshWindow = 30 * time.Minute

	// tokenRetryBackoff 同一账号续期失败后的退避：窗口内只试一次，
	// 避免 token 已死时每 10 分钟对上游打一次无意义请求。
	tokenRetryBackoff = 20 * time.Minute

	// minTokenWatchInterval 巡检间隔下限：低于它只会徒增上游压力，没有任何收益。
	minTokenWatchInterval = time.Minute
)

// refreshWindowFor 返回该 provider 的提前续期窗口。
// 优先级：provider 专属配置 > 通用 refresh_window > 内置默认。
func (s *Scheduler) refreshWindowFor(provider string) time.Duration {
	if d, ok := s.cfg.RefreshWindowByProvider[provider]; ok && d > 0 {
		return d
	}
	if s.cfg.RefreshWindow > 0 {
		return s.cfg.RefreshWindow
	}
	return defaultRefreshWindow
}

// tokenLoop 常驻看护循环。启动即先跑一轮：login 后重启的场景可以立刻把禁用号复活，
// 而不是等满一个间隔。
func (s *Scheduler) tokenLoop(ctx context.Context) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil || s.cfg.TokenWatchDisabled {
		return
	}
	interval := s.cfg.TokenWatchInterval
	if interval <= 0 {
		interval = defaultTokenWatchInterval
	}
	if interval < minTokenWatchInterval {
		interval = minTokenWatchInterval
	}
	s.RunTokenWatchNow()
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.RunTokenWatchNow()
		}
	}
}

// RunTokenWatchNow 巡检一轮：把所有「剩余有效期已进入窗口」的账号提前续期。
//
// 与 keepalive 的两处关键差异：
//  1. **用 ListAll 而不是 List**：List() 的语义是「仅 codebuddy」（providerMatch 里
//     空 provider == codebuddy），用它遍历会把 trae 整个漏掉——历史上 trae 从未被
//     续期过，就是因为按 List() 遍历；
//  2. **包含 disabled 账号**：凭证类禁用的号只能靠续期复活，跳过就等于永久死号；
//  3. **按剩余寿命而非钟点**触发：短寿命凭证（trae 8h）不可能靠每天一次的钟点巡检养活。
func (s *Scheduler) RunTokenWatchNow() {
	now := time.Now()
	for _, st := range s.cfg.Pool.ListAll() {
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		prov := a.ProviderOrDefault()
		// 没有 refreshToken 的账号里只有 trae 能自助续期（X-Cloudide-Token 换新 JWT，
		// 见 trae.go refreshViaGetUserToken）；其余家没有 refreshToken 就无从刷新。
		if a.RefreshToken == "" && prov != "trae" && prov != "deepseek" {
			continue
		}
		// 被「凭证类」原因禁用、但手上令牌仍在有效期内 → 直接解禁，不必等续期窗口。
		//
		// 为什么必须有这条：典型链路是「token 过期 → 判定 session dead → 禁用 → 人工
		// 重新登录」。重新登录换来的是**满寿命**令牌（trae 8h），剩余寿命远大于 2h 窗口，
		// 光靠「续期成功才复活」这条永远等不到窗口，账号就一直躺在禁用名单里。
		// ReviveIfCredDead 内部只认凭证类 reason，余额/风控类禁用不会被误复活。
		if st.Disabled && a.ExpiresAt > now.Unix() {
			if s.cfg.Pool.ReviveIfCredDead(a.UID) {
				log.Printf("token-watch %s: 凭证仍在有效期（%s 后过期），已自动解除凭证类禁用（provider=%s）",
					logfmt.UID8(a.UID), shortDur(time.Duration(a.ExpiresAt-now.Unix())*time.Second), prov)
			}
		}
		remaining := time.Duration(a.ExpiresAt-now.Unix()) * time.Second
		// 同一枚**已过期**令牌不再重试：续期接口只接受未过期的 JWT，过期后重试必然 401，
		// 只会每 20 分钟给上游送一次无效请求。等用户重新登录换了令牌（指纹变化）再试。
		fp := tokenFingerprint(a.AccessToken)
		if a.ExpiresAt > 0 && remaining <= 0 && s.tokenKnownDead(st.UID, fp) {
			continue
		}
		// ExpiresAt 未知（0/负数）时无法判断寿命，直接当成「需要续期」处理一次；
		// 成功后 provider 会写回 ExpiresAt，失败则由退避控制重试频率。
		if a.ExpiresAt > 0 && remaining > s.refreshWindowFor(prov) {
			continue
		}
		if s.tokenRecentlyTried(st.UID, now) {
			continue
		}
		s.markTokenTried(st.UID, now)

		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			// 已过期 → 记下这枚令牌的指纹，后续巡检直接跳过（见上方 tokenKnownDead），
			// 直到用户重新登录换了令牌才再次尝试。
			if a.ExpiresAt > 0 && remaining <= 0 {
				s.markTokenDead(st.UID, fp)
			}
			s.reportTokenFailure(a, prov, remaining, err)
			continue
		}
		s.clearTokenDead(st.UID)
		if err := a.SaveAtomic(); err != nil {
			log.Printf("ERR: token-watch %s: save auth failed: %v", logfmt.UID8(a.UID), err)
		}
		s.cfg.Pool.ClearSessionDead(a.UID)
		revived := false
		if st.Disabled {
			revived = s.cfg.Pool.ReviveIfCredDead(a.UID)
		}
		log.Printf("token-watch %s: renewed provider=%s left_before=%s left_after=%s revived=%v",
			logfmt.UID8(a.UID), prov, shortDur(remaining), shortDur(time.Duration(a.ExpiresAt-now.Unix())*time.Second), revived)
	}
}

// tokenRecentlyTried 失败退避判定。
func (s *Scheduler) tokenRecentlyTried(uid string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.tokenTried[uid]
	return ok && now.Sub(last) < tokenRetryBackoff
}

// markTokenTried 记录本次尝试时间。
func (s *Scheduler) markTokenTried(uid string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenTried[uid] = now
}

// tokenKnownDead 该 uid 当前的令牌是否已被确认「过期且续期无望」。
func (s *Scheduler) tokenKnownDead(uid, fp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	known, ok := s.tokenDead[uid]
	return ok && known == fp
}

// markTokenDead 记下「这枚令牌已过期且自救失败」，避免重复无效重试。
func (s *Scheduler) markTokenDead(uid, fp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenDead[uid] = fp
}

// clearTokenDead 续期成功 → 该 uid 不再处于「等重新登录」状态。
func (s *Scheduler) clearTokenDead(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokenDead, uid)
}

// tokenFingerprint 令牌指纹：长度 + 尾部若干字符即可区分「换没换过令牌」，
// 不用哈希也就没有把完整令牌写进日志/内存键的风险。
func tokenFingerprint(tok string) string {
	if len(tok) <= 12 {
		return fmt.Sprintf("%d:%s", len(tok), tok)
	}
	return fmt.Sprintf("%d:%s", len(tok), tok[len(tok)-12:])
}

// reportTokenFailure 分类打印续期失败原因。重点是区分「还能自救」与「已经只能人工」：
// 前者只是本次没换到（网络抖动/上游 5xx，退避后自动重试），后者必须给人一条可复制的
// 重新登录命令——否则日志里只有一句 401，没人知道该怎么修。
func (s *Scheduler) reportTokenFailure(a *auth.Auth, prov string, remaining time.Duration, err error) {
	var ue *upstream.Error
	isDead := errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead

	// token 已过期：GetUserToken 只认未过期的 JWT，此时无法自助续期，只能重新登录。
	if a.ExpiresAt > 0 && remaining <= 0 {
		log.Printf("WARN: token-watch %s: %s token 已过期 %s，无法自助续期（GetUserToken 只接受未过期的 JWT）"+
			" → 需重新登录：docker exec -i reverse-proxy /app/login %s <名> < cred.json",
			logfmt.UID8(a.UID), prov, shortDur(-remaining), prov)
		return
	}
	if isDead {
		// 未过期却判 session dead：多半是账号在别处被踢下线（同账号多端登录），
		// 继续重试没有意义，直接给出重新登录提示。
		log.Printf("WARN: token-watch %s: %s 续期被判 session dead（left=%s）：多端登录会把旧 session 顶掉，"+
			"请重新登录：docker exec -i reverse-proxy /app/login %s <名> < cred.json",
			logfmt.UID8(a.UID), prov, shortDur(remaining), prov)
		return
	}
	log.Printf("token-watch %s: %s 续期失败（left=%s，%s 后重试）：%v",
		logfmt.UID8(a.UID), prov, shortDur(remaining), tokenRetryBackoff, err)
}

// shortDur 把时长压成一眼能读的短串（2h13m / -12h30m / 45s）。
func shortDur(d time.Duration) string {
	neg := ""
	if d < 0 {
		neg = "-"
		d = -d
	}
	if h := int(d / time.Hour); h > 0 {
		return fmt.Sprintf("%s%dh%dm", neg, h, int((d%time.Hour)/time.Minute))
	}
	if m := int(d / time.Minute); m > 0 {
		return fmt.Sprintf("%s%dm", neg, m)
	}
	return neg + d.Round(time.Second).String()
}
