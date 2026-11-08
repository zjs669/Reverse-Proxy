// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"reverse-proxy/internal/prompt"
)

// Config 顶层配置。
type Config struct {
// Admin Web 管理台。
	Admin struct {
		Enabled bool `json:"enabled"`
		// Password 登录口令；空 = 用主密钥 api_key 校验。
		Password string `json:"password"`
		// AllowCIDR 允许访问的来源网段；空 = 只允许回环。
		// 公网访问必须显式把网段加进来，否则一律 404（不暴露管理台存在）。
		AllowCIDR []string `json:"allow_cidr"`
	} `json:"admin"`

	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权；主密钥，不限区域
// APIKeys 分区域密钥表：密钥 → 区域（cn/intl），只能访问对应区域账号池。
// 与主密钥 api_key 互不影响。
	APIKeys map[string]string `json:"api_keys"`
// AccountKeys 账号级密钥表：名字 → {uid: 专属密钥}。命中即锁死该账号（不轮换）。
// normalize 生成反向索引 AccountKeyToUID（secret→UID）供鉴权反查。
	AccountKeys map[string]map[string]string `json:"account_keys"`
	// AccountKeyToUID 反向索引（secret → UID），由 normalize 从 AccountKeys 生成，不持久化。
	AccountKeyToUID map[string]string `json:"-"`
// ProviderKeys 供应商级密钥表：密钥 → 供应商名。命中即只走该供应商账号池
// （与区域叠加过滤）。与 APIKeys/AccountKeyToUID/api_key 互斥。
	ProviderKeys map[string]string `json:"provider_keys"`
	// ProviderKeyToProvider 反向索引（secret → provider），由 normalize 生成，不持久化。
	ProviderKeyToProvider map[string]string `json:"-"`
	AuthFile  string `json:"auth_file"`  // ./auths/auth.json（单文件模式，优先）
	AuthDir   string `json:"auth_dir"`   // ./auths（目录模式，回退）
	StateFile string `json:"state_file"` // ./data/state.json

// Models 各供应商「排除清单」：列出的模型 ID 不出现在 /v1/models。
// 主数据源是上游动态拉取，此段仅藏掉少数会报错/不想暴露的模型。
	Models map[string][]string `json:"models"`

	Server struct {
// MaxBodyMB 聊天请求体上限（MB，默认 8）。超限直接 413，不截断后喂上游——
// 截断的 JSON 会让上游 unmarshal 报 unexpected EOF，网关却罚号。
		MaxBodyMB int `json:"max_body_mb"`
	} `json:"server"`

	Cooldown struct {
// hard_credit / err_threshold / err_cooldown 已退役：硬冷却固定次日 04:00，
// 连续错误并入熔断器。旧 config 里的这些键因 JSON 未知字段自然忽略。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
// Disabled 关闭网关侧全部频率限制与风控（不冷却、不禁用、不罚号），仅用于压测。
		Disabled bool `json:"disabled"`
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]
		ActivityHours  []int `json:"activity_hours"`  // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled 显式禁用开关（缺省 true）。
// 用独立 bool 而非空数组/哨兵：后者已被"未配置→回落默认"占用，改判会静默翻转老配置。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
		// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
		ActivityReportCount int `json:"activity_report_count"`

// ── token 自动续期看护（internal/scheduler/token.go）────────────────
// 补 keepalive_hours 的缺陷：trae 的 JWT 只有 8h，每天刷一次等于整天死号。
// 按剩余寿命连续巡检，默认开启。TokenWatchInterval 默认 10m，低于 1m 回落 1m。
		TokenWatchInterval string `json:"token_watch_interval"`
		// RefreshWindow 通用提前续期窗口（默认 "30m"）：剩余有效期小于它即换新。
		RefreshWindow string `json:"refresh_window"`
		// RefreshWindowByProvider 按 provider 覆盖窗口（如 {"trae":"2h"}）。
		// 取值必须显著小于该家凭证寿命，否则每次巡检都「还没到期」而永远不续期。
		RefreshWindowByProvider map[string]string `json:"refresh_window_by_provider"`
		// TokenWatchEnabled 显式开关（缺省 true）。
		TokenWatchEnabled bool `json:"token_watch_enabled"`
		// 猫猫旅行已退役 travel_interval_minutes：旅行现为独立排程（travel_hours）。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
	} `json:"schedule"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
// UserAgent 出站 UA 覆盖（空 = 默认）；全部出站请求生效。
// 官网「使用端」列基于 UA 做归因（官方桌面 UA 为 WorkBuddy/<version>），默认不改。
		UserAgent string `json:"user_agent"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode custom（默认）= 网关用自有系统提示词替换客户端 system/developer；
		// passthrough = 透传客户端原始 system（降级重试仍会切到 Degraded）。
		Mode string `json:"mode"` // "custom" / "passthrough"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3；0 = 关闭熔断
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

// DeepSeek 会话状态配置。上下文靠上游会话树传递，网关必须持久化
// 「客户端会话键 → 上游会话坐标」，否则每次请求都新建会话、丢上下文。
	DeepSeek struct {
// SessionTTL 本地会话坐标有效期（默认 "24h"）。必须显著小于上游 ttl_seconds（实测 3 天），
// 否则拿过期 chat_session_id 反复撞 biz_code=1。
		SessionTTL string `json:"session_ttl"`
		// SessionFile 会话坐标持久化文件，默认 "./data/deepseek_sessions.json"。
		// 空字符串 = 纯内存（重启丢上下文，但会话会自动重建）。
		SessionFile string `json:"session_file"`
	} `json:"deepseek"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`

	// DeepSeek 会话配置（解析后）
	DeepSeekSessionTTL time.Duration `json:"-"`

	// token 自动续期看护（解析后）
	TokenWatchIntervalDur   time.Duration            `json:"-"`
	RefreshWindowDur        time.Duration            `json:"-"`
	RefreshWindowByProvider map[string]time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthFile:  "./auths/auth.json",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.MaxBodyMB = 8 // 请求体上限默认 8MB
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	// 开关「缺省 true」靠这几行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.ActivityReportCount = 5 // 领猫前置需 5 次对话，5 连发刷满 chat_5
	// token 自动续期看护：trae 的 JWT 寿命 8h，窗口取 2h（≈寿命 1/4）留足重试余量；
	// 其余家长寿命/自带 refreshToken，30m 窗口足够。
	c.Schedule.TokenWatchInterval = "10m"
	c.Schedule.RefreshWindow = "30m"
	c.Schedule.RefreshWindowByProvider = map[string]string{"trae": "2h"}
	c.Schedule.TokenWatchEnabled = true
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "custom" // 缺省 custom：网关自有提示词从源头消灭 system 指纹误报
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.DeepSeek.SessionTTL = "24h"
	c.DeepSeek.SessionFile = "./data/deepseek_sessions.json"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 REVERSE_PROXY_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// envStr / envInt / envBool 读环境变量覆盖配置，空值或非法值忽略。
func envStr(key string, dst *string) {
	if v := os.Getenv(key); v != "" { *dst = v }
}
func envInt(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil { *dst = n }
	}
}
func envBool(key string, dst *bool) {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil { *dst = b }
	}
}

func applyEnv(c *Config) {
	envStr("REVERSE_PROXY_LISTEN", &c.Listen)
	envStr("REVERSE_PROXY_API_KEY", &c.APIKey)
	if v := os.Getenv("REVERSE_PROXY_API_KEY_INTL"); v != "" {
		if c.APIKeys == nil { c.APIKeys = map[string]string{} }
		c.APIKeys[v] = "intl"
	}
	if v := os.Getenv("REVERSE_PROXY_AUTH_FILE"); v != "" {
		c.AuthFile = v
	} else if v := os.Getenv("REVERSE_PROXY_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	envStr("REVERSE_PROXY_STATE_FILE", &c.StateFile)
	envInt("REVERSE_PROXY_MAX_BODY_MB", &c.Server.MaxBodyMB)
	envStr("REVERSE_PROXY_SOFT_RATE", &c.Cooldown.SoftRate)
	envStr("REVERSE_PROXY_SOFT_RATE_MAX", &c.Cooldown.SoftRateMax)
	envBool("REVERSE_PROXY_COOLDOWN_DISABLED", &c.Cooldown.Disabled)
	envInt("REVERSE_PROXY_TIMEOUT_SECONDS", &c.Upstream.TimeoutSeconds)
	envInt("REVERSE_PROXY_HEADER_TIMEOUT_SECONDS", &c.Upstream.HeaderTimeoutSeconds)
	envInt("REVERSE_PROXY_IDLE_TIMEOUT_SECONDS", &c.Upstream.IdleTimeoutSeconds)
	envStr("REVERSE_PROXY_USER_AGENT", &c.Upstream.UserAgent)
	envBool("REVERSE_PROXY_SANITIZE_FINGERPRINTS", &c.Features.SanitizeBlacklistFingerprints)
	envStr("REVERSE_PROXY_PROMPT_MODE", &c.Prompt.Mode)
	envStr("REVERSE_PROXY_PROMPT_FILE", &c.Prompt.File)
}

// parseDur 解析时长配置，失败时带上字段名报错。
func parseDur(field, v string) (time.Duration, error) {
	if strings.TrimSpace(v) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return d, nil
}

func (c *Config) normalize() error {
	var err error
	// api_keys 区域值规范化：大小写宽容，非法区域值直接报错（fail fast，
	// 避免拼错 "inlt" 静默变成空区域=全账号可见）。
	for k, r := range c.APIKeys {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "cn":
			c.APIKeys[k] = "cn"
		case "intl":
			c.APIKeys[k] = "intl"
		default:
			return fmt.Errorf("api_keys[%q]: %q 不是合法区域（cn / intl）", maskKey(k), r)
		}
	}
// account_keys 校验：{名字: {uid: secret}}，secret 非空且全局唯一（fail fast）。
// 同时生成反向索引 AccountKeyToUID 供 withAuth 反查。
	if c.AccountKeyToUID == nil {
		c.AccountKeyToUID = make(map[string]string)
	}
	for name, m := range c.AccountKeys {
		for uid, secret := range m {
			if strings.TrimSpace(secret) == "" {
				return fmt.Errorf("account_keys[%q]: 密钥不能为空", maskKey(name))
			}
			if _, dup := c.AccountKeyToUID[secret]; dup {
				return fmt.Errorf("account_keys: 密钥 %s 在多处重复出现（账户 key 必须唯一）", maskKey(secret))
			}
			c.AccountKeyToUID[secret] = uid
		}
	}
	// provider_keys 校验：provider 值必须是指定集合之一；且同一把 key 不得同时是
	// 区域 key / 账号 key / 主密钥（语义冲突，fail fast）。
	if c.ProviderKeyToProvider == nil {
		c.ProviderKeyToProvider = make(map[string]string)
	}
	knownProviders := map[string]bool{"codebuddy": true, "trae": true, "qoder": true, "deepseek": true}
	for secret, provider := range c.ProviderKeys {
		if strings.TrimSpace(secret) == "" {
			return fmt.Errorf("provider_keys: 密钥不能为空")
		}
		pv := strings.ToLower(strings.TrimSpace(provider))
		if !knownProviders[pv] {
			return fmt.Errorf("provider_keys[%q]: %q 不是已知供应商（codebuddy / trae / qoder / deepseek）", maskKey(secret), provider)
		}
		if _, dup := c.APIKeys[secret]; dup {
			return fmt.Errorf("provider_keys[%q]: 其密钥同时出现在 api_keys 中（区域 key 与供应商 key 互斥）", maskKey(secret))
		}
		if _, dup := c.AccountKeyToUID[secret]; dup {
			return fmt.Errorf("provider_keys[%q]: 其密钥同时出现在 account_keys 中（账号 key 与供应商 key 互斥）", maskKey(secret))
		}
		if secret == c.APIKey {
			return fmt.Errorf("provider_keys[%q]: 其密钥与主密钥 api_key 冲突", maskKey(secret))
		}
		c.ProviderKeyToProvider[secret] = pv
	}
	// max_body_mb 非法（0/负数）直接报错：0 若被静默当成默认 8MB，用户以为"不限"，
	// 大请求又被静默 413——不如 fail fast 提示显式配大上限。
	if c.Server.MaxBodyMB <= 0 {
		return fmt.Errorf("server.max_body_mb: %d 非法（需为正整数，单位 MB）", c.Server.MaxBodyMB)
	}
	// 空值回落默认（Default() 已置值；此兜底覆盖显式 ""）。
	if c.Cooldown.SoftRateMax == "" { c.Cooldown.SoftRateMax = "2h" }
	if c.DeepSeek.SessionTTL == "" { c.DeepSeek.SessionTTL = "24h" }
	// 时长批量解析：字段名 → (配置值, 目标指针)。
	for _, d := range []struct{ field, val string; dst *time.Duration }{
		{"cooldown.soft_rate", c.Cooldown.SoftRate, &c.SoftRateDur},
		{"cooldown.soft_rate_max", c.Cooldown.SoftRateMax, &c.SoftRateMaxDur},
		{"pool.breaker_cooldown", c.Pool.BreakerCooldown, &c.BreakerCooldownDur},
		{"pool.breaker_cooldown_max", c.Pool.BreakerCooldownMax, &c.BreakerCooldownMaxD},
		{"deepseek.session_ttl", c.DeepSeek.SessionTTL, &c.DeepSeekSessionTTL},
		{"session_sticky.ttl", c.SessionSticky.TTL, &c.SessionTTL},
		{"session_sticky.gc_interval", c.SessionSticky.GCInterval, &c.SessionGCInterval},
	} {
		if *d.dst, err = parseDur(d.field, d.val); err != nil { return err }
	}
	if c.DeepSeekSessionTTL <= 0 {
		return fmt.Errorf("deepseek.session_ttl: %q 非法（需为正时长）", c.DeepSeek.SessionTTL)
	}
	// breaker_threshold：0 = 关闭熔断器（不再因连续失败熔断账号）；<0 视为非法回落默认 3。
	if c.Pool.BreakerThreshold < 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	// 0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if c.Schedule.ActivityReportCount <= 0 {
		c.Schedule.ActivityReportCount = 1
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	if err := c.normalizeTokenWatch(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizeTokenWatch 解析续期看护间隔。**不静默回落**非法值：配错（如写成 "10"）
// 会让看护退化成一分钟一次狂打上游或永不触发，都无明显报错，只能 fail fast 暴露。
func (c *Config) normalizeTokenWatch() error {
	if c.Schedule.TokenWatchInterval == "" {
		c.Schedule.TokenWatchInterval = "10m"
	}
	d, err := time.ParseDuration(c.Schedule.TokenWatchInterval)
	if err != nil {
		return fmt.Errorf("schedule.token_watch_interval: %w", err)
	}
	if d <= 0 {
		return fmt.Errorf("schedule.token_watch_interval: %q 非法（需为正时长，如 \"10m\"）", c.Schedule.TokenWatchInterval)
	}
	// 下限 1m：更密只会徒增上游压力，续期窗口本身已留足余量。
	if d < time.Minute {
		d = time.Minute
	}
	c.TokenWatchIntervalDur = d

	if c.Schedule.RefreshWindow == "" {
		c.Schedule.RefreshWindow = "30m"
	}
	if c.RefreshWindowDur, err = parseDur("schedule.refresh_window", c.Schedule.RefreshWindow); err != nil {
		return err
	}
	if c.RefreshWindowDur <= 0 {
		return fmt.Errorf("schedule.refresh_window: %q 非法（需为正时长，如 \"30m\"）", c.Schedule.RefreshWindow)
	}

	c.RefreshWindowByProvider = make(map[string]time.Duration, len(c.Schedule.RefreshWindowByProvider))
	for prov, raw := range c.Schedule.RefreshWindowByProvider {
		pd, perr := time.ParseDuration(raw)
		if perr != nil {
			return fmt.Errorf("schedule.refresh_window_by_provider[%s]: %w", prov, perr)
		}
		if pd <= 0 {
			return fmt.Errorf("schedule.refresh_window_by_provider[%s]: %q 非法（需为正时长）", prov, raw)
		}
		c.RefreshWindowByProvider[prov] = pd
	}
	return nil
}

// normalizePrompt 校验 prompt.mode 并按需加载提示词；非法 mode 启动报错（不静默回落）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "custom":
		c.Prompt.Mode = "custom"
	case "passthrough":
		c.Prompt.Mode = "passthrough"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// maskKey 密钥脱敏展示（错误信息里不落全文）。
func maskKey(k string) string {
	if len(k) <= 8 {
		return "***"
	}
	return k[:6] + "***" + k[len(k)-4:]
}

// validateScheduleHours 校验排程小时落在 0-23。非法值 fail fast 并指向正确开关
// （checkin_enabled / keepalive_enabled），否则用户以为关了实际照常执行。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
		return err
	}
	return checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
