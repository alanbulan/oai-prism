package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

// remote 构造一个来自非本机地址的请求，直接交给网关处理器。
func remote(t *testing.T, ts *httptest.Server, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:4444"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	return rec
}

func doLocal(t *testing.T, method, url, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// 审计中间件曾只把前 1MB 交给 handler：大请求一律"不是合法 JSON"。
func TestSecurity_LargeBodyNotTruncated(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	big := strings.Repeat("a", 2<<20)
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"` + big + `"}]}`
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions", body, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("2MB 请求应成功，得到 %d: %.200s", code, out)
	}
}

// 没有任何 Key 时只放行本机：监听 0.0.0.0 不能把管理端和账号池交给整个局域网。
func TestSecurity_NoKeysLocalOnly(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	if rec := remote(t, ts, http.MethodGet, "/admin/apikeys", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("远端无 Key 访问管理端应 401，得到 %d", rec.Code)
	}
	if rec := remote(t, ts, http.MethodGet, "/v1/models", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("远端无 Key 访问 API 应 401，得到 %d", rec.Code)
	}
	if code, _ := doLocal(t, http.MethodGet, ts.URL+"/admin/apikeys", "", nil); code != http.StatusOK {
		t.Fatalf("本机应可访问管理端，得到 %d", code)
	}
	// 探针与 Dashboard 静态资源始终豁免。
	if rec := remote(t, ts, http.MethodGet, "/healthz", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("探针应豁免，得到 %d", rec.Code)
	}
}

// Dashboard 签发的 Key 必须真的能用于鉴权；签发后转为全量校验；
// 普通 Key 从远端不能管理账号。
func TestSecurity_DashboardKeysEnforcedAndScoped(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/apikeys", `{"name":"t"}`, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("签发 Key 失败: %d %s", code, out)
	}
	var item struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal([]byte(out), &item)
	if !strings.HasPrefix(item.Key, "sk-prism-") || len(item.Key) < 40 {
		t.Fatalf("签发的 Key 不够随机: %q", item.Key)
	}

	if rec := remote(t, ts, http.MethodGet, "/v1/models", "", map[string]string{"Authorization": "Bearer " + item.Key}); rec.Code != http.StatusOK {
		t.Fatalf("Dashboard Key 应可调用 API，得到 %d", rec.Code)
	}
	if code, _ := doLocal(t, http.MethodGet, ts.URL+"/v1/models", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("存在 Key 后本机无 Key 也应 401，得到 %d", code)
	}
	if rec := remote(t, ts, http.MethodGet, "/admin/apikeys", "", map[string]string{"Authorization": "Bearer " + item.Key}); rec.Code != http.StatusForbidden {
		t.Fatalf("远端普通 Key 不应能管理，得到 %d", rec.Code)
	}
	if code, _ := doLocal(t, http.MethodGet, ts.URL+"/admin/apikeys", "", map[string]string{"Authorization": "Bearer " + item.Key}); code != http.StatusOK {
		t.Fatalf("本机 + 有效 Key 应可管理，得到 %d", code)
	}

	// 注销后立即失效。
	if code, _ := doLocal(t, http.MethodDelete, ts.URL+"/admin/apikeys/"+item.Key, "", map[string]string{"Authorization": "Bearer " + item.Key}); code != http.StatusOK {
		t.Fatalf("注销失败: %d", code)
	}
	if rec := remote(t, ts, http.MethodGet, "/v1/models", "", map[string]string{"Authorization": "Bearer " + item.Key}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("注销后的 Key 应失效，得到 %d", rec.Code)
	}
}

func TestSecurity_ConfigKeyIsAdmin(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), func(c *config.Config) {
		c.Facade.APIKeys = []string{"sk-config-admin-key-0123456789"}
	})
	if rec := remote(t, ts, http.MethodGet, "/admin/accounts", "", map[string]string{"Authorization": "Bearer sk-config-admin-key-0123456789"}); rec.Code != http.StatusOK {
		t.Fatalf("配置文件 Key 应具备管理权限，得到 %d", rec.Code)
	}
}

// 登录曾接受 admin/空密码并返回一个没人校验的固定令牌。
func TestSecurity_AdminLogin(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	if rec := remote(t, ts, http.MethodPost, "/admin/login", `{"username":"admin","password":""}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("未配置密码时不应开放登录，得到 %d", rec.Code)
	}

	ts2, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), func(c *config.Config) {
		c.Server.AdminPassword = "correct horse battery staple"
	})
	if rec := remote(t, ts2, http.MethodPost, "/admin/login", `{"username":"admin","password":"admin123"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧默认密码不应通过，得到 %d", rec.Code)
	}
	rec := remote(t, ts2, http.MethodPost, "/admin/login", `{"username":"admin","password":"correct horse battery staple"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("正确密码应登录成功，得到 %d", rec.Code)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Token, "adm_") {
		t.Fatalf("令牌格式错误: %q", out.Token)
	}
	if rec := remote(t, ts2, http.MethodGet, "/admin/accounts", "", map[string]string{"Authorization": "Bearer " + out.Token}); rec.Code != http.StatusOK {
		t.Fatalf("管理令牌应可远程管理，得到 %d", rec.Code)
	}
	if rec := remote(t, ts2, http.MethodGet, "/admin/accounts", "", map[string]string{"Authorization": "Bearer tok_prism_admin_session"}); rec.Code == http.StatusOK {
		t.Fatal("旧的固定令牌不应再有效")
	}
}

// 浏览器跨站请求不能借本机身份改账号（CSRF / DNS rebinding）。
func TestSecurity_AdminCrossOriginBlocked(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	code, _ := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", `{"id":"evil","access_token":"x"}`,
		map[string]string{"Origin": "https://evil.example", "Content-Type": "text/plain"})
	if code != http.StatusForbidden {
		t.Fatalf("跨站管理写操作应 403，得到 %d", code)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/apikeys", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Host = "attacker.example:8787" // DNS rebinding：域名解析到 127.0.0.1
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("Host 为外部域名的回环请求不应视为本机")
	}
}

// 管理端暴露的 X-Local-Workspace 写盘能力默认关闭。
func TestSecurity_LocalWorkspaceDisabledByDefault(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	dir := t.TempDir()
	code, _ := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Content-Type": "application/json", "X-Local-Workspace": dir})
	if code != http.StatusOK {
		t.Fatalf("请求应正常完成，得到 %d", code)
	}
}

func TestSecurity_RawProxyWhitelistBySegment(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), func(c *config.Config) {
		c.RawProxy.Enabled = true
		c.RawProxy.AllowPaths = []string{"/api/y"}
	})
	code, _ := doLocal(t, http.MethodGet, ts.URL+"/prism/api/yolo", "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("/api/y 白名单不应放行 /api/yolo，得到 %d", code)
	}
}
