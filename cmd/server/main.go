// main.go Reverse-Proxy 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"reverse-proxy/internal/admin"
	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/dsconv"
	"reverse-proxy/internal/logfmt"
	"reverse-proxy/internal/pool"
	"reverse-proxy/internal/redisstore"
	"reverse-proxy/internal/scheduler"
	"reverse-proxy/internal/server"
	"reverse-proxy/internal/session"
	"reverse-proxy/internal/upstream"
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

	// 零账号不是致命错误：容器必须先跑起来，才有 `docker exec ... /app/login` 这条接入
	// 路径（restarting 状态的容器无法 exec）；否则凭证文件缺失会把网关打成
	// 「退出码 1 → restart: unless-stopped 拉起 → 再退出」的无限重启循环。
	auths, srcDesc, err := loadAuths(cfg)
	if err != nil {
		log.Printf("WARN: [auth] 读取账号失败：%v", err)
	}
	if len(auths) == 0 {
		log.Printf("WARN: [auth] 当前没有账号（auth_file=%q auth_dir=%q）：网关以空池启动，上游请求一律 503",
			cfg.AuthFile, cfg.AuthDir)
		log.Printf("WARN: [auth] 添加账号：docker exec -it <容器名> /app/login codebuddy cn <昵称>（写入 %s 后 %s 内自动入池，无需重启）",
			cfg.AuthFile, authWatchInterval)
	} else {
		log.Printf("loaded %d account(s) from %s", len(auths), srcDesc)
		logAccountHealth(auths)
	}

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	if len(auths) > 0 {
		p.SyncToDir(auths) // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）
	} else {
		// 空池启动：清掉 state.json 继承来的 placeholder 条目（无凭证也会被 Pick 选中），
		// 但不落盘——历史 credits/冷却等状态等账号文件出现时由 SyncToDir 接回。
		p.DropPlaceholders()
	}

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	// cooldown.disabled=true：关闭网关侧全部频率限制与风控——后续错误不再施加
	// 冷却/熔断/禁用（pool 入口空操作），并抹平 state.json 继承来的历史惩罚，
	// 使本地任何账号立即可用（账号级密钥不会因上一轮的 429 被挡在门外）。
	if cfg.Cooldown.Disabled {
		p.SetPenaltyDisabled(true)
		if n := p.ClearPenalties(); n > 0 {
			log.Printf("限流冷却已禁用（cooldown.disabled=true）：抹平 %d 个账号的历史冷却/禁用状态", n)
		} else {
			log.Printf("限流冷却已禁用（cooldown.disabled=true）：不冷却/不禁用任何账号")
		}
	}

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

	h := server.NewHandler(buildHandlerConfig(cfg, p, up, sessRouter, sessCount, redisMode))

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

	// 凭证热加载：login 在容器内是一次性进程（docker exec），与本进程没有调用通道，
	// 文件是唯一交界面——轮询对齐既能免去「加账号必须重启容器」（掐断在途 SSE），
	// 也让管理台删号在数秒内生效。
	go watchAuths(ctx, cfg, *cfgPath, p, auths, func(nc *Config) {
		// 与管理员台热重载同口径：只换 handler 侧密钥（api_key/api_keys/account_keys），
		// 让 login 写入的账号专属密钥立即可用。
		h.Reload(buildHandlerConfig(nc, p, up, sessRouter, sessCount, redisMode))
	})

	// ── Web 管理台（docs/admin-console.md）──────────────────────────────
	// 内嵌在同一进程、同一端口（/admin/*），不另起服务。
	// 默认关闭；开启后默认只允许回环/内网访问，CIDR 外返回 404。
	ring := newLogRingHandle(2000)
	// 日志同时写 stdout 和环形缓冲，管理台的「日志」页才有东西看。
	// 注意：只接管 log 包（log.Printf），请求级的表格日志走 fmt.Fprintf(os.Stdout)，
	// 那部分不进缓冲 —— 它是给人看的流水，不是排障信息。
	log.SetOutput(io.MultiWriter(os.Stdout, ring.Writer()))

	writer := &cfgWriter{path: *cfgPath}

	var adminSrv *admin.Server
	if cfg.Admin.Enabled {
		deps := buildAdminDeps(p, up, sch, ring, writer, *cfgPath, func(nc *Config) {
			// 热重载：把新配置推给 handler，旧密钥立刻失效、新密钥立刻生效
			h.Reload(buildHandlerConfig(nc, p, up, sessRouter, sessCount, redisMode))
			log.Printf("[admin] 配置已热重载（api_key=%v, account_keys=%d）",
				nc.APIKey != "", len(nc.AccountKeys))
		})
		// 补齐需要运行时才知道的两项
		deps.RedisMode = func() string { return redisMode }
		deps.StickyCount = sessCount

		a, aerr := admin.New(admin.Config{
			Enabled:   true,
			Password:  cfg.Admin.Password,
			AllowCIDR: cfg.Admin.AllowCIDR,
			LogRing:   ring.Source(),
			Deps:      deps,
		})
		if aerr != nil {
			// 管理台起不来不该拖垮网关：记一条错误然后跳过
			log.Printf("WARN: [admin] 未启用：%v", aerr)
		} else {
			adminSrv = a
			log.Printf("Web 管理台已启用：/admin（允许来源 %v）", cfg.Admin.AllowCIDR)
		}
	}

	// 根 handler：/admin 前缀交给管理台，其余走网关。
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adminSrv != nil && strings.HasPrefix(r.URL.Path, "/admin") {
			adminSrv.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           root,
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

	log.Printf("Reverse-Proxy listening on %s (api_key=%v, region_keys=%d, account_keys=%d, provider_keys=%d)", cfg.Listen, cfg.APIKey != "", len(cfg.APIKeys), len(cfg.AccountKeys), len(cfg.ProviderKeys))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// knownProviders 网关已实现的上游（与 upstream.registerProviders 对齐）。
var knownProviders = []string{"codebuddy", "trae", "qoder", "doubao", "deepseek"}

// buildHandlerConfig 组装 server.Config。
//
// 抽成函数是为了热重载能复用：管理台改完密钥后要拿同一套依赖
// 重新构造一份配置推给 Handler.Reload，不能只换 APIKey —— 那样
// AccountKeyToUID 反查表还是旧的，账号级密钥会整片失效。
func buildHandlerConfig(
	cfg *Config,
	p *pool.Pool,
	up *upstream.Client,
	sessRouter *session.Router,
	sessCount func() int,
	redisMode string,
) server.Config {
	return server.Config{
		Pool:                  p,
		Upstream:              up,
		APIKey:                cfg.APIKey,
		APIKeys:               cfg.APIKeys,
		AccountKeyToUID:       cfg.AccountKeyToUID,
		ProviderKeyToProvider: cfg.ProviderKeyToProvider,
		Session:               sessRouter,
		StickyCount:           sessCount,
		RedisMode:             redisMode,
		SoftCooldown:          cfg.SoftRateDur,
		Models:                cfg.Models,
		CooldownDisabled:      cfg.Cooldown.Disabled,
		PromptMode:            cfg.Prompt.Mode,
		PromptText:            cfg.PromptText,
		MaxBodyBytes:          int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
	}
}

// logAccountHealth 启动时体检凭证：过期/临期打 WARN 并给出续期命令，缺账号的供应商也提示一句。
// 否则这些账号只在第一次真实请求时以 503 / 拉模型失败暴露，排查成本高（交接文档 §5.2~§5.4）。
func logAccountHealth(auths []*auth.Auth) {
	now := time.Now()
	present := make(map[string]bool, len(knownProviders))
	seen := make(map[string]string, len(auths)) // uid → 首个占有它的槽位标签
	for _, a := range auths {
		p := a.ProviderOrDefault()
		present[p] = true
		// 同 uid 出现两份凭证（典型成因：旧版 login 续期时写了 trae_2 而不是覆盖 trae_1）：
		// 池按 UID 建 entry，而 LoadFile 遍历 map 顺序随机，最终哪枚令牌生效不确定——
		// 表现就是“明明重登了还是 401”，必须启动就暴露而不是让人去猜随机数。
		if a.UID != "" {
			slot := a.FileKey
			if slot == "" {
				slot = a.Nickname
			}
			if prev, dup := seen[a.UID]; dup {
				log.Printf("WARN: [auth] uid=%s 在 %s 与 %s 各有一份凭证，入池时后者覆盖前者（顺序随机）：请删掉过期那条",
					logfmt.UID8(a.UID), prev, slot)
			} else {
				seen[a.UID] = slot
			}
		}
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
				// GetUserToken 认 X-Cloudide-Session（= JWT 里的 data.source_id，登录会话，长期有效），
				// 所以 JWT 过期后依然能自愈，token 看护会自动换新；只有连会话凭证也失效
				// （在别处登出、或 auth.json 里没有 sourceId）才真需要人工重登。
				suffix = "，但 Trae 可用登录会话（X-Cloudide-Session）自愈，token 看护会自动换新令牌"
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

// authWatchInterval 凭证文件轮询周期。
//
// login 是容器内的一次性进程（docker exec），与本进程没有调用通道，文件是唯一交界面；
// 重启容器虽然也能加载账号，但会掐断在途 SSE，所以改为按固定周期对齐。
const authWatchInterval = 5 * time.Second
// loadAuths 读取账号凭证：优先 auth_file（单文件，auth.json 顶层 map），为空时回退 auth_dir。
// 返回 src 为空串即「没读到任何账号」——这是合法的启动态，调用方不应据此退出。
// 内部不打日志：启动时由调用方详述一次，热加载每 5s 一轮不能刷屏。
func loadAuths(cfg *Config) (auths []*auth.Auth, src string, err error) {
	var firstErr error
	if cfg.AuthFile != "" {
		a, e := auth.LoadFile(cfg.AuthFile)
		switch {
		case e != nil && !os.IsNotExist(e):
			// 文件存在但读不动/解析失败（JSON 损坏等）：记账给调用方打日志，
			// 但仍继续尝试 auth_dir（两者可共存，单文件坏了目录里的还能用）。
			firstErr = fmt.Errorf("load auth_file %s: %w", cfg.AuthFile, e)
		case e == nil && len(a) > 0:
			return a, cfg.AuthFile, nil
		}
	}
	if cfg.AuthDir != "" {
		a, e := auth.LoadDir(cfg.AuthDir)
		if e != nil {
			return nil, "", fmt.Errorf("load auth dir %s: %w", cfg.AuthDir, e)
		}
		if len(a) > 0 {
			return a, cfg.AuthDir, nil
		}
	}
	return nil, "", firstErr
}

// watchAuths 周期对齐凭证文件与账号池：新增账号入池、已删除的剔除、重新登录写回的新凭证生效。
//
// onConfigChanged 在账号有新增时顺带重载 config.json——login 会把新账号的专属密钥写进
// account_keys，不重载则新密钥要等重启才认。
//
// 只在磁盘快照真发生变化时才动池：SyncToDir 会用磁盘凭证替换池内 entry，
// 每轮无条件替换会与正在进行中的 token 刷新竞态。
func watchAuths(ctx context.Context, cfg *Config, cfgPath string, p *pool.Pool,
	initial []*auth.Auth, onConfigChanged func(*Config)) {
	prev := indexAuths(initial)
	t := time.NewTicker(authWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fresh, src, err := loadAuths(cfg)
		if err != nil {
			continue // 目录暂时不可读等：保持现状，下轮再看
		}
		cur := indexAuths(fresh)
		added, removed, updated := diffAuths(prev, cur)
		if len(added)+len(removed)+len(updated) == 0 {
			continue
		}
		p.SyncToDir(fresh)
		srcLabel := src
		if srcLabel == "" {
			srcLabel = "空池"
		}
		log.Printf("[auth] 账号文件变更已生效（%s）：新增 %d / 移除 %d / 凭证更新 %d",
			srcLabel, len(added), len(removed), len(updated))
		for _, uid := range added {
			log.Printf("[auth]   + %s %s/%s", logfmt.UID8(uid), cur[uid].ProviderOrDefault(), cur[uid].Nickname)
		}
		for _, uid := range removed {
			log.Printf("[auth]   - %s", logfmt.UID8(uid))
		}
		if len(added) > 0 && onConfigChanged != nil {
			if nc, e := Load(cfgPath); e == nil {
				onConfigChanged(nc)
			} else {
				log.Printf("WARN: [auth] 新账号已入池，但重载配置失败（专属密钥暂不可用）：%v", e)
			}
		}
		prev = cur
	}
}

// indexAuths 以 UID 为键建索引（池也按 UID 建 entry，口径一致）。
func indexAuths(auths []*auth.Auth) map[string]*auth.Auth {
	m := make(map[string]*auth.Auth, len(auths))
	for _, a := range auths {
		m[a.UID] = a
	}
	return m
}

// diffAuths 比较两轮磁盘快照：added 新出现的 UID、removed 消失的 UID、
// updated = 同一 UID 的凭证变了（重新登录覆盖槽位 / 本进程刷新后落盘）。
func diffAuths(prev, cur map[string]*auth.Auth) (added, removed, updated []string) {
	for uid, a := range cur {
		old, ok := prev[uid]
		if !ok {
			added = append(added, uid)
			continue
		}
		if a.AccessToken != old.AccessToken || a.RefreshToken != old.RefreshToken ||
			a.ExpiresAt != old.ExpiresAt || a.ProviderOrDefault() != old.ProviderOrDefault() {
			updated = append(updated, uid)
		}
	}
	for uid := range prev {
		if _, ok := cur[uid]; !ok {
			removed = append(removed, uid)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(updated)
	return added, removed, updated
}
