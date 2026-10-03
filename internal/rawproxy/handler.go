// Package rawproxy 实现"原样反代"通道。
//
// 它与 facade 的区别是：facade 需要理解协议并做翻译，
// rawproxy 只做字节搬运 + 凭据注入。后者的价值在于——
//
//  1. 即使上游改了字段名、我们还没跟上，原样通道依然能用；
//  2. 用户自己的前端（比如直接从 prism.openai.com 抓下来改的前端）
//     可以无改动地指向本地代理。
//
// 因此这条通道是"保底可用"的，facade 是"用起来舒服"的。
package rawproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/capture"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/metrics"
	"github.com/oai-prism/oaiprism/internal/prism"
)

// Handler 是原样反代处理器。
type Handler struct {
	cfg    *config.Config
	log    *slog.Logger
	pool   *account.Pool
	client *prism.Client
	app    *metrics.App
	rec    *capture.Recorder

	bufPool sync.Pool
}

// New 构造处理器。
func New(cfg *config.Config, log *slog.Logger, pool *account.Pool, client *prism.Client, app *metrics.App, rec *capture.Recorder) *Handler {
	h := &Handler{cfg: cfg, log: log, pool: pool, client: client, app: app, rec: rec}
	h.bufPool = sync.Pool{
		New: func() any {
			size := cfg.RawProxy.BufferSize
			if size <= 0 {
				size = 64 << 10
			}
			b := make([]byte, size)
			return &b
		},
	}
	return h
}

// Register 挂载子树路由。
func (h *Handler) Register(mux *http.ServeMux) {
	if !h.cfg.RawProxy.Enabled {
		return
	}
	prefix := strings.TrimRight(h.cfg.RawProxy.Prefix, "/")
	if prefix == "" {
		prefix = "/prism"
	}
	// 以 "/" 结尾的模式匹配整棵子树——这正是我们需要的：
	// 上游端点众多且随时会新增，逐个登记不现实。
	mux.Handle(prefix+"/", h)
	mux.Handle(prefix, h)
}

// 跳转头（hop-by-hop）。这些头描述的是"这一段连接"，
// 原样透传会导致下一跳误解析连接语义。
var hopByHop = map[string]struct{}{
	"connection":          {},
	"proxy-connection":    {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	// 由我们按目标 body 重新设置。
	"content-length": {},
	// Host 由 http.Client 依据 URL 设置。
	"host": {},
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	prefix := strings.TrimRight(h.cfg.RawProxy.Prefix, "/")
	if prefix == "" {
		prefix = "/prism"
	}
	upstreamPath := strings.TrimPrefix(r.URL.Path, prefix)
	if upstreamPath == "" {
		upstreamPath = "/"
	}
	if !strings.HasPrefix(upstreamPath, "/") {
		upstreamPath = "/" + upstreamPath
	}
	if r.URL.RawQuery != "" {
		upstreamPath += "?" + r.URL.RawQuery
	}

	if !h.allowed(upstreamPath) {
		h.writeErr(w, http.StatusForbidden, "路径不在白名单内: "+upstreamPath+
			"（如需放开请修改 raw_proxy.allow_paths）")
		return
	}

	// 1) 取账号。
	lease, err := h.acquire(r)
	if err != nil {
		h.writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer lease.Release()

	acct := lease.Account
	cred := acct.Credential()
	if cred == nil || !cred.Usable() {
		h.writeErr(w, http.StatusServiceUnavailable, "账号凭据不可用")
		return
	}

	// 2) 读入站 body。
	body, tooLarge, err := h.readBody(r)
	if err != nil {
		h.writeErr(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	if tooLarge {
		h.writeErr(w, http.StatusRequestEntityTooLarge, "请求体超过上限")
		return
	}

	// 3) 组装出站请求头。
	hdr := make(http.Header, len(r.Header)+8)
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if _, skip := hopByHop[lk]; skip {
			continue
		}
		// 客户端的认证信息一律丢弃，换成池里账号的。
		// 这一步是安全边界：否则调用方可以用自己的 cookie 覆盖账号池。
		if lk == "authorization" || lk == "cookie" {
			continue
		}
		for _, vv := range v {
			hdr.Add(k, vv)
		}
	}
	h.setCredentials(hdr, cred)

	// 4) 发请求。
	p := prism.Principal{Client: acct.Client, Cred: cred, ExtraHeaders: nil, AccountID: acct.ID}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, err := h.client.Do(ctx, p, r.Method, upstreamPath, hdr, body, nil)
	if err != nil {
		h.pool.MarkResult(acct, &creds.APIError{Op: r.Method + " " + upstreamPath, Status: 502, Body: err.Error()}, 0)
		h.app.HTTPRequests.Inc(metricPath(upstreamPath), r.Method, "502")
		h.writeErr(w, http.StatusBadGateway, "上游请求失败: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		// 4xx 说明凭据或路径有问题，让账号池知道。
		h.pool.MarkResult(acct, &creds.APIError{Op: r.Method + " " + upstreamPath, Status: resp.StatusCode}, 0)
	} else {
		h.pool.MarkResult(acct, nil, 0)
	}

	// 5) 回写响应头。
	out := w.Header()
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if _, skip := hopByHop[lk]; skip {
			continue
		}
		// 关键安全点：绝不把上游的 Set-Cookie 透给调用方。
		// 那是账号池里账号的会话凭据，一旦泄漏等于把账号送人。
		if lk == "set-cookie" {
			continue
		}
		for _, vv := range v {
			out.Add(k, vv)
		}
	}
	out.Set("X-Oaiprism-Account", acct.ID)
	w.WriteHeader(resp.StatusCode)

	// 6) 流式搬运。
	written, copyErr := h.copyBody(w, resp)
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		h.log.Debug("原样反代中断", "path", upstreamPath, "err", copyErr)
	}

	if h.rec != nil {
		h.rec.RecordRaw("request", r.Method, upstreamPath, 0, acct.ID, hdr, body)
		h.rec.RecordRaw("response", r.Method, upstreamPath, resp.StatusCode, acct.ID, resp.Header, nil)
	}

	h.app.HTTPRequests.Inc(metricPath(upstreamPath), r.Method, itoa(resp.StatusCode))
	h.app.HTTPDuration.Observe(time.Since(started).Seconds(), metricPath(upstreamPath), r.Method)
	h.log.Debug("原样反代完成",
		"method", r.Method, "path", upstreamPath,
		"status", resp.StatusCode, "bytes", written,
		"account", acct.ID, "dur", time.Since(started).Round(time.Millisecond))
}

// copyBody 用池化缓冲搬运响应体。
//
// 对 SSE / 未知长度的流式响应逐块 Flush；
// 对已知长度的大文件则交给 net/http 自己的缓冲策略，避免无谓的 syscall。
func (h *Handler) copyBody(w http.ResponseWriter, resp *http.Response) (int64, error) {
	bp := h.bufPool.Get().(*[]byte)
	buf := *bp
	defer func() { h.bufPool.Put(bp) }()

	streaming := resp.ContentLength < 0 ||
		strings.Contains(resp.Header.Get("Content-Type"), "event-stream")

	if !streaming {
		n, err := io.CopyBuffer(w, resp.Body, buf)
		return n, err
	}

	fl, canFlush := w.(http.Flusher)
	var total int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			wn, werr := w.Write(buf[:n])
			total += int64(wn)
			if werr != nil {
				return total, werr
			}
			if canFlush {
				fl.Flush()
			}
			if wn < n {
				return total, io.ErrShortWrite
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

// readBody 读入站 body。
//
// 若开启抓包则最多缓冲 max_body 字节（我们需要看到完整报文才能校准协议），
// 否则直接透传 Reader。大文件上传始终走流式，避免把内存打爆。
func (h *Handler) readBody(r *http.Request) ([]byte, bool, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, false, nil
	}
	max := h.cfg.Server.MaxBodyBytes
	if max <= 0 {
		max = 64 << 20
	}

	needBuffer := h.rec != nil && r.ContentLength > 0 && r.ContentLength <= 4<<20
	if !needBuffer {
		limited := io.LimitReader(r.Body, max+1)
		b, err := io.ReadAll(limited)
		if err != nil {
			return nil, false, err
		}
		if int64(len(b)) > max {
			return nil, true, nil
		}
		return b, false, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > max {
		return nil, true, nil
	}
	return b, false, nil
}

// acquire 选账号。支持客户端自带凭据（若配置允许）与显式指定账号。
func (h *Handler) acquire(r *http.Request) (*account.Lease, error) {
	// 显式指定账号。
	if id := strings.TrimSpace(r.Header.Get("X-Oaiprism-Account")); id != "" {
		if a := h.pool.Get(id); a != nil && a.Acquire(time.Now()) {
			return &account.Lease{Account: a}, nil
		}
	}

	// 粘性键：用客户端提供的会话标识，或退化为"无粘性"。
	sticky := strings.TrimSpace(r.Header.Get("X-Oaiprism-Session"))
	return h.pool.Acquire(r.Context(), sticky)
}

// setCredentials 注入账号凭据与浏览器化请求头。
func (h *Handler) setCredentials(hdr http.Header, cred *creds.Credential) {
	if ck := cred.EffectiveCookie(); ck != "" {
		hdr.Set("Cookie", ck)
	}
	if cred.AccessToken != "" {
		hdr.Set("Authorization", "Bearer "+strings.TrimPrefix(cred.AccessToken, "Bearer "))
	}
	if hdr.Get("Origin") == "" {
		hdr.Set("Origin", h.cfg.Upstream.Origin)
	}
	if hdr.Get("Referer") == "" {
		hdr.Set("Referer", h.cfg.Upstream.Referer)
	}
	if hdr.Get("User-Agent") == "" || h.cfg.Upstream.UserAgent != "" {
		hdr.Set("User-Agent", h.cfg.Upstream.UserAgent)
	}
	if hdr.Get("Accept-Language") == "" {
		hdr.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	}
	for k, v := range h.cfg.Upstream.Headers {
		if hdr.Get(k) == "" {
			hdr.Set(k, v)
		}
	}
	for k, v := range cred.Headers {
		hdr.Set(k, v)
	}
}

// allowed 检查路径白名单。
func (h *Handler) allowed(path string) bool {
	list := h.cfg.RawProxy.AllowPaths
	if len(list) == 0 {
		return true
	}
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	for _, a := range list {
		if a == "" {
			continue
		}
		// 按路径段匹配：条目 "/api/y" 只放行 /api/y 与 /api/y/...，
		// 不能顺带放行 /api/yolo 这类恰好同前缀的其它端点。
		base := strings.TrimSuffix(a, "/")
		if p == a || p == base || strings.HasPrefix(p, base+"/") {
			return true
		}
	}
	return false
}

func (h *Handler) writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	buf := make([]byte, 0, 256)
	buf = append(buf, `{"error":{"message":"`...)
	buf = appendJSONEscaped(buf, msg)
	buf = append(buf, `","type":"proxy_error","code":"`...)
	buf = append(buf, itoa(status)...)
	buf = append(buf, `"}}`...)
	_, _ = w.Write(buf)
}

func appendJSONEscaped(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if c < 0x20 {
				dst = append(dst, ' ')
				continue
			}
			dst = append(dst, c)
		}
	}
	return dst
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [12]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// metricPath 把路径归一化成低基数标签。
//
// 原样反代的路径里带 UUID（/api/projects/{uuid}/thumbnail），
// 直接当标签会让时间序列数量随项目数线性增长，最终拖垮 Prometheus。
func metricPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		if len(s) == 36 && strings.Count(s, "-") == 4 {
			segs[i] = "{id}"
			continue
		}
		if len(s) >= 24 {
			segs[i] = "{id}"
		}
	}
	return "/" + strings.Join(segs, "/")
}
