// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权；主密钥，不限区域
	// APIKeys 分区域密钥表：密钥 → 区域（"cn"/"intl"）。
	// 表内密钥只能访问对应区域的账号池；与主密钥 api_key 互不影响。
	// 例：{"api_keys": {"<key1>": "cn", "<key2>": "intl"}}。
	APIKeys map[string]string `json:"api_keys"`
	// AccountKeys 账号级密钥表：名字 → {uid: 专属网关密钥(secret)}。
	// 外层键是展示用「名字」（与 auth.json 的账号标签对应，如 qoder_1 / codebuddy_2，
	// 也可自定义成任意好记的名字），内层是单个 {uid: secret} 映射（一个名字锁一个账号）。
	// 命中该表的请求只使用该账号（锁号，不轮换到池中其他号），
	// 用于 dsh 等客户端配置：每个号配一个提供方条目，请求只走该号。
	// normalize 会生成反向索引 AccountKeyToUID（secret → UID）供鉴权快速反查。
	// 例：{"account_keys": {"qoder_1": {"<qoder-uid>-…": "<key1>"}, "codebuddy_2": {"<codebuddy-uid>-…": "<key2>"}}}。
	AccountKeys map[string]map[string]string `json:"account_keys"`
	// AccountKeyToUID 反向索引（secret → UID），由 normalize 从 AccountKeys 生成，不持久化。
	AccountKeyToUID map[string]string `json:"-"`
	// ProviderKeys 供应商级密钥表：密钥(secret) → 供应商名（"trae"/"qoder" 等）。
	// 命中该表的请求只走对应供应商的账号池（与区域叠加过滤），用于把不同供应商
	// 隔离成独立网关入口（如 trae 专用 key 只路由到 trae 账号）。
	// 例：{"provider_keys": {"<key>": "trae"}}。空 = 不启用（向后兼容）。
	// 与 APIKeys / AccountKeyToUID / 主密钥 api_key 互斥：同一把 key 不得出现于多表。
	ProviderKeys map[string]string `json:"provider_keys"`
	// ProviderKeyToProvider 反向索引（secret → provider），由 normalize 生成，不持久化。
	ProviderKeyToProvider map[string]string `json:"-"`
	AuthFile  string `json:"auth_file"`  // ./auths/auth.json（单文件模式，优先）
	AuthDir   string `json:"auth_dir"`   // ./auths（目录模式，回退）
	StateFile string `json:"state_file"` // ./data/state.json

	// Models 各供应商「排除清单」（可选）：列出的模型 ID 不会出现在 /v1/models。
	// key：codebuddy(CN WorkBuddy) / intl(国际版) / trae / qoder / doubao。
	// 主数据源是上游动态拉取（/v1/models 实时返回上游可用模型，新增模型自动出现）；
	// 此段仅藏掉少数会报错/不想暴露的模型。段为空/缺省 = 不排除任何模型。
	Models map[string][]string `json:"models"`

	Server struct {
		// MaxBodyMB 聊天请求体大小上限（单位 MB，默认 8）。
		// 请求体超过该值直接返回 413 request_body_too_large，不再静默截断后喂给上游
		// （issue #41：截断的 JSON 让上游 unmarshal 报 unexpected EOF，网关却罚号）。
		// 0/负数视为非法 → normalize 回落默认并记录。
		MaxBodyMB int `json:"max_body_mb"`
	} `json:"server"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
		// Disabled 关闭限流冷却：true 时 429/404 不再自动冷却账号
		// （仅余额耗尽、session 死亡仍按原逻辑）。与 pool.breaker_threshold=0 配合可彻底关闭限流。
		Disabled bool `json:"disabled"`
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		TravelHours    []int `json:"travel_hours"`    // [9,21]
		ActivityHours  []int `json:"activity_hours"`  // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
		TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
		ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
		// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
		// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
		ActivityReportCount int `json:"activity_report_count"`

		// ── token 自动续期看护（internal/scheduler/token.go）────────────────
		//
		// 为什么直接写字面量而不放进上面那堆 *_enabled 里：这是对 keepalive_hours
		// 「按钟点巡检」缺陷的修补——trae 的 JWT 只有 8h，每天只刷一次等于整天死号，
		// 且续期接口只认未过期的 token（过期即无法自救）。看护按「剩余寿命」连续巡检，
		// 是让短寿命凭证能长期存活的前提，故默认开启。
		//
		// TokenWatchInterval 巡检间隔（默认 "10m"）；低于 "1m" 会回落 1m。
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
		// UserAgent 出站 User-Agent 覆盖（空 = 现状 `CLI/2.63.2 CodeBuddy/2.63.2`）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// issue #42 深挖：官网「使用端」列基于出站请求 UA 的服务端归因，官方 WorkBuddy
		// 桌面 UA 为 `WorkBuddy/<version>`。指纹净化考虑：默认值保持现状（可配而非改死），
		// 仅当用户显式配置才改写。
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
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
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

	// DeepSeek DeepSeek 网页版供应商的会话状态配置。
	// 上下文靠上游会话树（chat_session_id + parent_message_id）传递，网关必须把
	// 「客户端会话键 → 上游会话坐标」持久化，否则每次请求都会新建会话、丢上下文。
	DeepSeek struct {
		// SessionTTL 本地会话坐标的有效期，默认 "24h"。
		// 必须显著小于上游的 ttl_seconds（实测 259200 = 3 天），否则会拿着过期
		// chat_session_id 反复撞 biz_code=1（每次都要多花一次 create + 一次重试）。
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

// Load 从文件读，再用 WB2A_* env 覆盖。
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

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_API_KEY_INTL"); v != "" {
		if c.APIKeys == nil {
			c.APIKeys = make(map[string]string)
		}
		c.APIKeys[v] = "intl"
	}
	if v := os.Getenv("WB2A_AUTH_FILE"); v != "" {
		c.AuthFile = v
	} else if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_MAX_BODY_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.MaxBodyMB = n
		}
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_COOLDOWN_DISABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Cooldown.Disabled = b
		}
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
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
	// account_keys 校验：结构为 {名字: {uid: secret}}；secret 不能为空、且必须全局唯一
	// （同一把密钥不得出现在两个账号下，fail fast）。同时生成反向索引
	// AccountKeyToUID（secret → UID），供 withAuth 快速反查。
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
	knownProviders := map[string]bool{"codebuddy": true, "trae": true, "qoder": true, "doubao": true, "deepseek": true}
	for secret, provider := range c.ProviderKeys {
		if strings.TrimSpace(secret) == "" {
			return fmt.Errorf("provider_keys: 密钥不能为空")
		}
		pv := strings.ToLower(strings.TrimSpace(provider))
		if !knownProviders[pv] {
			return fmt.Errorf("provider_keys[%q]: %q 不是已知供应商（codebuddy / trae / qoder / doubao / deepseek）", maskKey(secret), provider)
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
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	// deepseek 会话坐标 TTL：空值回落 24h；非法值 fail fast（配错会导致每轮都重建会话）。
	if c.DeepSeek.SessionTTL == "" {
		c.DeepSeek.SessionTTL = "24h"
	}
	if c.DeepSeekSessionTTL, err = time.ParseDuration(c.DeepSeek.SessionTTL); err != nil {
		return fmt.Errorf("deepseek.session_ttl: %w", err)
	}
	if c.DeepSeekSessionTTL <= 0 {
		return fmt.Errorf("deepseek.session_ttl: %q 非法（需为正时长，如 \"24h\"）", c.DeepSeek.SessionTTL)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
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

// normalizeTokenWatch 解析 token 自动续期看护的时长配置。
//
// 与前排 *_hours 不同，这里**不静默回落**非法值：续期间隔配错（如写成 "10"）会让看护
// 要么退化成一分钟一次狂打上游、要么永远不触发，两种后果都不会有明显报错，只能靠
// 启动时 fail fast 暴露。
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
	if c.RefreshWindowDur, err = time.ParseDuration(c.Schedule.RefreshWindow); err != nil {
		return fmt.Errorf("schedule.refresh_window: %w", err)
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

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom 模式）。
//
// mode 非法（非 custom/passthrough）启动报错，避免静默回落到某一分支；
// custom 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
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

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
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
