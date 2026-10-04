// Package upstream 是网关到 prism.openai.com 的出站传输，替代原来的 8790 TLS 桥 + 8791 浏览器 oracle：
//
//   - Chrome 的 TLS / HTTP2 指纹（bogdanfinn/tls-client）—— Cloudflare 校验 JA3/JA4，标准库过不去；
//   - 与指纹一致的浏览器请求头（User-Agent、sec-ch-ua、Accept-Language……）；
//   - Prism 会话：用账号的 access token 经 /auth/session 换发 prism_session_token（6 小时一换）；
//   - 按 Prism 前端的规则给需要的请求附上 OpenAI-Sentinel-Token（纯 Go 签发，见 internal/sentinel）。
//
// 它实现 http.RoundTripper，插在 httpc 客户端底下：上层代码（重试、账号池、协议翻译）完全不变。
package upstream

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"github.com/oai-prism/oaiprism/internal/sentinel"
)

// Options 配置传输层。
type Options struct {
	Profile     *sentinel.Profile // 浏览器指纹；nil 时读 ProfilePath，再不行用内置
	ProfilePath string
	Proxy       string // 出站代理（http/https/socks5）
	CacheDir    string // sdk.js 缓存目录
	Logger      *slog.Logger

	// Signer 可注入（测试或多个传输共用一个签发器）；nil 时自建。
	Signer *sentinel.Signer
}

// Transport 是 Chrome 指纹的 http.RoundTripper。
type Transport struct {
	api     tls_client.HttpClient // prism 业务请求：不带 cookie jar，Cookie 头由我们拼
	signer  *sentinel.Signer
	profile *sentinel.Profile
	log     *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session // access token → 会话
}

type session struct {
	mu    sync.Mutex
	token string
	at    time.Time
}

const (
	sessionTTL = 6 * time.Hour // prism_session_token 实测寿命约 12 小时，留足余量
	prismHost  = "prism.openai.com"
	// SentinelHeader 是 Prism 前端的 PRISM_SENTINEL_HEADER。
	SentinelHeader = "OpenAI-Sentinel-Token"
	// unavailableToken 是前端在 SDK 不可用时发送的占位（PRISM_SENTINEL_UNAVAILABLE_TOKEN）。
	unavailableToken = `{"e":"prism_sdk_unavailable","flow":"prism_inference"}`
)

// New 创建传输层。
func New(o Options) (*Transport, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	var prof *sentinel.Profile
	if o.Signer != nil {
		prof = o.Signer.Profile()
	} else {
		p, err := loadProfile(o)
		if err != nil {
			return nil, err
		}
		prof = p
	}
	api, err := newClient(o.Proxy, nil, prof)
	if err != nil {
		return nil, err
	}
	t := &Transport{api: api, profile: prof, log: o.Logger, sessions: map[string]*session{}}
	t.signer = o.Signer
	if t.signer == nil {
		sc, err := newClient(o.Proxy, tls_client.NewCookieJar(), prof)
		if err != nil {
			return nil, err
		}
		t.signer, err = sentinel.New(sentinel.Options{
			Doer: &doer{c: sc, profile: prof}, Profile: prof, CacheDir: o.CacheDir, Logger: o.Logger,
		})
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}

func loadProfile(o Options) (*sentinel.Profile, error) {
	if o.Profile != nil {
		return o.Profile, nil
	}
	if o.ProfilePath != "" {
		p, err := sentinel.LoadProfile(o.ProfilePath)
		if err != nil {
			return nil, fmt.Errorf("读取 Sentinel 指纹失败: %w", err)
		}
		return p, nil
	}
	return sentinel.DefaultProfile(), nil
}

// newClient 建一个与指纹同版本的 Chrome TLS 客户端。扩展顺序每个连接随机打乱：
// Chrome 110 起就是这样（JA3 每次都变，JA4 不变），固定顺序反而一眼看出不是 Chrome。
func newClient(proxy string, jar tls_client.CookieJar, p *sentinel.Profile) (tls_client.HttpClient, error) {
	_, tmpl := tlsTemplate(p)
	opts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(tmpl),
		tls_client.WithRandomTLSExtensionOrder(),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithTimeoutSeconds(0), // 流式与长轮询由 context 控制
		tls_client.WithTransportOptions(&tls_client.TransportOptions{DisableCompression: true}),
	}
	if proxy != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxy))
	}
	if jar != nil {
		opts = append(opts, tls_client.WithCookieJar(jar))
	}
	c, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("创建 Chrome 指纹客户端失败: %w", err)
	}
	return c, nil
}

// Signer 返回 Sentinel 签发器（管理页展示运行状况用）。
func (t *Transport) Signer() *sentinel.Signer { return t.signer }

// Close 释放签发器。
func (t *Transport) Close() { t.signer.Close() }

// RoundTrip 实现 http.RoundTripper。
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	hdr, err := t.header(req)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if req.Body != nil && req.Body != http.NoBody {
		body = req.Body
	}
	freq, err := fhttp.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), body)
	if err != nil {
		return nil, err
	}
	freq.ContentLength = req.ContentLength
	freq.GetBody = req.GetBody
	freq.Header = fhttp.Header(hdr)
	freq.Header[fhttp.HeaderOrderKey] = headerOrder

	resp, err := t.api.Do(freq)
	if err != nil {
		return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: err}
	}
	if resp.StatusCode == http.StatusUnauthorized && isPrismHost(req.URL) {
		t.forgetSession(req.Header)
	}
	return toResponse(resp, req), nil
}

// header 算出真正发出去的请求头：浏览器通用头以指纹为准；发往 Prism 的请求换成
// 浏览器那样的 Cookie（不带 Authorization），需要时附上 Sentinel token。
func (t *Transport) header(req *http.Request) (http.Header, error) {
	ctx := req.Context()
	hdr := http.Header{}
	for k, vs := range req.Header {
		if dropRequestHeader(k) {
			continue
		}
		hdr[k] = append([]string(nil), vs...)
	}
	t.browserHeaders(hdr)
	if !isPrismHost(req.URL) || strings.HasPrefix(req.URL.Path, "/y/") {
		// 别家站点不带 Prism 的 Cookie；Y-Sweet 文档接口（/y/d/<id>/…）只认文档令牌
		// （Authorization: Bearer），也不要 Sentinel
		hdr.Del("Cookie")
		return hdr, nil
	}
	if ck := t.cookie(ctx, req.Header); ck != "" {
		hdr.Set("Cookie", ck)
	}
	hdr.Del("Authorization")
	hdr.Del("X-Prism-Sentinel-Request-Id")
	if RequiresProof(req.Method, req.URL.Path, req.Header) {
		tok, err := t.signer.Token(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			t.log.Warn("Sentinel 签发失败，按前端做法发送占位令牌", "path", req.URL.Path, "err", err)
			tok = unavailableToken
		}
		hdr.Set(SentinelHeader, tok)
	}
	return hdr, nil
}

func isPrismHost(u *url.URL) bool { return strings.EqualFold(u.Hostname(), prismHost) }

// RequiresProof 照搬 Prism 前端 withPrismSentinel 的判定（requiresBrowserProof）：
// /api/ 下除登录、埋点等少数端点外都要；Server Action（next-action 头）与沙箱代理也要。
func RequiresProof(method, path string, h http.Header) bool {
	if method == http.MethodOptions {
		return false
	}
	if h.Get("Next-Action") != "" || strings.HasPrefix(path, "/s/sandboxes/") || strings.HasPrefix(path, "/s/codex_v2_") {
		return true
	}
	if !strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/api/ff/") || sandboxResourcesRe.MatchString(path) {
		return false
	}
	return !proofExempt[strings.TrimSuffix(path, "/")]
}

var (
	sandboxResourcesRe = regexp.MustCompile(`^/api/sandbox/proxy/sandbox-resources(?:/|$)`)
	proofExempt        = map[string]bool{
		"/api/auth/session": true, "/api/auth/anonymous-session": true, "/api/auth/redirect": true,
		"/api/auth/signout": true, "/api/auth/popup-callback": true, "/api/auth/popup-callback/set-code": true,
		"/api/maintenance": true, "/api/metrics": true, "/api/us": true, "/api/user-events": true,
	}
	// Chrome 发 fetch 时的请求头顺序（没列出的排在后面）。
	headerOrder = []string{
		"content-length", "sec-ch-ua-platform", "user-agent", "sec-ch-ua", "content-type",
		"sec-ch-ua-mobile", "accept", "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
		"referer", "accept-encoding", "accept-language", "cookie", "priority",
	}
)

func dropRequestHeader(k string) bool {
	switch strings.ToLower(k) {
	case "host", "content-length", "connection", "keep-alive", "proxy-connection", "transfer-encoding",
		"te", "trailer", "upgrade", "user-agent", "accept-language", "accept-encoding", "openai-sentinel-token",
		"cache-control", "pragma":
		return true
	}
	return strings.HasPrefix(strings.ToLower(k), "sec-ch-ua")
}

// browserHeaders 写入与指纹一致的浏览器通用头。
func (t *Transport) browserHeaders(h http.Header) {
	p := t.profile
	h.Set("User-Agent", p.UserAgent())
	h.Set("sec-ch-ua", p.SecCHUA())
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", p.Platform())
	h.Set("Accept-Language", p.AcceptLanguage())
	h.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	if h.Get("Accept") == "" {
		h.Set("Accept", "*/*")
	}
	if h.Get("Sec-Fetch-Mode") == "cors" && h.Get("Priority") == "" {
		h.Set("Priority", "u=1, i")
	}
}

// cookie 拼出发给 Prism 的 Cookie：access token + 新鲜的 session token（与旧 TLS 桥一致）。
// 请求里找不到 access token 时原样使用调用方的 Cookie。
func (t *Transport) cookie(ctx context.Context, h http.Header) string {
	orig := h.Get("Cookie")
	at := accessToken(h)
	if at == "" {
		return orig
	}
	s := t.sessionFor(at)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" || time.Since(s.at) > sessionTTL {
		if tok, err := t.exchange(ctx, at); err != nil {
			t.log.Warn("Prism 会话换发失败（沿用旧值）", "err", err)
		} else {
			s.token, s.at = tok, time.Now()
		}
	}
	ck := "prism_oai_access_token=" + at
	if s.token != "" {
		ck += "; prism_session_token=" + s.token
	} else if st := cookieValue(orig, "prism_session_token"); st != "" {
		ck += "; prism_session_token=" + st
	}
	return ck
}

func (t *Transport) sessionFor(at string) *session {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[at]
	if s == nil {
		s = &session{}
		t.sessions[at] = s
	}
	return s
}

func (t *Transport) forgetSession(h http.Header) {
	if at := accessToken(h); at != "" {
		s := t.sessionFor(at)
		s.mu.Lock()
		s.token = ""
		s.mu.Unlock()
	}
}

// exchange 用 access token 换一个新的 prism_session_token。
func (t *Transport) exchange(ctx context.Context, at string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := fhttp.NewRequestWithContext(ctx, http.MethodGet, "https://"+prismHost+"/auth/session", nil)
	if err != nil {
		return "", err
	}
	h := http.Header{
		"Accept":         {"*/*"},
		"Origin":         {"https://" + prismHost},
		"Referer":        {"https://" + prismHost + "/"},
		"Sec-Fetch-Site": {"same-origin"},
		"Sec-Fetch-Mode": {"cors"},
		"Sec-Fetch-Dest": {"empty"},
		"Cookie":         {"prism_oai_access_token=" + at},
	}
	t.browserHeaders(h)
	req.Header = fhttp.Header(h)
	req.Header[fhttp.HeaderOrderKey] = headerOrder
	resp, err := t.api.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	for _, c := range resp.Header.Values("Set-Cookie") {
		if v, ok := strings.CutPrefix(c, "prism_session_token="); ok {
			if i := strings.IndexByte(v, ';'); i >= 0 {
				v = v[:i]
			}
			if v != "" && resp.StatusCode == http.StatusOK {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("/auth/session HTTP %d，没有下发 prism_session_token", resp.StatusCode)
}

func accessToken(h http.Header) string {
	if v := cookieValue(h.Get("Cookie"), "prism_oai_access_token"); v != "" {
		return v
	}
	if v, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func cookieValue(all, name string) string {
	for _, p := range strings.Split(all, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(p), name+"="); ok {
			return v
		}
	}
	return ""
}

// toResponse 把 fhttp 的响应转成标准库的；压缩体在这里解开（Chrome 一定会协商压缩，
// 而上层期望拿到明文 —— 交给上层的响应不再带 Content-Encoding）。
func toResponse(r *fhttp.Response, req *http.Request) *http.Response {
	h := http.Header(r.Header)
	body := r.Body
	length := r.ContentLength
	if ce := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding"))); ce != "" && ce != "identity" && body != nil {
		body = fhttp.DecompressBodyByType(body, ce)
		h.Del("Content-Encoding")
		h.Del("Content-Length")
		length = -1
	}
	h.Del(fhttp.HeaderOrderKey)
	h.Del(fhttp.PHeaderOrderKey)
	return &http.Response{
		Status: r.Status, StatusCode: r.StatusCode,
		Proto: r.Proto, ProtoMajor: r.ProtoMajor, ProtoMinor: r.ProtoMinor,
		Header: h, Body: body, ContentLength: length, Request: req,
		Uncompressed: length == -1 && r.ContentLength != -1,
	}
}

// doer 让 Signer 通过 Chrome 指纹客户端访问 sentinel.openai.com 与页面。
type doer struct {
	c       tls_client.HttpClient
	profile *sentinel.Profile
}

func (d *doer) Do(ctx context.Context, r sentinel.Request) (int, []byte, error) {
	var body io.Reader
	if r.Body != nil {
		body = strings.NewReader(string(r.Body))
	}
	req, err := fhttp.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return 0, nil, err
	}
	h := http.Header{}
	for _, kv := range r.Header {
		h.Set(kv[0], kv[1])
	}
	p := d.profile
	h.Set("User-Agent", p.UserAgent())
	h.Set("sec-ch-ua", p.SecCHUA())
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", p.Platform())
	h.Set("Accept-Language", p.AcceptLanguage())
	h.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	req.Header = fhttp.Header(h)
	req.Header[fhttp.HeaderOrderKey] = headerOrder
	resp, err := d.c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rd := io.Reader(resp.Body)
	if ce := strings.ToLower(resp.Header.Get("Content-Encoding")); ce != "" && ce != "identity" {
		rd = fhttp.DecompressBodyByType(resp.Body, ce)
	}
	b, err := io.ReadAll(io.LimitReader(rd, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// IsPrism 判断上游是不是 prism.openai.com —— 只有它需要浏览器指纹与 Sentinel，
// 测试里的 httptest 服务器、自定义上游都走标准库。
func IsPrism(u *url.URL) bool {
	return u != nil && strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), prismHost)
}

var shared struct {
	mu      sync.Mutex
	signer  *sentinel.Signer
	byProxy map[string]*Transport
}

// Shared 返回进程内共享的传输层：同一出站代理复用一个（连接池随之共享），所有传输共用
// 一个签发器 —— 整个网关就像一个开着一个 Prism 标签页的浏览器。
func Shared(o Options) (*Transport, error) {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if t := shared.byProxy[o.Proxy]; t != nil {
		return t, nil
	}
	if shared.signer == nil {
		prof, err := loadProfile(o)
		if err != nil {
			return nil, err
		}
		if o.Logger == nil {
			o.Logger = slog.Default()
		}
		o.Logger.Info("浏览器指纹", "profile", sentinel.Describe(prof), "tls", TLSTemplate(prof))
		if msg := sentinel.Staleness(prof, time.Now()); msg != "" {
			o.Logger.Warn(msg)
		}
		cacheDir := o.CacheDir
		if cacheDir == "" {
			if d, err := os.UserCacheDir(); err == nil {
				cacheDir = filepath.Join(d, "oaiprism")
			}
		}
		sc, err := newClient(o.Proxy, tls_client.NewCookieJar(), prof)
		if err != nil {
			return nil, err
		}
		shared.signer, err = sentinel.New(sentinel.Options{
			Doer: &doer{c: sc, profile: prof}, Profile: prof, CacheDir: cacheDir, Logger: o.Logger,
		})
		if err != nil {
			return nil, err
		}
	}
	o.Signer = shared.signer
	t, err := New(o)
	if err != nil {
		return nil, err
	}
	if shared.byProxy == nil {
		shared.byProxy = map[string]*Transport{}
	}
	shared.byProxy[o.Proxy] = t
	return t, nil
}

// SharedSigner 返回共享签发器（还没建过时为 nil）。
func SharedSigner() *sentinel.Signer {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	return shared.signer
}
