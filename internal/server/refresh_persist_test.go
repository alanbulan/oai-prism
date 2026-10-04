package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// 端到端回归：刷新经 server 装配的回调写回 SQLite。Dashboard 编辑会从 SQLite
// 整池重建，此后的刷新必须拿到轮换后的 refresh_token，而不是已作废的旧值。
func TestAdmin_RefreshPersistsRotatedTokenToSQLite(t *testing.T) {
	var (
		mu    sync.Mutex
		n     int
		used  = map[string]bool{}
		gotRT []string
	)
	jwt := func(claims map[string]any) string {
		b, _ := json.Marshal(claims)
		return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
	}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		rt := req["refresh_token"]
		gotRT = append(gotRT, rt)
		w.Header().Set("Content-Type", "application/json")
		if used[rt] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh_token_reused"}`))
			return
		}
		used[rt] = true
		n++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  jwt(map[string]any{"exp": time.Now().Add(240 * time.Hour).Unix(), "n": n}),
			"refresh_token": fmt.Sprintf("rt-rotated-%d", n),
			"expires_in":    864000,
		})
	}))
	t.Cleanup(tokenSrv.Close)

	accounts := []config.AccountConfig{{
		ID:             "main",
		AccessToken:    jwt(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}),
		RefreshToken:   "rt-original",
		MaxConcurrency: 8,
	}}
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, accounts, func(c *config.Config) {
		c.Creds.OAuthTokenURL = tokenSrv.URL
	})

	refresh := func() {
		t.Helper()
		resp, err := http.Post(ts.URL+"/admin/accounts/main/refresh", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := readBody(resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("刷新状态码 = %d: %s", resp.StatusCode, body)
		}
	}

	refresh()

	// Dashboard 改名 → syncPoolFromSQLite 从库里重建整个池。
	put, _ := http.NewRequest(http.MethodPut, ts.URL+"/admin/accounts/main",
		bytes.NewReader([]byte(`{"name":"改过名","max_concurrency":8}`)))
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d", resp.StatusCode)
	}

	refresh() // 用的若是库里没更新的旧 refresh_token，上面的假端点会回 invalid_grant

	mu.Lock()
	defer mu.Unlock()
	if len(gotRT) != 2 || gotRT[0] != "rt-original" || gotRT[1] != "rt-rotated-1" {
		t.Fatalf("两次刷新应依次使用原始与轮换后的 refresh_token，实际 %v", gotRT)
	}

	list, err := http.Get(ts.URL + "/admin/accounts")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readBody(list)
	list.Body.Close()
	if !strings.Contains(body, "改过名") {
		t.Fatalf("Dashboard 改名应保留: %s", body)
	}
}

// oauth_client_id 是本地刷新用的元数据，不能作为 HTTP 头发给 Prism。
// 覆盖两种来源：SQLite 的 oauth_client_id 列，以及旧 accounts.json 塞在 headers 里的写法。
func TestE2E_OAuthClientIDNotForwardedUpstream(t *testing.T) {
	cases := map[string]config.AccountConfig{
		"专用字段": {
			ID: "main", AccessToken: "good-token", MaxConcurrency: 8,
			OAuthClientID: "app_per_account",
		},
		"旧版 Headers 写法": {
			ID: "main", AccessToken: "good-token", MaxConcurrency: 8,
			Headers: map[string]string{"oauth_client_id": "app_per_account"},
		},
	}
	for name, acct := range cases {
		t.Run(name, func(t *testing.T) {
			// 账号经 MigrateIfEmpty 入库，池从 SQLite 读回（oauth_client_id 列 → 凭据）。
			ts, up, srv := newTestServerWithSrv(t, &fakeUpstream{t: t}, []config.AccountConfig{acct}, nil)

			a := srv.Pool().Get("main")
			cred := a.Credential()
			if cred.OAuthClientID != "app_per_account" {
				t.Fatalf("从 SQLite 读回的凭据应带 OAuthClientID，得到 %q", cred.OAuthClientID)
			}
			// SQLite 没有 headers 列，读回的账号本无附加头。在读回的凭据上补一个对照头：
			// 若读回路径把 oauth_client_id 混进了 Headers，它会跟着一起被转发。
			next := cred.Clone()
			if next.Headers == nil {
				next.Headers = map[string]string{}
			}
			next.Headers["X-Test-Device"] = "dev-1"
			a.StoreCredential(next)

			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
				strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := readBody(resp)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("状态码 = %d: %s", resp.StatusCode, body)
			}

			up.mu.Lock()
			defer up.mu.Unlock()
			if len(up.reqHeaders) == 0 {
				t.Fatal("上游没有收到任何请求")
			}
			forwarded := false
			for _, h := range up.reqHeaders {
				for k := range h {
					if strings.EqualFold(k, "oauth_client_id") || strings.EqualFold(k, "oauth-client-id") {
						t.Fatalf("上游收到了 %s 头", k)
					}
				}
				if h.Get("X-Test-Device") == "dev-1" {
					forwarded = true
				}
			}
			// 反证：账号级附加头确实在转发，上面的"没收到"才有意义。
			if !forwarded {
				t.Fatal("账号级附加头 X-Test-Device 未转发，断言失去意义")
			}
		})
	}
}
