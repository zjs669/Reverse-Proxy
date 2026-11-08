// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/logfmt"
	"reverse-proxy/internal/pool"
	"reverse-proxy/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool

	// ── token 自动续期看护（见 token.go）────────────────────────────────────
	//
	// TokenWatchInterval 巡检间隔（0 = 默认 10m）。与 KeepaliveHours 的「按钟点」巡检
	// 互补：这是「按剩余寿命」的连续看护，专治 trae 那种 8h 短寿命凭证。
	TokenWatchInterval time.Duration
	// RefreshWindow 通用提前续期窗口（0 = 默认 30m）：剩余有效期小于它即换新。
	RefreshWindow time.Duration
	// RefreshWindowByProvider 按 provider 覆盖窗口（如 trae → 2h）。
	// 必须显著小于该家凭证寿命，否则永远追不上过期。
	RefreshWindowByProvider map[string]time.Duration
	// TokenWatchDisabled 显式关闭自动续期看护（schedule.token_watch_enabled=false）。
	TokenWatchDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// tokenTried 续期失败退避记录：uid → 上次尝试时刻（见 token.go）。
	tokenTried map[string]time.Time

	// tokenDead 「已过期且自救失败」的令牌指纹：uid → 指纹。指纹不变就不再重试，
	// 等重新登录换掉令牌后自动恢复尝试（见 token.go）。
	tokenDead map[string]string
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	return &Scheduler{
		cfg:        cfg,
		adoptTried: make(map[string]string),
		tokenTried: make(map[string]time.Time),
		tokenDead:  make(map[string]string),
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !s.cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !s.cfg.TravelDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.TravelHours), taskTravel})
	}
	if !s.cfg.ActivityDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.ActivityHours), taskActivity})
	}
	if !s.cfg.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	// token 自动续期看护是「按剩余寿命」的独立循环，不受下面钟点排程影响，
	// 故单独起一个 goroutine（见 token.go）。ctx 取消时自然退出。
	go s.tokenLoop(ctx)
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，只等退出信号。
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			for _, k := range kinds {
				switch k {
				case taskCheckin:
					s.RunCheckinNow()
				case taskTravel:
					s.RunTravelNow()
				case taskActivity:
					s.RunActivityNow()
				case taskKeepalive:
					s.RunKeepaliveNow()
				}
			}
		}
	}
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
//
// 签到按 provider 分派（upstream.Client.Checkin）：不再硬编码只跑 CodeBuddy。
// 没有签到体系的供应商返回 Unsupported，静默跳过——所以这里不需要 provider 白名单。
func (s *Scheduler) RunCheckinNow() {
	// 必须用 ListAll：List() == ListIn("","")，空 provider 的语义是「仅 codebuddy」，
	// trae/qoder/deepseek 全会被漏掉——定时签到实际只覆盖 codebuddy 一家。
	for _, st := range s.cfg.Pool.ListAll() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// CheckinOne 内部已含「签到 + 刷余额 + 解冻」。
		s.CheckinOne(a)
	}
}

// CheckinOne 对单个账号签到 + 刷余额 + 解冻，返回签到结果供管理台展示。
//
// **签到后必须刷余额**：签到本身只把积分发到账，网关侧 pool 里缓存的 credits
// 还是旧值。不刷的话管理台点完签到积分列纹丝不动，用户会以为没签上。
//
// 分级记日志的意义：默认时点有 09:00 / 21:00 两趟，第二趟必然「今天已签」。
// 若把它和真失败一样打 WARN，日志里就再也看不出哪次是真出问题。
//
// 导出供管理台「签到」「一键签到」按钮调用——与定时任务走同一条路径，
// 避免手点和自动跑出两套行为。
func (s *Scheduler) CheckinOne(a *auth.Auth) upstream.CheckinResult {
	res, err := s.cfg.Upstream.Checkin(a)
	if err != nil {
		log.Printf("WARN: checkin %s: %v", logfmt.UID8(a.UID), err)
		return upstream.CheckinResult{State: upstream.CheckinFailed, Msg: err.Error()}
	}
	switch res.State {
	case upstream.CheckinOK:
		log.Printf("checkin %s: +%d（连签 %d 天）", logfmt.UID8(a.UID), res.Credit, res.StreakDays)
	case upstream.CheckinAlready:
		if res.Msg != "" {
			log.Printf("checkin %s: %s", logfmt.UID8(a.UID), res.Msg)
		} else {
			log.Printf("checkin %s: 今日已签", logfmt.UID8(a.UID))
		}
	case upstream.CheckinUnsupported, upstream.CheckinPending:
		// 没有签到体系 / 接口待接入：静默，避免每天刷无意义日志
	case upstream.CheckinFailed:
		log.Printf("WARN: checkin %s: %s", logfmt.UID8(a.UID), res.Msg)
	}
	// 无论签到结果如何都刷一次余额并尝试解冻：
	// 签到失败不代表余额没恢复（可能是别的设备签过了）。
	s.refreshCredits(a.UID, a)
	return res
}

// refreshCredits 查余额并尝试解冻。
//
// 走 Client.Usage（codebuddy/trae/qoder 都实现了），而不是只有 CodeBuddy 才有的
// UserResource——否则 trae/qoder 签到了也不会解冻。
func (s *Scheduler) refreshCredits(uid string, a *auth.Auth) {
	raw, err := s.cfg.Upstream.Usage(a)
	if err != nil {
		log.Printf("usage %s: %v", logfmt.UID8(uid), err)
		return
	}
	if raw == nil {
		return // 该供应商没有额度接口
	}
	s.cfg.Pool.ReenableIfCredits(uid, upstream.CreditsInt64(raw))
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
//
// 每号上报 N 条（ActivityReportCount，默认 5）：N 条共用同一 conversationId
// （reverse-proxy-<ms>），模拟同一会话内 N 轮对话——这是领养猫（buddy/first）对话量
// 门槛的实测刷法（chat_5 前置需 5 次对话）。requestId 各条独立（同会话多轮）。
// 账号内 N 条之间间隔 activityReportGap（1.5s）避免秒发触发风控。
//
// 0/缺省 ActivityReportCount = 1 条，兼容旧行为（仅点亮连登 + 解锁 first_buddy）。
//
// 上报成功后：① streak 自检（回读连登，发现「200 但静默丢弃」）；
// ② 无猫账号立即重试领养（travelAdoptForce）——对话量刚补满的新状态，不算重试，
// 豁免 adoptTriedToday 当日防抖（旅行排程 09 点已领养过且 skip，10 点上报补满后
// 不能依赖下一轮旅行领养，就地闭环）。
func (s *Scheduler) RunActivityNow() {
	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		// 非 CodeBuddy 账号（trae/qoder）无 WorkBuddy 活跃上报/领猫体系，跳过。
		if st.Provider != "" && st.Provider != "codebuddy" {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if !first {
			time.Sleep(activityAccountDelay)
		}
		first = false
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("reverse-proxy-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d: %v", logfmt.UID8(a.UID), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("activity %s: report %d/%d ok", logfmt.UID8(a.UID), i, count)
			ok++
			if i < count {
				time.Sleep(activityReportGap) // 账号内 5 条之间间隔，避免秒发风控
			}
		}
		if ok < count {
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检（只留结论行）
		s.travelAdoptForce(a)    // 无猫账号对话量刚补满 → 立即重试领养（豁免防抖）
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report OK): %v", logfmt.UID8(a.UID), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", logfmt.UID8(a.UID))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.UID8(a.UID), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	// 用 ListAll 而非 List：List() 是「仅 codebuddy」语义，下方那条 trae 分支
	// 在 List() 下永远不会被触发（这正是 trae 一直没被续期的原因）。
	for _, st := range s.cfg.Pool.ListAll() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// trae 续期不需要 refreshToken（X-Cloudide-Token 换新 JWT）；其余家没有 refreshToken 就无从刷新。
		if a.RefreshToken == "" && a.ProviderOrDefault() != "trae" && a.ProviderOrDefault() != "deepseek" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.UID8(st.UID), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.UID8(st.UID), err)
		}
	}
}
