// Package dsconv 保存 DeepSeek 上游会话状态（chat_session_id + parent_message_id）。
//
// 为什么单独一个包：DeepSeek 的上下文不是靠 messages 数组传的，而是靠上游会话树——
// 同一 chat_session_id 下传 parent_message_id（上轮的 assistant message id）即可
// 自动带上全部前文。因此网关必须把「某个客户端窗口 → 上游会话 + 消息指针」持久化，
// 否则每次请求都会新建会话、丢失上下文。
//
// 与 internal/session 的区别：session.Router 记的是「会话键 → 账号 uid」（粘性路由），
// 这里记的是「会话键 → 上游会话坐标」。两者互补，不重叠。
//
// 生命周期约束：
//   - 上游 chat_session/create 返回 ttl_seconds=259200（3 天），过期后 completion 返回
//     biz_code=1 "invalid chat session id"，此时必须丢弃重建。
//   - 换账号必须重建：同一个 chat_session_id 在另一个账号下不存在。
//   - 因此 Entry 里同时存 AccountUID，取用时校验，不一致即视为 miss。
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
	// AccountUID 建立该上游会话的账号；换号后该会话作废（上游不共享）。
	AccountUID string `json:"account_uid"`
	// ChatSessionID 上游 /chat_session/create 返回的 UUID。
	ChatSessionID string `json:"chat_session_id"`
	// ParentMessageID 下一轮要传的 parent_message_id；0 表示还没有消息（首轮传 null）。
	ParentMessageID int64 `json:"parent_message_id"`
	// UpdatedAt 最近一次成功交互时间（Unix 秒），用于 TTL 过期判定。
	UpdatedAt int64 `json:"updated_at"`
}

// Config Store 配置。
type Config struct {
	// TTL 本地记录的有效期；<=0 取默认 24h。
	// 必须显著小于上游 3 天，否则会拿着已过期的 chat_session_id 反复撞 biz_code=1。
	TTL time.Duration
	// Path 持久化文件路径；空 = 纯内存（重启即丢，会话会自动重建但上下文丢失）。
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
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
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
	// 换号：该 chat_session_id 在新账号下不存在，必须重建。
	if accountUID != "" && e.AccountUID != accountUID {
		return Entry{}, false
	}
	return e, true
}

// Put 写入/覆盖会话坐标（并标记待落盘）。
func (s *Store) Put(key string, e Entry) {
	if key == "" || e.ChatSessionID == "" {
		return
	}
	e.UpdatedAt = time.Now().Unix()
	s.mu.Lock()
	s.entries[key] = e
	s.dirty = true
	s.mu.Unlock()
}

// Delete 删除会话坐标（会话失效/换号时调用）。
func (s *Store) Delete(key string) {
	if key == "" {
		return
	}
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
		if s.expired(e, now) {
			dead = append(dead, k)
		}
	}
	for _, k := range dead {
		delete(s.entries, k)
	}
	if len(dead) > 0 {
		s.dirty = true
	}
	s.mu.Unlock()
	return len(dead)
}

// Flush 把内存状态原子写盘（cfg.Path 为空时无操作）。
// 仅在有变更时写，避免每次请求都打盘。
func (s *Store) Flush() error {
	if s.cfg.Path == "" {
		return nil
	}
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
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.cfg.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.cfg.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfg.Path)
}

// load 从磁盘恢复（文件不存在/损坏时静默从空开始——会话丢失只会导致重建，不影响可用性）。
func (s *Store) load() {
	if s.cfg.Path == "" {
		return
	}
	raw, err := os.ReadFile(s.cfg.Path)
	if err != nil {
		return
	}
	var m map[string]Entry
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	now := time.Now().Unix()
	s.mu.Lock()
	for k, v := range m {
		if s.expired(v, now) {
			continue
		}
		s.entries[k] = v
	}
	s.mu.Unlock()
}

// expired 报告条目是否超过 TTL。
//
// 用 Duration 比较而不是 int64(TTL/time.Second)：后者在 TTL < 1s 时会截断成 0，
// 语义退化成「永不过期」（阈值比较恒为 false 直到整整 1 秒后）。
func (s *Store) expired(e Entry, now int64) bool {
	if e.UpdatedAt <= 0 {
		return true
	}
	return time.Duration(now-e.UpdatedAt)*time.Second >= s.cfg.TTL
}
