// signin 一次性批量签到工具：读取 auth.json 中全部 codebuddy（WorkBuddy）账号，
// 自动 RefreshToken（过期时），逐个调 daily-checkin，顺手查余额。
package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

type row struct {
	file     string
	uid      string
	nick     string
	status   string // OK | ALREADY | FAIL | AUTH_INVALID | LOAD_ERR
	detail   string
	remain   int64
	hasQuota bool
}

func main() {
	authFile := "auths/auth.json"
	if len(os.Args) > 1 {
		authFile = os.Args[1]
	}
	all, err := auth.LoadFile(authFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", authFile, err)
		os.Exit(1)
	}
	// 仅处理 codebuddy（WorkBuddy）账号，与旧版 workbuddy-*.json 范围一致。
	var auths []*auth.Auth
	for _, a := range all {
		if a.ProviderOrDefault() == "codebuddy" {
			auths = append(auths, a)
		}
	}
	if len(auths) == 0 {
		fmt.Fprintf(os.Stderr, "no codebuddy account in %s\n", authFile)
		os.Exit(1)
	}
	up := upstream.New()

	var rows []row
	okN, alreadyN, failN := 0, 0, 0
	for _, a := range auths {
		r := row{file: a.FileKey}
		r.uid, r.nick = a.UID, a.Nickname

		// refresh 过期 token
		if a.NeedsRefresh(2 * 3600) {
			if err := up.RefreshToken(a); err != nil {
				if ue, ok := err.(*upstream.Error); ok && ue.Kind == upstream.ErrSessionDead {
					r.status = "AUTH_INVALID"
				} else {
					r.status = "FAIL"
				}
				r.detail = "refresh: " + short(err.Error())
				rows = append(rows, r)
				failN++
				continue
			}
			// refresh 后写回文件（权限问题已修复）；落盘失败必须暴露，否则重启回旧 token
			if err := a.SaveAtomic(); err != nil {
				log.Printf("signin %s save: %v", a.UID, err)
			}
		}

		err = up.DailyCheckin(a)
		switch {
		case err == nil:
			r.status = "OK"
			okN++
		default:
			// DailyCheckin 已签到返回 code!=0 错误
			if isAlready(err.Error()) {
				r.status = "ALREADY"
				r.detail = short(err.Error())
				alreadyN++
			} else {
				r.status = "FAIL"
				r.detail = short(err.Error())
				failN++
			}
		}
		// 顺手查余额
		if remain, qerr := up.UserResource(a); qerr == nil {
			r.remain, r.hasQuota = remain, true
		}
		rows = append(rows, r)
	}

	// 报告
	fmt.Printf("uid                                  | nick        | status       | remain | detail\n")
	fmt.Printf("-------------------------------------+-------------+--------------+--------+------------------------------\n")
	for _, r := range rows {
		remain := "-"
		if r.hasQuota {
			remain = fmt.Sprintf("%d", r.remain)
		}
		fmt.Printf("%-36s | %-11s | %-12s | %-6s | %s\n",
			trunc(r.uid, 36), trunc(r.nick, 11), r.status, remain, r.detail)
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}

// 已签判定：code 非 0 且含 "已签到"/"already"/"checkin" 等字样
func isAlready(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}
