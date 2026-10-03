package facade

import (
	"context"
	"sync"
	"time"
)

// keyedLocker 提供"按 key 串行"的能力，并带引用计数避免 map 无限增长。
//
// 用途：同一个会话的首次请求会并发触发"建项目"，
// 没有这个锁的话 10 个并发请求会建出 10 个项目，白白浪费上游配额。
type keyedLocker struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func newKeyedLocker() *keyedLocker {
	return &keyedLocker{m: make(map[string]*lockEntry, 64)}
}

// Lock 返回一个 unlock 函数。必须 defer 调用。
func (k *keyedLocker) Lock(key string) func() {
	k.mu.Lock()
	e, ok := k.m[key]
	if !ok {
		e = &lockEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()

	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// projectEntry 是缓存的项目句柄。
type projectEntry struct {
	id       string
	bucket   int // 所属项目池分桶，用于轮转
	expires  time.Time
	creating bool
}

// projectCache 缓存"会话 -> 上游项目"的映射。
//
// 为什么要缓存：POST /api/projects 是一次完整的服务端建工程操作，
// 比推理调用本身还慢。而 Prism 的项目在会话存续期内是复用的，
// 所以每个会话只需要建一次。
//
// 键必须包含 accountID：项目是账号私有资源，
// 换号之后旧项目 ID 会直接 404。
type projectCache struct {
	ttl      time.Duration
	poolSize int

	mu sync.RWMutex
	m  map[string]projectEntry

	locks *keyedLocker
	rr    int
}

func newProjectCache(ttl time.Duration, poolSize int) *projectCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if poolSize <= 0 {
		poolSize = 1
	}
	return &projectCache{
		ttl:      ttl,
		poolSize: poolSize,
		m:        make(map[string]projectEntry, 256),
		locks:    newKeyedLocker(),
	}
}

func (p *projectCache) key(accountID, conversation string) string {
	// 用 | 分隔：accountID 是 UUID 形态，不含 |，不会歧义。
	return accountID + "|" + conversation
}

// Get 取出未过期的项目 ID（bucket 用于把同一账号的并发会话摊到多个项目上，
// 避免所有会话挤在一个项目里串行化）。
func (p *projectCache) Get(accountID, conversation string, now time.Time) (string, bool) {
	p.mu.RLock()
	e, ok := p.m[p.key(accountID, conversation)]
	p.mu.RUnlock()
	if !ok || now.After(e.expires) || e.id == "" {
		return "", false
	}
	return e.id, true
}

// Put 写入缓存。
func (p *projectCache) Put(accountID, conversation, projectID string, now time.Time) {
	if projectID == "" {
		return
	}
	p.mu.Lock()
	p.m[p.key(accountID, conversation)] = projectEntry{
		id:      projectID,
		expires: now.Add(p.ttl),
	}
	p.mu.Unlock()
}

// Invalidate 主动失效（项目被上游删除时）。
func (p *projectCache) Invalidate(accountID, conversation string) {
	p.mu.Lock()
	delete(p.m, p.key(accountID, conversation))
	p.mu.Unlock()
}

// Lock 返回按"账号+会话"粒度的互斥锁。
func (p *projectCache) Lock(accountID, conversation string) func() {
	return p.locks.Lock(p.key(accountID, conversation))
}

// Size 当前缓存条目数（用于指标）。
func (p *projectCache) Size() int {
	p.mu.RLock()
	n := len(p.m)
	p.mu.RUnlock()
	return n
}

// gc 清理过期条目。
func (p *projectCache) gc(ctx context.Context) {
	t := time.NewTicker(p.ttl / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			p.mu.Lock()
			for k, e := range p.m {
				if now.After(e.expires) {
					delete(p.m, k)
				}
			}
			p.mu.Unlock()
		}
	}
}
