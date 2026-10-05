package account

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
)

// fakeJWT 造一个不签名的 JWT（只读 payload）。
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(body) + ".sig"
}

// fakeTokenEndpoint 模拟 auth.openai.com/oauth/token：每次刷新都轮换 refresh_token，
// 并且像真实上游一样拒绝已经用过的 refresh_token（refresh_token_reused）。
type fakeTokenEndpoint struct {
	t *testing.T

	mu       sync.Mutex
	n        int
	used     map[string]bool
	gotRT    []string // 每次请求带来的 refresh_token
	gotCID   []string // 每次请求带来的 client_id
	entered  chan struct{}
	release  chan struct{} // 非 nil 时，处理请求前阻塞到它被关闭
	srv      *httptest.Server
	lastResp oauthReply
}

type oauthReply struct {
	AccessToken  string
	RefreshToken string
}

func newFakeTokenEndpoint(t *testing.T) *fakeTokenEndpoint {
	f := &fakeTokenEndpoint{t: t, used: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTokenEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	_ = json.NewDecoder(r.Body).Decode(&req)

	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	rt := req["refresh_token"]
	f.gotRT = append(f.gotRT, rt)
	f.gotCID = append(f.gotCID, req["client_id"])
	w.Header().Set("Content-Type", "application/json")
	if req["grant_type"] != "refresh_token" || f.used[rt] {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh_token_reused"}`))
		return
	}
	f.used[rt] = true
	f.n++
	f.lastResp = oauthReply{
		AccessToken: fakeJWT(f.t, map[string]any{
			"exp":   time.Now().Add(10 * 24 * time.Hour).Unix(),
			"email": "fresh@example.com",
			"n":     f.n, // 让每次签发的 access_token 都不同
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_plan_type": "plus",
			},
		}),
		RefreshToken: fmt.Sprintf("rt-new-%d", f.n),
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  f.lastResp.AccessToken,
		"refresh_token": f.lastResp.RefreshToken,
		"expires_in":    int64((10 * 24 * time.Hour).Seconds()),
	})
}

func (f *fakeTokenEndpoint) last() (rt, cid string, resp oauthReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.gotRT) == 0 {
		return "", "", resp
	}
	return f.gotRT[len(f.gotRT)-1], f.gotCID[len(f.gotCID)-1], f.lastResp
}

func openTestStore(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(path, nopLog())
	if err != nil {
		t.Fatalf("打开 SQLite 失败: %v", err)
	}
	return s
}

// poolFromStore 模拟 server.New 的启动流程：从 SQLite 读账号建池并挂上回写回调。
func poolFromStore(t *testing.T, tok *fakeTokenEndpoint, store *SQLiteStore) (*Pool, *creds.Refresher) {
	t.Helper()
	list, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Upstream.BaseURL = tok.srv.URL // 走标准库传输，不走 Prism 的 Chrome 指纹传输
	cfg.Creds.OAuthTokenURL = tok.srv.URL
	cfg.Creds.OAuthClientID = "app_global_fallback"
	cfg.Creds.Accounts = list
	p, err := NewPool(cfg, nopLog())
	if err != nil {
		t.Fatal(err)
	}
	p.SetOnRefreshed(NewRefreshPersister(store, cfg.Creds.PersistRefreshMin, nopLog()).Persist)
	r, err := creds.NewRefresher(cfg.Creds, cfg.Upstream, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, r
}

func seedOAuthAccount(t *testing.T, store *SQLiteStore) {
	t.Helper()
	err := store.SaveAccount(config.AccountConfig{
		ID:             "oauth-1",
		Name:           "Dashboard 上改过的名字",
		Tags:           []string{"oauth", "vip"},
		MaxConcurrency: 5,
		Plan:           "pro",
		AccessToken: fakeJWT(t, map[string]any{
			"exp": time.Now().Add(-time.Minute).Unix(), // 已过期
		}),
		RefreshToken:  "rt-original",
		OAuthClientID: "app_per_account",
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 回归：刷新后的 token 必须落盘，否则重启后拿已消费的 refresh_token 去刷新 → invalid_grant。
func TestRefreshAccount_PersistsRotatedTokensAcrossRestart(t *testing.T) {
	tok := newFakeTokenEndpoint(t)
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	store := openTestStore(t, dbPath)
	seedOAuthAccount(t, store)

	p, r := poolFromStore(t, tok, store)
	if _, err := p.RefreshAccount(context.Background(), r, p.Get("oauth-1")); err != nil {
		t.Fatalf("首次刷新失败: %v", err)
	}
	rt, cid, resp := tok.last()
	if rt != "rt-original" {
		t.Fatalf("首次刷新应使用库里的 refresh_token，实际用了另一枚")
	}
	if cid != "app_per_account" {
		t.Fatalf("刷新应使用账号自己的 oauth_client_id，实际 client_id = %q", cid)
	}

	// 库内 token 列已更新，Dashboard 编辑的列原样保留。
	list, err := store.Load()
	if err != nil || len(list) != 1 {
		t.Fatalf("Load: %v (%d 行)", err, len(list))
	}
	got := list[0]
	if got.AccessToken != resp.AccessToken || got.RefreshToken != resp.RefreshToken {
		t.Fatal("刷新后的 access_token / refresh_token 未写回 SQLite")
	}
	if got.Name != "Dashboard 上改过的名字" || got.MaxConcurrency != 5 ||
		len(got.Tags) != 2 || got.Tags[1] != "vip" {
		t.Fatalf("回写不应改动 name / tags / max_concurrency，得到 %q %v %d",
			got.Name, got.Tags, got.MaxConcurrency)
	}
	if got.Plan != "plus" || got.Email != "fresh@example.com" {
		t.Fatalf("plan / email 应随新 token 更新，得到 %q / %q", got.Plan, got.Email)
	}
	if got.OAuthClientID != "app_per_account" || got.Headers != nil {
		t.Fatalf("oauth_client_id 应保留在专用字段、不进 Headers，得到 %q / %v", got.OAuthClientID, got.Headers)
	}

	// 模拟重启：关库、重开、重新建池，再刷新一次。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2 := openTestStore(t, dbPath)
	t.Cleanup(func() { _ = store2.Close() })
	p2, r2 := poolFromStore(t, tok, store2)
	if _, err := p2.RefreshAccount(context.Background(), r2, p2.Get("oauth-1")); err != nil {
		t.Fatalf("重启后刷新失败（大概率拿了已作废的 refresh_token）: %v", err)
	}
	if rt, cid, _ := tok.last(); rt != resp.RefreshToken || cid != "app_per_account" {
		t.Fatalf("重启后应使用轮换后的 refresh_token 与账号自己的 client_id，实际 rt 匹配=%v client_id=%q",
			rt == resp.RefreshToken, cid)
	}
}

// 刷新途中池被整体重建（Dashboard 编辑 → syncPoolFromSQLite）：
// 新池里的同 ID 账号读到的是回写前的旧值，必须被移植成新凭据。
func TestRefreshAccount_RebuildDuringRefreshGetsNewCredential(t *testing.T) {
	tok := newFakeTokenEndpoint(t)
	store := openTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	t.Cleanup(func() { _ = store.Close() })
	seedOAuthAccount(t, store)
	p, r := poolFromStore(t, tok, store)
	stale, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	tok.entered = make(chan struct{}, 1)
	tok.release = make(chan struct{})
	done := make(chan error, 1)
	old := p.Get("oauth-1")
	go func() {
		_, err := p.RefreshAccount(context.Background(), r, old)
		done <- err
	}()

	<-tok.entered // 刷新请求已到达上游
	if err := p.Build(stale); err != nil {
		t.Fatal(err)
	}
	if p.Get("oauth-1") == old {
		t.Fatal("Build 应生成新的 Account 对象")
	}
	close(tok.release)
	if err := <-done; err != nil {
		t.Fatalf("刷新失败: %v", err)
	}

	_, _, resp := tok.last()
	if c := p.Get("oauth-1").Credential(); c.RefreshToken != resp.RefreshToken {
		t.Fatal("重建后的账号仍持有已消费的 refresh_token")
	}
}

func TestRefreshPersister_Throttle(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveAccount(config.AccountConfig{ID: "a", AccessToken: "at-0", RefreshToken: "rt-0"}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	pr := NewRefreshPersister(store, time.Hour, nopLog())
	pr.now = func() time.Time { return now }

	tokens := func() (string, string) {
		t.Helper()
		list, err := store.Load()
		if err != nil || len(list) != 1 {
			t.Fatalf("Load: %v", err)
		}
		return list[0].AccessToken, list[0].RefreshToken
	}

	pr.Persist("a", &creds.Credential{AccessToken: "at-1", RefreshToken: "rt-0"})
	if at, _ := tokens(); at != "at-1" {
		t.Fatalf("首次回写不应被节流，access_token = %q", at)
	}

	now = now.Add(10 * time.Minute)
	pr.Persist("a", &creds.Credential{AccessToken: "at-2", RefreshToken: "rt-0"})
	if at, _ := tokens(); at != "at-1" {
		t.Fatalf("节流窗口内且 refresh_token 未变，应跳过，access_token = %q", at)
	}

	now = now.Add(time.Minute)
	pr.Persist("a", &creds.Credential{AccessToken: "at-3", RefreshToken: "rt-1"})
	if at, rt := tokens(); at != "at-3" || rt != "rt-1" {
		t.Fatalf("refresh_token 轮换必须立即落盘，得到 %q / %q", at, rt)
	}

	now = now.Add(2 * time.Hour)
	pr.Persist("a", &creds.Credential{AccessToken: "at-4", RefreshToken: "rt-1"})
	if at, _ := tokens(); at != "at-4" {
		t.Fatalf("超过节流间隔应回写，access_token = %q", at)
	}
}

func TestSQLiteStore_UpdateTokens(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	t.Cleanup(func() { _ = store.Close() })

	if err := store.UpdateTokens("ghost", TokenUpdate{AccessToken: "x"}); !errors.Is(err, ErrAccountNotInStore) {
		t.Fatalf("不存在的账号应返回 ErrAccountNotInStore，得到 %v", err)
	}
	if list, _ := store.Load(); len(list) != 0 {
		t.Fatal("UpdateTokens 不应凭空插入账号")
	}

	if err := store.SaveAccount(config.AccountConfig{
		ID: "a", Plan: "pro", Email: "a@x", AccessToken: "at-0", RefreshToken: "rt-0",
	}); err != nil {
		t.Fatal(err)
	}
	// 空字段 = 不改。
	if err := store.UpdateTokens("a", TokenUpdate{AccessToken: "at-1"}); err != nil {
		t.Fatal(err)
	}
	list, _ := store.Load()
	if a := list[0]; a.AccessToken != "at-1" || a.RefreshToken != "rt-0" || a.Plan != "pro" || a.Email != "a@x" {
		t.Fatalf("空字段不应覆盖原值，得到 %+v", a)
	}
}

// 旧库没有 oauth_client_id 列：打开时自动追加，旧行可读；Dashboard 编辑不清空它。
func TestSQLiteStore_MigratesOAuthClientIDColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
		CREATE TABLE accounts (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			plan TEXT DEFAULT 'pro',
			email TEXT DEFAULT '',
			cookies TEXT DEFAULT '',
			access_token TEXT DEFAULT '',
			refresh_token TEXT DEFAULT '',
			max_concurrency INTEGER DEFAULT 2,
			tags TEXT DEFAULT '',
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO accounts (id, name, access_token, refresh_token) VALUES ('old', 'old', 'at', 'rt');`)
	_ = raw.Close()
	if err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t, dbPath)
	list, err := store.Load()
	if err != nil || len(list) != 1 {
		t.Fatalf("迁移后读取旧行失败: %v", err)
	}
	if list[0].OAuthClientID != "" || list[0].Headers != nil {
		t.Fatalf("旧行没有 oauth_client_id，得到 %q / %v", list[0].OAuthClientID, list[0].Headers)
	}

	// 旧 accounts.json 的写法：放在 Headers 里，SaveAccount 应收进专用列。
	if err := store.SaveAccount(config.AccountConfig{
		ID: "old", Name: "old", Headers: map[string]string{"oauth_client_id": "app_x"},
	}); err != nil {
		t.Fatal(err)
	}
	// 模拟 Dashboard 的 PUT：不带 Headers。
	if err := store.SaveAccount(config.AccountConfig{ID: "old", Name: "改名"}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	// 重开时列已存在，迁移必须是幂等的。
	store = openTestStore(t, dbPath)
	t.Cleanup(func() { _ = store.Close() })
	list, err = store.Load()
	if err != nil || len(list) != 1 {
		t.Fatalf("重开后读取失败: %v", err)
	}
	if got := list[0]; got.Name != "改名" || got.OAuthClientID != "app_x" || got.Headers != nil {
		t.Fatalf("oauth_client_id 应跨编辑与重启保留在专用字段，得到 name=%q client=%q headers=%v",
			got.Name, got.OAuthClientID, got.Headers)
	}
}

// 后台巡检的提前量至少比巡检周期多一分钟：默认每 10 分钟巡检、提前 5 分钟刷新时，
// 离过期还剩 8 分钟的账号这一轮不刷，下一轮访问令牌已经过期。
func TestRefreshAll_SkewCoversInterval(t *testing.T) {
	tok := newFakeTokenEndpoint(t)
	store := openTestStore(t, filepath.Join(t.TempDir(), "accounts.db"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SaveAccount(config.AccountConfig{
		ID:           "oauth-8m",
		AccessToken:  fakeJWT(t, map[string]any{"exp": time.Now().Add(8 * time.Minute).Unix()}),
		RefreshToken: "rt-8m",
	}); err != nil {
		t.Fatal(err)
	}
	p, r := poolFromStore(t, tok, store)
	p.creds.RefreshSkew, p.creds.RefreshInterval = 5*time.Minute, 10*time.Minute
	p.refreshAll(context.Background(), r)
	if rt, _, _ := tok.last(); rt != "rt-8m" {
		t.Fatal("离过期 8 分钟（短于巡检周期）的账号这一轮就该刷新")
	}
}
