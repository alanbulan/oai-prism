package facade

import (
	"context"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// sandboxCache 缓存沙箱，以及"这个沙箱已经为哪些项目完成过工作区同步"。
//
// 两层缓存的粒度**刻意不同**：
//
//	沙箱本身按 (账号) 缓存 —— 一个容器能服务该账号下的多个项目；
//	工作区同步按 (账号, 项目) 缓存 —— 资源令牌绑定单个项目
//	（project_uuid 编码在 JWT 里）且只有 1 小时有效期。
//
// 把同步状态也缓存起来是性能关键：整套同步要 4 次往返，
// 而沙箱与文档都还是热的，重复做纯属浪费。
type sandboxCache struct {
	ttl   time.Duration
	mu    sync.RWMutex
	items map[string]*sandboxEntry
	locks *keyedLocker
}

type sandboxEntry struct {
	sb      *prism.Sandbox
	expires time.Time

	// projects: projectID -> 该次同步所用资源令牌的过期时刻。
	//
	// 拿令牌的过期时间当缓存失效点，而不是自己拍一个 TTL：
	// 令牌过期后沙箱就读不到项目资源了，再发请求必然失败，
	// 与其等失败再重试，不如到点就重新同步一遍。
	projects map[string]time.Time

	// filesProject / filesAt：这个沙箱的工作区里落的是哪个项目、哪个时刻的文件。
	// 沙箱只在第一次同步项目时把文件树落成工作区文件，之后在文档里新增的文件、
	// 以及再同步的别的项目的文件，都不会出现在工作区（2026-10-04 实测）。
	filesProject string
	filesAt      time.Time
}

func newSandboxCache(ttl time.Duration) *sandboxCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &sandboxCache{
		ttl:   ttl,
		items: make(map[string]*sandboxEntry, 8),
		locks: newKeyedLocker(),
	}
}

// Get 取未过期的沙箱。
func (c *sandboxCache) Get(accountID string) *prism.Sandbox {
	c.mu.RLock()
	e, ok := c.items[accountID]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expires) || !e.sb.Usable() {
		return nil
	}
	return e.sb
}

// Put 写入（或更新）沙箱。
//
// 若该账号已有条目且**是同一个沙箱令牌**，保留其 projects 同步记录 ——
// 并发申请撞车时不该把已经做好的同步成果丢掉。
// 换成新沙箱要清记录的话走 Invalidate。
func (c *sandboxCache) Put(accountID string, sb *prism.Sandbox) {
	if !sb.Usable() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := &sandboxEntry{sb: sb, expires: time.Now().Add(c.ttl), projects: map[string]time.Time{}}
	if old, ok := c.items[accountID]; ok && old.sb != nil && old.sb.Token == sb.Token {
		e.projects, e.filesProject, e.filesAt = old.projects, old.filesProject, old.filesAt
	}
	c.items[accountID] = e
}

// HoldsFiles 报告当前沙箱的工作区能否看到项目 projectID 在 since 之前登记的文件：
// 还没同步过任何项目的新沙箱算能（第一次同步就会落下它们）。
func (c *sandboxCache) HoldsFiles(accountID, projectID string, since time.Time) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[accountID]
	if !ok || e.filesProject == "" {
		return true
	}
	return e.filesProject == projectID && !e.filesAt.Before(since)
}

// Synced 报告该沙箱是否已为该项目完成工作区同步且令牌仍有效。
func (c *sandboxCache) Synced(accountID, projectID string) bool {
	if projectID == "" {
		return false
	}
	// projects 由 MarkSynced / InvalidateProject 在写锁下修改，读也必须在锁内：
	// 锁外读 map 与并发写撞上会直接 fatal（concurrent map read and map write）。
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[accountID]
	if !ok {
		return false
	}
	until, ok := e.projects[projectID]
	return ok && time.Now().Before(until)
}

// MarkSynced 记录一次成功的工作区同步。
func (c *sandboxCache) MarkSynced(accountID, projectID string, until time.Time) {
	if projectID == "" || until.IsZero() {
		return
	}
	c.mu.Lock()
	if e, ok := c.items[accountID]; ok {
		if e.projects == nil {
			e.projects = make(map[string]time.Time, 2)
		}
		e.projects[projectID] = until
		if e.filesProject == "" {
			e.filesProject, e.filesAt = projectID, time.Now()
		}
	}
	c.mu.Unlock()
}

// Invalidate 丢弃该账号的整个缓存（沙箱容器被回收时调用）。
func (c *sandboxCache) Invalidate(accountID string) {
	c.mu.Lock()
	delete(c.items, accountID)
	c.mu.Unlock()
}

// InvalidateIf 仅当缓存里仍是 sb 这个沙箱时才失效。
//
// 沙箱按账号共享：会话 A 判定容器坏了的同时，会话 B 可能已经换上了新容器，
// 无条件 Invalidate 会把 B 的新容器也踢掉，引发连环重建。
func (c *sandboxCache) InvalidateIf(accountID string, sb *prism.Sandbox) {
	c.mu.Lock()
	if e, ok := c.items[accountID]; ok && (sb == nil || e.sb == sb || (e.sb != nil && e.sb.Token == sb.Token)) {
		delete(c.items, accountID)
	}
	c.mu.Unlock()
}

// InvalidateProject 只让某个项目的同步记录失效，保留沙箱本身。
//
// 用在"同步失败但沙箱还在"的场景：重试时只要重做同步，
// 不必浪费一次容器分配。
func (c *sandboxCache) InvalidateProject(accountID, projectID string) {
	c.mu.Lock()
	if e, ok := c.items[accountID]; ok {
		delete(e.projects, projectID)
	}
	c.mu.Unlock()
}

// Lock 返回按账号粒度的互斥锁，保证同一账号只有一个在飞的沙箱申请。
func (c *sandboxCache) Lock(accountID string) func() {
	return c.locks.Lock(accountID)
}

// LockProject 返回按 (账号, 项目) 粒度的互斥锁。
//
// 保证同一个项目只有一个在飞的同步流程：并发的相同请求应当**等**
// 前一个同步完成，而不是各自去申请一份资源令牌 ——
// 后者会同时创建多个沙箱会话，白白消耗额度。
func (c *sandboxCache) LockProject(accountID, projectID string) func() {
	return c.locks.Lock(accountID + "\x00" + projectID)
}

// Size 当前缓存的沙箱数（供运维端点展示）。
func (c *sandboxCache) Size() int {
	c.mu.RLock()
	n := len(c.items)
	c.mu.RUnlock()
	return n
}

// gc 周期清理过期条目。
func (c *sandboxCache) gc(ctx context.Context) {
	t := time.NewTicker(c.ttl / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			c.mu.Lock()
			for k, e := range c.items {
				if now.After(e.expires) {
					delete(c.items, k)
					continue
				}
				// 顺带清掉过期的项目同步记录，避免 projects 无限增长。
				for pid, until := range e.projects {
					if now.After(until) {
						delete(e.projects, pid)
					}
				}
			}
			c.mu.Unlock()
		}
	}
}
