// Package creds 负责"认证凭据"的建模、解析与自愈。
//
// 这里刻意不假设凭据形态。OpenAI 体系里能拿到的东西至少有四种，
// 而调用方会拿到哪一种完全取决于他怎么登的录：
//
//  1. 浏览器整串 Cookie          __Secure-next-auth.session-token=...; ...
//  2. 只有 session-token 的值    -> 可以换出 accessToken
//  3. 已经有 JWT accessToken     -> 直接当 Bearer 用
//  4. 有 OAuth refresh_token     -> 长期最稳，可无限自动续期
//
// 本包统一把这四种收敛成一个不可变的 Credential 值对象，
// 上层只关心"能不能用"和"还能用多久"。
package creds

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// 常见 Cookie 名。
//
// ⚠️ 重要更正：Prism **不使用** next-auth 的 `__Secure-next-auth.session-token`。
// 实测（浏览器 DevTools 的 Cookie 表）Prism 有自己的一套 `prism_*` cookie：
//
//	prism_oai_access_token       ← 真正的 Bearer JWT（最关键）
//	prism_session_token          ← Prism 自己的会话 JWT（HS256，约 12 小时）
//	prism_oai_refresh_token      ← OAuth refresh token（rt.1.*，约 90 天）
//	prism_oai_earliest_refresh_at ← 允许刷新该 token 的最早时间戳
//	prism-did / oai-did          ← 设备标识
//
// next-auth 那几个名字仍然保留：Prism 的前身/ChatGPT 侧可能用到，
// 也可能是别的同类站点在用，保留不会有害。
const (
	CookieSessionToken      = "__Secure-next-auth.session-token"
	CookieSessionTokenLoose = "next-auth.session-token" // 非 Secure 部署下的旧名
	CookieAuthSession       = "__Secure-authjs.session-token"

	// Prism 专有。
	CookiePrismAccessToken     = "prism_oai_access_token"
	CookiePrismSessionToken    = "prism_session_token"
	CookiePrismRefreshToken    = "prism_oai_refresh_token"
	CookiePrismEarliestRefresh = "prism_oai_earliest_refresh_at"
	CookiePrismDeviceID        = "prism-did"

	CookieCFClearance = "cf_clearance"
	CookieDeviceID    = "oai-did"
)

// allSessionCookieNames 是"能换出 access token 的会话 cookie"全集。
var allSessionCookieNames = []string{
	CookiePrismSessionToken,
	CookieSessionToken,
	CookieSessionTokenLoose,
	CookieAuthSession,
}

// Credential 是一次认证所需的全部材料快照。
//
// 不可变：任何刷新都生成新对象并整体替换，避免读写竞争与"半更新"状态。
type Credential struct {
	// 身份元信息（从 JWT / session 接口解析得到）。
	AccountID string
	Email     string
	Plan      string
	UserID    string

	// AccessToken 是 chatgpt 的 JWT。
	AccessToken string
	// RefreshToken 是 OAuth refresh token，有它就能自愈。
	RefreshToken string
	// SessionToken 是会话 cookie 的值。
	SessionToken string

	// SessionCookieName 记录上面那个值来自哪个 cookie 名。
	//
	// 为什么需要它：Prism 用 prism_session_token，next-auth 系用
	// __Secure-next-auth.session-token。如果拼 Cookie 头时写错名字，
	// 上游会认为"没有会话"，表现为一次莫名其妙的 401。
	// 空值表示"按目标站点的约定选默认"（本项目默认 Prism）。
	SessionCookieName string

	// CookieHeader 是完整 Cookie 串（可选）。若为空则由上面的字段拼装。
	CookieHeader string

	// ExpiresAt 是 access token 的过期时间；零值表示未知。
	ExpiresAt time.Time

	// OAuthClientID 是签发 RefreshToken 的 OAuth client；空值时刷新用全局 creds.oauth_client_id。
	// 只用于本地刷新，绝不随请求发往上游。
	OAuthClientID string

	// Headers 是账号级附加头（设备指纹等），会原样转发给上游。
	Headers map[string]string

	// Source 记录凭据来源，便于排障：static / file / env / passthrough / oauth。
	Source string

	// UpdatedAt 上次刷新时间。
	UpdatedAt time.Time
}

// Usable 判断这份凭据是否具备发起请求的最低条件。
func (c *Credential) Usable() bool {
	if c == nil {
		return false
	}
	return c.AccessToken != "" || c.SessionToken != "" || c.CookieHeader != ""
}

// Expired 判断是否已过期。
func (c *Credential) Expired(now time.Time) bool {
	if c == nil || c.ExpiresAt.IsZero() {
		return false // 未知过期时间 -> 乐观认为可用，由 401 兜底
	}
	return !now.Before(c.ExpiresAt)
}

// NeedsRefresh 判断是否需要在 skew 窗口内提前刷新。
func (c *Credential) NeedsRefresh(now time.Time, skew time.Duration) bool {
	if c == nil || c.ExpiresAt.IsZero() {
		return false
	}
	return now.Add(skew).After(c.ExpiresAt)
}

// CanRefresh 判断是否具备自愈能力。
func (c *Credential) CanRefresh() bool {
	if c == nil {
		return false
	}
	return c.RefreshToken != "" || c.SessionToken != "" || HasSessionCookie(c.CookieHeader)
}

// EffectiveCookie 返回最终要发送的 Cookie 头。
func (c *Credential) EffectiveCookie() string {
	if c == nil {
		return ""
	}
	if c.CookieHeader != "" {
		return c.CookieHeader
	}
	var sb strings.Builder
	if c.SessionToken != "" {
		name := c.SessionCookieName
		if name == "" {
			// 目标站点是 Prism，默认用它自己的 cookie 名。
			name = CookiePrismSessionToken
		}
		sb.Grow(len(name) + len(c.SessionToken) + 2)
		sb.WriteString(name)
		sb.WriteByte('=')
		sb.WriteString(c.SessionToken)
	}
	if c.RefreshToken != "" && !strings.Contains(c.CookieHeader, CookiePrismRefreshToken) {
		if sb.Len() > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(CookiePrismRefreshToken)
		sb.WriteByte('=')
		sb.WriteString(c.RefreshToken)
	}
	if c.AccountID != "" && !strings.Contains(c.CookieHeader, CookieDeviceID) {
		if sb.Len() > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(CookieDeviceID)
		sb.WriteByte('=')
		sb.WriteString(c.AccountID)
	}
	return sb.String()
}

// Clone 深拷贝一份，供刷新流程基于旧值构造新值。
func (c *Credential) Clone() *Credential {
	if c == nil {
		return &Credential{}
	}
	n := *c
	if c.Headers != nil {
		n.Headers = make(map[string]string, len(c.Headers))
		for k, v := range c.Headers {
			n.Headers[k] = v
		}
	}
	return &n
}

// FromAccountConfig 把配置里的账号定义转成初始凭据。
func FromAccountConfig(a config.AccountConfig) *Credential {
	c := &Credential{
		AccountID:     a.AccountID,
		Email:         a.Email,
		Plan:          a.Plan,
		AccessToken:   strings.TrimSpace(a.AccessToken),
		RefreshToken:  strings.TrimSpace(a.RefreshToken),
		SessionToken:  strings.TrimSpace(a.SessionToken),
		CookieHeader:  strings.TrimSpace(a.Cookies),
		OAuthClientID: a.EffectiveOAuthClientID(),
		Headers:       upstreamHeaders(a.Headers),
		Source:        "static",
		UpdatedAt:     time.Now(),
	}
	if c.RefreshToken != "" {
		c.Source = "oauth"
	}

	// cookie_map 形式：{name: value} 拼成 header。
	if len(a.CookieMap) > 0 {
		parts := make([]string, 0, len(a.CookieMap))
		for k, v := range a.CookieMap {
			parts = append(parts, k+"="+v)
		}
		joined := strings.Join(parts, "; ")
		if c.CookieHeader == "" {
			c.CookieHeader = joined
		} else {
			c.CookieHeader = c.CookieHeader + "; " + joined
		}
	}

	// 从 Cookie 串里补出各种凭据。
	// 顺序有意为之：Prism 自己的 cookie 优先于 next-auth 那套。
	if c.SessionToken == "" {
		if name, v := CookieValueWithName(c.CookieHeader, allSessionCookieNames...); v != "" {
			c.SessionToken = v
			c.SessionCookieName = name
		}
	}
	// 整串 Cookie 里已经带了 access token 时，直接取出来 ——
	// 这样 JWT 的 exp / email / account_id 都能就地解析，
	// 不必先发一次 /api/auth/session。
	if c.AccessToken == "" {
		if v := CookieValue(c.CookieHeader, CookiePrismAccessToken); v != "" {
			c.AccessToken = v
		}
	}
	if c.RefreshToken == "" {
		if v := CookieValue(c.CookieHeader, CookiePrismRefreshToken); v != "" {
			c.RefreshToken = v
		}
	}
	if c.AccountID == "" {
		if v := CookieValue(c.CookieHeader, CookiePrismDeviceID, CookieDeviceID); v != "" {
			c.AccountID = v
		}
	}

	if a.ExpiresAt != nil {
		c.ExpiresAt = *a.ExpiresAt
	}

	// 有 JWT 就地解析出 exp / email / account_id，省掉一次 /api/auth/session 往返。
	if c.AccessToken != "" {
		c.applyJWT(strings.TrimPrefix(c.AccessToken, "Bearer "))
	}
	return c
}

// upstreamHeaders 返回要随请求转发给上游的账号级附加头：剔除旧版混在里面的
// oauth_client_id（本地刷新用的元数据，已由 EffectiveOAuthClientID 读走）。
// 不修改入参 —— 它可能是配置里共享的 map。
func upstreamHeaders(h map[string]string) map[string]string {
	drop := false
	for k := range h {
		if strings.EqualFold(k, config.LegacyOAuthClientIDHeader) {
			drop = true
			break
		}
	}
	if !drop {
		return h
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if !strings.EqualFold(k, config.LegacyOAuthClientIDHeader) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyJWT 解析 JWT payload（不验签，只取声明）。
//
// 不验签是合理的：这个 token 是上游签发的，我们只是读取它的元信息；
// 真正的校验发生在上游，我们伪造不了也不需要防伪造。
func (c *Credential) applyJWT(tok string) {
	claims, err := ParseJWT(tok)
	if err != nil {
		return
	}
	if c.ExpiresAt.IsZero() {
		if exp, ok := claims.Numeric("exp"); ok && exp > 0 {
			c.ExpiresAt = time.Unix(int64(exp), 0)
		}
	}
	if c.Email == "" {
		if v, ok := claims.String("email"); ok {
			c.Email = v
		}
	}
	if c.UserID == "" {
		if v, ok := claims.String("sub"); ok {
			c.UserID = v
		}
	}
	// ChatGPT 会把账号信息塞在自定义 claim 里。
	if auth, ok := claims.Map("https://api.openai.com/auth"); ok {
		if c.AccountID == "" {
			if v, ok := auth.String("chatgpt_account_id"); ok {
				c.AccountID = v
			}
		}
		if v, ok := auth.String("chatgpt_plan_type"); ok {
			c.Plan = v
		}
		// chatgpt_user_id（user-Wx7p... 形态）是 Prism start 请求
		// metadata.userId 的取值来源 —— 真实 Web 每轮都带（playwright
		// 抓包实证），缺了会导致 conversation 归属异常、续接失效。
		// 注意 sub 是 "google-oauth2|数字" 形态，不是它。
		if c.UserID == "" || !strings.HasPrefix(c.UserID, "user-") {
			if v, ok := auth.String("chatgpt_user_id"); ok {
				c.UserID = v
			} else if v, ok := auth.String("user_id"); ok {
				c.UserID = v
			}
		}
	}
	if c.Plan == "" {
		if v, ok := claims.String("chatgpt_plan_type"); ok {
			c.Plan = v
		}
	}
}

// CookieValue 从 Cookie 串里取第一个命中的值。
func CookieValue(header string, names ...string) string {
	if header == "" || len(names) == 0 {
		return ""
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[strings.ToLower(n)] = struct{}{}
	}
	for _, seg := range strings.Split(header, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		name, val, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		if _, hit := set[strings.ToLower(strings.TrimSpace(name))]; hit {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// CookieValueWithName 与 CookieValue 相同，但同时返回命中的 cookie 名。
//
// 需要它是因为"值从哪来"会影响后续怎么用：不同来源要用不同的
// cookie 名回写，名字错了 = 上游认为没有会话。
func CookieValueWithName(header string, names ...string) (string, string) {
	if header == "" || len(names) == 0 {
		return "", ""
	}
	want := make(map[string]string, len(names))
	for _, n := range names {
		want[strings.ToLower(n)] = n
	}
	for _, seg := range strings.Split(header, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		name, val, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		if canonical, hit := want[strings.ToLower(strings.TrimSpace(name))]; hit {
			return canonical, strings.TrimSpace(val)
		}
	}
	return "", ""
}

// HasSessionCookie 判断 Cookie 串里是否含可换出 access token 的会话 cookie。
func HasSessionCookie(header string) bool {
	return CookieValue(header, allSessionCookieNames...) != ""
}

// MergeCookie 把一个 Set-Cookie 回写到现有 Cookie 串上。
//
// 上游刷新会话时会下发新的 session-token，必须回写，
// 否则一旦并发请求触发多次刷新，旧值会把新值覆盖掉。
func MergeCookie(header string, sc *http.Cookie) string {
	if sc == nil || sc.Name == "" {
		return header
	}
	parts := []string{}
	replaced := false
	for _, seg := range strings.Split(header, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		name, _, ok := strings.Cut(seg, "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), sc.Name) {
			parts = append(parts, sc.Name+"="+sc.Value)
			replaced = true
			continue
		}
		parts = append(parts, seg)
	}
	if !replaced {
		parts = append(parts, sc.Name+"="+sc.Value)
	}
	return strings.Join(parts, "; ")
}

// ---------------------------- JWT ----------------------------

// Claims 是解析后的 JWT payload。
type Claims map[string]any

// ParseJWT 拆出 payload 段并反序列化。
func ParseJWT(token string) (Claims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("空 token")
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("JWT 段数不足: %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 有些实现会补齐 padding，兜一下。
		if raw2, err2 := base64.URLEncoding.DecodeString(parts[1]); err2 == nil {
			raw = raw2
		} else {
			return nil, fmt.Errorf("解码 JWT payload: %w", err)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("解析 JWT payload: %w", err)
	}
	return Claims(m), nil
}

func (c Claims) String(k string) (string, bool) {
	v, ok := c[k]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (c Claims) Numeric(k string) (float64, bool) {
	v, ok := c[k]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int64:
		return float64(n), true
	}
	return 0, false
}

func (c Claims) Map(k string) (Claims, bool) {
	v, ok := c[k]
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return Claims(m), true
}

// TokenExpiry 便捷函数：直接取过期时间。
func TokenExpiry(token string) (time.Time, bool) {
	claims, err := ParseJWT(token)
	if err != nil {
		return time.Time{}, false
	}
	exp, ok := claims.Numeric("exp")
	if !ok || exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}
