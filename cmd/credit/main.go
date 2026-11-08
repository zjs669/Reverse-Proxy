// credit.go — WorkBuddy 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法：./credit [-pretty]        环境变量 REVERSE_PROXY_AUTH_DIR 可改凭证目录（默认 ./auths）
//
// 接口：POST {billingBase}/v2/billing/meter/get-user-resource，
// 聚合所有 package 的 Cycle* 字段，TotalDosage 作 size 下限。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reverse-proxy/internal/upstream"
)

const (
	billingBaseCN   = "https://www.codebuddy.cn"
	billingBaseIntl = "https://www.codebuddy.ai"
)

// billingBaseFor 按账号 domain 选 billing 域：含 codebuddy.ai 走国际版。
func billingBaseFor(domain string) string {
	if strings.Contains(strings.ToLower(domain), "codebuddy.ai") {
		return billingBaseIntl
	}
	return billingBaseCN
}

type authFile struct {
	Auth struct {
		AccessToken string `json:"accessToken"`
		Domain      string `json:"domain"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
}

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// fetchUserResource 查单个账号的额度汇总。
func fetchUserResource(af *authFile) (remain, used, size int64, packs int, err error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	url := billingBaseFor(af.Auth.Domain) + "/v2/billing/meter/get-user-resource"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+af.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	// 空值不发送：上游对空 header 的处理不一致，缺省比空串安全。
	for k, v := range map[string]string{
		"X-User-Id":       af.Account.UID,
		"X-Enterprise-Id": af.Account.EnterpriseID,
		"X-Tenant-Id":     af.Account.EnterpriseID,
		"X-Domain":        af.Auth.Domain,
	} {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					TotalDosage int64                  `json:"TotalDosage"`
					Accounts    []upstream.PackageUsage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, 0, 0, err
	}
	if env.Code != 0 {
		return 0, 0, 0, 0, fmt.Errorf("code=%d %s", env.Code, env.Msg)
	}
	d := env.Data.Response.Data
	for _, a := range d.Accounts {
		r, u, sz := upstream.PackageStat(a)
		remain, used, size = remain+r, used+u, size+sz
	}
	packs = len(d.Accounts)
	// size 下限修正：used 用 size-remain 兜底；TotalDosage 更大时以它为准。
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if d.TotalDosage > size {
		size = d.TotalDosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, nil
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("REVERSE_PROXY_AUTH_DIR"); v != "" {
		authDir = v
	}
	files, _ := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	sort.Strings(files)

	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		var af authFile
		if raw, err := os.ReadFile(f); err != nil || json.Unmarshal(raw, &af) != nil {
			continue
		}
		res := accountResult{UID: af.Account.UID, Nickname: af.Account.Nickname}
		if af.Auth.AccessToken == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)
			continue
		}
		remain, used, size, packs, err := fetchUserResource(&af)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain, res.Used, res.Size = &remain, &used, &size
			res.Packages, res.OK = packs, true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond) // 限速：避免多号并发打上游
	}

	var totalRemain, totalUsed, totalSize int64
	okCount := 0
	for _, a := range accounts {
		if !a.OK {
			continue
		}
		okCount++
		totalRemain += *a.Remain
		totalUsed += *a.Used
		totalSize += *a.Size
	}

	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize)
		return
	}
	raw, _ := json.Marshal(map[string]any{
		"service": "workbuddy",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain": totalRemain, "used": totalUsed, "size": totalSize,
			"accounts": len(accounts), "ok": okCount,
			"failed": len(accounts) - okCount,
		},
		"accounts": accounts,
	})
	fmt.Println(string(raw))
}

// printPretty 人类可读日报：四行汇总 + 失败明细。
func printPretty(accounts []accountResult, totalRemain, totalUsed, totalSize int64) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	var pct int64
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("📊 WorkBuddy 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
