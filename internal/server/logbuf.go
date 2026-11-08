// logbuf.go 进程日志的环形缓冲，供 Web 管理台增量拉取。
//
// 为什么需要它：网关的日志只走 log.Printf → stdout，Web 端读不到。
// 这里用 io.MultiWriter 同时写 stdout 和这个环形缓冲，Web 用自增序号做增量拉取
// （前端每 2s 带 since=<seq> 来取新行），不用 WebSocket 也不用轮询整个文件。
package server

import (
	"strings"
	"sync"
)

// LogRing 固定容量的日志环形缓冲，按自增序号对外提供增量读取。
type LogRing struct {
	mu   sync.RWMutex
	cap  int
	seq  int64    // 已写入的总行号（单调递增，从 1 开始）
	ring []string // 长度 <= cap，最后一行是 ring[len-1]
}

// NewLogRing 建一个容量 n 的环形缓冲；n<=0 时取 2000。
func NewLogRing(n int) *LogRing {
	if n <= 0 {
		n = 2000
	}
	return &LogRing{cap: n, ring: make([]string, 0, n)}
}

// Write 让 LogRing 可以直接塞进 io.MultiWriter。
// 按行切分：一次 Write 可能带多行，也可能半行（log 包通常是整行）。
func (l *LogRing) Write(p []byte) (int, error) {
	text := string(p)
	// 去掉行尾换行，按行入队；空写忽略
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		l.append(line)
	}
	return len(p), nil
}

func (l *LogRing) append(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	if len(l.ring) >= l.cap {
		// 丢最旧的一行：整体左移代价高，改用覆盖 + 环形游标更划算，
		// 但 2000 行的 copy 每行一次也才几微秒，读路径简单更值。
		copy(l.ring, l.ring[1:])
		l.ring = l.ring[:len(l.ring)-1]
	}
	l.ring = append(l.ring, line)
}

// LogLine 增量拉取返回的一行。
type LogLine struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

// Since 返回序号 > since 的所有行，以及当前最大序号。
// since<=0 表示"给我缓冲里全部"。
func (l *LogRing) Since(since int64) ([]LogLine, int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]LogLine, 0, 64)
	// ring 的最后一行序号是 l.seq；第一行的序号是 l.seq-len(ring)+1
	firstSeq := l.seq - int64(len(l.ring)) + 1
	for i, text := range l.ring {
		s := firstSeq + int64(i)
		if s > since {
			out = append(out, LogLine{Seq: s, Text: text})
		}
	}
	return out, l.seq
}

// Tail 返回最后 n 行（给初始页面加载用）。n<=0 返回全部。
func (l *LogRing) Tail(n int) []LogLine {
	l.mu.RLock()
	defer l.mu.RUnlock()
	start := 0
	if n > 0 && len(l.ring) > n {
		start = len(l.ring) - n
	}
	firstSeq := l.seq - int64(len(l.ring)) + 1
	out := make([]LogLine, 0, len(l.ring)-start)
	for i := start; i < len(l.ring); i++ {
		out = append(out, LogLine{Seq: firstSeq + int64(i), Text: l.ring[i]})
	}
	return out
}

// ─── 管理台适配 ─────────────────────────────────────────────────────────────

// LogRingSource 管理台需要的最小接口。
//
// 用接口而不是让 admin 直接依赖 *LogRing：admin 包不 import server，
// 靠 cmd/server 把这个接口注入进去，保持依赖方向单一。
type LogRingSource interface {
	Since(since int64) (lines []LogLine, max int64)
	Tail(n int) []LogLine
}

// AsLogRingSource 供 cmd/server 注入管理台用。
func (l *LogRing) AsLogRingSource() LogRingSource { return logRingAdapter{l} }

type logRingAdapter struct{ r *LogRing }

func (a logRingAdapter) Since(since int64) ([]LogLine, int64) { return a.r.Since(since) }
func (a logRingAdapter) Tail(n int) []LogLine                { return a.r.Tail(n) }
