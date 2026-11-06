// logring_adapter.go 把 server 包的 LogRing 适配成 admin.LogRingSource。
package server

// AsLogRingSource 供 cmd/server 注入管理台用。
// 用适配器而不是让 admin 直接依赖 server.LogRing，是为了保持 admin 不反向依赖 server。
func (l *LogRing) AsLogRingSource() LogRingSource { return logRingAdapter{l} }

// LogRingSource 管理台需要的最小接口。
type LogRingSource interface {
	Since(since int64) (lines []LogLine, max int64)
	Tail(n int) []LogLine
}

type logRingAdapter struct{ r *LogRing }

func (a logRingAdapter) Since(since int64) ([]LogLine, int64) { return a.r.Since(since) }
func (a logRingAdapter) Tail(n int) []LogLine                 { return a.r.Tail(n) }
