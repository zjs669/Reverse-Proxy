// Package session 会话粘性路由：同一会话尽量绑定同一账号。
//
// 快路径 RLock 命中；未命中走写锁 re-check 后分配（防同 key 并发重复分配）。
// 分配优先未绑定会话的空闲账号，其余按全池哈希；变更异步镜像到 redisstore 防重启丢粘性。
package session

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"reverse-proxy/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// Config 路由依赖；Available 由 pool.AvailableUIDs 提供，Store 可为 redisstore.Noop。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	stop    chan struct{}
}

// New 构建路由器；Store 为 nil 用 Noop，TTL/GCInterval 非正取默认（30m / 5m）兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 { cfg.TTL = 30 * time.Minute }
	if cfg.GCInterval <= 0 { cfg.GCInterval = 5 * time.Minute }
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	r.stop = make(chan struct{})
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（读操作仅此处发生）。
// 已有本地绑定保留：Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 { return }
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists { continue }
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 { log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded) }
}

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
func (r *Router) Resolve(key string) (string, bool) {
	now := time.Now()
	available := r.availableSet()


	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if available[e.uid] {
			r.touch(key, e.uid, now)
			return e.uid, true
		}

	}


	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好，避免重复分配。
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key)
	}

	uids := r.availableSlice()
	if len(uids) == 0 { return "", false }


	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] { idle = append(idle, u) }
	}
	pool2 := idle
	if len(pool2) == 0 { pool2 = uids }
	uid := pool2[hashIndex(key, len(pool2))]

	prev, existed := r.entries[key]
	r.entries[key] = entry{uid: uid, lastActive: now}
	if existed && prev.uid != uid { r.cfg.Store.DelBind(key) }
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// touch 滚动 lastActive 并异步镜像（只在快路径命中时写最后一次）。
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 用于请求成功后重绑到实际成功号，让多轮对话稳定收敛到同一账号。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" { return }
	now := time.Now()
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found { delete(r.entries, key) }
	r.mu.Unlock()
	if found { r.cfg.Store.DelBind(key) }
	return found
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL { expiredKeys = append(expiredKeys, key) }
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableSet 把 Available() 的有序列表转集合（快路径命中校验用）。
func (r *Router) availableSet() map[string]bool {
	uids := r.availableSlice()
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice 安全调用 Available（nil 函数视空池）。
func (r *Router) availableSlice() []string {
	if r.cfg.Available == nil { return nil }
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey 从请求体提取会话键（metadata 优先，snake_case 优先于 camelCase），找不到返回空串。
// 两种命名都要识别：客户端实际发 camelCase（issue #35），漏掉会轮转账号、丢失上游缓存。
func ExtractKey(body []byte) string {
	if len(body) == 0 { return "" }
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil { return "" }
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" { return v }
		if v := strOrEmpty(meta["conversationId"]); v != "" { return v }
		if v := strOrEmpty(meta["user_id"]); v != "" { return v }
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" { return v }
	return strOrEmpty(obj["conversationId"])
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
