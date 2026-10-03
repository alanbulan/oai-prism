package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
)

// dynamicKeys 缓存 Dashboard 签发、持久化在 SQLite 里的 API Key。
//
// 鉴权每个请求都要用，不能每次查库；签发/注销后主动失效，
// 另有短 TTL 兜底（多实例共享同一个库时也能在几秒内生效）。
type dynamicKeys struct {
	store *account.SQLiteStore
	ttl   time.Duration

	mu     sync.Mutex
	keys   []string
	loaded time.Time
}

func newDynamicKeys(store *account.SQLiteStore) *dynamicKeys {
	return &dynamicKeys{store: store, ttl: 5 * time.Second}
}

// Get 返回当前有效的 Key 列表。
func (d *dynamicKeys) Get() []string {
	if d == nil || d.store == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.loaded) < d.ttl {
		return d.keys
	}
	list, err := d.store.ListAPIKeys()
	if err != nil {
		// 读库失败时沿用上一份：瞬时故障不应让所有调用方突然 401。
		return d.keys
	}
	keys := make([]string, 0, len(list))
	for _, it := range list {
		if k := strings.TrimSpace(it.Key); k != "" {
			keys = append(keys, k)
		}
	}
	d.keys, d.loaded = keys, time.Now()
	return d.keys
}

// Invalidate 让下一次 Get 重新读库（签发/注销 Key 后调用）。
func (d *dynamicKeys) Invalidate() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.loaded = time.Time{}
	d.mu.Unlock()
}

// adminSessions 是 /admin/login 签发的管理会话令牌（内存态，重启即失效）。
type adminSessions struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]time.Time
}

func newAdminSessions() *adminSessions {
	return &adminSessions{ttl: 12 * time.Hour, m: make(map[string]time.Time)}
}

// Issue 签发一个随机令牌。
func (a *adminSessions) Issue() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	tok := "adm_" + hex.EncodeToString(b[:])
	now := time.Now()
	a.mu.Lock()
	for k, exp := range a.m {
		if now.After(exp) {
			delete(a.m, k)
		}
	}
	a.m[tok] = now.Add(a.ttl)
	a.mu.Unlock()
	return tok
}

// Valid 校验令牌。
func (a *adminSessions) Valid(tok string) bool {
	if a == nil || !strings.HasPrefix(tok, "adm_") {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.m, tok)
		return false
	}
	return true
}

// handleAdminLogin 校验管理员密码并签发会话令牌。
//
// 早期实现接受 admin/admin123/admin/空密码，返回写死的令牌，而且没有任何
// 管理接口校验它 —— 登录形同虚设。现在密码只来自配置（server.admin_password
// 或 OAI_PRISM_ADMIN_PASSWORD），未配置时不开放密码登录。
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)

	want := s.cfg.Server.AdminPassword
	if want == "" {
		writeAdminErr(w, http.StatusForbidden,
			"未配置管理员密码（server.admin_password / OAI_PRISM_ADMIN_PASSWORD），不开放密码登录。"+
				"本机请求携带任一有效 API Key 即可管理（尚未创建任何 Key 时本机免 Key）；远程管理请使用配置文件中的 API Key")
		return
	}
	user := s.cfg.Server.AdminUser
	if user == "" {
		user = "admin"
	}
	okUser := subtle.ConstantTimeCompare([]byte(req.Username), []byte(user)) == 1
	okPass := subtle.ConstantTimeCompare([]byte(req.Password), []byte(want)) == 1
	if !okUser || !okPass {
		// 固定延迟，拖慢在线爆破。
		time.Sleep(time.Second)
		writeAdminErr(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"username": user,
		"role":     "superadmin",
		"token":    s.admins.Issue(),
	})
}

// clientIP 取审计用的客户端地址。
//
// X-Forwarded-For / X-Real-IP 是客户端可伪造的请求头，只在对端本身是
// 本机或内网（典型的同机/内网反向代理）时才采信。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if v := strings.TrimSpace(first); v != "" {
				return v
			}
		}
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
			return v
		}
	}
	return host
}
