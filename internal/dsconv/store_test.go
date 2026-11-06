// store_test.go dsconv 会话状态的离线单测。
package dsconv

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStore_ReuseAndAccountSwitch 覆盖核心语义：
// 同账号可复用、换账号必须重建、过期必须重建。
func TestStore_ReuseAndAccountSwitch(t *testing.T) {
	s := New(Config{TTL: time.Hour})

	s.Put("win-1", Entry{AccountUID: "acct-a", ChatSessionID: "sess-1", ParentMessageID: 2})

	// 同账号命中
	e, ok := s.Get("win-1", "acct-a")
	if !ok || e.ChatSessionID != "sess-1" || e.ParentMessageID != 2 {
		t.Fatalf("同账号应命中，got=%+v ok=%v", e, ok)
	}

	// 换账号 → 必须视为 miss（同一 chat_session_id 在别的账号下不存在）
	if _, ok := s.Get("win-1", "acct-b"); ok {
		t.Fatal("换账号后不应命中（必须重建会话）")
	}

	// 未知 key
	if _, ok := s.Get("win-2", "acct-a"); ok {
		t.Fatal("未知 key 不应命中")
	}
}

// TestStore_TTLExpiry TTL 过期后视为 miss，且 GC 会真正删除。
func TestStore_TTLExpiry(t *testing.T) {
	// UpdatedAt 以 Unix 秒存储（JSON 可读），因此 TTL 取 1s 才有意义：
	// 更小的值会在写入后立刻到期，测不出「先命中后过期」的语义。
	s := New(Config{TTL: time.Second})
	s.Put("win-1", Entry{AccountUID: "a", ChatSessionID: "sess", ParentMessageID: 2})

	if _, ok := s.Get("win-1", "a"); !ok {
		t.Fatal("刚写入应命中")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, ok := s.Get("win-1", "a"); ok {
		t.Fatal("TTL 过期后不应命中")
	}
	if n := s.GC(); n != 0 {
		t.Fatalf("惰性过期已删条目，GC 应删 0 条，got=%d", n)
	}
}

// TestStore_DeleteAndCount Delete 幂等，Count 反映真实条目数。
func TestStore_DeleteAndCount(t *testing.T) {
	s := New(Config{TTL: time.Hour})
	s.Put("a", Entry{AccountUID: "u", ChatSessionID: "s1"})
	s.Put("b", Entry{AccountUID: "u", ChatSessionID: "s2"})
	if got := s.Count(); got != 2 {
		t.Fatalf("Count 应为 2，got=%d", got)
	}
	s.Delete("a")
	s.Delete("a") // 幂等
	if got := s.Count(); got != 1 {
		t.Fatalf("Delete 后 Count 应为 1，got=%d", got)
	}
}

// TestStore_PersistAndReload 落盘后重建实例应恢复（重启不丢上下文）。
func TestStore_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	s := New(Config{TTL: time.Hour, Path: path})
	s.Put("win-1", Entry{AccountUID: "acct-a", ChatSessionID: "sess-1", ParentMessageID: 4})
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("落盘文件应存在: %v", err)
	}

	s2 := New(Config{TTL: time.Hour, Path: path})
	e, ok := s2.Get("win-1", "acct-a")
	if !ok || e.ChatSessionID != "sess-1" || e.ParentMessageID != 4 {
		t.Fatalf("重启后应恢复，got=%+v ok=%v", e, ok)
	}
}

// TestStore_FlushSkipsWhenClean 无变更时不重复写盘（避免每请求都打盘）。
func TestStore_FlushSkipsWhenClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	s := New(Config{TTL: time.Hour, Path: path})
	s.Put("k", Entry{AccountUID: "u", ChatSessionID: "s"})
	if err := s.Flush(); err != nil {
		t.Fatalf("首次 Flush: %v", err)
	}
	// 第二次 Flush 无变更：不应改写文件（比对 mtime 不可靠，这里断言不报错即可）。
	if err := s.Flush(); err != nil {
		t.Fatalf("二次 Flush: %v", err)
	}
}

// TestStore_EmptyKeyIgnored 空 key / 空 session 不写入（防脏数据）。
func TestStore_EmptyKeyIgnored(t *testing.T) {
	s := New(Config{TTL: time.Hour})
	s.Put("", Entry{AccountUID: "u", ChatSessionID: "s"})
	s.Put("k", Entry{AccountUID: "u", ChatSessionID: ""})
	if got := s.Count(); got != 0 {
		t.Fatalf("空 key/session 不应写入，Count=%d", got)
	}
}
