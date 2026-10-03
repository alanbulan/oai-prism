package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORS_Disabled 验证不启用时是零成本透传。
func TestCORS_Disabled(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	CORS("")(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if v := rec.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("未启用时不应发 CORS 头，得到 %q", v)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("应正常转发，得到 %d", rec.Code)
	}
}

// TestCORS_Preflight 验证 OPTIONS 预检在中间件层终结，不打到业务路由。
//
// 预检若打到业务路由，每个跨域请求都会多一次完整路由匹配与鉴权，
// 而且会拿到 405 —— 浏览器直接判跨域失败。
func TestCORS_Preflight(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	})
	rec := httptest.NewRecorder()
	CORS("http://localhost:3000")(next).ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil))

	if reached {
		t.Error("预检请求不应打到业务处理器")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("预检应返回 204，得到 %d", rec.Code)
	}
	if v := rec.Header().Get("Access-Control-Allow-Origin"); v != "http://localhost:3000" {
		t.Errorf("Allow-Origin = %q", v)
	}
}

// TestCORS_ActualRequest 验证真实请求带上 CORS 头且正常转发。
func TestCORS_ActualRequest(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	CORS("*")(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("真实请求应转发，得到 %d", rec.Code)
	}
	if v := rec.Header().Get("Access-Control-Allow-Origin"); v != "*" {
		t.Errorf("Allow-Origin = %q", v)
	}
	// 会话延续头必须在允许列表里，否则浏览器端无法读响应头续写会话。
	if v := rec.Header().Get("Access-Control-Allow-Headers"); v == "" {
		t.Error("缺少 Allow-Headers")
	}
	// Expose-Headers 不能少：x-prism-conversation-id 不在 CORS 安全列表里，
	// 不显式暴露浏览器就拒绝让 JS 读它 —— 跨域多轮续写会静默失效。
	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposed, "x-prism-conversation-id") {
		t.Errorf("Expose-Headers 未包含 x-prism-conversation-id: %q", exposed)
	}
}

// TestAPIKeyAuth_ExemptProbes 验证配置 API Key 时探针端点豁免，业务路由被拦截。
func TestAPIKeyAuth_ExemptProbes(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := APIKeyAuth([]string{"sk-test-key"}, nil, true)
	handler := mw(next)

	probes := []string{"/healthz", "/readyz", "/metrics"}
	for _, p := range probes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("探针 %s 应当豁免鉴权，得到状态码 %d", p, rec.Code)
		}
	}

	// 业务端点没有 key 应被 401 拦截
	recBiz := httptest.NewRecorder()
	reqBiz := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	handler.ServeHTTP(recBiz, reqBiz)
	if recBiz.Code != http.StatusUnauthorized {
		t.Errorf("未带 key 的业务请求应返回 401，得到 %d", recBiz.Code)
	}

	// 业务端点带有效 key 应放行 200
	recBizAuth := httptest.NewRecorder()
	reqBizAuth := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqBizAuth.Header.Set("Authorization", "Bearer sk-test-key")
	handler.ServeHTTP(recBizAuth, reqBizAuth)
	if recBizAuth.Code != http.StatusOK {
		t.Errorf("带有效 key 的业务请求应返回 200，得到 %d", recBizAuth.Code)
	}
}

// 鉴权通过后应写入调用方指纹（会话租户隔离依赖它），且指纹不含 Key 原文。
func TestAuth_SetsTenant(t *testing.T) {
	var got string
	h := Auth(AuthOptions{StaticKeys: []string{"sk-tenant-key"}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = Tenant(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer sk-tenant-key")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.HasPrefix(got, "k:") || strings.Contains(got, "sk-tenant-key") {
		t.Fatalf("租户指纹异常: %q", got)
	}
}

func TestIsLocalRequest(t *testing.T) {
	cases := []struct {
		remote, host string
		want         bool
	}{
		{"127.0.0.1:1", "localhost:8787", true},
		{"[::1]:1", "[::1]:8787", true},
		{"127.0.0.1:1", "127.0.0.1:8787", true},
		{"127.0.0.1:1", "evil.example:8787", false}, // DNS rebinding
		{"192.0.2.1:1", "localhost:8787", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		if got := IsLocalRequest(r); got != c.want {
			t.Errorf("IsLocalRequest(%s, %s) = %v, want %v", c.remote, c.host, got, c.want)
		}
	}
}
