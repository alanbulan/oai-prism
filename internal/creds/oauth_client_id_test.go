package creds

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

// oauth_client_id 是本地刷新用的元数据：进专用字段，绝不留在会转发给上游的 Headers 里。
func TestFromAccountConfig_OAuthClientID(t *testing.T) {
	t.Run("旧版 Headers 写法：读出并从 Headers 剔除", func(t *testing.T) {
		in := map[string]string{"oauth_client_id": "app_legacy", "X-Device": "dev-1"}
		c := FromAccountConfig(config.AccountConfig{ID: "a", Headers: in})
		if c.OAuthClientID != "app_legacy" {
			t.Fatalf("OAuthClientID = %q, want app_legacy", c.OAuthClientID)
		}
		if _, leaked := c.Headers["oauth_client_id"]; leaked {
			t.Fatalf("oauth_client_id 不应留在 Headers: %v", c.Headers)
		}
		if c.Headers["X-Device"] != "dev-1" {
			t.Fatalf("其他附加头应保留: %v", c.Headers)
		}
		if in["oauth_client_id"] != "app_legacy" {
			t.Fatal("不应修改配置里的原 map")
		}
	})

	t.Run("键名大小写不敏感", func(t *testing.T) {
		c := FromAccountConfig(config.AccountConfig{ID: "a", Headers: map[string]string{"OAuth_Client_ID": "app_x"}})
		if c.OAuthClientID != "app_x" || c.Headers != nil {
			t.Fatalf("得到 %q / %v", c.OAuthClientID, c.Headers)
		}
	})

	t.Run("专用字段优先于旧版 Headers", func(t *testing.T) {
		c := FromAccountConfig(config.AccountConfig{
			ID:            "a",
			OAuthClientID: "app_field",
			Headers:       map[string]string{"oauth_client_id": "app_legacy"},
		})
		if c.OAuthClientID != "app_field" || c.Headers != nil {
			t.Fatalf("得到 %q / %v", c.OAuthClientID, c.Headers)
		}
	})

	t.Run("没有 oauth_client_id 时 Headers 原样保留", func(t *testing.T) {
		in := map[string]string{"X-Device": "dev-1"}
		c := FromAccountConfig(config.AccountConfig{ID: "a", Headers: in})
		if c.OAuthClientID != "" || len(c.Headers) != 1 || c.Headers["X-Device"] != "dev-1" {
			t.Fatalf("得到 %q / %v", c.OAuthClientID, c.Headers)
		}
	})
}

func TestRefreshOAuth_UsesPerAccountClientID(t *testing.T) {
	var gotClientID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotClientID = req["client_id"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`))
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Upstream.BaseURL = srv.URL
	cfg.Creds.OAuthTokenURL = srv.URL
	cfg.Creds.OAuthClientID = "app_global"
	r, err := NewRefresher(cfg.Creds, cfg.Upstream, nil)
	if err != nil {
		t.Fatal(err)
	}

	next, err := r.RefreshOAuth(context.Background(), &Credential{RefreshToken: "rt-old", OAuthClientID: "app_account"})
	if err != nil {
		t.Fatal(err)
	}
	if gotClientID != "app_account" {
		t.Fatalf("应使用账号自己的 client_id，实际 %q", gotClientID)
	}
	if next.OAuthClientID != "app_account" {
		t.Fatalf("刷新后的凭据应保留 OAuthClientID，得到 %q", next.OAuthClientID)
	}

	if _, err := r.RefreshOAuth(context.Background(), &Credential{RefreshToken: "rt-old"}); err != nil {
		t.Fatal(err)
	}
	if gotClientID != "app_global" {
		t.Fatalf("账号未记录 client 时应退回全局 creds.oauth_client_id，实际 %q", gotClientID)
	}
}
