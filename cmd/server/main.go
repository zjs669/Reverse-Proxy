// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/dsconv"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	var auths []*auth.Auth
	srcDesc := ""
	if cfg.AuthFile != "" {
		a, err := auth.LoadFile(cfg.AuthFile)
		if err != nil {
			log.Printf("load auth_file %s failed: %v", cfg.AuthFile, err)
		} else {
			auths = a
			srcDesc = cfg.AuthFile
		}
	}
	if len(auths) == 0 && cfg.AuthDir != "" {
		a, err := auth.LoadDir(cfg.AuthDir)
		if err != nil {
			log.Fatalf("load auths: %v", err)
		}
		auths = a
		srcDesc = cfg.AuthDir
	}
	if len(auths) == 0 {
		log.Fatalf("no accounts loaded (auth_file=%q auth_dir=%q)", cfg.AuthFile, cfg.AuthDir)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), srcDesc)
	logAccountHealth(auths)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	// DeepSeek 上游会话状态（chat_session_id + parent_message_id）。
	// 必须持久化：上游会话 3 天过期、换号即失效，网关靠这份映射复用同一窗口的会话。
	dsStore := dsconv.New(dsconv.Config{
		TTL:  cfg.DeepSeekSessionTTL,
		Path: cfg.DeepSeek.SessionFile,
	})
	up := upstream.New()
	up.SetDeepSeekStore(dsStore)
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA 覆盖（issue #42）：非空才改写，空 = 现状 clientUA（指纹净化考虑）。
	up.UserAgent = cfg.Upstream.UserAgent

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,

		TokenWatchInterval:      cfg.TokenWatchIntervalDur,
		RefreshWindow:           cfg.RefreshWindowDur,
		RefreshWindowByProvider: cfg.RefreshWindowByProvider,
		TokenWatchDisabled:      !cfg.Schedule.TokenWatchEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	// token 自动续期看护：与上面的钟点保活互补，按剩余寿命提前换新（短寿命凭证的唯一活路）。
	if !cfg.Schedule.TokenWatchEnabled {
		log.Printf("token 自动续期已禁用（schedule.token_watch_enabled=false）")
	} else {
		wins := ""
		for prov, d := range cfg.RefreshWindowByProvider {
			wins += fmt.Sprintf("，%s %s", prov, d)
		}
		log.Printf("token 自动续期已启用：每 %s 巡检一次，窗口 默认 %s%s",
			cfg.TokenWatchIntervalDur, cfg.RefreshWindowDur, wins)
	}

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		APIKeys:      cfg.APIKeys,
		AccountKeyToUID: cfg.AccountKeyToUID,
		ProviderKeyToProvider: cfg.ProviderKeyToProvider,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown:     cfg.SoftRateDur,
		Models:           cfg.Models,
		CooldownDisabled: cfg.Cooldown.Disabled,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		MaxBodyBytes: int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// deepseek 会话状态：定期清理过期条目并落盘（30s 一次，仅在有变更时真正写盘）。
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				dsStore.GC()
				_ = dsStore.Flush()
				return
			case <-t.C:
				dsStore.GC()
				if err := dsStore.Flush(); err != nil {
					log.Printf("WARN: [deepseek] 会话状态落盘失败: %v", err)
				}
			}
		}
	}()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		_ = dsStore.Flush() // DeepSeek 会话坐标同样落盘，重启后上下文不丢
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v, region_keys=%d, account_keys=%d, provider_keys=%d)", cfg.Listen, cfg.APIKey != "", len(cfg.APIKeys), len(cfg.AccountKeys), len(cfg.ProviderKeys))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// knownProviders 网关已实现的上游（与 upstream.registerProviders 对齐）。
var knownProviders = []string{"codebuddy", "trae", "qoder", "doubao", "deepseek"}

// logAccountHealth 启动时体检凭证：过期/临期打 WARN 并给出续期命令，缺账号的供应商也提示一句。
// 否则这些账号只在第一次真实请求时以 503 / 拉模型失败暴露，排查成本高（交接文档 §5.2~§5.4）。
func logAccountHealth(auths []*auth.Auth) {
	now := time.Now()
	present := make(map[string]bool, len(knownProviders))
	for _, a := range auths {
		p := a.ProviderOrDefault()
		present[p] = true
		if a.ExpiresAt <= 0 {
			continue // 无过期信息（部分手填凭证），不做判断
		}
		label := a.Nickname
		if label == "" {
			label = logfmt.UID8(a.UID)
		}
		exp := time.Unix(a.ExpiresAt, 0)
		renew := renewCommand(p, a.Region())
		switch {
		case now.After(exp):
			suffix := ""
			switch {
			case p == "trae":
				// trae 可以不靠 refreshToken 自助续期（X-Cloudide-Token 换新 JWT），
				// 但那个接口只接受**未过期**的 JWT，所以过了 exp 就真的只能重新登录。
				suffix = "，已错过自助续期窗口（续期接口只接受未过期的 JWT），需人工重新登录"
			case a.RefreshToken == "":
				suffix = "，且无 refreshToken（不能自动刷新）"
			}
			log.Printf("WARN: [auth] %s/%s 凭证已于 %s 过期%s；续期：%s",
				p, label, exp.Format("2006-01-02 15:04"), suffix, renew)
		case exp.Sub(now) < time.Hour:
			log.Printf("WARN: [auth] %s/%s 凭证 %s 后过期（%s）；续期：%s",
				p, label, exp.Sub(now).Round(time.Hour), exp.Format("2006-01-02 15:04"), renew)
		}
	}
	for _, p := range knownProviders {
		if !present[p] {
			log.Printf("INFO: [auth] 供应商 %s 没有账号（该家对话/模型会返回 503）；接入：%s", p, renewCommand(p, "cn"))
		}
	}
}

// renewCommand 返回该账号的续期/登录命令（与 cmd/login 的参数顺序一致）。
// 只有 codebuddy 有 region 维度（`login codebuddy <cn|intl> <昵称>`）；trae/qoder/doubao
// 的参数是 `login <provider> <昵称>`，多写一个 cn 会被当成昵称，故单独分支。
func renewCommand(provider, region string) string {
	if provider == "codebuddy" && region == "intl" {
		return "./login codebuddy intl <昵称>"
	}
	if provider == "codebuddy" {
		return "./login codebuddy cn <昵称>"
	}
	if provider == "trae" || provider == "qoder" || provider == "doubao" || provider == "deepseek" {
		return "./login " + provider + " <昵称>"
	}
	return "./login " + provider + " cn <昵称>"
}
