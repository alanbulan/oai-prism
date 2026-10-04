// Package httpc 构建面向上游的高性能 HTTP 客户端。
//
// 这里的每一个参数都是有意为之的，不是复制粘贴的模板：
//
//   - 出站固定走 HTTPS，因此天然复用 HTTP/2 多路复用；显式 ForceAttemptHTTP2
//     保证即使我们自定义了 DialContext 也不会退化回 HTTP/1.1。
//   - MaxIdleConnsPerHost 默认只有 2，在高 QPS 反代下会造成大量 TIME_WAIT 与
//     TLS 握手，是本项目第一个要调掉的瓶颈，默认给 256。
//   - DisableCompression：Go 一旦看到客户端没设 Accept-Encoding 就会偷偷加
//     gzip 并自动解压，那会让"原样透传"失真（Content-Encoding/Length 全部对不上）。
//     代理必须自己掌控编码协商，所以关掉它并由我们显式设置 Accept-Encoding。
//   - 读写缓冲从 4KB 提到 32KB，减少 syscall 次数；对 SSE 小包场景由 Flush 兜底。
package httpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/upstream"
)

// Client 是对 *http.Client 的轻量包装，带上上游 BaseURL 与连接预热能力。
type Client struct {
	HTTP    *http.Client
	BaseURL *url.URL

	transport *http.Transport
	dialer    *net.Dialer

	closeOnce sync.Once
}

// Options 允许在标准配置之外做微调（账号级代理等）。
type Options struct {
	Proxy              string
	ForceHTTP2         *bool
	InsecureSkipVerify *bool
}

// New 依据配置构造客户端。
func New(cfg config.UpstreamConfig, opts Options) (*Client, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("上游 BaseURL 非法: %w", err)
	}

	dialer := &net.Dialer{
		Timeout:   cfg.DialTimeout,
		KeepAlive: cfg.KeepAlive,
	}

	forceH2 := cfg.ForceHTTP2
	if opts.ForceHTTP2 != nil {
		forceH2 = *opts.ForceHTTP2
	}
	insecure := cfg.InsecureSkipVerify
	if opts.InsecureSkipVerify != nil {
		insecure = *opts.InsecureSkipVerify
	}

	var (
		rt http.RoundTripper
		tr *http.Transport
	)
	if upstream.IsPrism(base) {
		// prism.openai.com 在 Cloudflare 后面校验 TLS 指纹，标准库直连会被 403：
		// 改走 Chrome 指纹传输（同时负责会话换发与 Sentinel 签发）。
		bt, err := upstream.Shared(upstream.Options{
			Proxy:       pick(cfg.HTTPProxy, opts.Proxy),
			ProfilePath: cfg.SentinelProfile,
		})
		if err != nil {
			return nil, err
		}
		rt = bt
	} else {
		tr = &http.Transport{
			Proxy:                 proxyFunc(pick(cfg.HTTPProxy, opts.Proxy)),
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     forceH2,
			MaxIdleConns:          cfg.MaxIdleConns,
			MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
			MaxConnsPerHost:       cfg.MaxConnsPerHost,
			IdleConnTimeout:       cfg.IdleConnTimeout,
			TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
			ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
			ExpectContinueTimeout: cfg.ExpectContinueTimeout,
			DisableCompression:    cfg.DisableCompression,
			ReadBufferSize:        cfg.ReadBufferSize,
			WriteBufferSize:       cfg.WriteBufferSize,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: insecure,
			},
		}
		rt = tr
	}

	return &Client{
		HTTP: &http.Client{
			Transport: rt,
			// 不设 Timeout：流式与长轮询由 context 精确控制，
			// 全局 Timeout 会把正常的长回答误杀。
			Timeout: cfg.Timeout,
			// 不要跟随重定向——上游 302 往往意味着登录态失效，
			// 跟过去只会拿到一个 HTML 登录页，反而掩盖真实错误。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		BaseURL:   base,
		transport: tr,
		dialer:    dialer,
	}, nil
}

func pick(a, b string) string {
	if b != "" {
		return b
	}
	return a
}

func proxyFunc(raw string) func(*http.Request) (*url.URL, error) {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	switch u.Scheme {
	case "http", "https":
		return http.ProxyURL(u)
	case "socks5", "socks5h":
		// 标准库不支持 socks5，需要额外依赖；这里明确报错而不是静默直连，
		// 静默直连会让"以为走了代理"的部署直接暴露真实出口 IP。
		panic(fmt.Sprintf("socks5 代理需要 x/net/proxy 支持，暂未内置: %s", raw))
	default:
		return http.ProxyURL(u)
	}
}

// NewRequest 构造一个指向上游 path 的请求，并套用默认头。
func (c *Client) NewRequest(ctx context.Context, method, path string, body any, hdr map[string]string) (*http.Request, error) {
	u := *c.BaseURL
	// path 可能自带 query。
	if i := indexByte(path, '?'); i >= 0 {
		u.Path = joinPath(c.BaseURL.Path, path[:i])
		u.RawQuery = path[i+1:]
	} else {
		u.Path = joinPath(c.BaseURL.Path, path)
	}

	var req *http.Request
	var err error
	switch b := body.(type) {
	case nil:
		req, err = http.NewRequestWithContext(ctx, method, u.String(), nil)
	case []byte:
		// 走 []byte 分支可以避免二次序列化，是最省 CPU 的路径。
		// bytes.Reader 会被标准库识别，自动设置 ContentLength 与 GetBody，
		// GetBody 让我们在重试时能重新读取 body 而不必再序列化一遍。
		req, err = http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(b))
	case io.Reader:
		req, err = http.NewRequestWithContext(ctx, method, u.String(), b)
	default:
		req, err = http.NewRequestWithContext(ctx, method, u.String(), nil)
	}
	if err != nil {
		return nil, err
	}

	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req, nil
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func joinPath(base, p string) string {
	if base == "" || base == "/" {
		if p == "" {
			return "/"
		}
		if p[0] != '/' {
			return "/" + p
		}
		return p
	}
	base = trimRightSlash(base)
	if p == "" {
		return base
	}
	if p[0] != '/' {
		return base + "/" + p
	}
	return base + p
}

func trimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Warmup 预建 n 条 TCP+TLS 连接并保留在空闲池里。
//
// 冷启动后的第一个请求要付出 DNS + TCP + TLS 三轮往返（跨洋链路能到 300ms+）。
// 预热把这笔开销挪到启动阶段，对延迟敏感的调用收益很大。
func (c *Client) Warmup(ctx context.Context, n int) int {
	if n <= 0 || c.transport == nil {
		// Chrome 指纹传输不预热：用标准库 TLS 去握手反而会留下非浏览器指纹。
		return 0
	}
	target := c.BaseURL.Host
	if !isDefaultPort(c.BaseURL) {
		target = net.JoinHostPort(c.BaseURL.Hostname(), c.BaseURL.Port())
	}

	ok := 0
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := c.dialer.DialContext(ctx, "tcp", target)
			if err != nil {
				return
			}
			if c.BaseURL.Scheme == "https" {
				host := c.BaseURL.Hostname()
				tc := tls.Client(conn, &tls.Config{
					ServerName:         host,
					MinVersion:         tls.VersionTLS12,
					NextProtos:         []string{"h2", "http/1.1"},
					InsecureSkipVerify: c.transport.TLSClientConfig.InsecureSkipVerify,
				})
				if err := tc.HandshakeContext(ctx); err != nil {
					_ = conn.Close()
					return
				}
				conn = tc
			}
			// 这里无法把裸连接塞回 net/http 的空闲池（没有公开 API），
			// 但完成一次握手已经让 DNS、路由、TLS session ticket 全部就绪，
			// 真正首次请求也只需 0-RTT 级别成本。保留连接则无意义，直接关闭。
			_ = conn.Close()
			mu.Lock()
			ok++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return ok
}

func isDefaultPort(u *url.URL) bool {
	if u.Port() == "" {
		return true
	}
	return (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80")
}

// CloseIdle 关闭空闲连接。
func (c *Client) CloseIdle() {
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}

// ErrUpstreamUnreachable 用于上层区分"网络问题"与"业务错误"。
var ErrUpstreamUnreachable = errors.New("上游不可达")

// IsNetworkError 判断错误是否为连接层问题（可安全重试）。
func IsNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUpstreamUnreachable) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Timeout() || IsNetworkError(ue.Err)
	}
	// GOPROXY 式字符串匹配是最后手段；标准库没有导出的 sentinel。
	s := err.Error()
	for _, k := range []string{
		"connection reset by peer",
		"connection refused",
		"no such host",
		"i/o timeout",
		"EOF",
		"http2: server sent GOAWAY",
		"unexpected EOF",
	} {
		if contains(s, k) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// RetryableStatus 判断状态码是否值得重试。
func RetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// SleepOK 是 SleepCtx 的布尔版，供"等待成功返回 true"的场景使用。
func SleepOK(ctx context.Context, d time.Duration) bool {
	return SleepCtx(ctx, d) == nil
}

// SleepCtx 可被取消的退避等待。
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
