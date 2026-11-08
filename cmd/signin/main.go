// signin 一次性批量签到工具：读 auth.json 全部账号，过期则 RefreshToken，
// 逐个走统一 Checkin 分派，顺手查余额。
package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/upstream"
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
	okN, alreadyN, failN, skipN := 0, 0, 0, 0
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

// 走统一分派（upstream.Checkin）：按 provider 选实现；旧的 Client.DailyCheckin 只认 CodeBuddy。
		res, cerr := up.Checkin(a)
		if cerr != nil {
			r.status = "FAIL"
			r.detail = short(cerr.Error())
			failN++
		} else {
			switch res.State.String() {
			case "ok":
				r.status = "OK"
				r.detail = short(res.Msg)
				okN++
			case "already":
				r.status = "ALREADY"
				r.detail = short(res.Msg)
				alreadyN++
			case "unsupported", "pending":
				r.status = "SKIP"
				r.detail = short(res.Msg)
				skipN++
			default:
				r.status = "FAIL"
				r.detail = short(res.Msg)
				failN++
			}
		}
// 余额走统一 Usage（CodeBuddy→billing meter，Qoder→/api/v1/me/usage）。
		if raw, qerr := up.Usage(a); qerr == nil && raw != nil {
			if n := upstream.CreditsInt64(raw); n >= 0 {
				r.remain, r.hasQuota = n, true
			}
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
	fmt.Printf("\ntotal=%d ok=%d already=%d skip=%d fail=%d\n", len(rows), okN, alreadyN, skipN, failN)
}

// isAlready 判定「今天已签」：业务错误里含这些字样。
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
