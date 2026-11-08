// Package dsconv 持久化 DeepSeek 上游会话坐标（chat_session_id + parent_message_id）。
//
// DeepSeek 上下文靠上游会话树而非 messages 数组传递，丢失坐标就会新建会话、丢上下文；
// 与 internal/session 的「会话键→账号」粘性路由互补。
// 上游会话 TTL 3 天，过期返回 biz_code=1 须重建；会话不跨账号，故 Entry 存 AccountUID 校验。
package dsconv

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry 一个客户端会话对应的上游会话坐标。
type Entry struct {
	// AccountUID 建立该上游会话的账号；换号后会话作废。
	AccountUID string `json:"account_uid"`

	ChatSessionID string `json:"chat_session_id"`
	// ParentMessageID 下一轮的 parent_message_id；0 表示首轮传 null。
	ParentMessageID int64 `json:"parent_message_id"`

	UpdatedAt int64 `json:"updated_at"`
}

// Config Store 配置。
type Config struct {
	// TTL 本地记录的有效期；<=0 取默认 24h。必须显著小于上游 3 天，否则会撞 biz_code=1。
	TTL time.Duration
	// Path 持久化文件路径；空 = 纯内存，重启丢上下文。
	Path string
}

// Store 会话状态存储（内存 + 可选磁盘持久化）。
type Store struct {
	mu      sync.RWMutex
	entries map[string]Entry
	cfg     Config
	dirty   bool
}

// New 构建 Store 并尝试从 cfg.Path 恢复。
func New(cfg Config) *Store {
	if cfg.TTL <= 0 { cfg.TTL = 24 * time.Hour }
	s := &Store{entries: make(map[string]Entry), cfg: cfg}
	s.load()
	return s
}

// Get 取会话坐标。命中但账号不符或已过期时返回 ok=false（调用方据此重建）。
func (s *Store) Get(key, accountUID string) (Entry, bool) {
	if key == "" {
		return Entry{}, false
	}
	now := time.Now().Unix()
	s.mu.RLock()
	e, found := s.entries[key]
	s.mu.RUnlock()
	if !found {
		return Entry{}, false
	}
	if s.expired(e, now) {
		s.Delete(key)
		return Entry{}, false
	}

	if accountUID != "" && e.AccountUID != accountUID {
		return Entry{}, false
	}
	return e, true
}

// Put 写入/覆盖会话坐标（并标记待落盘）。
func (s *Store) Put(key string, e Entry) {
	if key == "" || e.ChatSessionID == "" { return }
	e.UpdatedAt = time.Now().Unix()
	s.mu.Lock()
	s.entries[key] = e
	s.dirty = true
	s.mu.Unlock()
}

// Delete 删除会话坐标（会话失效/换号时调用）。
func (s *Store) Delete(key string) {
	if key == "" { return }
	s.mu.Lock()
	if _, ok := s.entries[key]; ok {
		delete(s.entries, key)
		s.dirty = true
	}
	s.mu.Unlock()
}

// Count 返回当前记录数（供 /status 观测）。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// GC 清理过期条目。
func (s *Store) GC() int {
	now := time.Now().Unix()
	s.mu.Lock()
	var dead []string
	for k, e := range s.entries {
		if s.expired(e, now) { dead = append(dead, k) }
	}
	for _, k := range dead {
		delete(s.entries, k)
	}
	if len(dead) > 0 { s.dirty = true }
	s.mu.Unlock()
	return len(dead)
}

// Flush 把内存状态原子写盘（cfg.Path 为空时无操作）。
// 仅在有变更时写，避免每次请求都打盘。
func (s *Store) Flush() error {
	if s.cfg.Path == "" { return nil }
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]Entry, len(s.entries))
	for k, v := range s.entries {
		snapshot[k] = v
	}
	s.dirty = false
	s.mu.Unlock()

	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil { return err }
	if dir := filepath.Dir(s.cfg.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil { return err }
	}
	tmp := s.cfg.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil { return err }
	return os.Rename(tmp, s.cfg.Path)
}

// load 从磁盘恢复；文件不存在或损坏时静默从空开始（会话重建即可，不影响可用性）。
func (s *Store) load() {
	if s.cfg.Path == "" { return }
	raw, err := os.ReadFile(s.cfg.Path)
	if err != nil { return }
	var m map[string]Entry
	if err := json.Unmarshal(raw, &m); err != nil { return }
	now := time.Now().Unix()
	s.mu.Lock()
	for k, v := range m {
		if s.expired(v, now) { continue }
		s.entries[k] = v
	}
	s.mu.Unlock()
}

// expired 报告条目是否超过 TTL。
// 用 Duration 比较：int64(TTL/time.Second) 在 TTL < 1s 时截断成 0，会退化成「永不过期」。
func (s *Store) expired(e Entry, now int64) bool {
	if e.UpdatedAt <= 0 { return true }
	return time.Duration(now-e.UpdatedAt)*time.Second >= s.cfg.TTL
}
