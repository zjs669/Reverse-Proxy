// Package provision 把「建号」能力从 cmd/login 下沉出来，供 CLI 与管理台共用。
//
// 为什么单独成包：Web 管理台要在网关进程内完成与 login 完全一致的「换凭证 / 归一 /
// 落盘」，两处各写一份必然分叉（历史教训：jwtExp 两套实现导致 expiresAt 写成 2100、
// 8h 令牌永不续期）。本包不含任何交互——没有标准输入、没有终端输出、不调 os.Exit，
// 只返回结果与 error，文案与提示留在 cmd/login 与 admin 层。
package provision

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// farFuture 无明确过期时的兜底时间戳（2100-01-01）。
//
// 只给「凭证本身不携带寿命、上游也没有 refresh 接口」的账号用；trae/qoder 这类
// 有真实寿命的凭证必须写有限值，否则 NeedsRefresh 永假、token 看护从不续期。
const farFuture = int64(4102444800)

// Credential 一个待入库的账号凭证（归一化后的中间形态）。
type Credential struct {
	Provider     string
	Region       string // cn / intl；决定键前缀（KeyPrefix）与上游基址。codebuddy/trae/qoder 都用
	UID          string
	Nickname     string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	EnterpriseID string
	Extra        map[string]string
}

// Entry 生成写入 auth.json 的条目形态。
//
// codebuddy 用嵌套形（{auth,account}，与插件 OAuth 输出一致）；
// 其余供应商用扁平形。两种形态 auth.Parse 都认，但混用会让续期写回把文件
// 改成另一种形状、与旧版 login 产出的文件不一致，故严格按供应商区分。
func (c *Credential) Entry() map[string]any {
	if c.Provider == "codebuddy" {
		return map[string]any{
			"provider": "codebuddy",
			"account": map[string]any{
				"uid":          c.UID,
				"enterpriseId": c.EnterpriseID,
				"nickname":     c.Nickname,
			},
			"auth": map[string]any{
				"accessToken":  c.AccessToken,
				"refreshToken": c.RefreshToken,
				"expiresAt":    c.ExpiresAt,
				"domain":       c.Domain,
			},
		}
	}
	e := map[string]any{
		"provider":    c.Provider,
		"uid":         c.UID,
		"nickname":    c.Nickname,
		"accessToken": c.AccessToken,
		"expiresAt":   c.ExpiresAt,
	}
	if c.RefreshToken != "" {
		e["refreshToken"] = c.RefreshToken
	}
	if len(c.Extra) > 0 {
		m := make(map[string]any, len(c.Extra))
		for k, v := range c.Extra {
			m[k] = v
		}
		e["extra"] = m
	}
	return e
}

// KeyPrefix 把 provider+region 映射成 auth.json 顶层键前缀。
func KeyPrefix(provider, region string) string {
	switch provider {
	case "codebuddy":
		if strings.EqualFold(region, "intl") {
			return "codebuddyai"
		}
		return "codebuddy"
	case "trae":
		// 国际版用 traeai 前缀，与 codebuddyai 同构：
		// 光看 trae_1 / trae_2 分不出打的是 trae.cn 还是 trae.ai，
		// 而这两家域名、认证头、积分接口全不同，排查时必须一眼可辨。
		if strings.EqualFold(region, "intl") {
			return "traeai"
		}
		return "trae"
	case "qoder":
		// 国际版用 qoderai 前缀，与 codebuddyai / traeai 同构：
		// 光看 qoder_1 / qoder_2 分不出打的是 gateway.qoder.com.cn 还是 api3.qoder.sh，
		// 而这两家的域名、模型目录、签到活动全不同，排查时必须一眼可辨。
		if strings.EqualFold(region, "intl") {
			return "qoderai"
		}
		return "qoder"
	case "doubao", "deepseek":
		return provider
	}
	return provider
}

// nextKey 计算 <prefix>_<n>：取已有同前缀最大索引 +1，默认 1。
func nextKey(top map[string]json.RawMessage, prefix string) string {
	maxIdx := 0
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `_(\d+)$`)
	for k := range top {
		if m := re.FindStringSubmatch(k); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > maxIdx {
				maxIdx = n
			}
		}
	}
	return fmt.Sprintf("%s_%d", prefix, maxIdx+1)
}

// reuseKeyForUID 若 auth.json 里已有同一 uid 的条目，返回那个键名（覆盖原槽位），
// 否则回落到 fallback。
//
// 为什么必须覆盖而不是追加：池按 UID 建 entry，LoadFile 遍历的是 map（顺序随机），
// 同 uid 两条凭证谁后 upsert 谁生效——重新登录写了 trae_2、而 trae_1 里的死令牌可能
// 胜出，表现就是「明明重登了还是 401」（还会多生一把没用的 account_key）。
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
		// 两种磁盘形态都要认：扁平形与嵌套形（网关续期后 SaveAtomic 写回的形态）。
		// 只读扁平 uid 会漏掉续期过的账号，于是第二次登录又新建一个同 uid 槽位。
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

// loadTop 读取 auth.json 顶层 map；文件不存在返回空 map（首次建号即此路径）。
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

// writeEntry 把 entry 写入 auth.json 的 key（tmp + rename 原子替换整个文件）。
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

// saveMu 串行化 auth.json 的读改写：并发建号（管理台连点、CLI 与管理台同时写）
// 会各自读到旧快照再整体写回，互相覆盖掉对方刚加的账号。
var saveMu sync.Mutex

// Save 把凭证写入 auth.json 的槽位，返回实际使用的标签（键名）。
//
// preferredKey 一般由调用方用 KeyPrefix+NextKey 算好，留空则本函数自行计算。
// 两处特殊处理：
//   - 豆包固定单槽位 doubao_1（续期覆盖，不让键名无限增长）；
//   - 同 uid 已有槽位时复用（见 reuseKeyForUID，避免「重登还是 401」）。
func Save(authFile, preferredKey string, c *Credential) (string, error) {
	if strings.TrimSpace(authFile) == "" {
		return "", fmt.Errorf("auth_file 未配置")
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return "", fmt.Errorf("凭证缺少 accessToken")
	}
	if strings.TrimSpace(c.UID) == "" {
		return "", fmt.Errorf("凭证缺少 uid")
	}
	if dir := filepath.Dir(authFile); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	saveMu.Lock()
	defer saveMu.Unlock()

	key := strings.TrimSpace(preferredKey)
	if c.Provider == "doubao" {
		key = "doubao_1"
	}
	if key == "" {
		top, err := loadTop(authFile)
		if err != nil {
			return "", err
		}
		key = nextKey(top, KeyPrefix(c.Provider, c.Region))
	}
	key = reuseKeyForUID(authFile, key, c.UID)
	if err := writeEntry(authFile, key, c.Entry()); err != nil {
		return "", fmt.Errorf("write %s: %w", authFile, err)
	}
	return key, nil
}

// FarFuture 暴露兜底时间戳，供调用方判断「是否无真实寿命」。
func FarFuture() int64 { return farFuture }
