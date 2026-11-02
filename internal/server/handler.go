// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 主密钥（空 = 不鉴权）；不限区域，可见/可用全部账号
	// APIKeys 分区域密钥表：密钥 → "cn"/"intl"。
	// 命中表的请求只能看到与使用对应区域的账号池；主密钥不受限。
	// 密钥互不相关：主密钥改动不影响表内密钥，反之亦然。
	APIKeys map[string]string
	// AccountKeyToUID 账号级密钥反向索引：密钥(secret) → 账号 UID。
	// 由配置层从 account_keys（名字 → {uid → 专属密钥}）在 normalize 阶段展平生成，仅供 withAuth 快速反查：
	// 命中该表的请求**只使用该账号**（不参与轮换、不会回退到池中其他号），
	// 用于「一个账号一把 key」的客户端配置（dsh 等）：key 与账号一一对应，
	// 换号行为由使用者显式决定，而非网关隐式调度。
	// 与 APIKeys 互斥：同一把 key 不得同时出现在两表（配置加载时校验）。
	AccountKeyToUID map[string]string
	// ProviderKeyToProvider 供应商级密钥反向索引：密钥(secret) → 供应商名（"trae"/"qoder" 等）。
	// 由配置层从 provider_keys（secret → provider）在加载阶段生成，仅供 withAuth 快速反查：
	// 命中该表的请求**只走该供应商的账号池**（与 region 叠加过滤），用于把不同供应商
	// 隔离成独立网关入口（如 trae 专用 key 只路由到 trae 账号）。
	// 与 APIKeys / AccountKeyToUID / 主密钥 api_key 互斥：同一把 key 不得出现于多表（配置加载时校验）。
	// 空值（无 provider_keys）时所有请求按既有 region/账号逻辑分流，向后兼容。
	ProviderKeyToProvider map[string]string
	MaxRotate   int // 单请求最多换号次数，默认 3
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	// Models 各供应商「排除清单」（可选）：列出的模型 ID 不会出现在 /v1/models。
	// key：codebuddy(CN WorkBuddy) / intl(国际版) / trae / qoder / doubao。
	// 主数据源是**上游动态拉取**（fetchDynamicModels，带 1h 缓存），上游新增模型会自动出现；
	// 此段仅用于藏掉少数会报错/不想暴露的模型（如付费旗舰、primary-model 别名）。
	// 段为空/缺省 = 不排除任何模型（全量动态）。绝不再整表写死。
	Models map[string][]string
	// CooldownDisabled 关闭限流冷却：true 时 429/404 不再自动冷却账号
	// （仅余额耗尽、session 死亡仍按原逻辑）。配合 pool.breaker_threshold=0 彻底关闭限流。
	CooldownDisabled bool
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
}

// ctxKey 密钥区域 context 键。
type ctxKey struct{}

// regionFromCtx 取请求绑定的密钥区域（"" = 主密钥/不限区域）。
func regionFromCtx(r *http.Request) string {
	if v, ok := r.Context().Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// ctxUIDKey 账号级密钥绑定的账号 UID context 键。
type ctxUIDKey struct{}

// uidFromCtx 取请求绑定的账号 UID（"" = 非账号级密钥，按区域/全池选号）。
func uidFromCtx(r *http.Request) string {
	if v, ok := r.Context().Value(ctxUIDKey{}).(string); ok {
		return v
	}
	return ""
}

// ctxProviderKey 供应商级密钥绑定的供应商名 context 键。
type ctxProviderKey struct{}

// providerFromCtx 取请求绑定的供应商（"" = 未绑定，回落 codebuddy）。
// 仅用于 pool 选号/列表/计数时的供应商过滤；选中账号后实际协议由账号自身 provider 决定。
func providerFromCtx(r *http.Request) string {
	if v, ok := r.Context().Value(ctxProviderKey{}).(string); ok {
		return v
	}
	return ""
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /usage", h.withAuth(h.usage))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" || len(h.cfg.APIKeys) > 0 || len(h.cfg.AccountKeyToUID) > 0 || len(h.cfg.ProviderKeyToProvider) > 0 {
			authz := r.Header.Get("Authorization")
			key := strings.TrimPrefix(authz, "Bearer ")
			uid := h.cfg.AccountKeyToUID[key]
			provider := h.cfg.ProviderKeyToProvider[key]
			// 四类密钥统一门禁：主密钥 / 区域密钥 / 账号级密钥 / 供应商级密钥。
			if !strings.HasPrefix(authz, "Bearer ") ||
				(key != h.cfg.APIKey && h.cfg.APIKeys[key] == "" && uid == "" && provider == "") {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			// 分区域密钥：把绑定区域随请求下传（选号/模型/状态全部按区域过滤）；
			// 主密钥区域为空 = 不限。供应商级密钥同样不限区域（只限 provider）。
			if key != h.cfg.APIKey {
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, h.cfg.APIKeys[key]))
			}
			// 账号级密钥：把绑定账号 UID 下传——该请求只走这一个号。
			// 区域仍取该号自身区域（账号级密钥不额外限制区域）：
			// intl 号必须走 intl 上游，否则首条 system 校验会失败。
			// provider 由该账号自身 provider 决定（PickByUIDIn 不走 provider 过滤）。
			if uid != "" {
				r = r.WithContext(context.WithValue(r.Context(), ctxUIDKey{}, uid))
				if acct := h.cfg.Pool.AccountByUID(uid); acct != nil {
					r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, acct.Region()))
				}
			} else if provider != "" {
				// 供应商级密钥：把绑定供应商下传——该请求只走该供应商的账号池
				// （与区域叠加：provider_keys 不含区域则 provider 限定 + 不限区域）。
				r = r.WithContext(context.WithValue(r.Context(), ctxProviderKey{}, provider))
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": ServiceName,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	region := regionFromCtx(r)
	uid := uidFromCtx(r)
	provider := providerFromCtx(r)
	var total, healthy, cooling, disabled, inFlightFull int
	var accounts []pool.Status
	if uid != "" {
		// 账号级密钥：只透出绑定的那一个号。
		total, healthy, cooling, disabled, inFlightFull = h.cfg.Pool.CountsDetailedForUID(uid)
		accounts = h.cfg.Pool.ListByUID(uid)
	} else {
		total, healthy, cooling, disabled, inFlightFull = h.cfg.Pool.CountsDetailedIn(region, provider)
		accounts = h.cfg.Pool.ListIn(region, provider)
	}
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"region":          regionDisplayName(region),
		"accounts":        accounts,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// usage 额度自动对账：逐账号拉取上游额度（当前 qoder 支持，只依赖 dt- 设备令牌，
// 不需要网页登录态），并把剩余额度写回 pool.Credits 供 /status 展示。
func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	region := regionFromCtx(r)
	uid := uidFromCtx(r)
	provider := providerFromCtx(r)
	var accounts []pool.Status
	if uid != "" {
		accounts = h.cfg.Pool.ListByUID(uid)
	} else {
		accounts = h.cfg.Pool.ListIn(region, provider)
	}
	items := make([]map[string]any, 0, len(accounts))
	for _, st := range accounts {
		item := map[string]any{
			"uid":      st.UID,
			"provider": st.Provider,
			"nickname": st.Nickname,
		}
		a := h.cfg.Pool.AccountByUID(st.UID)
		if a == nil {
			item["error"] = "account not found"
			items = append(items, item)
			continue
		}
		u, err := h.cfg.Upstream.Usage(a)
		if err != nil {
			item["error"] = err.Error()
		} else if u != nil {
			item["quota"] = u
			if q, ok := u.(*upstream.QoderUsage); ok {
				remain := q.UserQuota.Remaining + q.AddOnQuota.Remaining
				h.cfg.Pool.SetCredits(st.UID, int64(remain))
				item["remaining_credits"] = remain
			}
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": items})
}

// regionDisplayName 区域显示名（"" = 主密钥不限区域 → "all"）。
func regionDisplayName(region string) string {
	if region == "" {
		return "all"
	}
	return region
}

// 静态 CN 模型表（api-reference §5，动态接口失败时的回退）。
var staticModels = []map[string]any{
		{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	}

// 静态 Intl 模型表（2026-10-25 GET /v3/config agents[cli].models 快照，动态失败时回退）。
var staticModelsIntl = []map[string]any{
		{"id": "default-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 176000},
		{"id": "fast-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "balanced-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "primary-model", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "hy4-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "step-5-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "deepseek-v4.1-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "deepseek-v4.1-flash-sg", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-6-astra", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-6.1-sol", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-6-sol", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-6-luna", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-5.6-sol", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-5.6-terra", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-5.6-luna", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-5.5", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gpt-5.4", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "grok-4.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "gemini-3.8-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "glm-5.3-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "glm-5.3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
		{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 1000000},
		{"id": "minimax-m3.1-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "kimi-k3.1-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "kimi-k3.1-pro-highspeed", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
		{"id": "kimi-k3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	}

// dynamicModelsCache 动态模型缓存（按区域分桶：cn/intl 模型表互不污染）。
var dynamicModelsCache struct {
	sync.RWMutex
	buckets  map[string]*modelsBucket
}

type modelsBucket struct {
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
// 分区域密钥：模型表取自对应区域的账号（模型能力跨区域可能不同）。
// 账号级密钥：模型表取自绑定的那一个号（该号可能有独立模型权限）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	region := regionFromCtx(r)
	uid := uidFromCtx(r)
	provider := providerFromCtx(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(region, provider, uid),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
// uid 非空 = 账号级密钥：只从该账号拉模型；否则按 region+provider 从池中任一健康账号拉。
func (h *Handler) modelList(region, provider, uid string) []map[string]any {
	// 主数据源 = 上游动态拉取（权威、实时，带 1h 缓存）。上游新增模型会自动出现，
	// 不再依赖写死的清单。config 的 models.<key> 仅作为「排除清单」藏掉少数报错模型。
	if infos := h.fetchDynamicModels(region, provider, uid); len(infos) > 0 {
		deny := h.modelDenylist(region, provider, uid)
		mk := func(id string, mi upstream.ModelInfo) map[string]any {
			entry := map[string]any{
				"id":                id,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    mi.ContextWindow,
				"max_output_tokens": mi.MaxTokens,
			}
			if mi.ContextWindow == 0 {
				entry["context_length"] = 131072 // 兜底
			}
			// 视觉能力标注：让客户端在选到视觉模型时允许附图、不再误报
			// “当前模型不支持图片”。仅作启发式标注，上游是否真能读图由上游决定。
			// 同时写两个字段以兼容不同客户端：
			//   - OpenAI 官方标准用 `modalities: ["text","image"]`（WorkBuddy/CodeBuddy 等认这个）；
			//   - 部分客户端认 `capabilities: ["vision"]`，一并保留。
			if isVisionModel(id) {
				entry["capabilities"] = []string{"vision"}
				entry["modalities"] = []string{"text", "image"}
			} else {
				entry["modalities"] = []string{"text"}
			}
			return entry
		}
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			if deny[mi.ID] {
				continue
			}
			// 思考强度遍历：按每个模型自己声明的 supportedEfforts 逐档展开成「模型:档位」。
			// 上游目录里各模型的档位不同（2 档 / 3 档 / 不支持思考），故完全按数据来；
			// 有档位的模型只列各档（不再额外列裸名，裸名仍可用 = 走上游默认档）；
			// 不支持思考的模型没有档位，保留裸名。
			if len(mi.Efforts) == 0 {
				out = append(out, mk(mi.ID, mi))
				continue
			}
			for _, e := range mi.Efforts {
				out = append(out, mk(mi.ID+":"+e, mi))
			}
		}
		if len(out) > 0 {
			return out
		}
		// 排除清单把全部动态模型都过滤掉了 → 回落静态表（避免返回空列表）。
	}
	fallback := staticModels
	if region == "intl" {
		fallback = staticModelsIntl
	}
	// 动态拉取失败时，回落到「该供应商自己的静态表」（trae/qoder/doubao 各自模型名），
	// 而不是无脑回落到 codebuddy 表。供应商来源二选一：
	//   - 供应商级密钥：provider 直接给定；
	//   - 账号级密钥（现在的主力形态，provider 为空）：取绑定账号自身的 provider。
	// 否则一个 trae/qoder 账号一旦拉取失败，就会错误地显示 codebuddy 的模型清单。
	fbProvider := provider
	if fbProvider == "" && uid != "" {
		if acct := h.cfg.Pool.AccountByUID(uid); acct != nil {
			fbProvider = acct.ProviderOrDefault()
		}
	}
	if fbProvider != "" && fbProvider != "codebuddy" {
		if pm := h.cfg.Upstream.ProviderStaticModels(fbProvider); len(pm) > 0 {
			return pm
		}
		// 该供应商从没成功拉过目录（如冷启动就遇到上游故障）：宁可返回空表，
		// 也不把 codebuddy 的模型清单冒充成 trae/qoder 的。
		return nil
	}
	return fallback
}

// isVisionModel 根据模型 ID 启发式判断是否具备视觉（多模态读图）能力。
// 用于 /v1/models 标注 capabilities，让客户端在选到视觉模型时允许附图、不再误报
// “当前模型不支持图片”。仅启发式标注：上游是否真能读图仍由上游决定。
func isVisionModel(id string) bool {
	lower := strings.ToLower(id)
	// 注意：不能用笼统的 "deepseek"，因为 DeepSeek-V4-Pro 系列实测不支持看图；
	// 仅 V4.1 系（DeepSeek-V4.1-Flash 等）带视觉，故精确匹配子串 "deepseek-v4.1"。
	for _, kw := range []string{"seed-2.1", "vision", "glm-5v", "gemini", "gpt-4o", "claude", "hunyuan-vision", "deepseek-v4.1"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// modelDenylist 返回当前供应商/区域要隐藏的模型 ID 集合（config models.<key> 作为排除清单）。
// 命中即隐藏：这些模型不出现在 /v1/models，但客户端若显式请求仍会透传上游（由上游决定是否可用）。
// key 优先级：供应商级密钥(provider) > 账号级密钥(uid 所属供应商/区域) > 区域(intl→intl，其余→codebuddy)。
// 段为空/缺省 = 不排除任何模型（全量动态）。
func (h *Handler) modelDenylist(region, provider, uid string) map[string]bool {
	key := provider
	if key == "" && uid != "" {
		if acct := h.cfg.Pool.AccountByUID(uid); acct != nil {
			if acct.Region() == "intl" {
				key = "intl"
			} else {
				key = acct.ProviderOrDefault()
			}
		}
	}
	if key == "" {
		if region == "intl" {
			key = "intl"
		} else {
			key = "codebuddy"
		}
	}
	ids, ok := h.cfg.Models[key]
	if !ok || len(ids) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			set[id] = true
		}
	}
	return set
}

// fetchDynamicModels 从对应区域+供应商池中任一健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
// uid 非空 = 账号级密钥：只从该绑定账号拉（缓存桶按 uid 隔离，避免多账号互相覆盖）。
// provider 非空 = 供应商级密钥：只从该供应商账号拉（与 region 叠加）。
func (h *Handler) fetchDynamicModels(region, provider, uid string) []upstream.ModelInfo {
	bucketKey := region + "|" + provider
	if uid != "" {
		bucketKey = "uid:" + uid
	}
	dynamicModelsCache.Lock()
	if dynamicModelsCache.buckets == nil {
		dynamicModelsCache.buckets = make(map[string]*modelsBucket)
	}
	b, ok := dynamicModelsCache.buckets[bucketKey]
	if !ok {
		b = &modelsBucket{}
		dynamicModelsCache.buckets[bucketKey] = b
	}
	dynamicModelsCache.Unlock()

	bIds := func() []upstream.ModelInfo {
		dynamicModelsCache.RLock()
		defer dynamicModelsCache.RUnlock()
		if len(b.ids) > 0 && time.Since(b.fetched) < dynamicModelsTTL {
			return b.ids
		}
		// 失败负缓存：冷却期内不再请求上游。
		if !b.lastFail.IsZero() && time.Since(b.lastFail) < modelsFetchFailCooldown {
			return nil
		}
		return nil
	}
	if ids := bIds(); ids != nil {
		return ids
	}

	var acct *auth.Auth
	if uid != "" {
		// 账号级：锁定该账号（即使它当前不健康也尝试拉一次，失败则回退静态表）。
		acct = h.cfg.Pool.AccountByUID(uid)
	} else {
		acct = h.cfg.Pool.PickIn(region, provider)
	}
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 失败必须留日志：之前这里是静默的，只能靠 /v1/models 返回静态表来猜，
		// 无法区分「上游改版 / 凭证失效 / 网络不通」。
		if err != nil {
			log.Printf("WARN: [models] 动态拉取失败 provider=%s uid=%s: %v（%s 内改用静态回退表）",
				acct.ProviderOrDefault(), logfmt.UID8(acct.UID), err, modelsFetchFailCooldown)
		} else {
			log.Printf("WARN: [models] 动态拉取返回空 provider=%s uid=%s（%s 内改用静态回退表）",
				acct.ProviderOrDefault(), logfmt.UID8(acct.UID), modelsFetchFailCooldown)
		}
		// 拉取失败惩罚该账号，避免下次 Pick 又选中同一个反复失败；lastFail 保持负缓存。
		h.cfg.Pool.NoteError(acct.UID)
		dynamicModelsCache.Lock()
		b.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	b.ids = infos
	b.fetched = time.Now()
	b.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 思考强度变体：「模型:档位」→ 模型名去掉档位后缀 + 注入 reasoning_effort。
	// 档位来自 /v1/models 里该模型自己的 supportedEfforts（见 modelList 的逐档展开）。
	if base, eff := upstream.SplitModelEffort(peek.Model); eff != "" {
		body = upstream.ApplyModelEffort(body, base, eff)
		peek.Model = base
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	defer st.done()

	tried := map[string]bool{}
	// emptyRetries 非流式空响应重试计数（仅 qoder，见 Aggregate 后的重试分支）。
	emptyRetries := 0
	var lastErr error
	// 分区域密钥：本请求只允许使用对应区域的账号（主密钥 region="" 不限）。
	region := regionFromCtx(r)
	// 供应商级密钥：本请求只走绑定供应商的账号池（provider="" 回落 codebuddy）。
	provider := providerFromCtx(r)
	// 账号级密钥：本请求只允许使用绑定的那一个号（pinnedUID 非空）。
	// 语义：锁号——失败就报错，绝不轮换到别的号（"一把 key 一个账号"的承诺）。
	pinnedUID := uidFromCtx(r)

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil && pinnedUID == "" {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough 非降级期：透传客户端原始 system（不改写）。
	// 国际版追加：上游强校验首条消息必须是 system（11128 "first message is not
	// system prompt"），passthrough 且客户端未带 system 时用 Degraded 兜底前插
	// （custom 模式 Rewrite 已保证，EnsureSystem 幂等无操作）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "passthrough" && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}
	if region == "intl" {
		body = prompt.EnsureSystem(body, prompt.Degraded)
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：账号级密钥 > 粘性号 > 普通轮换。
		var acct *auth.Auth
		if pinnedUID != "" {
			// 账号级密钥（锁号）：只允许用绑定的那一个号。
			// 该号不可用（冷却/占满/未加载）时直接失败，绝不轮换到别的号——
			// 这正是"一把 key 一个账号"的承诺所在。
			acct = h.cfg.Pool.PickByUIDIn(pinnedUID, region)
			if acct == nil {
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("pinned account %s unavailable (cooling/disabled/not loaded)", logfmt.UID8(pinnedUID))
				break
			}
		} else if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDIn(stickyUID, region)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满/非本区域）→ 解绑，本次回落普通轮换。
				unbindSticky()
			}
		}
		if acct == nil && pinnedUID == "" {
			// 模型感知选号：请求携带 model 时启用 6004 模型级冷却豁免
			// （PickExcludingForModelIn 内部当 model 为空时即退化为普通轮换）。
			acct = h.cfg.Pool.PickExcludingForModelIn(tried, peek.Model, region, provider)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		// 选定账号对应的供应商实现：stream/aggregate/classify 全部走该供应商协议
		// （codebuddy 的 Provider 透传 OpenAI 兼容 SSE；trae/qoder 做协议翻译）。
		prov := h.cfg.Upstream.ProviderFor(acct)

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh uid=%s: save auth failed: %v", logfmt.UID8(acct.UID), err)
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := prov.Classify(status, string(respBody))
			// 内容拦截误报（passthrough 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试。
			// 第二次仍被拦（用户内容本身触发审核）→ 走既有错误路径返回客户端。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), peek.Model)
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = prov.StreamTo(w, stats, peek.Model)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			rc.Close()
			return
		}
		resp, err := prov.Aggregate(rc, peek.Model)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		// 空响应重试：qoder 上游偶发「HTTP 200 但流里一个正文都没有」（实测
		// deepseek-flash:medium），客户端会把空 content 当失败并带着退避反复重试。
		// 非流式尚未回包，可同账号原样重发；流式已经开始输出，无法回退，故不在此处理。
		if prov.Name() == "qoder" && isEmptyCompletion(resp) && emptyRetries < 2 {
			emptyRetries++
			log.Printf("WARN: [server] empty completion uid=%s model=%q -> retry %d", logfmt.UID8(acct.UID), peek.Model, emptyRetries)
			delete(tried, acct.UID)
			releaseHeld()
			time.Sleep(time.Second)
			continue
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if pinnedUID != "" {
		msg = "pinned account " + pinnedUID + " unavailable (cooling/disabled/not loaded)"
	} else if region != "" {
		msg = "no available account in region " + region + " (cooling/disabled/not logged in)"
	}
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	st.status = http.StatusServiceUnavailable
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 七条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 默认 Cooldown(CoolSoft, soft_rate) 连续触发指数退避（封顶 soft_rate_max）；
//     若上游 body 为模型级 6004 且带重置时间 → CooldownSoftForModel（until=重置墙钟，
//     封顶 soft_rate_max，记录触发模型供切模型豁免）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError），passthrough 模式走降级重试。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 关闭限流冷却：不再冷却账号，仅由调用方继续轮转（lastErr 已记录）。
		if h.cfg.CooldownDisabled {
			return
		}
		// 模型级 6004 且带「将在 … 重置」时间（issue #31）：冷却到上游明说的重置墙钟
		// （封顶 soft_rate_max），记录触发模型 → 该账号对**其他模型**请求可豁免冷却。
		// 解析失败（无时间文案 / 非 6004）→ 退回既有 600s 基数 + 指数退避现况。
		if upstream.IsModelRateLimit(body) {
			if resetAt, ok := upstream.ParseSoftRateReset(body); ok {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "6004 model rate limit")
				return
			}
		}
		// 其余 soft_rate：软冷却基数来自 soft_rate（默认 600s）；同一账号连续触发时
		// pool 内部按 softStreak 指数退避并封顶 soft_rate_max。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		// 关闭限流冷却时跳过（不罚账号）。
		if !h.cfg.CooldownDisabled {
			h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
		}
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		// 关闭限流冷却时跳过（不罚账号、不喂熔断）。
		if !h.cfg.CooldownDisabled {
			h.cfg.Pool.NoteError(uid)
		}
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// isEmptyCompletion 判断非流式聚合结果是否没有任何可展示内容：
// content 为空（或缺失）且没有 tool_calls。仅用于 qoder 空流重试判定。
func isEmptyCompletion(resp map[string]any) bool {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return true
	}
	c, _ := choices[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		return true
	}
	if s, _ := msg["content"].(string); strings.TrimSpace(s) != "" {
		return false
	}
	if tc, _ := msg["tool_calls"].([]any); len(tc) > 0 {
		return false
	}
	return true
}
