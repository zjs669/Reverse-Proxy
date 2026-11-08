// Package logfmt 统一网关日志的 uid 截断（前 8 位，防越界）与模块前缀约定。
package logfmt

// UID8 返回 uid 的前 8 位；空 uid 返回 "-"。
func UID8(uid string) string {
	if uid == "" { return "-" }
	if len(uid) > 8 { return uid[:8] }
	return uid
}
