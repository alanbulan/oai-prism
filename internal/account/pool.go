package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/httpc"
)

// ErrNoAccount 表示池里没有任何可用账号。
var ErrNoAccount = errors.New("账号池为空或全部不可用")

// Pool 是账号池与调度器。
//
// 无锁读取热路径：账号切片本身用 atomic.Pointer 持有，
// 热重载时整体替换切片，正在飞的请求继续用旧切片，不会看到半更新状态。
type Pool struct {
	cfg    config.PoolConfig
	creds  config.CredsConfig
	up     config.UpstreamConfig
	log    *slog.Logger
	client *httpc.Client

	accounts atomic.Pointer[[]*Account]
	sticky   *stickyMap

	rr atomic.Uint64
}

// NewPool 按配置构建账号池。
func NewPool(cfg *config.Config, log *slog.Logger) (*Pool, error) {
	baseClient, err := httpc.New(cfg.Upstream, httpc.Options{})
	if err != nil {
		return nil, err
	}

	p := &Pool{
		cfg:    cfg.Pool,
		creds:  cfg.Creds,
		up:     cfg.Upstream,
		log:    log,
		client: baseClient,
		sticky: newStickyMap(64, cfg.Pool.StickyTTL),
	}
	if err := p.Build(cfg.Creds.Accounts); err != nil {
		return nil, err
	}
	return p, nil
}

// Client 返回共享的上游客户端（无账号专属代理时使用）。
func (p *Pool) Client() *httpc.Client { return p.client }

// Build 用给定的账号配置重建池。
func (p *Pool) Build(cfgs []config.AccountConfig) error {
	built := make([]*Account, 0, len(cfgs))
	seen := make(map[string]struct{}, len(cfgs))

	for i, ac := range cfgs {
		if !ac.IsEnabled() {
			continue
		}
		id := ac.ID
		if id == "" {
			id = fmt.Sprintf("acct-%d", i+1)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("账号 ID 重复: %s", id)
		}
		seen[id] = struct{}{}

		c := creds.FromAccountConfig(ac)
		if c == nil {
			continue
		}

		client := p.client
		// 账号级出口代理：为每个不同代理建一个客户端（连接池也随之独立）。
		if ac.Proxy != "" && ac.Proxy != p.up.HTTPProxy {
			oc, err := httpc.New(p.up, httpc.Options{Proxy: ac.Proxy})
			if err != nil {
				return fmt.Errorf("账号 %s 构造代理客户端失败: %w", id, err)
			}
			client = oc
		}

		if ac.Name == "" {
			ac.Name = id
		}
		a := newAccount(ac, client, c)
		built = append(built, a)
	}

	sort.SliceStable(built, func(i, j int) bool {
		// 高权重优先，让 least_inflight 在平票时也倾向优质账号。
		return built[i].Weight > built[j].Weight
	})

	p.accounts.Store(&built)
	if len(built) == 0 {
		// 空池是启动初期的正常状态（凭据还没放进来），降级为 Debug，
		// 免得让人误以为配置出了问题——真正的提示在 server 层给。
		p.log.Debug("账号池为空")
	} else {
		p.log.Info("账号池已构建", "count", len(built))
	}
	return nil
}

// Accounts 返回当前账号快照。
func (p *Pool) Accounts() []*Account {
	ptr := p.accounts.Load()
	if ptr == nil {
		return nil
	}
	return *ptr
}

// Size 账号数量。
func (p *Pool) Size() int {
	ptr := p.accounts.Load()
	if ptr == nil {
		return 0
	}
	return len(*ptr)
}

// Healthy 统计当前可用账号数。
func (p *Pool) Healthy(now time.Time) int {
	n := 0
	for _, a := range p.Accounts() {
		if a.Available(now) {
			n++
		}
	}
	return n
}

// Get 按 ID 取账号。
func (p *Pool) Get(id string) *Account {
	for _, a := range p.Accounts() {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// Lease 是一次账号占用，必须 Release。
type Lease struct {
	Account *Account
	pool    *Pool
	key     string
	once    sync.Once
}

// Release 归还账号额度。
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.Account.Release()
	})
}

// Acquire 领取一个账号。
//
// stickyKey 非空时优先复用之前绑定过的账号——Prism 的项目/会话是账号私有的，
// 同一对话换号会直接 404，所以粘性不是优化而是正确性要求。
func (p *Pool) Acquire(ctx context.Context, stickyKey string) (*Lease, error) {
	now := time.Now()
	all := p.Accounts()
	if len(all) == 0 {
		return nil, ErrNoAccount
	}

	if stickyKey != "" {
		if id, ok := p.sticky.Get(stickyKey, now); ok {
			for _, a := range all {
				if a.ID != id {
					continue
				}
				if a.Available(now) && a.Acquire(now) {
					// 命中即续期：粘性 TTL 必须按"最后一次使用"计，
					// 否则活跃会话满 TTL 后照样被换号（项目/会话是账号私有的）。
					p.sticky.Put(stickyKey, a.ID, now)
					return &Lease{Account: a, pool: p, key: stickyKey}, nil
				}
				// 粘性账号只是并发满了（未冷却、凭据可用）：等它腾出槽位，
				// 而不是立刻改绑到别的账号 —— 改绑会让会话的项目/沙箱全部作废。
				if a.Busy(now) {
					if l := p.waitSticky(ctx, a, stickyKey); l != nil {
						return l, nil
					}
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				break
			}
		}
	}

	if a := p.pick(all, now); a != nil {
		if stickyKey != "" {
			p.sticky.Put(stickyKey, a.ID, now)
		}
		return &Lease{Account: a, pool: p, key: stickyKey}, nil
	}

	// 全部不可用：等最早解除冷却的那个，而不是直接失败。
	if wait, target := p.earliestAvailable(all, now); target != nil {
		// 但如果冷却全是"凭据失效"造成的，等下去毫无意义 ——
		// 直接给出可操作的错误，而不是让调用方白等一个冷却周期。
		if p.allAuthFailed(all) {
			return nil, fmt.Errorf(
				"全部 %d 个账号的凭据都已失效（上游返回 401/403）。"+
					"等冷却没有意义，请更新凭据后重试", len(all))
		}

		// 等待也要有上限：一个 HTTP 请求挂几分钟等账号解冻是不可接受的，
		// 客户端早就超时了，而调用方拿不到任何有用信息。
		if max := p.cfg.MaxWait; max > 0 && wait > max {
			p.log.Warn("账号池全部冷却且等待时间超过上限",
				"wait", wait.Round(time.Second), "max", max, "account", target.ID)
			return nil, fmt.Errorf(
				"账号池全部冷却，最早需等待 %s（超过上限 %s）。请增加账号或调大 pool.max_wait",
				wait.Round(time.Second), max)
		}

		p.log.Warn("账号池全部冷却，等待",
			"wait", wait.Round(time.Millisecond), "account", target.ID)

		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}

		now = time.Now()
		for _, a := range p.Accounts() {
			if a.Available(now) && a.Acquire(now) {
				if stickyKey != "" {
					p.sticky.Put(stickyKey, a.ID, now)
				}
				return &Lease{Account: a, pool: p, key: stickyKey}, nil
			}
		}
	}

	// 冷却是时间问题，并发上限则可能是容量不足——后者值得明确报出来。
	busy := 0
	for _, a := range all {
		if a.maxConc > 0 && a.inflight.Load() >= a.maxConc {
			busy++
		}
	}
	if busy == len(all) {
		return nil, fmt.Errorf("%w: 全部 %d 个账号已达并发上限", ErrNoAccount, len(all))
	}
	return nil, ErrNoAccount
}

// waitSticky 在粘性账号满并发时短暂等待其空出槽位。
//
// 上限取 pool.max_wait（未配置时 10 秒）：等不到再按常规策略改绑，
// 门面层会据 BoundAccountID 把续接状态降级为全量上下文，不会拿着
// 旧账号的项目 ID 去打新账号。
func (p *Pool) waitSticky(ctx context.Context, a *Account, stickyKey string) *Lease {
	limit := p.cfg.MaxWait
	if limit <= 0 {
		limit = 10 * time.Second
	}
	deadline := time.Now().Add(limit)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		now := time.Now()
		if a.Available(now) && a.Acquire(now) {
			p.sticky.Put(stickyKey, a.ID, now)
			return &Lease{Account: a, pool: p, key: stickyKey}
		}
		if !a.Busy(now) {
			return nil // 进入冷却或凭据失效：不值得再等
		}
	}
	return nil
}

// pick 依据策略选一个可用账号。
func (p *Pool) pick(all []*Account, now time.Time) *Account {
	switch p.cfg.Strategy {
	case "round_robin":
		n := len(all)
		start := int(p.rr.Add(1)-1) % n
		for i := 0; i < n; i++ {
			a := all[(start+i)%n]
			if a.Available(now) && a.Acquire(now) {
				return a
			}
		}
	case "random":
		n := len(all)
		start := rand.IntN(n)
		for i := 0; i < n; i++ {
			a := all[(start+i)%n]
			if a.Available(now) && a.Acquire(now) {
				return a
			}
		}
	case "weighted":
		totalW := 0
		for _, a := range all {
			if a.Available(now) {
				totalW += a.Weight
			}
		}
		if totalW > 0 {
			target := rand.IntN(totalW)
			acc := 0
			for _, a := range all {
				if !a.Available(now) {
					continue
				}
				acc += a.Weight
				if acc > target && a.Acquire(now) {
					return a
				}
			}
		}
	case "sticky_hash", "least_inflight":
		fallthrough
	default:
		// least_inflight：在途数最少者优先，平票取权重高者。
		// 这是代理场景的默认最优解——它天然把慢请求摊开，
		// 避免 round_robin 把新请求继续压在已经拥堵的账号上。
		var best *Account
		var bestScore float64
		for _, a := range all {
			if !a.Available(now) {
				continue
			}
			score := float64(a.inflight.Load())
			if a.Weight > 0 {
				score /= float64(a.Weight)
			}
			if best == nil || score < bestScore {
				best, bestScore = a, score
			}
		}
		if best != nil && best.Acquire(now) {
			return best
		}
	}
	return nil
}

// earliestAvailable 找到最快会解除冷却的账号。
func (p *Pool) earliestAvailable(all []*Account, now time.Time) (time.Duration, *Account) {
	var best time.Duration
	var target *Account
	for _, a := range all {
		if !a.cred.Load().Usable() {
			continue
		}
		d := a.CooldownRemaining(now)
		if d <= 0 {
			continue
		}
		if target == nil || d < best {
			best, target = d, a
		}
	}
	if target == nil {
		return 0, nil
	}
	// 加 20ms 抖动，避免所有等待者同时醒来抢同一个账号。
	return best + 20*time.Millisecond, target
}

// allAuthFailed 判断池里所有账号是否都处于"凭据失效"状态。
func (p *Pool) allAuthFailed(all []*Account) bool {
	if len(all) == 0 {
		return false
	}
	for _, a := range all {
		if !a.AuthFailed() {
			return false
		}
	}
	return true
}

// Rebind 把 stickyKey 重新绑定到指定账号（用于账号失效后的迁移）。
func (p *Pool) Rebind(key string, a *Account) {
	if key == "" || a == nil {
		return
	}
	p.sticky.Put(key, a.ID, time.Now())
}

// Rebucket 清除粘性绑定。
func (p *Pool) Unbind(key string) {
	p.sticky.Delete(key)
}

// MarkResult 根据请求结果更新账号健康状态。
//
// 这是"自愈"的关键闭环：401 -> 换号并尝试刷新；429 -> 冷却；
// 5xx -> 记一次失败但不清零成功率。
func (p *Pool) MarkResult(a *Account, err error, retryAfter time.Duration) {
	now := time.Now()
	if err == nil {
		a.MarkSuccess()
		return
	}
	switch {
	case creds.IsAuthError(err):
		a.MarkAuthFailed()
		d := a.MarkFailure(now, p.cfg.Cooldown, p.cfg.CooldownBackoff, p.cfg.MaxCooldown)
		p.log.Warn("账号认证失效，进入冷却", "account", a.ID, "cooldown", d.Round(time.Second), "err", err)
	case creds.IsRateLimited(err):
		d := retryAfter
		if d <= 0 {
			d = a.MarkFailure(now, p.cfg.Cooldown, p.cfg.CooldownBackoff, p.cfg.MaxCooldown)
		} else {
			a.Cooldown(now, d)
		}
		p.log.Warn("账号被限流，进入冷却", "account", a.ID, "cooldown", d.Round(time.Second))
	case httpc.IsNetworkError(err):
		a.failures.Add(1)
	default:
		a.failures.Add(1)
	}
}

// ---------------------------- 后台任务 ----------------------------

// StartBackground 启动凭据刷新与健康巡检。返回的 cancel 用于停止。
func (p *Pool) StartBackground(ctx context.Context) {
	if p.creds.AutoRefresh {
		interval := p.creds.RefreshInterval
		if interval <= 0 {
			interval = 10 * time.Minute
		}
		go p.refreshLoop(ctx, interval)
	}
	if p.cfg.HealthCheck {
		interval := p.cfg.HealthCheckInterval
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		go p.healthLoop(ctx, interval)
	}
	go p.sticky.janitor(ctx)
}

func (p *Pool) refreshLoop(ctx context.Context, interval time.Duration) {
	r, err := creds.NewRefresher(p.creds, p.up, p.client)
	if err != nil {
		p.log.Error("初始化刷新器失败", "err", err)
		return
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	// 启动时先跑一轮，处理"进程重启后凭据已过期"的情况。
	p.refreshAll(ctx, r)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshAll(ctx, r)
		}
	}
}

func (p *Pool) refreshAll(ctx context.Context, r *creds.Refresher) {
	skew := p.creds.RefreshSkew
	if skew <= 0 {
		skew = 5 * time.Minute
	}
	now := time.Now()

	for _, a := range p.Accounts() {
		c := a.Credential()
		if c == nil {
			continue
		}
		// 只刷新"快过期"或"已经挂了"的账号，避免无谓的刷新风暴。
		need := c.NeedsRefresh(now, skew) || c.Expired(now)
		if !need {
			continue
		}
		if !c.CanRefresh() {
			p.log.Debug("账号无法自愈，跳过刷新", "account", a.ID, "source", c.Source)
			continue
		}
		if _, err := p.RefreshAccount(ctx, r, a); err != nil {
			p.log.Warn("账号刷新失败", "account", a.ID, "err", err)
		}
	}
}

// RefreshAccount 刷新单个账号，并保证同一账号同一时刻只有一次刷新在飞。
//
// 这里用 Mutex + 条件变量而不是 x/sync/singleflight：
// 我们需要"等待者拿到刷新后的新凭据"这一语义，singleflight 只能共享结果，
// 且会引入一个额外依赖。
func (p *Pool) RefreshAccount(ctx context.Context, r *creds.Refresher, a *Account) (*creds.Credential, error) {
	if !a.refreshing.CompareAndSwap(false, true) {
		// 已有刷新在飞，等它结束然后直接读结果。
		a.refreshMu.Lock()
		for a.refreshing.Load() {
			a.refreshCond.Wait()
		}
		a.refreshMu.Unlock()
		if c := a.Credential(); c != nil && c.Usable() {
			return c, nil
		}
		return nil, fmt.Errorf("账号 %s 刷新失败", a.ID)
	}

	var (
		next *creds.Credential
		err  error
	)
	defer func() {
		a.refreshMu.Lock()
		a.refreshing.Store(false)
		a.refreshCond.Broadcast()
		a.refreshMu.Unlock()
	}()

	cur := a.Credential()
	next, err = r.Refresh(ctx, cur)
	if err != nil {
		return nil, err
	}
	a.StoreCredential(next)
	a.MarkSuccess()
	p.log.Info("账号凭据已刷新",
		"account", a.ID,
		"source", next.Source,
		"expires_at", next.ExpiresAt.UTC().Format(time.RFC3339),
		"plan", next.Plan)
	return next, nil
}

func (p *Pool) healthLoop(ctx context.Context, interval time.Duration) {
	path := p.cfg.HealthCheckPath
	if path == "" {
		path = "/api/auth/session"
	}
	timeout := p.cfg.HealthCheckTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, a := range p.Accounts() {
				c := a.Credential()
				// 只探那些"没有过期信息"或"已经过期"的账号——
				// 有明确未来 exp 的账号交给刷新循环处理，探了也是浪费配额。
				if c == nil {
					continue
				}
				now := time.Now()
				if !c.ExpiresAt.IsZero() && !c.Expired(now) {
					continue
				}
				func() {
					cctx, cancel := context.WithTimeout(ctx, timeout)
					defer cancel()
					_, err := p.probe(cctx, a, path)
					if err != nil {
						p.log.Debug("健康检查失败", "account", a.ID, "err", err)
						p.MarkResult(a, &creds.APIError{Op: "health", Status: 401, Body: err.Error()}, 0)
						return
					}
					a.MarkSuccess()
				}()
			}
		}
	}
}

// probe 发送一次轻量请求验证账号可用性。
//
// 直接复用池里的 refresh 路径的会话接口，而不是发一个真实推理请求——
// 后者会消耗额度且可能污染会话历史。
func (p *Pool) probe(ctx context.Context, a *Account, path string) (int, error) {
	c := a.Credential()
	if c == nil || !c.Usable() {
		return 0, fmt.Errorf("凭据不可用")
	}

	hdr := map[string]string{
		"Accept":     "application/json",
		"User-Agent": p.up.UserAgent,
		"Referer":    p.up.Referer,
		"Origin":     p.up.Origin,
	}
	if ck := c.EffectiveCookie(); ck != "" {
		hdr["Cookie"] = ck
	}
	if c.AccessToken != "" {
		hdr["Authorization"] = "Bearer " + c.AccessToken
	}
	for k, v := range c.Headers {
		hdr[k] = v
	}

	req, err := a.Client.NewRequest(ctx, "GET", path, nil, hdr)
	if err != nil {
		return 0, err
	}
	resp, err := a.Client.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer drainClose(resp)
	if resp.StatusCode != 200 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return 200, nil
}

// ---------------------------- 粘性表 ----------------------------

type stickyEntry struct {
	accountID string
	expires   time.Time
}

type stickyShard struct {
	mu sync.RWMutex
	m  map[string]stickyEntry
}

// stickyMap 是分片 + TTL 的会话粘性表。
//
// 分片的目的很直接：单个 RWMutex 在高 QPS 下会成为全局瓶颈，
// 而这张表在每次请求上都要读一次，属于典型热路径。
type stickyMap struct {
	shards [64]stickyShard
	mask   uint64
	ttl    time.Duration
}

func newStickyMap(_ int, ttl time.Duration) *stickyMap {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	s := &stickyMap{mask: 63, ttl: ttl}
	for i := range s.shards {
		s.shards[i].m = make(map[string]stickyEntry, 64)
	}
	return s
}

func (s *stickyMap) idx(key string) uint32 {
	// FNV-1a，内联实现避免额外函数调用。
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	var h uint64 = offset64
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= prime64
	}
	return uint32(h & s.mask)
}

func (s *stickyMap) Get(key string, now time.Time) (string, bool) {
	sh := &s.shards[s.idx(key)]
	sh.mu.RLock()
	e, ok := sh.m[key]
	sh.mu.RUnlock()
	if !ok {
		return "", false
	}
	if now.After(e.expires) {
		sh.mu.Lock()
		delete(sh.m, key)
		sh.mu.Unlock()
		return "", false
	}
	return e.accountID, true
}

func (s *stickyMap) Put(key, accountID string, now time.Time) {
	sh := &s.shards[s.idx(key)]
	sh.mu.Lock()
	sh.m[key] = stickyEntry{accountID: accountID, expires: now.Add(s.ttl)}
	sh.mu.Unlock()
}

func (s *stickyMap) Delete(key string) {
	sh := &s.shards[s.idx(key)]
	sh.mu.Lock()
	delete(sh.m, key)
	sh.mu.Unlock()
}

// janitor 周期清理过期条目，防止长期运行内存泄漏。
func (s *stickyMap) janitor(ctx context.Context) {
	t := time.NewTicker(s.ttl / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			for i := range s.shards {
				sh := &s.shards[i]
				sh.mu.Lock()
				for k, e := range sh.m {
					if now.After(e.expires) {
						delete(sh.m, k)
					}
				}
				sh.mu.Unlock()
			}
		}
	}
}

// drainClose 排空并关闭响应体。
//
// 必须排空再关闭，否则这条连接不会被 net/http 放回空闲池，
// 在高 QPS 下表现为连接数持续爬升、TIME_WAIT 溢出。
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
