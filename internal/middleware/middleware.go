// Package middleware 提供入站请求的横切关注点。
//
// 顺序很重要，链式顺序即代码顺序：
//
//	Recover -> RequestID -> Metrics -> RateLimit -> Auth -> handler
//
// Recover 必须在最外层，否则一次 panic 会带走整个连接且没有任何日志。
// Metrics 在 Auth 之前，这样鉴权失败也能被观测到。
package middleware

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/metrics"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
)

// RequestID 从上下文取出请求 ID。
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// WithRequestID 写入请求 ID。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// CtxKeyLogError 是"请求日志错误摘要"的通道键。
//
// SSE 场景下 HTTP 状态码早已是 200（响应头在首帧前就发出），
// 真正的失败只存在于事件流里 —— 审计中间件从响应上什么都看不到。
// 不记的话，请求流水里会出现"200 + 2ms + 无错误"的记录，
// 排障时完全看不出这次请求其实失败了（2026-10-03 Codex 断流排查实证）。
// facade 通过 RecordLogError 写入，审计中间件落库时一并记录。
type CtxKeyLogError struct{}

// LogErrorBox 是并发安全的错误收集器 —— handler 可能在多个
// goroutine 里写（流式主流程、心跳、桥回调），落库在另一个 goroutine 读。
type LogErrorBox struct {
	mu      sync.Mutex
	msgs    []string
	account string
	// 本次请求的 token 用量（facade 按实际收发内容精确计数后写入）
	promptTokens, completionTokens int
	aborted                        bool
	model                          string
}

// RecordLogModel 记录本次请求实际使用的模型（网关换过模型名时）。
//
// 流水默认记客户端请求的模型名；Claude Code 发的是 claude-sonnet-4-5 这类名字，网关
// 换成默认的 GPT 模型发给上游，流水里却显示 Claude 模型（2026-10-05 用户反馈）。
func RecordLogModel(r *http.Request, model string) {
	if box, ok := r.Context().Value(CtxKeyLogError{}).(*LogErrorBox); ok && model != "" {
		box.mu.Lock()
		box.model = model
		box.mu.Unlock()
	}
}

// LogModel 取出 RecordLogModel 记录的模型。
func LogModel(ctx context.Context) string {
	if box, ok := ctx.Value(CtxKeyLogError{}).(*LogErrorBox); ok {
		box.mu.Lock()
		defer box.mu.Unlock()
		return box.model
	}
	return ""
}

// RecordLogAbort 记录"客户端在回答结束前断开了连接"。流式响应头早已是 200，
// 不记的话这类请求在流水里是一条没有错误的成功记录（2026-10-05：Claude Code
// 等满 5 分钟没收到数据后断开，流水显示 200）。落库时按 499 记。
func RecordLogAbort(r *http.Request) {
	if box, ok := r.Context().Value(CtxKeyLogError{}).(*LogErrorBox); ok {
		box.mu.Lock()
		box.aborted = true
		box.mu.Unlock()
	}
}

// LogAborted 判断本请求是否被客户端中途断开。
func LogAborted(ctx context.Context) bool {
	if box, ok := ctx.Value(CtxKeyLogError{}).(*LogErrorBox); ok {
		box.mu.Lock()
		defer box.mu.Unlock()
		return box.aborted
	}
	return false
}

// RecordLogUsage 记录本次请求的 token 用量。
// 续接失败后重试会再次写入：以最后一次成功运行为准（失败的那次不会写）。
func RecordLogUsage(r *http.Request, prompt, completion int) {
	if box, ok := r.Context().Value(CtxKeyLogError{}).(*LogErrorBox); ok {
		box.mu.Lock()
		box.promptTokens, box.completionTokens = prompt, completion
		box.mu.Unlock()
	}
}

// LogUsage 取出 RecordLogUsage 记录的用量。
func LogUsage(ctx context.Context) (prompt, completion int) {
	if box, ok := ctx.Value(CtxKeyLogError{}).(*LogErrorBox); ok {
		box.mu.Lock()
		defer box.mu.Unlock()
		return box.promptTokens, box.completionTokens
	}
	return 0, 0
}

// RecordLogAccount 记录本次请求实际路由到的账号。
//
// 不能靠请求头回写：X-Oaiprism-Account 是客户端可控的输入，
// 失败时流水里就会出现调用方伪造的账号。
func RecordLogAccount(r *http.Request, accountID string) {
	if box, ok := r.Context().Value(CtxKeyLogError{}).(*LogErrorBox); ok && accountID != "" {
		box.mu.Lock()
		box.account = accountID
		box.mu.Unlock()
	}
}

// LogAccount 取出 RecordLogAccount 记录的账号。
func LogAccount(ctx context.Context) string {
	if box, ok := ctx.Value(CtxKeyLogError{}).(*LogErrorBox); ok {
		box.mu.Lock()
		defer box.mu.Unlock()
		return box.account
	}
	return ""
}

func (b *LogErrorBox) append(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 摘要截断：错误文本可能包含整段上游响应体。
	if len(msg) > 400 {
		msg = msg[:400]
	}
	b.msgs = append(b.msgs, msg)
}

func (b *LogErrorBox) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.msgs))
	copy(out, b.msgs)
	return out
}

// RecordLogError 供 handler 在"响应已开始后的内部失败"时记录错误摘要。
// 未经过审计中间件的请求（单测直调 handler）调用它是无害的 no-op。
func RecordLogError(r *http.Request, format string, args ...any) {
	if box, ok := r.Context().Value(CtxKeyLogError{}).(*LogErrorBox); ok {
		box.append(fmt.Sprintf(format, args...))
	}
}

// LogErrors 取出本请求已记录的错误摘要（审计中间件用）。
func LogErrors(ctx context.Context) []string {
	if box, ok := ctx.Value(CtxKeyLogError{}).(*LogErrorBox); ok {
		return box.snapshot()
	}
	return nil
}

// Middleware 是标准签名。
type Middleware func(http.Handler) http.Handler

// Chain 按给定顺序组合中间件。第一个是最外层。
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// ---------------------------- Recover ----------------------------

// Recover 捕获 panic，返回 500 并记录堆栈。
func Recover(log *slog.Logger, app *metrics.App) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler 是标准库约定的"静默中断"，
					// 不该记成错误，直接重新抛出交给 net/http 处理。
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					if app != nil {
						app.Panics.Inc()
					}
					log.Error("handler panic",
						"path", r.URL.Path,
						"method", r.Method,
						"panic", rec,
						"stack", string(debug.Stack()))
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"message":"内部错误","type":"server_error"}}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------- CORS ----------------------------

// CORS 处理跨域，origin 为空时不启用。
//
// 两个刻意的设计：
//  1. 预检（OPTIONS）在这里直接终结，不打到业务路由 ——
//     否则每个跨域请求都会多一次无意义的完整路由匹配。
//  2. 不启用时返回透传的 next，零成本（没有闭包判断，只有一次赋值）。
func CORS(origin string) Middleware {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	allowHeaders := "authorization, content-type, x-prism-conversation-id, x-oaiprism-session, x-oaiprism-previous"
	// Expose-Headers 是关键：自定义响应头（尤其是 x-prism-conversation-id）
	// 不在 CORS 安全列表里，浏览器默认不给 JS 读。少了它，跨域客户端
	// 拿不到会话 ID，多轮续写直接断链 —— 而且不报错，只是"每次都是新会话"。
	exposeHeaders := "x-prism-conversation-id, x-request-id"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
			w.Header().Set("Access-Control-Expose-Headers", exposeHeaders)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---------------------------- RequestID ----------------------------

// RequestIDMiddleware 为每个请求生成/透传请求 ID。
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = r.Header.Get("X-Correlation-Id")
		}
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

var reqIDState uint64 = 0x2545f4914f6cdd1d
var reqIDMu sync.Mutex

// newRequestID 生成一个短 ID。
//
// 这里用 mutex 保护 xorshift 是有意的：请求 ID 生成频率是"每请求一次"，
// 远低于 SSE 增量写出的频率，为它做 CAS 循环是过度优化；
// 而 mutex 版本更容易读对。
func newRequestID() string {
	reqIDMu.Lock()
	s := reqIDState
	s ^= s << 13
	s ^= s >> 7
	s ^= s << 17
	reqIDState = s
	reqIDMu.Unlock()

	const alphabet = "0123456789abcdef"
	var buf [16]byte
	for i := range buf {
		buf[i] = alphabet[s&15]
		s = s>>4 | (uint64(time.Now().UnixNano()&0xF) << 60)
	}
	return "req_" + string(buf[:])
}

// ---------------------------- Metrics ----------------------------

// statusRecorder 记录状态码与写出字节数。
//
// 必须实现 Flusher，否则包一层就会让所有流式响应的 Flush 失效——
// 这是包装 ResponseWriter 最经典的坑。
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	flush   func()
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if s.flush != nil {
		s.flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层 writer。
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// MetricsMiddleware 采集入站指标。
func MetricsMiddleware(app *metrics.App) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if app == nil {
				next.ServeHTTP(w, r)
				return
			}
			path := routeLabel(r.URL.Path)
			start := time.Now()

			rec := &statusRecorder{ResponseWriter: w}
			if f, ok := w.(http.Flusher); ok {
				rec.flush = f.Flush
			}

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			code := itoa(rec.status)
			app.HTTPRequests.Inc(path, r.Method, code)
			app.HTTPDuration.Observe(time.Since(start).Seconds(), path, r.Method)
		})
	}
}

// routeLabel 把路径归一化成低基数标签。
//
// 直接用原始 path 会让指标基数爆炸（每个 UUID 一条时间序列），
// 最终把 Prometheus 打挂。这是监控里最常见的自伤方式之一。
func routeLabel(p string) string {
	switch {
	case p == "/":
		return "/"
	case strings.HasPrefix(p, "/v1/chat/completions"):
		return "/v1/chat/completions"
	case strings.HasPrefix(p, "/v1/completions"):
		return "/v1/completions"
	case strings.HasPrefix(p, "/v1/responses"):
		return "/v1/responses"
	case strings.HasPrefix(p, "/v1/messages"):
		return "/v1/messages"
	case strings.HasPrefix(p, "/v1/models"):
		return "/v1/models"
	case strings.HasPrefix(p, "/healthz"):
		return "/healthz"
	case strings.HasPrefix(p, "/readyz"):
		return "/readyz"
	case strings.HasPrefix(p, "/metrics"):
		return "/metrics"
	case strings.HasPrefix(p, "/admin/"):
		return p
	}
	// 原样反代通道：按最后两段归类，保留端点语义但去掉 UUID。
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) >= 2 {
		last := segs[len(segs)-1]
		if len(last) >= 32 || looksLikeUUID(last) {
			if len(segs) >= 3 {
				return "/" + strings.Join(segs[:len(segs)-2], "/") + "/*"
			}
			if len(segs) >= 2 {
				return "/" + segs[0] + "/*"
			}
		}
	}
	if len(segs) > 3 {
		return "/" + strings.Join(segs[:3], "/")
	}
	return p
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ---------------------------- Auth ----------------------------

// Tenant 返回本次请求的调用方身份（API Key 指纹）。
//
// 用于会话状态的租户隔离：两个不同 Key 的调用方即便发出完全相同的
// 首条消息，也绝不能共享会话链（上一轮句柄 / 项目 / 历史）。
// 未经鉴权中间件（或本机免 Key 访问）时返回空串。
func Tenant(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyPrincipal).(string); ok {
		return v
	}
	return ""
}

// WithTenant 写入调用方身份（测试与内部调用用）。
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, tenant)
}

// tenantOf 把 Key 压成不可逆的短指纹：会话键与日志里只出现指纹，不出现 Key 本身。
func tenantOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "k:" + hex.EncodeToString(sum[:8])
}

// AuthOptions 描述鉴权策略。
type AuthOptions struct {
	// StaticKeys 来自配置文件（facade.api_keys）。持有者同时具备管理权限 ——
	// 它们是运维显式下发的，与 Dashboard 签发的普通调用 Key 区分开。
	StaticKeys []string
	// DynamicKeys 返回 Dashboard 签发并持久化在 SQLite 里的 Key（可为 nil）。
	// 每请求调用一次，实现方需自行缓存。
	DynamicKeys func() []string
	// AdminToken 校验 /admin/login 签发的会话令牌（可为 nil）。
	AdminToken func(token string) bool
	// ExemptPaths 精确豁免（探针端点）。
	ExemptPaths []string
	// ExemptPrefixes 前缀豁免（Dashboard 静态资源、OAuth 浏览器回调等）。
	ExemptPrefixes []string
	// AdminPrefix 管理端前缀（默认 /admin/）。
	AdminPrefix string
	// CORSOrigin 是额外允许的跨域来源（管理端写操作的 Origin 校验用）。
	CORSOrigin string

	App *metrics.App
}

// Auth 是统一鉴权中间件。
//
// 规则（安全默认值优先）：
//  1. 没有任何可用 Key（配置与 Dashboard 都为空）时，只放行本机请求 ——
//     监听 0.0.0.0 却完全不设防，等于把账号池与管理端交给整个局域网；
//  2. 存在 Key 时，所有非豁免路由都必须带有效 Key（或管理会话令牌）；
//  3. 管理端（/admin/*）要求更高权限：管理会话令牌、配置文件里的 Key，
//     或"本机 + 有效 Key"。Dashboard 签发的普通 Key 从远端不能管理账号；
//  4. 管理端写操作做 Origin 校验，挡住浏览器跨站请求（CSRF / DNS rebinding）。
//
// 比较一律 constant-time。
func Auth(o AuthOptions) Middleware {
	exempt := map[string]struct{}{"/healthz": {}, "/readyz": {}, "/metrics": {}}
	for _, p := range o.ExemptPaths {
		if p = strings.TrimSpace(p); p != "" {
			exempt[p] = struct{}{}
		}
	}
	static := toByteKeys(o.StaticKeys)
	adminPrefix := o.AdminPrefix
	if adminPrefix == "" {
		adminPrefix = "/admin/"
	}
	corsOrigin := strings.TrimRight(strings.TrimSpace(o.CORSOrigin), "/")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 探针端点豁免鉴权：k8s / Docker / 监控系统探针拿 401 会导致容器被反复重启。
			if _, ok := exempt[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			for _, p := range o.ExemptPrefixes {
				if p != "" && strings.HasPrefix(r.URL.Path, p) {
					next.ServeHTTP(w, r)
					return
				}
			}

			key := requestKey(r)
			var dynamic [][]byte
			if o.DynamicKeys != nil {
				dynamic = toByteKeys(o.DynamicKeys())
			}
			isStatic := matchAny(static, key)
			isAdminToken := key != "" && o.AdminToken != nil && o.AdminToken(key)
			valid := isStatic || isAdminToken || matchAny(dynamic, key)
			local := IsLocalRequest(r)
			anyKeys := len(static) > 0 || len(dynamic) > 0

			switch {
			case valid:
			case !anyKeys && local:
				// 尚未配置任何 Key 的本机单用户场景：放行。
			default:
				if o.App != nil {
					o.App.AuthFailed.Inc()
				}
				msg := "API Key 无效或缺失"
				if !anyKeys {
					msg = "尚未配置任何 API Key：仅允许本机访问。请在 Dashboard 生成 Key 或配置 facade.api_keys"
				}
				writeAuthError(w, http.StatusUnauthorized, "invalid_api_key", msg)
				return
			}

			if strings.HasPrefix(r.URL.Path, adminPrefix) {
				if !(isAdminToken || isStatic || local) {
					writeAuthError(w, http.StatusForbidden, "admin_forbidden",
						"管理接口仅允许本机、管理员会话或配置文件中的 API Key 访问")
					return
				}
				if !safeAdminOrigin(r, corsOrigin) {
					writeAuthError(w, http.StatusForbidden, "cross_origin_forbidden", "拒绝跨站管理请求")
					return
				}
			}

			tenant := ""
			if key != "" && valid {
				tenant = tenantOf(key)
			}
			next.ServeHTTP(w, r.WithContext(WithTenant(r.Context(), tenant)))
		})
	}
}

// APIKeyAuth 是 Auth 的静态 Key 简化版（兼容老调用点与测试）。
func APIKeyAuth(keys []string, app *metrics.App, enabled bool, exemptPaths ...string) Middleware {
	if !enabled {
		return func(next http.Handler) http.Handler { return next }
	}
	return Auth(AuthOptions{StaticKeys: keys, ExemptPaths: exemptPaths, App: app})
}

// requestKey 兼容三种常见携带方式。
func requestKey(r *http.Request) string {
	key := bearerToken(r.Header.Get("Authorization"))
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("api-key"))
	}
	return key
}

func toByteKeys(keys []string) [][]byte {
	out := make([][]byte, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, []byte(k))
		}
	}
	return out
}

func writeAuthError(w http.ResponseWriter, status int, code, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="oaiprism"`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{"error": map[string]string{
		"message": msg, "type": "invalid_request_error", "code": code,
	}})
	_, _ = w.Write(body)
}

// IsLocalRequest 判断请求是否来自本机。
//
// 两个条件缺一不可：
//   - 对端地址是回环地址；
//   - Host 头是 localhost 或 IP 字面量 —— 防 DNS rebinding：恶意网页把
//     自己的域名解析到 127.0.0.1 后，浏览器发来的 Host 仍是那个域名。
func IsLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "" || strings.EqualFold(h, "localhost") || strings.HasSuffix(strings.ToLower(h), ".localhost") {
		return true
	}
	return net.ParseIP(h) != nil
}

// safeAdminOrigin 对管理端写操作做 Origin 校验。
//
// 浏览器对跨站请求必带 Origin；非浏览器客户端（curl/脚本）不带，放行。
// Origin 必须与 Host 同源，或等于配置的 CORS 来源。
func safeAdminOrigin(r *http.Request, corsOrigin string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// 不带 Origin 的跨站请求只可能来自非浏览器；Sec-Fetch-Site 再兜一层。
		site := r.Header.Get("Sec-Fetch-Site")
		return site == "" || site == "same-origin" || site == "none"
	}
	origin = strings.TrimRight(origin, "/")
	if corsOrigin != "" && strings.EqualFold(origin, corsOrigin) {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func bearerToken(h string) string {
	h = strings.TrimSpace(h)
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func matchAny(valid [][]byte, key string) bool {
	if key == "" {
		return false
	}
	kb := []byte(key)
	ok := false
	for _, v := range valid {
		// 不 break：让比较次数与 key 数量固定，不泄漏"命中了第几个"。
		if subtle.ConstantTimeCompare(v, kb) == 1 {
			ok = true
		}
	}
	return ok
}

// ---------------------------- RateLimit ----------------------------

// RateLimiter 是全局令牌桶。
//
// 单实例场景下不需要分布式限流；跨实例时应当在网关层做。
// 这里的目的是保护上游账号不被自己的重试打爆。
type RateLimiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64
	last   time.Time

	app *metrics.App
}

// NewRateLimiter 构造限流器。rate<=0 表示不限流。
func NewRateLimiter(rate float64, burst int, app *metrics.App) *RateLimiter {
	if rate <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = int(rate)
		if burst < 1 {
			burst = 1
		}
	}
	return &RateLimiter{
		tokens: float64(burst),
		burst:  float64(burst),
		rate:   rate,
		last:   time.Now(),
		app:    app,
	}
}

// Middleware 返回限流中间件。
func (l *RateLimiter) Middleware() Middleware {
	if l == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.allow(time.Now()) {
				if l.app != nil {
					l.app.RateLimited.Inc()
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"请求过于频繁，请稍后重试","type":"rate_limit_exceeded"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (l *RateLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens += elapsed.Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
	}
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	return false
}

// ---------------------------- 工具 ----------------------------

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
