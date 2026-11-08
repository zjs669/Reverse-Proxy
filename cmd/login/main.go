// login.go — 统一交互式授权向导（CodeBuddy / Trae / Qoder / DeepSeek）。
//
// 用法：login <provider> <name>     codebuddy 国际版：login codebuddy intl <name>
//   name 留空则默认等于键名（如 trae_1）。键名索引自动顺延，同 uid 重登覆盖原槽位。
//   codebuddy 走 OAuth 设备流；其余四家读 stdin 的凭证 JSON，写入 ./auths/auth.json。
//
// 换凭证/归一/落盘全在 internal/provision，与 Web 管理台共用——两边各写一份必然分叉。
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"reverse-proxy/internal/provision"
)

const defaultAuthFile = "./auths/auth.json"

// ─── 入口 ───────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <provider> <name>\n  provider: codebuddy|trae|qoder|deepseek\n  name:     账号显示名（可空；codebuddy 国际版用 login codebuddy intl <name>）")
	}
	provider := strings.ToLower(os.Args[1])
	// region 仅 codebuddy 使用；其余供应商第二位置直接是名称。
	regionArg, nickname := "cn", ""
	switch {
	case provider == "codebuddy" && len(os.Args) >= 3 && isRegion(os.Args[2]):
		regionArg = os.Args[2]
		if len(os.Args) >= 4 { nickname = os.Args[3] }
	case len(os.Args) >= 3:
		nickname = os.Args[2]
	}
	authFile := os.Getenv("REVERSE_PROXY_AUTH_FILE")
	if authFile == "" { authFile = defaultAuthFile }
	if err := os.MkdirAll(filepath.Dir(authFile), 0o755); err != nil {
		fatal("mkdir auth dir: %v", err)
	}
	switch provider {
	case "codebuddy":  loginCodeBuddy(regionArg, nickname, authFile)
	case "trae":       loginPaste("trae", nickname, authFile, traeInstructions)
	case "qoder":      loginQoder(nickname, authFile)
	case "deepseek":   loginPaste("deepseek", nickname, authFile, deepseekInstructions)
	default:           fatal("unknown provider %q (want codebuddy|trae|qoder|deepseek)", provider)
	}
}

// isRegion 判断字符串是否为合法区域（仅 codebuddy 使用）。
func isRegion(s string) bool {
	return strings.EqualFold(s, "cn") || strings.EqualFold(s, "intl")
}

// ─── CodeBuddy（OAuth 设备流，自动）─────────────────────────────────────────

func loginCodeBuddy(regionArg, nickname, authFile string) {
	sess, err := provision.StartCodeBuddy(regionArg)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("请在浏览器打开并完成登录：\n\n  %s\n\n", sess.AuthURL)
	if commandExists("xclip") {
		_ = copyClipboard(sess.AuthURL)
		fmt.Println("(已复制到剪贴板)")
	}
	fmt.Println("完成登录后按回车继续...")
	readLine()
	// 轮询拿 token（3s 间隔，与网页版同节奏）
	var cred *provision.Credential
	var lastErr error
	for i := 0; i < 20; i++ {
		c, pending, perr := sess.Poll()
		if perr != nil {
			lastErr = perr
		} else if !pending && c != nil {
			cred = c
			break
		}
		fmt.Printf("  …等待登录完成（%d/20，3s 后重试）\r", i+1)
		time.Sleep(3 * time.Second)
	}
	fmt.Println()
	if cred == nil {
		if lastErr != nil {
			fatal("登录未完成：%v", lastErr)
		}
		fatal("登录未完成：请确认已在浏览器完成登录，再运行本命令重试")
	}
	cred.Nickname = nickname
	key, err := provision.Save(authFile, "", cred)
	if err != nil {
		fatal("write auth.json: %v", err)
	}
	ensureAccountKey(key, cred.UID)
	if ok, msg := provision.Checkin(cred.Domain, cred.AccessToken, cred.UID, cred.EnterpriseID); ok {
		fmt.Println("  签到: 成功")
	} else if msg != "" {
		fmt.Printf("  签到: %s\n", msg)
	}
	fmt.Printf("\n✅ 配置成功：键 %s（provider=codebuddy region=%s nickname=%s uid=%s）\n", key, regionArg, nickname, cred.UID)
	restartGateway()
}

// ─── 粘贴型凭证（Trae / DeepSeek）──────────────────────────────────

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

const deepseekInstructions = `DeepSeek 网页版（chat.deepseek.com）凭证获取方式：
  1) 浏览器登录 https://chat.deepseek.com
  2) F12 → Application → Local Storage → https://chat.deepseek.com
     找到 userToken，其值是 {"value":"<你的 token>","__version":"0"}，复制 value 字段；
     或者 F12 → Network → 任选一个 /api/v0/ 请求 → Request Headers →
     Authorization: Bearer <你的 token>，复制 Bearer 后面那串。

  续期：DeepSeek 没有 refresh 接口，token 失效（网关日志报 code=40003）后
  重新从浏览器复制一次，再跑同一条命令覆盖即可。

  粘贴格式（二选一）：
    A) 只要 token（最简，推荐）：{ "accessToken":"<userToken>" }
    B) 带设备指纹（可选）：{ "accessToken":"<userToken>", "uid":"deepseek-1", "extra": { "device_id":"<uuid>" } }

命令示例：  login deepseek 我的DeepSeek`

const qoderInstructions = `Qoder 凭证获取方式：
  A) 推荐——只给 PAT：https://qoder.com.cn/account/integrations 创建 Personal Access Token
     粘贴：{"pat":"pt-...","uid":"<可选>","extra":{"machineID":...,"machineToken":...,"machineType":...}}
  B) 已有 dt-/drt-（如设备流抓到）：
     粘贴：{"accessToken":"dt-...","refreshToken":"drt-...","uid":"...",
            "extra":{"machineID":"...","machineToken":"...","machineType":"..."}}`

func loginPaste(provider, nickname, authFile, instructions string) {
	fmt.Println(instructions)
	fmt.Println()
	fmt.Println("粘贴 JSON 后按回车，再按 Ctrl+D（或输入 END 独占一行）结束：")

	input, err := readPastedJSON()
	if err != nil {
		fatal("读取凭证失败: %v", err)
	}
	cred, err := provision.FromPaste(provider, input, nickname)
	if err != nil {
		fatal("%v", err)
	}
	key, err := provision.Save(authFile, "", cred)
	if err != nil {
		fatal("write auth.json: %v", err)
	}
	fmt.Println()
	fmt.Printf("✅ 配置成功：键 %s（provider=%s nickname=%s uid=%s）\n", key, provider, nickname, cred.UID)
	ensureAccountKey(key, cred.UID)
	restartGateway()
}

// ─── Qoder（PAT 兑换 或 dt-/drt- 直接填）────────────────────────────────────

func loginQoder(nickname, authFile string) {
	fmt.Println(qoderInstructions)
	fmt.Println()
	fmt.Println("粘贴 JSON 后按回车，再按 Ctrl+D（或输入 END 独占一行）结束：")

	input, err := readPastedJSON()
	if err != nil {
		fatal("读取凭证失败: %v", err)
	}
	if s, _ := input["pat"].(string); strings.TrimSpace(s) != "" {
		fmt.Println("→ 用 PAT 兑换 jobToken ...")
	}
	cred, err := provision.FromPaste("qoder", input, nickname)
	if err != nil {
		fatal("%v", err)
	}
	key, err := provision.Save(authFile, "", cred)
	if err != nil {
		fatal("write auth.json: %v", err)
	}
	fmt.Println()
	fmt.Printf("✅ 配置成功：键 %s（provider=qoder nickname=%s uid=%s）\n", key, nickname, cred.UID)
	ensureAccountKey(key, cred.UID)
	restartGateway()
}

// ─── 凭证输入 ───────────────────────────────────────────────────────────────

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

// ─── account_keys / 重启 ────────────────────────────────────────────────────

// randomSecret 生成 48 位十六进制随机 API 密钥。
func randomSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x%x", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(b)
}

// ensureAccountKey 登录后把该账号的对外调用密钥同步进 config.json 的 account_keys。
// 已存在则不覆盖（避免每次重登换 key 打乱客户端配置）；读不到 auth.json 时用空 uid 兜底。
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
		fmt.Printf("⚠️  未找到 %s（%v），跳过自动写入 account_keys。\n", path, err)
		return
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal(raw, &cfg) != nil {
		fmt.Println("⚠️  config.json 解析失败，跳过自动写入 account_keys。")
		return
	}
	ak := map[string]map[string]string{}
	if v, ok := cfg["account_keys"]; ok {
		_ = json.Unmarshal(v, &ak)
	}
	if ak == nil {
		ak = map[string]map[string]string{}
	}
	if ak[label] == nil {
		ak[label] = map[string]string{}
	}
	secret, existed := ak[label][uid]
	if !existed || strings.TrimSpace(secret) == "" {
		secret, existed = randomSecret(), false
		ak[label][uid] = secret
	} else {
		existed = true
	}
	b, err := json.Marshal(ak)
	if err != nil {
		fmt.Println("⚠️  序列化 account_keys 失败：", err)
		return
	}
	cfg["account_keys"] = b
	pretty, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Println("⚠️  序列化 config.json 失败：", err)
		return
	}
	// 原地截断重写（O_TRUNC）：单文件 bind mount 下 rename 会 EBUSY。
	if err := os.WriteFile(path, append(pretty, '\n'), 0o644); err != nil {
		fmt.Printf("⚠️  写入 %s 失败：%v\n", path, err)
		return
	}
	if existed {
		fmt.Printf("🔑 account_keys 已存在密钥（%s / %s），沿用：%s\n", label, uid, secret)
	} else {
		fmt.Printf("🔑 已生成 API 密钥并写入 account_keys（%s / %s）：\n   %s\n", label, uid, secret)
	}
	fmt.Println("   → 容器版自动热加载；仅老版本需 docker restart reverse-proxy")
}

func restartGateway() {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Println("(未检测到 docker；当前版本会自动热加载账号，无需手动重启)")
		return
	}
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return
	}
	if strings.Contains(string(out), "reverse-proxy") {
		fmt.Println("→ 检测到本地网关容器；当前版本会自动热加载账号，跳过重启")
	}
}

// ─── 杂项 ───────────────────────────────────────────────────────────────────

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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}
