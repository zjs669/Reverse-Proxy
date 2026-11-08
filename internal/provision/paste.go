// paste.go 粘贴型凭证（Trae sourceId / Qoder PAT / DeepSeek userToken）
// 的归一化：把用户从 F12 里翻出来的零散字段补成完整可入池的 Credential。
//
// 这里每一条「自动补全」都对应过一次真实踩坑，注释随代码保留，不要按直觉简化：
//   - trae 只认 sourceId（X-Cloudide-Session）铸新 JWT；过期的 JWT 永远换不回来。
//   - qoder 的 dt- 是不透明串，必须写一个**有限**的估算寿命，否则 token 看护永不续期。
//   - DeepSeek 的凭证不携带寿命，统一按 30 天估算，便于到期提醒。
package provision

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/upstream"
)

// FromPaste 把原始凭证归一成 Credential。
// provider 只接受 trae / qoder / deepseek（codebuddy 走设备流）。
func FromPaste(provider string, input map[string]any, nickname string) (*Credential, error) {
	return FromPasteRegion(provider, "", input, nickname)
}

// FromPasteRegion 带区域的版本。
//
// region 目前只有 trae 用得上（国内 cn / 国际 intl 是两套独立体系）。
// 它会被写进 extra.region，后续 upstream 按它选端点：
//
//	换 token  api.trae.cn  / api.trae.ai
//	对话/模型 trae-api-cn.mchost.guru / coresg-normal.trae.ai
//	积分     api.trae.cn/trae/api/v2/... / ug-normal.trae.ai/trae/api/v1/...
//
// 不传 region（空串）保持旧行为：按国内版处理。
func FromPasteRegion(provider, region string, input map[string]any, nickname string) (*Credential, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	region = strings.ToLower(strings.TrimSpace(region))
	switch provider {
	case "trae", "qoder", "deepseek":
	case "":
		return nil, fmt.Errorf("缺少供应商")
	default:
		return nil, fmt.Errorf("不支持粘贴建号：%s（CodeBuddy 请用授权链接）", provider)
	}
	if input == nil {
		return nil, fmt.Errorf("缺少凭证内容")
	}
	switch provider {
	case "deepseek":
		input = adaptDeepseekCredential(input)
	}

	accessToken := strVal(input["accessToken"])
	uid := strVal(input["uid"])
	refreshToken := strVal(input["refreshToken"])
	extra := normalizeExtra(input["extra"])

	// trae / qoder 的区域必须记进 extra.region：后续所有上游调用（换 token、对话、
	// 积分、签到）都靠它选端点（upstream.qoderRegion / traeProvider.isIntl）。
	// 不写这一条，国际版账号会被当成国内版打到国内基址，全部 404/401/403。
	if (provider == "trae" || provider == "qoder") && region != "" {
		if extra == nil {
			extra = map[string]string{}
		}
		extra["region"] = region
	}

	switch provider {
	case "trae":
		// sourceId 是长期登录会话：拿它向 GetUserToken 铸一枚新 8h JWT。
		// 只粘 JWT 也能用（从载荷解 exp / uid / source_id），但 JWT 过期后无法自救。
		if sid := strVal(input["sourceId"]); sid != "" {
			if extra == nil {
				extra = map[string]string{}
			}
			if extra["sourceId"] == "" {
				extra["sourceId"] = sid
			}
			if accessToken == "" {
				jwt, err := MintTraeJWTRegion(sid, region)
				if err != nil {
					return nil, err
				}
				accessToken = jwt
			}
		}
		// X-Cloudide-Session（data.source_id）与 X-Uid（data.id）都在 JWT 载荷里，
		// 用户很难自己从 F12 翻出来，这里统一自动解析补齐，最少只需粘一个字段。
		if accessToken != "" {
			if uid == "" {
				uid = auth.JWTDataString(accessToken, "id")
			}
			if extra == nil {
				extra = map[string]string{}
			}
			if extra["sourceId"] == "" {
				extra["sourceId"] = auth.JWTSourceID(accessToken)
			}
		}

	case "qoder":
		pat := strVal(input["pat"])
		if pat != "" {
			jt, rt, _, err := qoderExchangePAT(pat, region)
			if err != nil {
				return nil, fmt.Errorf("PAT 兑换失败: %w", err)
			}
			accessToken = jt
			if rt != "" {
				refreshToken = rt
			}
			if uid == "" {
				if u, e := qoderUserinfo(jt, region); e == nil && u != "" {
					uid = u
				}
			}
			// PAT 收进 extra：网关临期用它自动换新 jt-（对齐 qoder.go RefreshToken 路线 1）
			if extra == nil {
				extra = map[string]string{}
			}
			extra["pat"] = pat
		}
		if accessToken == "" {
			return nil, fmt.Errorf("凭证缺少 accessToken（或 pat 兑换失败）")
		}
		if uid == "" {
			// userinfo 拿不到 uid 时的兜底随机值。
			//
			// qoder 的 uid **不只是账号标识**：它会被塞进 COSY 身份的 aid/uid
			// 与 cosy-user 头，上游据此判定登录态。随机值必然与真实身份不符，
			// 结果是 /algo 全量 403 105（签名对、身份错）。
			// 所以这是最后手段：能拿到真实 uid 时绝不要走到这里。
			uid = strings.ReplaceAll(genUUID(), "-", "")[:16]
		}
		// 机器指纹缺失则自动生成（PAT 路线无需用户手填）
		if extra == nil {
			extra = map[string]string{}
		}
		fp := genMachineFingerprint()
		for _, f := range []string{"machineID", "machineToken", "machineType"} {
			if strings.TrimSpace(extra[f]) == "" {
				extra[f] = fp[f]
			}
		}
	}

	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("凭证缺少 accessToken")
	}
	if strings.TrimSpace(uid) == "" {
		return nil, fmt.Errorf("凭证缺少 uid（该家凭证里没有账号标识，请在 JSON 里补一个 uid）")
	}

	expiresAt := auth.JWTExp(accessToken)
	switch provider {
	case "qoder":
		// dt- 的寿命不可从凭证本身读出（不透明串），但实测约 30 天，上游会静默让它过期。
		// 必须写有限值：早先写 farFuture(2100) 会让 NeedsRefresh 永假、看护从不刷新，
		// 凭证一过期该号就永久 "Login expired"。PAT 换来的 jt- 观测寿命 24h，更短。
		expiresAt = time.Now().Add(30 * 24 * time.Hour).Unix()
		if strVal(input["pat"]) != "" {
			expiresAt = time.Now().Add(24 * time.Hour).Unix()
		}
	case "deepseek":
		// sessionid / userToken 固定约 30 天过期；写真实估算值便于到期提醒与自动失效。
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(30 * 24 * time.Hour).Unix()
		}
	default: // trae
		if expiresAt <= 0 {
			expiresAt = farFuture
		}
	}

	return &Credential{
		Provider:     provider,
		Region:       region, // ★ 必须回填：Save 用它算键前缀（KeyPrefix）
		UID:          uid,
		Nickname:     nickname,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Extra:        extra,
	}, nil
}

// MintTraeJWT 用登录会话标识（X-Cloudide-Session）铸一枚新的 Cloud-IDE-JWT。
// 复用 upstream 的续期实现（与网关 token 看护同一条路径），避免协议细节两处分叉。
func MintTraeJWT(sid string) (string, error) {
	return MintTraeJWTRegion(sid, "")
}

// MintTraeJWTRegion 带区域的铸令牌。
//
// region 必须传：国内版与国际版的换 token 端点不同（api.trae.cn / api.trae.ai），
// 拿国际版 sourceId 去打国内版端点会直接 401。upstream.traeProvider.isIntl()
// 靠 Extra["region"] 选基址，所以这里必须把它带上。
func MintTraeJWTRegion(sid, region string) (string, error) {
	ex := map[string]string{"sourceId": sid}
	if r := strings.ToLower(strings.TrimSpace(region)); r != "" {
		ex["region"] = r
	}
	a := &auth.Auth{Provider: "trae", Extra: ex}
	if err := upstream.New().RefreshToken(a); err != nil {
		return "", fmt.Errorf("用 sourceId 换新 JWT 失败: %w（若会话已失效，请直接在浏览器 F12 复制 Authorization 里的 Cloud-IDE-JWT）", err)
	}
	jwt := strings.TrimSpace(a.AccessToken)
	if jwt == "" {
		return "", fmt.Errorf("上游没返回令牌（sourceId 应当是 X-Cloudide-Session，不是 X-Cloudide-Token）")
	}
	return jwt, nil
}

// ─── 凭证归一 ───────────────────────────────────────────────────────────────

func strVal(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func normalizeExtra(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, val := range m {
		if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// adaptDeepseekCredential 把浏览器复制的凭证归一为标准结构。支持三种输入形态：
//   - {"accessToken":"<token>"}                     结构化（原样通过）
//   - {"userToken":{"value":"<token>"}}             localStorage 原始 JSON
//   - {"token":"<token>","id":"<uid>"}              users/current 响应片段
//
// uid 缺省填 deepseek-1（DeepSeek 账号 id 是 UUID，用固定标签更好认）。
func adaptDeepseekCredential(input map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range input {
		out[k] = v
	}
	if strVal(out["accessToken"]) == "" {
		if m, ok := out["userToken"].(map[string]any); ok {
			if v := strVal(m["value"]); v != "" {
				out["accessToken"] = v
			}
		}
	}
	if strVal(out["accessToken"]) == "" {
		if v := strVal(out["token"]); v != "" {
			out["accessToken"] = v
		}
	}
	if strVal(out["uid"]) == "" {
		if id := strVal(out["id"]); id != "" {
			out["uid"] = id
		} else {
			out["uid"] = "deepseek-1"
		}
	}
	// extra 只保留 device_id。DeepSeek 鉴权只认 Bearer userToken，Cookie 实测非必需——
	// 不收集、不保存，避免把浏览器会话凭证落到磁盘。
	extra := map[string]any{}
	if e, ok := out["extra"].(map[string]any); ok {
		if v := strVal(e["device_id"]); v != "" {
			extra["device_id"] = v
		}
	}
	if v := strVal(out["device_id"]); v != "" {
		if _, exists := extra["device_id"]; !exists {
			extra["device_id"] = v
		}
	}
	if len(extra) > 0 {
		out["extra"] = extra
	}
	return out
}

// ─── Qoder PAT 兑换 / userinfo ──────────────────────────────────────────────

// qoderOpenAPIBase 返回 Qoder OpenAPI 基址（按区域）。
//
// 国内 center 与网关同属 qoder.com.cn 体系；国际版在 center.qoder.sh。
// 两者 userinfo / jobToken/exchange 的路径相同，只有域名不同。
func qoderOpenAPIBase(region string) string {
	if strings.EqualFold(region, "intl") {
		return "https://center.qoder.sh"
	}
	return "https://openapi.qoder.com.cn"
}

func qoderExchangePAT(pat, region string) (token, refresh string, expiresAt int64, err error) {
	body, _ := json.Marshal(map[string]string{"personal_token": pat})
	req, e := http.NewRequest(http.MethodPost, qoderOpenAPIBase(region)+"/api/v1/jobToken/exchange", bytes.NewReader(body))
	if e != nil {
		return "", "", 0, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "reverse-proxy")
	req.Header.Set("Cosy-Version", "1.0.1")
	req.Header.Set("Cosy-ClientType", "5")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, e := client.Do(req)
	if e != nil {
		return "", "", 0, e
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", 0, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var out struct {
		Token        string `json:"token"`
		JobToken     string `json:"job_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", "", 0, fmt.Errorf("parse: %w", err)
	}
	t := out.Token
	if t == "" {
		t = out.JobToken
	}
	if t == "" {
		return "", "", 0, fmt.Errorf("empty token")
	}
	if out.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Millisecond).Unix()
	}
	return t, out.RefreshToken, expiresAt, nil
}

func qoderUserinfo(jt, region string) (string, error) {
	req, e := http.NewRequest(http.MethodGet, qoderOpenAPIBase(region)+"/api/v1/userinfo", nil)
	if e != nil {
		return "", e
	}
	req.Header.Set("Authorization", "Bearer "+jt)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, e := client.Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	var out struct {
		ID   string `json:"id"`
		UID  string `json:"uid"`
		Data struct {
			UserID string `json:"user_id"`
			UserId string `json:"userId"`
			ID     string `json:"id"`
			UID    string `json:"uid"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rb, &out)
	// userinfo 返回形态不固定。**Qoder CN 实测只返回顶层 id**
	// （01a09e88-b3ac-7225-98ff-3f90b318d290），没有顶层 uid，
	// 其余分支是旧形态兜底（与 qoder_bootstrap.py fetch_userinfo 对齐）。
	//
	// 漏掉 id 的代价不是少个展示字段：uid 会退化成随机 hex（见下方兜底），
	// 而它同时是 COSY 身份里的 aid/uid 与 cosy-user 头，
	// 于是所有 /algo 请求（模型目录、对话、签到）稳定 403 105 Login expired。
	for _, u := range []string{out.ID, out.UID, out.Data.UserID, out.Data.UserId, out.Data.ID, out.Data.UID} {
		if strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u), nil
		}
	}
	return "", nil
}

// genMachineFingerprint 生成 COSY 持久机器指纹（与 qoder_bootstrap.py
// gen_machine_fingerprint / qoderwork2api EnsureMachineFingerprint 对齐）。
func genMachineFingerprint() map[string]string {
	seed := genUUID() + genUUID()
	seed = strings.ReplaceAll(seed, "-", "")
	if len(seed) > 50 {
		seed = seed[:50]
	}
	return map[string]string{
		"machineID":    genUUID(),
		"machineToken": base64.RawURLEncoding.EncodeToString([]byte(seed)),
		"machineType":  strings.ReplaceAll(genUUID(), "-", "")[:18],
	}
}

// genUUID 生成随机 UUIDv4（crypto/rand）。
func genUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
