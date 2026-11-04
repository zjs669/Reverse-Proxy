// login.go — 统一交互式授权向导（CodeBuddy / Trae / Qoder / Doubao）。
//
// 用法：
//
//	login <provider> <name>
//
//	  provider : codebuddy | trae | qoder | doubao
//	  name     : 账号显示名（不校验，留空则默认等于键名，如 trae_1）
//
// 行为：
//   - 按 <provider> 计算单文件键名：qoder_1 / trae_1 / codebuddy_1 / codebuddyai_1 / doubao_1 ...
//     索引自动顺延，不与已有键冲突。
//   - codebuddy 国际版用：login codebuddy intl <name>（region 仅 codebuddy 使用，intl → 键前缀 codebuddyai）。
//   - codebuddy：自动跑 OAuth 设备流（打印授权 URL → 等待浏览器登录 → 轮询拿 token）。
//   - trae / qoder / doubao：打印「如何获取凭证」，然后从标准输入读一段凭证 JSON，写盘。
//   - 写盘后尽力 docker restart 网关加载新账号。
//
// 凭证写入 ./auths/auth.json（单文件顶层 map，键即上面算出的名字）。
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/upstream"
)

const (
	clientUA       = "CLI/2.63.2 CodeBuddy/2.63.2"
	defaultAuthFile = "./auths/auth.json"
	farFuture      = int64(4102444800) // 2100-01-01，无明确过期时兜底
)

// region 一组区域端点（仅 codebuddy 用）。
type region struct {
	Name      string
	Base      string
	Origin    string
	StateFile string
}

var regions = map[string]region{
	"cn": {
		Name:      "CN",
		Base:      "https://copilot.tencent.com",
		Origin:    "https://www.codebuddy.cn",
		StateFile: filepath.Join(os.TempDir(), "reverse-proxy-login-cn.json"),
	},
	"intl": {
		Name:      "Intl",
		Base:      "https://www.codebuddy.ai",
		Origin:    "https://www.codebuddy.ai",
		StateFile: filepath.Join(os.TempDir(), "reverse-proxy-login-intl.json"),
	},
}

// ─── 入口 ───────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <provider> <name>\n  provider: codebuddy|trae|qoder|doubao|deepseek\n  name:     账号显示名（可空；codebuddy 国际版用 login codebuddy intl <name>）")
	}
	provider := strings.ToLower(os.Args[1])

	// 统一形态：login <平台> <名称>。
	// region 仅 codebuddy 使用（login codebuddy [intl] [name]）；其余供应商第二位置直接是名称。
	regionArg := "cn"
	nickname := ""
	switch {
	case provider == "codebuddy" && len(os.Args) >= 3 && isRegion(os.Args[2]):
		regionArg = os.Args[2]
		if len(os.Args) >= 4 {
			nickname = os.Args[3]
		}
	case len(os.Args) >= 3:
		nickname = os.Args[2]
	}

	authFile := os.Getenv("REVERSE_PROXY_AUTH_FILE")
	if authFile == "" {
		authFile = defaultAuthFile
	}
	if err := os.MkdirAll(filepath.Dir(authFile), 0o755); err != nil {
		fatal("mkdir auth dir: %v", err)
	}

	prefix := keyPrefix(provider, regionArg)
	top, err := loadTop(authFile)
	if err != nil {
		fatal("read %s: %v", authFile, err)
	}
	key := nextKey(top, prefix)
	// 豆包工作为单账号槽位：续期时直接覆盖 doubao_1，避免键名无限增长。
	if provider == "doubao" {
		key = "doubao_1"
	}
	if nickname == "" {
		nickname = key // 不校验，给个好看的默认名（等于键名）
	}

	switch provider {
	case "codebuddy":
		loginCodeBuddy(regionArg, nickname, authFile, key)
	case "trae":
		loginPaste("trae", nickname, authFile, key, traeInstructions)
	case "qoder":
		loginQoder(nickname, authFile, key)
	case "doubao":
		loginPaste("doubao", nickname, authFile, key, doubaoWorkInstructions)
	case "deepseek":
		loginPaste("deepseek", nickname, authFile, key, deepseekInstructions)
	default:
		fatal("unknown provider %q (want codebuddy|trae|qoder|doubao)", provider)
	}
}

// keyPrefix 把 provider+region 映射成 auth.json 顶层键前缀。
func keyPrefix(provider, region string) string {
	switch provider {
	case "codebuddy":
		if strings.EqualFold(region, "intl") {
			return "codebuddyai"
		}
		return "codebuddy"
	case "trae":
		return "trae"
	case "qoder":
		return "qoder"
	case "doubao":
		return "doubao"
	case "deepseek":
		return "deepseek"
	}
	return provider
}

// isRegion 判断字符串是否为合法区域（仅 codebuddy 使用）。
func isRegion(s string) bool {
	return strings.EqualFold(s, "cn") || strings.EqualFold(s, "intl")
}

// loadTop 读取 auth.json 顶层 map；文件不存在返回空 map。
func loadTop(path string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("auth.json parse_error: %w", err)
	}
	return top, nil
}

// nextKey 计算 <prefix>_<n>：取已有同前缀最大索引 +1，默认 1。
func nextKey(top map[string]json.RawMessage, prefix string) string {
	max := 0
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `_(\d+)$`)
	for k := range top {
		if m := re.FindStringSubmatch(k); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > max {
				max = n
			}
		}
	}
	return fmt.Sprintf("%s_%d", prefix, max+1)
}

// reuseKeyForUID 若 auth.json 里已有同一 uid 的扁平形条目，返回那个键名（覆盖原槽位），
// 否则回落到 nextKey 算出的新键。
//
// 为什么必须覆盖而不是追加：池按 UID 建 entry，LoadFile 遍历的是 map（顺序随机），
// 同 uid 两条凭证谁后 upsert 谁生效——重新登录写了 trae_2、而 trae_1 里的死令牌可能胜出，
// 表现就是「明明重登了还是 401」（而且 account_keys 会多生一把没用的新密钥）。
// doubao 写死单槽位 doubao_1 正是同一考虑。命中多个历史槽位时取键名最小的一条，保证确定。
func reuseKeyForUID(path, fallback, uid string) string {
	if strings.TrimSpace(uid) == "" {
		return fallback
	}
	top, err := loadTop(path)
	if err != nil {
		return fallback
	}
	hits := make([]string, 0, 2)
	for k, val := range top {
		// 两种磁盘形态都要认：扁平形（login 写的）与嵌套形（网关续期后 SaveAtomic 写回的
		// {"auth":{…},"account":{"uid":…}}）。只读扁平 uid 会漏掉续期过的账号，
		// 于是第二次 login 又新建一个同 uid 槽位（入池时谁生效不确定）——这正是要避免的事。
		var probe struct {
			UID     string `json:"uid"`
			Account struct {
				UID string `json:"uid"`
			} `json:"account"`
		}
		if json.Unmarshal(val, &probe) != nil {
			continue
		}
		got := strings.TrimSpace(probe.UID)
		if got == "" {
			got = strings.TrimSpace(probe.Account.UID)
		}
		if got == uid {
			hits = append(hits, k)
		}
	}
	if len(hits) == 0 {
		return fallback
	}
	sort.Strings(hits)
	return hits[0]
}

// writeEntry 把 entry 写入 auth.json 的 key（原子替换整文件）。
func writeEntry(path, key string, entry map[string]any) error {
	top, err := loadTop(path)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	top[key] = b
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ─── CodeBuddy（OAuth 设备流，自动）─────────────────────────────────────────

func loginCodeBuddy(regionArg, nickname, authFile, key string) {
	rg := pickRegion(regionArg)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	hdrs := commonHeaders(rg)

	// 1) 拿授权 URL + state
	data, _, err := doJSON(client, http.MethodPost, rg.Base+"/v2/plugin/auth/state?platform=CLI", hdrs, bytes.NewReader([]byte("{}")))
	if err != nil {
		fatal("auth state failed: %v", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		fatal("auth state: missing state or authUrl")
	}
	_ = os.WriteFile(rg.StateFile, mustJSON(loginState{State: st.State}), 0o600)

	fmt.Println("请在浏览器打开并完成登录：")
	fmt.Println()
	fmt.Printf("  %s\n", st.AuthURL)
	fmt.Println()
	if commandExists("xclip") {
		_ = copyClipboard(st.AuthURL)
		fmt.Println("(已复制到剪贴板)")
	}
	fmt.Println("完成登录后按回车继续...")
	readLine()

	// 2) 轮询拿 token（带重试，容忍用户手慢）
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	ok := false
	for i := 0; i < 20; i++ {
		tokRaw, _, errTok := doJSON(client, http.MethodGet, rg.Base+"/v2/plugin/auth/token?state="+st.State, hdrs, nil)
		if errTok == nil {
			if err := json.Unmarshal(tokRaw, &tok); err == nil && tok.AccessToken != "" {
				ok = true
				break
			}
		}
		fmt.Printf("  …等待登录完成（%d/20，3s 后重试）\r", i+1)
		time.Sleep(3 * time.Second)
	}
	fmt.Println()
	if !ok {
		fatal("登录未完成：请确认已在浏览器完成登录，再运行本命令重试")
	}
	if tok.Domain == "" {
		tok.Domain = rg.Origin
	}

	// 3) 拿 uid / nickname
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		hdrs(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(client, http.MethodGet, rg.Base+"/v2/plugin/login/account?state="+st.State, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	if strings.TrimSpace(acct.UID) == "" {
		fatal("无法获取 uid，登录态可能已失效，请重试")
	}
	// nickname 以用户输入为准（不校验上游值）
	_ = acct.Nickname

	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	if expiresAt <= 0 {
		expiresAt = farFuture
	}

	entry := map[string]any{
		"provider": "codebuddy",
		"account": map[string]any{
			"uid":          acct.UID,
			"enterpriseId": acct.EnterpriseID,
			"nickname":     nickname,
		},
		"auth": map[string]any{
			"accessToken":  tok.AccessToken,
			"refreshToken": tok.RefreshToken,
			"expiresAt":    expiresAt,
			"domain":       tok.Domain,
		},
	}
	if err := writeEntry(authFile, key, entry); err != nil {
		fatal("write auth.json: %v", err)
	}
	ensureAccountKey(key, acct.UID)
	_ = os.Remove(rg.StateFile)

	// 签到（幂等，不阻塞）
	codeBuddyCheckin(tok.Domain, tok.AccessToken, acct.UID, acct.EnterpriseID)

	fmt.Println()
	fmt.Printf("✅ 配置成功：键 %s（provider=codebuddy region=%s nickname=%s uid=%s）\n", key, regionArg, nickname, acct.UID)
	restartGateway()
}

// ─── Trae / Doubao（粘贴凭证 JSON）──────────────────────────────────────────

const traeInstructions = `Trae 凭证获取方式（**只需要一个长期会话标识**）：
  首选： source_id（= JWT payload.data.source_id = 请求头 X-Cloudide-Session）
    1) trae.cn 已登录页面 → F12 → Network → 任一请求 → Request Headers → 复制 X-Cloudide-Session 的值
    2) 或者 F12 → Console 执行： fetch('/cloudide/api/v3/common/GetUserToken',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'}).then(r=>r.json()).then(d=>console.log(d.Result.Token))
       拿到 JWT 后在 Console 解 payload： JSON.parse(atob(<JWT>.split('.')[1].replace(/-/g,'+').replace(/_/g,'/'))).data.source_id
  也可以： 直接粘一枚当前有效的 Cloud-IDE-JWT（向导从里面自动解析 source_id，下次就能靠会话续期）

请粘贴如下 JSON（两种任选一种）：
{
  "sourceId": "<X-Cloudide-Session 的值>"      // 只给这个：向导用它铸一枚新 JWT
}
{
  "accessToken": "<Cloud-IDE-JWT>"             // 只给这个：自动解析 exp / data.id / data.source_id
}
可选字段："uid"（X-Uid，一般省）、"refreshToken"（走 ExchangeToken 兜底路）、
       "extra": { "appId":"…", "appVersion":"…" }

为什么推荐 source_id：它是 Trae 的登录会话标识（长期有效），续期接口 GetUserToken 只靠它
就能铸出新的 8h JWT；而 JWT 本身一旦过期就再也不能换（实测回 401 / code 20101 Token 无效）。
存下它，就不需要每 8h 重粘一次令牌。`

const doubaoWorkInstructions = `豆包工作（Doubao Work / 豆包桌面客户端）凭证获取方式：
  打开豆包工作客户端并登录（账号自带积分额度）→ F12 → Network → 任一 chat 请求 →
  复制右侧 Request Headers 里的「Cookie:」整段值（形如 sessionid=xxx; ttwid=yyy; ...）。
  注意：用「账号 Cookie」而不是火山方舟 API Key——这样扣的是你豆包工作账号里的积分，不额外花钱。

  续期（约每 30 天一次）：登录态过期后客户端会报错，重新从 F12 复制 Cookie，
  再次运行同一命令即可覆盖，无需改任何字段。

  粘贴格式（二选一，直接粘都行）：
    A) 原始 Cookie（推荐，从 F12 整段复制即可）：
       { "sessionid":"<sessionid>", "ttwid":"1%7C...", "passport_csrf_token":"<csrf>", ... }
    B) 结构化（也可）：
       { "accessToken":"<sessionid>", "uid":"doubao-work-1",
         "extra": { "ttwid":"...", "passport_csrf_token":"..." } }

命令示例：  login doubao 我的豆包`


const deepseekInstructions = `DeepSeek 网页版（chat.deepseek.com）凭证获取方式：
  1) 浏览器登录 https://chat.deepseek.com
  2) F12 → Application → Local Storage → https://chat.deepseek.com
     找到 userToken，其值是 {"value":"<你的 token>","__version":"0"}，复制 value 字段；
     或者 F12 → Network → 任选一个 /api/v0/ 请求 → Request Headers →
     Authorization: Bearer <你的 token>，复制 Bearer 后面那串。

  续期：DeepSeek 没有 refresh 接口，token 失效（网关日志报 code=40003）后
  重新从浏览器复制一次，再跑同一条命令覆盖即可。

  粘贴格式（二选一）：
    A) 只要 token（最简，推荐）：
       { "accessToken":"<userToken>" }
    B) 带设备指纹 / Cookie（更贴近浏览器，可选）：
       { "accessToken":"<userToken>", "uid":"deepseek-1",
         "extra": { "device_id":"<uuid>" } }

命令示例：  login deepseek 我的DeepSeek`

func loginPaste(provider, nickname, authFile, key, instructions string) {
	fmt.Println(instructions)
	fmt.Println()
	fmt.Println("粘贴 JSON 后按回车，再按 Ctrl+D（或输入 END 独占一行）结束：")

	input, err := readPastedJSON()
	if err != nil {
		fatal("读取凭证失败: %v", err)
	}
	// 豆包：允许直接粘贴 F12 复制的整段 Cookie（扁平 key/value），自动归一字段。
	if provider == "doubao" {
		input = adaptDoubaoCredential(input)
	}
	// DeepSeek：允许直接粘贴 localStorage 的 userToken 或 users/current 响应片段。
	if provider == "deepseek" {
		input = adaptDeepseekCredential(input)
	}

	accessToken, _ := input["accessToken"].(string)
	accessToken = strings.TrimSpace(accessToken)
	uid, _ := input["uid"].(string)
	uid = strings.TrimSpace(uid)
	refreshToken, _ := input["refreshToken"].(string)
	extra := normalizeExtra(input["extra"])
	// Trae 的长期凭证是登录会话标识 source_id（= 请求头 X-Cloudide-Session）。
	// 只给 sourceId 时也接受：拿它向 GetUserToken 铸一枚新 JWT 再入库，
	// 用户从此不需要掐着 8h 去重粘令牌（实测：会话能铸新令牌，过期 JWT 不能）。
	if provider == "trae" {
		if sid, _ := input["sourceId"].(string); strings.TrimSpace(sid) != "" {
			sid = strings.TrimSpace(sid)
			if extra == nil {
				extra = map[string]any{}
			}
			if cur, _ := extra["sourceId"].(string); strings.TrimSpace(cur) == "" {
				extra["sourceId"] = sid
			}
			if accessToken == "" {
				fmt.Println("→ 只给了 sourceId，正向 GetUserToken 铸新 Cloud-IDE-JWT …")
				accessToken = traeMintJWT(sid)
			}
		}
	}
	// Trae：X-Cloudide-Session（data.source_id）与 X-Uid（data.id）都在 JWT 载荷里，
	// 用户很难自己从 F12 翻出来，这里统一自动解析补齐，最少只需粘 accessToken。
	if provider == "trae" && accessToken != "" {
		if uid == "" {
			if id := auth.JWTDataString(accessToken, "id"); id != "" {
				uid = id
				fmt.Println("→ 已从 JWT 解析 X-Uid（data.id）:", uid)
			}
		}
		if extra == nil {
			extra = map[string]any{}
		}
		if cur, _ := extra["sourceId"].(string); strings.TrimSpace(cur) == "" {
			if sid := auth.JWTSourceID(accessToken); sid != "" {
				extra["sourceId"] = sid
				fmt.Println("→ 已从 JWT 解析 X-Cloudide-Session（source_id）:", sid)
			}
		}
	}

	if accessToken == "" {
		fatal("凭证缺少 accessToken")
	}
	if uid == "" {
		fatal("凭证缺少 uid")
	}

	expiresAt := jwtExp(accessToken)
	if expiresAt <= 0 {
		if provider == "doubao" || provider == "deepseek" {
			// sessionid / userToken 固定约 30 天过期；写入真实过期时刻便于网关到期提醒与自动失效。
			expiresAt = time.Now().Add(30 * 24 * time.Hour).Unix()
		} else {
			expiresAt = farFuture
		}
	}

	entry := map[string]any{
		"provider":    provider,
		"uid":         uid,
		"nickname":    nickname,
		"accessToken": accessToken,
	}
	if refreshToken != "" {
		entry["refreshToken"] = refreshToken
	}
	entry["expiresAt"] = expiresAt
	if len(extra) > 0 {
		entry["extra"] = extra
	}

	// 同 uid 续期必须回到原槽位（而不是 trae_2/qoder_2）：见 reuseKeyForUID 注释。
	key = reuseKeyForUID(authFile, key, uid)

	if err := writeEntry(authFile, key, entry); err != nil {
		fatal("write auth.json: %v", err)
	}
	fmt.Println()
	fmt.Printf("✅ 配置成功：键 %s（provider=%s nickname=%s uid=%s）\n", key, provider, nickname, uid)
	ensureAccountKey(key, uid)
	restartGateway()
}

// traeMintJWT 用登录会话标识（X-Cloudide-Session）铸一枚新的 Cloud-IDE-JWT。
// 直接复用 upstream 的续期实现（与网关 token 看护同一条路径），避免协议细节两处分叉。
func traeMintJWT(sid string) string {
	a := &auth.Auth{Provider: "trae", Extra: map[string]string{"sourceId": sid}}
	if err := upstream.New().RefreshToken(a); err != nil {
		fatal("用 sourceId 换新 JWT 失败：%v\n（若会话已失效，请直接在浏览器 F12 复制 Authorization 里的 Cloud-IDE-JWT）", err)
	}
	jwt := strings.TrimSpace(a.AccessToken)
	if jwt == "" {
		fatal("上游没返回令牌（sourceId 可能被用错了头名：它应当是 X-Cloudide-Session，不是 X-Cloudide-Token）")
	}
	fmt.Println("→ 已用登录会话（source_id）铸出新 Cloud-IDE-JWT，exp/uid/sourceId 接下来自动解析")
	return jwt
}

// ─── Qoder（PAT 兑换 或 dt-/drt- 直接填）────────────────────────────────────

func loginQoder(nickname, authFile, key string) {
	fmt.Println(`Qoder 凭证获取方式：
  A) 推荐——只给 PAT：https://qoder.com.cn/account/integrations 创建 Personal Access Token
     粘贴：{"pat":"pt-...","uid":"<可选>","extra":{"machineID":...,"machineToken":...,"machineType":...}}
  B) 已有 dt-/drt-（如设备流抓到）：
     粘贴：{"accessToken":"dt-...","refreshToken":"drt-...","uid":"...",
            "extra":{"machineID":"...","machineToken":"...","machineType":"..."}}`)
	fmt.Println()
	fmt.Println("粘贴 JSON 后按回车，再按 Ctrl+D（或输入 END 独占一行）结束：")

	input, err := readPastedJSON()
	if err != nil {
		fatal("读取凭证失败: %v", err)
	}

	pat, _ := input["pat"].(string)
	pat = strings.TrimSpace(pat)
	accessToken, _ := input["accessToken"].(string)
	accessToken = strings.TrimSpace(accessToken)
	refreshToken, _ := input["refreshToken"].(string)
	uid, _ := input["uid"].(string)
	uid = strings.TrimSpace(uid)
	extra := normalizeExtra(input["extra"])

	var exchangeErr error
	if pat != "" {
		fmt.Println("→ 用 PAT 兑换 jobToken ...")
		jt, rt, exp, e := qoderExchangePAT(pat)
		if e != nil {
			fatal("PAT 兑换失败: %v", e)
		}
		accessToken = jt
		if rt != "" {
			refreshToken = rt
		}
		if exp > 0 {
			// 优先用返回过期；否则后续默认 24h
		}
		if uid == "" {
			if u, e2 := qoderUserinfo(jt); e2 == nil && u != "" {
				uid = u
			}
		}
		// PAT 收进 extra：网关临期用它自动换新 jt-（对齐 qoder.go RefreshToken 路线 1）
		if extra == nil {
			extra = map[string]any{}
		}
		extra["pat"] = pat
		_ = exchangeErr
	}

	if accessToken == "" {
		fatal("凭证缺少 accessToken（或 pat 兑换失败）")
	}
	if uid == "" {
		// userinfo 拿不到 uid（接口拒绝/字段形态不符）时兜底随机 uid：
		// uid 仅作账号标识与展示，上游鉴权走 accessToken（对齐 qoder_bootstrap.py 的 uuid4 兜底）。
		uid = strings.ReplaceAll(genUUID(), "-", "")[:16]
		fmt.Println("→ userinfo 未取到 uid，使用随机 uid 兜底:", uid)
	}
	// 机器指纹缺失则自动生成（PAT 路线无需用户手填，向导提示与实际行为一致）
	if extra == nil {
		extra = map[string]any{}
	}
	fp := genMachineFingerprint()
	for _, f := range []string{"machineID", "machineToken", "machineType"} {
		if strings.TrimSpace(fmt.Sprint(extra[f])) == "" {
			extra[f] = fp[f]
		}
	}

	// dt- 的寿命不可从凭证本身读出（不透明串），但实测约 30 天，且上游会静默让它过期。
	// 这里必须写一个**有限的**估算值：早先写 farFuture(2100) 会让 NeedsRefresh 永远为 false，
	// token watch 从不刷新，凭证一过期该号就永久 "Login expired"（qoder 就是这样挂的）。
	// 写 30 天后，配合 refresh_window 会提前续期；即便估算偏早，也只是多刷几次。
	expiresAt := time.Now().Add(30 * 24 * time.Hour).Unix()
	if pat != "" {
		// PAT 换来的 jt- 观测寿命 24h，更短。
		expiresAt = time.Now().Add(24 * time.Hour).Unix()
	}

	entry := map[string]any{
		"provider":    "qoder",
		"uid":         uid,
		"nickname":    nickname,
		"accessToken": accessToken,
	}
	if refreshToken != "" {
		entry["refreshToken"] = refreshToken
	}
	entry["expiresAt"] = expiresAt
	entry["extra"] = extra

	// 同 uid 续期回到原槽位（避免 qoder_1/qoder_2 同 uid 双写，见 reuseKeyForUID）。
	key = reuseKeyForUID(authFile, key, uid)

	if err := writeEntry(authFile, key, entry); err != nil {
		fatal("write auth.json: %v", err)
	}
	fmt.Println()
	fmt.Printf("✅ 配置成功：键 %s（provider=qoder nickname=%s uid=%s）\n", key, nickname, uid)
	ensureAccountKey(key, uid)
	restartGateway()
}

// ─── 凭证辅助 ───────────────────────────────────────────────────────────────

func normalizeExtra(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := map[string]any{}
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

// adaptDoubaoCredential 把用户从 F12 复制的「整段 Cookie」(扁平 key/value) 归一为
// loginPaste 需要的结构：sessionid → accessToken，ttwid / passport_csrf_token → extra；
// uid 缺省填 doubao-work-1。这样续期时用户无需手动改字段，直接粘贴 Cookie 即可。
// 已结构化的 {"accessToken":...,"extra":{...}} 形式也能正常通过（仅做补全）。
func adaptDoubaoCredential(input map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range input {
		out[k] = v
	}
	// sessionid 作为 accessToken（Cookie 里的核心登录态）
	if s, _ := out["accessToken"].(string); strings.TrimSpace(s) == "" {
		if sid, _ := out["sessionid"].(string); strings.TrimSpace(sid) != "" {
			out["accessToken"] = strings.TrimSpace(sid)
		}
	}
	// uid 缺省填充
	if u, _ := out["uid"].(string); strings.TrimSpace(u) == "" {
		out["uid"] = "doubao-work-1"
	}
	// 把代码实际会用到的配套 Cookie 收进 extra（doubao.go 的 cookie() 只拼这俩）
	extra := map[string]any{}
	if e, ok := out["extra"].(map[string]any); ok {
		for k, v := range e {
			extra[k] = v
		}
	}
	promote := func(src, dst string) {
		v, _ := out[src].(string)
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, exists := extra[dst]; !exists {
			extra[dst] = v
		}
	}
	promote("ttwid", "ttwid")
	// passport_csrf_token 可能藏在 passport_csrf_token_default 里
	if _, ok := extra["passport_csrf_token"]; !ok {
		promote("passport_csrf_token", "passport_csrf_token")
		promote("passport_csrf_token_default", "passport_csrf_token")
	}
	if len(extra) > 0 {
		out["extra"] = extra
	}
	return out
}


// adaptDeepseekCredential 把用户从浏览器复制的凭证归一为 loginPaste 需要的结构。
// 支持三种输入形态：
//   - {"accessToken":"<token>"}                     结构化（原样通过）
//   - {"userToken":{"value":"<token>"}}             localStorage 原始 JSON
//   - {"token":"<token>","id":"<uid>"}              users/current 响应片段
// uid 缺省填 deepseek-1（DeepSeek 账号 id 是 UUID，用固定标签更好认）。
func adaptDeepseekCredential(input map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range input {
		out[k] = v
	}
	// accessToken 缺省时从几种常见形态里捞。
	if s, _ := out["accessToken"].(string); strings.TrimSpace(s) == "" {
		// localStorage userToken: {"value":"<token>","__version":"0"}
		if m, ok := out["userToken"].(map[string]any); ok {
			if v, _ := m["value"].(string); strings.TrimSpace(v) != "" {
				out["accessToken"] = strings.TrimSpace(v)
			}
		}
	}
	if s, _ := out["accessToken"].(string); strings.TrimSpace(s) == "" {
		if v, _ := out["token"].(string); strings.TrimSpace(v) != "" {
			out["accessToken"] = strings.TrimSpace(v)
		}
	}
	// uid 缺省：优先用 users/current 的账号 id（UUID），否则固定标签。
	if u, _ := out["uid"].(string); strings.TrimSpace(u) == "" {
		if id, _ := out["id"].(string); strings.TrimSpace(id) != "" {
			out["uid"] = strings.TrimSpace(id)
		} else {
			out["uid"] = "deepseek-1"
		}
	}
	// extra 只保留 device_id。DeepSeek 鉴权只认 Bearer userToken，
	// Cookie 实测非必需——不收集、不保存，避免把浏览器会话凭证落到磁盘。
	extra := map[string]any{}
	if e, ok := out["extra"].(map[string]any); ok {
		if v, _ := e["device_id"].(string); strings.TrimSpace(v) != "" {
			extra["device_id"] = strings.TrimSpace(v)
		}
	}
	if v, _ := out["device_id"].(string); strings.TrimSpace(v) != "" {
		if _, exists := extra["device_id"]; !exists {
			extra["device_id"] = strings.TrimSpace(v)
		}
	}
	if len(extra) > 0 {
		out["extra"] = extra
	}
	return out
}

func readPastedJSON() (map[string]any, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, err
	}
	// 允许以独占一行的 END 结尾
	lines := strings.Split(string(data), "\n")
	var buf []string
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "END" {
			break
		}
		buf = append(buf, ln)
	}
	clean := strings.TrimSpace(strings.Join(buf, "\n"))
	if clean == "" {
		return nil, fmt.Errorf("空输入")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(clean), &m); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	return m, nil
}

// jwtExp 不验证签名，仅读 payload.exp（Cloud-IDE-JWT 为 3 段 base64url）。
// 实现统一交给 auth.JWTExp：本函数旧版本先补 '=' 再用 RawURLEncoding 解码，
// 载荷长度不是 4 的倍数时必然 illegal base64 data → 返回 0 → 写入 farFuture 占位值，
// 8h 短寿命账号因此双双失明（NeedsRefresh 永假、看护从不续期也不告警）。
func jwtExp(token string) int64 {
	return auth.JWTExp(token)
}

// ─── Qoder PAT 兑换 / userinfo ──────────────────────────────────────────────

func qoderExchangePAT(pat string) (token, refresh string, expiresAt int64, err error) {
	body, _ := json.Marshal(map[string]string{"personal_token": pat})
	req, e := http.NewRequest(http.MethodPost, "https://openapi.qoder.com.cn/api/v1/jobToken/exchange", bytes.NewReader(body))
	if e != nil {
		return "", "", 0, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "reverse-proxy")
	req.Header.Set("Cosy-Version", "1.0.1")
	req.Header.Set("Cosy-ClientType", "5")
	resp, e := http.DefaultClient.Do(req)
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

func qoderUserinfo(jt string) (string, error) {
	req, e := http.NewRequest(http.MethodGet, "https://openapi.qoder.com.cn/api/v1/userinfo", nil)
	if e != nil {
		return "", e
	}
	req.Header.Set("Authorization", "Bearer "+jt)
	req.Header.Set("Accept", "application/json")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}
	var out struct {
		UID  string `json:"uid"`
		Data struct {
			UserID string `json:"user_id"`
			UserId string `json:"userId"`
			UID    string `json:"uid"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rb, &out)
	// userinfo 返回形态不固定：可能是顶层 uid，也可能是嵌套 data.user_id/userId/uid
	// （与 qoder_bootstrap.py fetch_userinfo 的解析链对齐）。
	for _, u := range []string{out.UID, out.Data.UserID, out.Data.UserId, out.Data.UID} {
		if strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u), nil
		}
	}
	return "", nil
}

// genMachineFingerprint 生成 COSY 持久机器指纹（与 qoder_bootstrap.py
// gen_machine_fingerprint / qoderwork2api EnsureMachineFingerprint 对齐）。
func genMachineFingerprint() map[string]string {
	seed := (genUUID() + genUUID())
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

// ─── CodeBuddy OAuth 公共件 ────────────────────────────────────────────────

func pickRegion(arg string) region {
	name := strings.ToLower(strings.TrimSpace(arg))
	if name == "" {
		name = "cn"
	}
	r, ok := regions[name]
	if !ok {
		fatal("unknown region %q (want cn|intl)", name)
	}
	return r
}

func commonHeaders(r region) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", r.Origin)
		req.Header.Set("Referer", r.Origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(regions["cn"])(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

type loginState struct {
	State string `json:"state"`
}

func codeBuddyCheckin(domain, token, uid, entID string) {
	host := domain
	if host == "" {
		host = "https://www.codebuddy.cn"
	}
	req, err := http.NewRequest(http.MethodPost, host+"/v2/billing/meter/daily-checkin", bytes.NewReader([]byte("{}")))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-Id", uid)
	if entID != "" {
		req.Header.Set("X-Enterprise-Id", entID)
		req.Header.Set("X-Tenant-Id", entID)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("  (签到跳过: %v)\n", err)
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	var env apiEnvelope
	if json.Unmarshal(rb, &env) == nil && env.Code == 0 {
		fmt.Println("  签到: 成功")
	} else {
		fmt.Printf("  签到: %s\n", truncate(string(rb), 120))
	}
}

// ─── 杂项 ───────────────────────────────────────────────────────────────────

// randomSecret 生成 48 位十六进制随机 API 密钥。
func randomSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x%x", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(b)
}

// ensureAccountKey 在登录成功后，把该账号的对外调用密钥同步进 config.json 的 account_keys：
//   - 若该 label/uid 已存在非空密钥 → 沿用，不覆盖（续期不会把已有 key 冲掉）。
//   - 否则自动生成一把随机密钥写入。
//
// 路径由 REVERSE_PROXY_CONFIG_FILE 指定，缺省 ./config.json。
// 注意：容器里 config.json 必须可写挂载（docker-compose 去掉 :ro），否则写不进去。
// 重写时保持顶层键原有顺序，仅替换/追加 account_keys，其余字段逐字节保留。
func ensureAccountKey(label, uid string) {
	if strings.TrimSpace(uid) == "" {
		return
	}
	path := os.Getenv("REVERSE_PROXY_CONFIG_FILE")
	if path == "" {
		path = "./config.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("⚠️  未找到 %s（%v），跳过自动写入 account_keys，请手动把密钥加到 config.json。\n", path, err)
		return
	}

	// 按原顺序解析顶层键值对（key 保序 + value 原始字节）。
	dec := json.NewDecoder(bytes.NewReader(raw))
	tk, err := dec.Token()
	if err != nil {
		fmt.Printf("⚠️  config.json 解析失败：%v\n", err)
		return
	}
	if d, ok := tk.(json.Delim); !ok || d != '{' {
		fmt.Printf("⚠️  config.json 顶层不是 JSON 对象，跳过自动写入。\n")
		return
	}
	type kv struct {
		k string
		v json.RawMessage
	}
	var items []kv
	foundAK := false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			fmt.Printf("⚠️  config.json 解析失败：%v\n", err)
			return
		}
		k, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			fmt.Printf("⚠️  config.json 解析失败：%v\n", err)
			return
		}
		if k == "account_keys" {
			foundAK = true
		}
		items = append(items, kv{k, v})
	}

	// 解析并修改 account_keys。
	ak := map[string]map[string]string{}
	for _, it := range items {
		if it.k == "account_keys" {
			_ = json.Unmarshal(it.v, &ak)
		}
	}
	if ak == nil {
		ak = map[string]map[string]string{}
	}
	if ak[label] == nil {
		ak[label] = map[string]string{}
	}
	secret, existed := ak[label][uid]
	if !existed || strings.TrimSpace(secret) == "" {
		secret = randomSecret()
		ak[label][uid] = secret
		existed = false
	} else {
		existed = true
	}
	akCompact, err := json.Marshal(ak)
	if err != nil {
		fmt.Printf("⚠️  序列化 account_keys 失败：%v\n", err)
		return
	}
	if !foundAK {
		items = append(items, kv{"account_keys", akCompact})
	}

	// 重拼：保留原顺序，仅把 account_keys 换成新值；其余值用 json.Indent 规整缩进。
	var sb strings.Builder
	sb.WriteString("{\n")
	for i, it := range items {
		val := it.v
		if it.k == "account_keys" {
			val = akCompact
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, val, "  ", "  "); err != nil {
			pretty.Reset()
			pretty.Write(val)
		}
		kb, _ := json.Marshal(it.k)
		sb.WriteString("  ")
		sb.Write(kb)
		sb.WriteString(": ")
		sb.Write(pretty.Bytes())
		if i < len(items)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("}\n")

	// 注意：config.json 常以「单文件 bind mount」挂进容器，"写临时文件再 rename" 会因
	// 挂载点被占用而报 EBUSY；这里直接原地截断重写（O_TRUNC），对 bind mount 安全。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Printf("⚠️  打开 %s 失败：%v（config.json 可能是只读挂载，请去掉 docker-compose 里的 :ro）\n", path, err)
		return
	}
	if _, err := f.WriteString(sb.String()); err != nil {
		_ = f.Close()
		fmt.Printf("⚠️  写入 %s 失败：%v\n", path, err)
		return
	}
	if err := f.Close(); err != nil {
		fmt.Printf("⚠️  关闭 %s 失败：%v\n", path, err)
		return
	}

	if existed {
		fmt.Printf("🔑 account_keys 已存在密钥（%s / %s），沿用不覆盖：%s\n", label, uid, secret)
	} else {
		fmt.Printf("🔑 已自动生成 API 密钥并写入 account_keys（%s / %s）：\n   %s\n", label, uid, secret)
	}
	fmt.Println("   → 重启网关后生效：docker restart reverse-proxy")
}

func restartGateway() {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Println("(未检测到 docker，请手动重启网关加载新账号)")
		return
	}
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return
	}
	if strings.Contains(string(out), "reverse-proxy") {
		fmt.Println("→ 重启网关加载新账号 ...")
		_ = exec.Command("docker", "restart", "reverse-proxy").Run()
		time.Sleep(2 * time.Second)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func readLine() {
	r := bufio.NewReader(os.Stdin)
	_, _ = r.ReadString('\n')
}

func copyClipboard(s string) error {
	if commandExists("xclip") {
		cmd := exec.Command("xclip", "-selection", "clipboard")
		cmd.Stdin = strings.NewReader(s)
		return cmd.Run()
	}
	if commandExists("xsel") {
		cmd := exec.Command("xsel", "--clipboard", "--input")
		cmd.Stdin = strings.NewReader(s)
		return cmd.Run()
	}
	return fmt.Errorf("no clipboard tool")
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}
