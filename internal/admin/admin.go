// Package admin 是 Web 管理台：给 Docker 部署的网关提供可视化界面，
// 功能对齐 Android App（看账号、改密钥、手动续期、切配置）。
//
// 设计要点（详见 docs/admin-console.md）：
//   - 内嵌在网关同一进程、同一端口（/admin/*），不另起服务 —— 管理台要读写
//     config.json/auth.json/state.json，这些已被网关独占且有锁与原子写；
//     续期/清冷却/看池状态也活在进程内存里，只有同进程能直接调。
//   - 前端是内嵌的原生 HTML/JS（go:embed），不引入 Node 构建链。
//   - 默认关闭；开启后默认只允许内网 CIDR 访问，CIDR 外返回 404（不暴露存在）。
//   - Cookie(HttpOnly/SameSite) 鉴权，不复用 Bearer —— 浏览器里放 Bearer
//     会被 XSS 拿走，Cookie+HttpOnly 才是浏览器场景的标准做法。
package admin

import (
	_ "embed"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config 管理台运行参数，由 cmd/server 从 config.json 的 admin 段组装。
type Config struct {
	// Enabled 总开关。默认 false —— 管理台能改密钥、能读全部凭证，
	// 默认开等于给所有现有部署静默加一个高权限入口。
	Enabled bool
	// Password 登录密码。空 = 用网关主密钥 api_key 当密码。
	Password string
	// AllowCIDR 允许访问的来源网段；空 = 只允许回环。
	// 公网访问必须显式把网段加进来。
	AllowCIDR []string
	// LogRing 网关日志环形缓冲，供 /admin/api/logs 增量拉取。
	LogRing LogRingSource
	// Deps 各项能力的实现，由 cmd/server 注入。
	Deps Deps
}

// LogRingSource 是日志缓冲的最小接口（避免 admin 反向依赖 server 包的实现）。
type LogRingSource interface {
	Since(since int64) (lines []LogLineDTO, max int64)
	Tail(n int) []LogLineDTO
}

// LogLineDTO 一行日志。
type LogLineDTO struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

// Deps 管理台需要的网关能力。全部由 cmd/server 用闭包注入，
// admin 包不 import pool/scheduler/upstream，避免循环依赖。
type Deps struct {
	// 概览
	Uptime       func() time.Duration
	PoolCounts   func() (total, healthy, cooling, disabled, inFlightFull int)
	RedisMode    func() string
	StickyCount  func() int

	// 账号
	ListAccounts func() []AccountDTO
	AccountByUID func(uid string) (*AccountDTO, bool)
	ModelsOf     func(uid string) ([]string, error)
	// UsageOf 查单号剩余额度（当前仅 qoder 有上游接口，其余返回 Supported=false）。
	UsageOf func(uid string) (UsageDTO, error)
	RenewOne     func(uid string) (expiresAt int64, err error)
	RenewAll     func() error
	ReviveOne    func(uid string) error
	ResetKey     func(uid string) (string, error)
	DeleteAcct   func(uid string) error

	// 密钥 / 配置
	ReadKeys  func() (KeysDTO, error)
	WriteKeys func(KeysDTO) error
	ReadSettings  func() (map[string]any, error)
	WriteSettings func(map[string]any) error

	// 建号（账号页「+ 添加账号」）
	//
	// AddAccount 走粘贴型供应商（trae/qoder/deepseek）：归一凭证 → 写 auth.json
	// → 生成专属密钥 → 同步账号池。StartOAuth 走设备流（codebuddy），返回一个可轮询的
	// 句柄；admin 包不 import provision，具体实现由 cmd/server 注入。
	AddAccount func(AddAccountReq) (AddAccountResult, error)
	StartOAuth func(provider, nickname string) (OAuthFlow, error)

	// CurrentAPIKey 返回网关当前主密钥；admin.password 为空时用它做登录口令，
	// 同时充当 Cookie 签名材料。
	CurrentAPIKey func() string

	// Checkin 每日签到（分供应商语义，见 upstream.Checkin）。
	// CheckinOne 对单号签到；CheckinAll 对全池签到，返回每号结果供前端逐条展示。
	CheckinOne func(uid string) (CheckinDTO, error)
	CheckinAll func() []CheckinDTO

	// 身份自检（Qoder COSY）：qoder 的 uid 同时是 COSY 身份与 cosy-user 头，
	// 写错会全量 403 105 但界面看不出原因，所以需要一条只读诊断 + 一次性纠正入口。
	// IdentityOf 只比对不修改；AdoptIdentity 把上游确认的真实 uid 写进 extra（不动池主键）。
	IdentityOf    func(uid string) (IdentityDTO, error)
	AdoptIdentity func(uid, realUID string) (IdentityDTO, error)
}

// CheckinDTO 一次签到的前端视图。
//
// State 取值与 upstream.CheckinState 一一对应，但**不直接复用那个枚举**：
// admin 包不 import upstream（靠 cmd/server 注入闭包），这里用字符串中转。
type CheckinDTO struct {
	Label  string `json:"label,omitempty"`
	UID    string `json:"uid,omitempty"`
	State  string `json:"state"` // ok | already | unsupported | pending | failed
	Credit int64  `json:"credit,omitempty"`
	Streak int    `json:"streak_days,omitempty"`
	Msg    string `json:"msg,omitempty"`
}

// AccountDTO 账号对外视图。**绝不包含 accessToken/refreshToken/extra** ——
// 管理台一旦被 XSS 就是全号失窃，所以凭证字段根本不出这个结构体。
// IdentityDTO 一次身份自检的前端视图（当前仅 Qoder 适用）。
//
// AccountUID 是池主键、CosyUID 是 /algo 请求实际带的身份、RealUID 是上游确认的，
// 三者不一致（Match=false）就是「403 105 但凭证没过期」的根因。
type IdentityDTO struct {
	AccountUID string `json:"account_uid"`
	CosyUID    string `json:"cosy_uid"`
	RealUID    string `json:"real_uid,omitempty"`
	Match      bool   `json:"match"`
	Err        string `json:"err,omitempty"`
}

type AccountDTO struct {
	UID            string `json:"uid"`
	Label          string `json:"label"`          // auth.json 的键（codebuddy_1 等）
	Nickname       string `json:"nickname,omitempty"`
	Provider       string `json:"provider"`
	Region         string `json:"region,omitempty"`
	ExpiresAt      int64  `json:"expires_at"`
	RemainSeconds  int64  `json:"remain_sec"`
	Cooling        bool   `json:"cooling"`
	CoolKind       string `json:"cool_kind,omitempty"`
	CoolRemainSec  int64  `json:"cool_remain_sec,omitempty"`
	Disabled       bool   `json:"disabled"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	SuccessCount   int64  `json:"success_count"`
	ErrTotal       int64  `json:"err_total"`
	InFlight       int    `json:"in_flight"`
	BreakerFails   int    `json:"breaker_fails"`
	LastSuccess    int64  `json:"last_success,omitempty"`
	LastErr        int64  `json:"last_err,omitempty"`
	// AccountKey 该号的账号级密钥（明文）。调用方按需掩码。
	AccountKey string `json:"account_key,omitempty"`
	Renewable  bool   `json:"renewable"`
	// Credits 池内已知的剩余额度（0 = 没查过或该供应商不支持）。
	Credits int64 `json:"credits"`
	// CreditsUnit Credits 的单位。空 = 「积分」（历史默认）。
	// Trae 国际版按美元计费，Credits 存的是美分，必须标成 "USD"，
	// 否则列表把 99 美分当 99 积分显示，差 100 倍。
	CreditsUnit string `json:"credits_unit,omitempty"`
}

// UsageDTO 单账号额度查询结果。
//
// Supported=false 不是错误，而是能力缺失：目前 qoder / codebuddy / trae 有额度接口，
// DeepSeek 是网页会话接口（没有积分概念），Client.Usage 返回 (nil, nil)，
// 前端据 Hint 展示原因。
type UsageDTO struct {
	Supported bool    `json:"supported"`
	Provider  string  `json:"provider"`
	Remaining float64 `json:"remaining"`   // 套餐剩余 + 加油包剩余
	Total     float64 `json:"total"`       // 套餐总额 + 加油包总额
	Used      float64 `json:"used"`
	Unit      string  `json:"unit,omitempty"`
	Percent   float64 `json:"percent"` // 上游给的已用百分比
	PlanName  string  `json:"plan_name,omitempty"`
	Hint      string  `json:"hint,omitempty"` // 不支持时的说明文案
}

// KeysDTO 四类密钥。与 config.go 的四层鉴权一一对应。
type KeysDTO struct {
	APIKey       string              `json:"api_key"`
	APIKeys      map[string]string   `json:"api_keys"`       // 区域密钥：密钥 → cn|intl
	ProviderKeys map[string]string   `json:"provider_keys"`  // 供应商密钥：密钥 → provider
	AccountKeys  map[string]map[string]string `json:"account_keys"` // 账号密钥：标签 → {uid: 密钥}
}

// AddAccountReq 添加账号请求。
// provider 取 codebuddy / codebuddy_intl / trae / qoder / deepseek；
// 粘贴型供应商把原始凭证 JSON 放 input（sourceId / pat / sessionid / userToken 均可，
// 归一逻辑在 provision.FromPaste，与 CLI 完全一致）。
// codebuddy 不走本结构，改走设备流（/admin/api/accounts/oauth/start）。
type AddAccountReq struct {
	Provider string         `json:"provider"`
	Nickname string         `json:"nickname,omitempty"`
	Input    map[string]any `json:"input,omitempty"`
}

// AddAccountResult 建号结果。account_key 只在这里明文回传一次（前端提示用户复制）。
type AddAccountResult struct {
	Label      string `json:"label"`
	UID        string `json:"uid"`
	Provider   string `json:"provider"`
	AccountKey string `json:"account_key,omitempty"`
	InPool     bool   `json:"in_pool"`
}

// OAuthFlow 一次设备流授权（由 cmd/server 注入实现，admin 不依赖 provision）。
// Poll 每 3s 被调一次：pending=true 表示用户还没在浏览器里完成登录；
// 返回非 nil 结果表示已完成建号（含 auth.json 写入与专属密钥生成）。
type OAuthFlow interface {
	AuthURL() string
	Poll() (res *AddAccountResult, pending bool, err error)
}

// OAuthJob 前端轮询看到的授权任务状态。
type OAuthJob struct {
	Status     string `json:"status"` // pending | ok | error
	Label      string `json:"label,omitempty"`
	UID        string `json:"uid,omitempty"`
	AccountKey string `json:"account_key,omitempty"`
	Error      string `json:"error,omitempty"`
}

// oauthJob 内部任务状态；expire 到了就清掉（避免 ticket 表无限增长）。
type oauthJob struct {
	status string
	res    *AddAccountResult
	errMsg string
	expire time.Time
}

// Server 管理台 HTTP 处理器。
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	cidrs   []*net.IPNet
	authKey []byte // HMAC 签 Cookie 的密钥材料

	mu       sync.Mutex
	fails    int       // 连续登录失败次数
	lockTill time.Time // 爆破锁定截止

	// oauth 设备流任务表：ticket → 状态。前端拿 ticket 轮询 /oauth/status。
	oauthMu sync.Mutex
	oauth   map[string]*oauthJob
}

const (
	cookieName = "rp_admin"
	tokenTTL   = 12 * time.Hour
	maxFails   = 5
	lockPeriod = time.Minute

	// oauthPollInterval / oauthTimeout：网页设备流的轮询节奏与总时长。
	// 与 CLI 的 20×3s 对齐（90~120s 足够用户扫码/登录，超时让用户重新生成链接）。
	oauthPollInterval = 3 * time.Second
	oauthTimeout      = 120 * time.Second
	// oauthKeep 完成后 ticket 仍保留的时长：前端可能轮询两次才渲染完，
	// 立刻删会造成「第一次拿到 ok、刷新变 404」的误判。
	oauthKeep = 10 * time.Minute
)

// New 构造管理台。cfg.Enabled=false 时返回 nil（调用方不挂载路由）。
func New(cfg Config) (*Server, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), oauth: map[string]*oauthJob{}}

	// CIDR 解析：空 = 只允许回环
	if len(cfg.AllowCIDR) == 0 {
		cfg.AllowCIDR = []string{"127.0.0.1/32", "::1/128"}
	}
	for _, c := range cfg.AllowCIDR {
		_, n, err := net.ParseCIDR(strings.TrimSpace(c))
		if err != nil {
			return nil, fmt.Errorf("admin.allow_cidr: %q 不是合法网段: %w", c, err)
		}
		s.cidrs = append(s.cidrs, n)
	}
	// 再自动补一条「容器默认路由网关 /32」。
	//
	// 为什么必需：Docker 端口映射（-p 7863:7863）下，宿主机浏览器进来时容器看到的
	// 源地址是宿主机在 docker 网桥上的地址（= 容器默认网关，如 172.18.0.1），不是
	// 127.0.0.1。只配回环的 allow_cidr 会让本机访问也吃 404（实测），而把整个
	// Docker 网段写进配置又没必要——只放行网关这一个地址即可：同网桥的其他容器
	// 源地址是同段内的 .2/.3…，不因此获得访问权。配置里因此无需出现任何内网网段。
	for _, c := range localGatewayCIDRs() {
		if _, n, err := net.ParseCIDR(c); err == nil {
			s.cidrs = append(s.cidrs, n)
			log.Printf("[admin] 已自动放行容器默认网关 %s（Docker 端口映射下的宿主机源地址）", c)
		}
	}

	// Cookie 签名密钥：优先用显式 password，否则用主密钥。
	// 两者都空 = 网关闭鉴权，这种情况拒绝启用管理台（否则人人可进）。
	secret := cfg.Password
	if secret == "" {
		secret = cfg.Deps.CurrentAPIKey()
	}
	if secret == "" {
		return nil, fmt.Errorf("admin: 未设置 admin.password 且网关 api_key 为空，拒绝启用管理台（无凭据可校验）")
	}
	s.authKey = []byte("rp-admin|" + secret)

	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /admin/", s.page)
	s.mux.HandleFunc("POST /admin/api/login", s.login)
	s.mux.HandleFunc("POST /admin/api/logout", s.logout)

	s.mux.HandleFunc("GET /admin/api/overview", s.guard(s.overview))
	s.mux.HandleFunc("GET /admin/api/accounts", s.guard(s.accounts))
	// 建号：粘贴型直接入池，codebuddy 走下面两条设备流接口。
	s.mux.HandleFunc("POST /admin/api/accounts/add", s.guard(s.addAccount))
	s.mux.HandleFunc("POST /admin/api/accounts/oauth/start", s.guard(s.oauthStart))
	s.mux.HandleFunc("GET /admin/api/accounts/oauth/status", s.guard(s.oauthStatus))
	s.mux.HandleFunc("GET /admin/api/accounts/{uid}", s.guard(s.accountDetail))
	s.mux.HandleFunc("POST /admin/api/accounts/{uid}/renew", s.guard(s.renew))
	s.mux.HandleFunc("POST /admin/api/accounts/{uid}/revive", s.guard(s.revive))
	s.mux.HandleFunc("POST /admin/api/accounts/{uid}/key", s.guard(s.resetKey))
	s.mux.HandleFunc("DELETE /admin/api/accounts/{uid}", s.guard(s.deleteAccount))
	s.mux.HandleFunc("GET /admin/api/accounts/{uid}/models", s.guard(s.models))
	s.mux.HandleFunc("GET /admin/api/accounts/{uid}/usage", s.guard(s.accountUsage))
	s.mux.HandleFunc("POST /admin/api/renew-all", s.guard(s.renewAll))
	// 签到：单号（账号详情页按钮）+ 全池一键。
	s.mux.HandleFunc("POST /admin/api/accounts/{uid}/checkin", s.guard(s.checkinOne))
	s.mux.HandleFunc("POST /admin/api/checkin-all", s.guard(s.checkinAll))
	// 身份自检：GET 只读比对；POST 采纳上游真实 uid（写 extra.cosyUID，不动池主键）。
	s.mux.HandleFunc("GET /admin/api/accounts/{uid}/identity", s.guard(s.identity))
	s.mux.HandleFunc("POST /admin/api/accounts/{uid}/identity", s.guard(s.adoptIdentity))
	s.mux.HandleFunc("GET /admin/api/keys", s.guard(s.readKeys))
	s.mux.HandleFunc("POST /admin/api/keys", s.guard(s.writeKeys))
	s.mux.HandleFunc("GET /admin/api/settings", s.guard(s.readSettings))
	s.mux.HandleFunc("POST /admin/api/settings", s.guard(s.writeSettings))
	s.mux.HandleFunc("GET /admin/api/logs", s.guard(s.logs))
}

// ServeHTTP 先做 CIDR 门禁：不在白名单直接 404（不是 403 —— 不暴露管理台存在）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.allowedIP(r) {
		http.NotFound(w, r)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) allowedIP(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range s.cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ─── 鉴权 ──────────────────────────────────────────────────────────────

// token 生成签名串：<过期unix>.<hmac>。无状态，不存 session。
func (s *Server) token() string {
	exp := time.Now().Add(tokenTTL).Unix()
	payload := strconv.FormatInt(exp, 10)
	return payload + "." + s.sign(payload)
}

func (s *Server) sign(payload string) string {
	m := hmac.New(sha256.New, s.authKey)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) validToken(tok string) bool {
	i := strings.LastIndex(tok, ".")
	if i <= 0 {
		return false
	}
	payload, sig := tok[:i], tok[i+1:]
	// 定长比较，防时序侧信道
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign(payload))) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < exp
}

// guard 包一层：校验 Cookie + 拒绝非 JSON 写请求（CSRF 第一重）。
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			// 写操作要求自定义头：跨站表单带不上这个头（CSRF 第二重，配合 SameSite）
			if r.Header.Get("X-Requested-With") == "" {
				writeErr(w, http.StatusForbidden, "missing X-Requested-With")
				return
			}
		}
		c, err := r.Cookie(cookieName)
		if err != nil || !s.validToken(c.Value) {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		next(w, r)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	// 爆破保护
	s.mu.Lock()
	if time.Now().Before(s.lockTill) {
		wait := time.Until(s.lockTill).Seconds()
		s.mu.Unlock()
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("失败次数过多，请 %.0f 秒后再试", wait))
		return
	}
	s.mu.Unlock()

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}

	want := s.cfg.Password
	if want == "" {
		want = s.cfg.Deps.CurrentAPIKey()
	}
	// 定长比较
	if body.Password == "" || subtle.ConstantTimeCompare([]byte(body.Password), []byte(want)) != 1 {
		s.mu.Lock()
		s.fails++
		if s.fails >= maxFails {
			s.lockTill = time.Now().Add(lockPeriod)
			s.fails = 0
		}
		s.mu.Unlock()
		writeErr(w, http.StatusUnauthorized, "密码不对")
		return
	}

	s.mu.Lock()
	s.fails = 0
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.token(),
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil, // 走 HTTPS 反代时自动加 Secure
		MaxAge:   int(tokenTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ─── 小工具 ────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// randomKey 生成 32 字节随机 → 64 位小写 hex。
// 与 gen_account_keys.py 的 secrets.token_hex(32) 同口径，两边可互换。
func randomKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败不可恢复，返回空让调用方报错
		return ""
	}
	return hex.EncodeToString(b)
}

// localGatewayCIDRs 返回本机默认路由网关地址的 /32 CIDR 列表；读不到返回 nil。
//
// 用途见 New()：Docker 端口映射下，宿主机请求进入容器时的源地址就是容器默认
// 网关，所以白名单必须覆盖它，但只覆盖这一个地址即可。
// 只解析 /proc/net/route 的 IPv4 默认路由（Linux 容器）；非 Linux 或无该文件时
// 静默返回 nil（不影响纯回环部署）。表字段：Iface Destination Gateway Flags …，
// Destination=00000000 为默认路由；Gateway 按主机字节序的十六进制存放
// （x86/arm64 均为小端，故低字节在前）。
func localGatewayCIDRs() []string {
	raw, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue // 0.0.0.0 = 无网关（直连网段）
		}
		ip := net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		out = append(out, ip.String()+"/32")
	}
	return out
}

// ─── 管理台单页 ─────────────────────────────────────────────────────────────

//go:embed web/index.html
var indexHTML []byte

// page 返回管理台单页。所有 #/xxx 路由都是前端处理的，这里只吐同一个 HTML。
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 管理台页面不缓存，避免改完代码浏览器还跑旧版
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}
