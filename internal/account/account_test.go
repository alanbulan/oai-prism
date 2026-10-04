package account

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"golang.org/x/time/rate"
)

func nopLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestParseAccounts_LooseFieldNames 验证凭据文件的字段名容错。
//
// 这条容错是刻意的：凭据文件是用户手抄的，
// 让人对着文档数下划线是糟糕的体验，也容易抄错。
func TestParseAccounts_LooseFieldNames(t *testing.T) {
	raw := []byte(`{
	  "accounts": [
	    {
	      "id": "a1",
	      "accessToken": "tok-camel",
	      "session_token": "sess-snake",
	      "max_concurrency": 4
	    },
	    {
	      "id": "a2",
	      "Refresh-Token": "rt-kebab",
	      "email": "x@y.z"
	    }
	  ]
	}`)

	list, err := ParseAccounts(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("账号数 = %d, want 2", len(list))
	}
	if list[0].AccessToken != "tok-camel" {
		t.Errorf("camelCase 未识别: %q", list[0].AccessToken)
	}
	if list[0].SessionToken != "sess-snake" {
		t.Errorf("snake_case 未识别: %q", list[0].SessionToken)
	}
	if list[0].MaxConcurrency != 4 {
		t.Errorf("数值字段未识别: %d", list[0].MaxConcurrency)
	}
	if list[1].RefreshToken != "rt-kebab" {
		t.Errorf("kebab-case 未识别: %q", list[1].RefreshToken)
	}
	if list[1].Email != "x@y.z" {
		t.Errorf("email 未识别: %q", list[1].Email)
	}
}

func TestParseAccounts_BareArray(t *testing.T) {
	list, err := ParseAccounts([]byte(`[{"id":"x","cookies":"c=1"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Cookies != "c=1" {
		t.Fatalf("裸数组解析失败: %+v", list)
	}
}

// accounts.json 两种写法都要认：专用字段 oauth_client_id，以及旧版塞在 headers 里的。
func TestParseAccounts_OAuthClientID(t *testing.T) {
	list, err := ParseAccounts([]byte(`[
	  {"id":"new","oauthClientId":"app_field"},
	  {"id":"old","headers":{"oauth_client_id":"app_legacy"}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := list[0].EffectiveOAuthClientID(); got != "app_field" {
		t.Errorf("专用字段未识别: %q", got)
	}
	if got := list[1].EffectiveOAuthClientID(); got != "app_legacy" {
		t.Errorf("旧版 headers 写法未兜底: %q", got)
	}
}

func TestParseAccounts_Empty(t *testing.T) {
	for _, in := range []string{"", "   ", "null"} {
		list, err := ParseAccounts([]byte(in))
		if err != nil {
			t.Errorf("ParseAccounts(%q) 不应报错: %v", in, err)
		}
		if len(list) != 0 {
			t.Errorf("ParseAccounts(%q) 应返回空列表", in)
		}
	}
}

func TestParseAccounts_BadJSON(t *testing.T) {
	if _, err := ParseAccounts([]byte(`{"accounts": [}`)); err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
}

func TestStore_LoadMissingFileIsOK(t *testing.T) {
	// 关键行为：凭据文件不存在时不能报错——
	// 服务必须能先起来，等用户把凭据放进去。
	s := NewStore(filepath.Join(t.TempDir(), "nope.json"), nopLog())
	list, err := s.Load()
	if err != nil {
		t.Fatalf("文件不存在时不应报错: %v", err)
	}
	if list != nil {
		t.Fatal("应返回 nil")
	}
	if s.Exists() {
		t.Fatal("Exists 应为 false")
	}
}

func TestStore_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets", "accounts.json")
	s := NewStore(path, nopLog())

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	in := []config.AccountConfig{{
		ID:          "main",
		Name:        "主账号",
		Cookies:     "__Secure-next-auth.session-token=abc",
		AccessToken: "tok",
		ExpiresAt:   &exp,
	}}
	if err := s.Persist(in); err != nil {
		t.Fatal(err)
	}

	// 权限必须是 0600：文件里有能直接登录账号的凭据。
	// Windows 上 POSIX 权限位基本无效（只看只读位），因此只在类 Unix 上断言。
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("凭据文件权限过宽: %v", fi.Mode().Perm())
		}
	}

	out, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "main" || out[0].AccessToken != "tok" {
		t.Fatalf("回读不一致: %+v", out)
	}

	// 自己写完不应被 Watcher 认为是外部变更。
	if s.Changed() {
		t.Fatal("Persist 之后不应立即判定为已变更")
	}
}

func TestStore_DetectsExternalChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	s := NewStore(path, nopLog())

	if err := os.WriteFile(path, []byte(`[{"id":"a","access_token":"t1"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.Changed() {
		t.Fatal("刚加载完不应判定为变更")
	}

	// 改内容 + 改 mtime
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`[{"id":"a","access_token":"t2"},{"id":"b","access_token":"t3"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 文件系统时间戳精度可能不足，显式推一下 mtime。
	if err := os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !s.Changed() {
		t.Fatal("外部修改后应判定为变更")
	}
}

// ---------------------------- 账号池 ----------------------------

func testPool(t *testing.T, strategy string, accounts ...config.AccountConfig) *Pool {
	t.Helper()
	cfg := config.Default()
	cfg.Pool.Strategy = strategy
	cfg.Pool.Cooldown = 50 * time.Millisecond
	cfg.Pool.CooldownBackoff = 1
	cfg.Creds.Accounts = accounts
	p, err := NewPool(cfg, nopLog())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPool_AcquireRelease(t *testing.T) {
	p := testPool(t, "least_inflight",
		config.AccountConfig{ID: "a", AccessToken: "t1"},
		config.AccountConfig{ID: "b", AccessToken: "t2"},
	)
	if p.Size() != 2 {
		t.Fatalf("账号数 = %d", p.Size())
	}

	lease, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if lease.Account.Inflight() != 1 {
		t.Fatalf("在途数应为 1，得到 %d", lease.Account.Inflight())
	}
	lease.Release()
	if lease.Account.Inflight() != 0 {
		t.Fatalf("释放后在途数应为 0，得到 %d", lease.Account.Inflight())
	}
	// 重复 Release 必须安全（defer + 显式调用是常见写法）。
	lease.Release()
}

func TestPool_LeastInflightSpread(t *testing.T) {
	p := testPool(t, "least_inflight",
		config.AccountConfig{ID: "a", AccessToken: "t1"},
		config.AccountConfig{ID: "b", AccessToken: "t2"},
	)

	// 占住 a，后续请求应当全部落到 b。
	first, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	for i := 0; i < 5; i++ {
		l, err := p.Acquire(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if l.Account.ID == first.Account.ID {
			t.Fatalf("least_inflight 未避开在途账号: 都是 %s", l.Account.ID)
		}
		l.Release()
	}
}

func TestPool_MaxConcurrency(t *testing.T) {
	p := testPool(t, "least_inflight",
		config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 1},
	)

	l1, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()

	// 唯一账号已满，应当报错而不是超发。
	if _, err := p.Acquire(context.Background(), "other"); err == nil {
		t.Fatal("超过并发上限时应当报错")
	}
}

func TestPool_Sticky(t *testing.T) {
	p := testPool(t, "round_robin",
		config.AccountConfig{ID: "a", AccessToken: "t1"},
		config.AccountConfig{ID: "b", AccessToken: "t2"},
	)

	l1, err := p.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	owner := l1.Account.ID
	l1.Release()

	// 粘性：同一 key 必须一直命中同一账号，
	// 否则 Prism 的项目/会话会 404。
	for i := 0; i < 10; i++ {
		l, err := p.Acquire(context.Background(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		if l.Account.ID != owner {
			t.Fatalf("粘性失效：%s != %s", l.Account.ID, owner)
		}
		l.Release()
	}
}

func TestPool_MarkFailureCoolDownAndRecover(t *testing.T) {
	p := testPool(t, "least_inflight",
		config.AccountConfig{ID: "a", AccessToken: "t1"},
		config.AccountConfig{ID: "b", AccessToken: "t2"},
	)

	// 让 a 认证失效。
	a := p.Get("a")
	p.MarkResult(a, &creds.APIError{Op: "x", Status: 401}, 0)

	if a.Available(time.Now()) {
		t.Fatal("认证失败后账号应进入冷却")
	}

	// 冷却期内所有请求应当落到 b。
	for i := 0; i < 3; i++ {
		l, err := p.Acquire(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if l.Account.ID == "a" {
			t.Fatal("冷却中的账号不应被调度")
		}
		l.Release()
	}

	// 冷却结束后恢复。
	time.Sleep(80 * time.Millisecond)
	if !a.Available(time.Now()) {
		t.Fatal("冷却结束后账号应恢复可用")
	}
}

func TestPool_AllCoolingWaits(t *testing.T) {
	p := testPool(t, "least_inflight", config.AccountConfig{ID: "a", AccessToken: "t1"})
	a := p.Get("a")
	a.Cooldown(time.Now(), 60*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 全池冷却时应当等待而不是立刻失败——
	// 对调用方而言，"等 60ms"远好于"直接报错"。
	l, err := p.Acquire(ctx, "")
	if err != nil {
		t.Fatalf("应当等待后成功: %v", err)
	}
	l.Release()
}

func TestPool_EmptyReturnsError(t *testing.T) {
	p := testPool(t, "least_inflight")
	if _, err := p.Acquire(context.Background(), ""); err == nil {
		t.Fatal("空池应当报错")
	}
}

func TestPool_DisabledAccountSkipped(t *testing.T) {
	no := false
	p := testPool(t, "least_inflight",
		config.AccountConfig{ID: "a", AccessToken: "t1", Enabled: &no},
		config.AccountConfig{ID: "b", AccessToken: "t2"},
	)
	if p.Size() != 1 {
		t.Fatalf("被禁用的账号不应进池，size=%d", p.Size())
	}
	if p.Get("a") != nil {
		t.Fatal("被禁用的账号不应可查")
	}
}

func TestPool_DuplicateIDRejected(t *testing.T) {
	cfg := config.Default()
	cfg.Creds.Accounts = []config.AccountConfig{
		{ID: "dup", AccessToken: "t"},
		{ID: "dup", AccessToken: "t"},
	}
	if _, err := NewPool(cfg, nopLog()); err == nil {
		t.Fatal("重复账号 ID 应当报错")
	}
}

func TestPool_ReloadKeepsServing(t *testing.T) {
	p := testPool(t, "least_inflight", config.AccountConfig{ID: "a", AccessToken: "t1"})

	// 持有一个 lease 的同时重建池：旧 lease 必须仍能安全释放，
	// 新请求则用新池。热重载不能打断在途请求。
	l, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Build([]config.AccountConfig{
		{ID: "a", AccessToken: "t1"},
		{ID: "c", AccessToken: "t3"},
	}); err != nil {
		t.Fatal(err)
	}
	if p.Size() != 2 {
		t.Fatalf("重建后 size=%d", p.Size())
	}

	l.Release() // 不应 panic
	l2, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

func TestPool_ConcurrentAcquire(t *testing.T) {
	p := testPool(t, "least_inflight",
		config.AccountConfig{ID: "a", AccessToken: "t1", MaxConcurrency: 16},
		config.AccountConfig{ID: "b", AccessToken: "t2", MaxConcurrency: 16},
	)

	var wg sync.WaitGroup
	var maxSeen int64
	var mu sync.Mutex
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := p.Acquire(context.Background(), "")
			if err != nil {
				return
			}
			n := l.Account.Inflight()
			mu.Lock()
			if n > maxSeen {
				maxSeen = n
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			l.Release()
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxSeen > 16 {
		t.Fatalf("并发上限被突破: %d > 16", maxSeen)
	}
	// 全部释放后应当归零。
	for _, a := range p.Accounts() {
		if a.Inflight() != 0 {
			t.Fatalf("账号 %s 泄漏在途计数: %d", a.ID, a.Inflight())
		}
	}
}

func TestTokenBucket(t *testing.T) {
	b := rate.NewLimiter(rate.Limit(10), 2) // 10/s，突发 2
	now := time.Now()

	if !b.AllowN(now, 1) || !b.AllowN(now, 1) {
		t.Fatal("突发额度应当可用")
	}
	if b.AllowN(now, 1) {
		t.Fatal("突发用尽后应被拒绝")
	}
	// 过 200ms 应当补充约 2 个令牌。
	if !b.AllowN(now.Add(200*time.Millisecond), 1) {
		t.Fatal("令牌应随时间补充")
	}
}

func TestStickyMap_TTLAndCleanup(t *testing.T) {
	s := newStickyMap(64, 50*time.Millisecond)
	now := time.Now()
	s.Put("k", "acct", now)

	if v, ok := s.Get("k", now); !ok || v != "acct" {
		t.Fatal("未命中刚写入的粘性")
	}
	if _, ok := s.Get("k", now.Add(time.Second)); ok {
		t.Fatal("过期后不应命中")
	}

	s.Put("k2", "a2", now)
	s.Delete("k2")
	if _, ok := s.Get("k2", now); ok {
		t.Fatal("删除后不应命中")
	}
}

func TestStickyMap_ShardDistribution(t *testing.T) {
	s := newStickyMap(64, time.Minute)
	seen := map[uint32]int{}
	for i := 0; i < 1000; i++ {
		seen[s.idx(string(rune('a'+i%26))+string(rune(i)))] = 1
	}
	if len(seen) < 20 {
		t.Fatalf("分片分布过于集中，只用到 %d 个分片", len(seen))
	}
}

// TestPool_AuthFailedFailsFast 是回归测试。
//
// 背景：配错凭据时账号会进入冷却。如果调度器傻等冷却结束，
// 每个请求都会挂满一整个冷却周期（默认 60s）才失败 ——
// 客户端早就超时了，调用方还拿不到"是凭据的问题"这条关键信息。
// 凭据失效必须快速失败，限流才值得等。
func TestPool_AuthFailedFailsFast(t *testing.T) {
	p := testPool(t, "least_inflight", config.AccountConfig{ID: "a", AccessToken: "t1"})
	a := p.Get("a")

	// 标记为凭据失效（401），冷却 60 秒。
	a.MarkAuthFailed()
	a.MarkFailure(time.Now(), 60*time.Second, 1, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := p.Acquire(ctx, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("凭据全失效时应当直接报错，而不是等冷却")
	}
	if elapsed > time.Second {
		t.Fatalf("应当快速失败，实际等了 %v", elapsed)
	}
	if !strings.Contains(err.Error(), "凭据") {
		t.Errorf("错误信息应指出是凭据问题: %v", err)
	}
}

// TestPool_MaxWaitCapsBlocking 验证等待上限。
func TestPool_MaxWaitCapsBlocking(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.Strategy = "least_inflight"
	cfg.Pool.MaxWait = 100 * time.Millisecond
	cfg.Creds.Accounts = []config.AccountConfig{{ID: "a", AccessToken: "t1"}}
	p, err := NewPool(cfg, nopLog())
	if err != nil {
		t.Fatal(err)
	}

	// 限流型冷却（不是凭据失效），冷却 30 秒。
	a := p.Get("a")
	a.Cooldown(time.Now(), 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err = p.Acquire(ctx, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("等待超过上限时应当报错")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("等待上限未生效，实际等了 %v", elapsed)
	}
	if !strings.Contains(err.Error(), "max_wait") {
		t.Errorf("错误信息应提示调大 max_wait: %v", err)
	}
}

// 其它网关（sub2api）的导出文件：凭据嵌套在 credentials 里，client_id 即签发 refresh_token 的 OAuth client。
func TestParseAccountList_ExportFile(t *testing.T) {
	raw := []byte(`{"exported_at":"2026-10-04T12:44:07Z","proxies":[],"accounts":[{
		"name":"gpt-x","platform":"openai","type":"oauth","concurrency":10,
		"credentials":{"access_token":"at","refresh_token":"rt.1.x","email":"x@example.com",
			"plan_type":"plus","client_id":"app_X","chatgpt_account_id":"acct-2"},
		"extra":{"codex_5h_used_percent":45}}]}`)
	list, err := ParseAccountList(raw)
	if err != nil || len(list) != 1 {
		t.Fatalf("解析失败: %v %d", err, len(list))
	}
	a := list[0]
	if a.ID != "" || a.Name != "gpt-x" || a.AccessToken != "at" || a.RefreshToken != "rt.1.x" ||
		a.Email != "x@example.com" || a.Plan != "plus" || a.OAuthClientID != "app_X" || a.AccountID != "acct-2" {
		t.Fatalf("字段不对: %+v", a)
	}
	if a.MaxConcurrency != 0 || len(a.Tags) != 1 || a.Tags[0] != "oauth" {
		t.Fatalf("导出方的并发不应照搬、oauth 账号应打标签: %+v", a)
	}
	if !HasCredentials(a) {
		t.Fatal("应识别出凭据")
	}

	// 单个账号对象也认；file 形式补默认 id
	if list, _ := ParseAccounts([]byte(`{"name":"solo","cookies":"c=1"}`)); len(list) != 1 || list[0].ID != "file-1" {
		t.Fatalf("单个对象: %+v", list)
	}
	if _, err := ParseAccountList([]byte(`{"accounts":[{"platform":"anthropic","credentials":{"access_token":"x"}}]}`)); err == nil {
		t.Fatal("非 OpenAI 平台应报错")
	}
}
