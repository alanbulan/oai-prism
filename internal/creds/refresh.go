package creds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/httpc"
)

// Refresher 负责把"快过期的凭据"变成"新的凭据"。
//
// 两条自愈路径，按可靠性排序：
//
//	refresh_token -> auth.openai.com/oauth/token   （最稳，可无限续期）
//	session cookie -> prism/api/auth/session        （次之，session 本身会过期）
type Refresher struct {
	cfg    config.CredsConfig
	up     config.UpstreamConfig
	client *httpc.Client
}

// NewRefresher 构造刷新器。client 允许传 nil，届时内部自建一个。
func NewRefresher(cfg config.CredsConfig, up config.UpstreamConfig, client *httpc.Client) (*Refresher, error) {
	if client == nil {
		c, err := httpc.New(up, httpc.Options{})
		if err != nil {
			return nil, err
		}
		client = c
	}
	return &Refresher{cfg: cfg, up: up, client: client}, nil
}

// SessionResponse 兼容 /api/auth/session 的多种返回形态。
type SessionResponse struct {
	AccessToken  string `json:"accessToken"`
	Expires      string `json:"expires"`
	AuthProvider string `json:"authProvider"`
	User         struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Name  string `json:"name"`
	} `json:"user"`
	Account struct {
		ID       string `json:"id"`
		PlanType string `json:"planType"`
	} `json:"account"`
	// 兼容 camelCase / snake_case 两种命名。
	AccessTokenSnake string `json:"access_token"`
	AccountIDSnake   string `json:"account_id"`
	PlanSnake        string `json:"plan_type"`
	EmailSnake       string `json:"email"`
}

// FetchSession 用当前凭据调 /api/auth/session，换回一份完整凭据。
func (r *Refresher) FetchSession(ctx context.Context, cur *Credential) (*Credential, error) {
	if cur == nil {
		return nil, fmt.Errorf("凭据为空")
	}
	ck := cur.EffectiveCookie()
	if ck == "" && cur.AccessToken == "" {
		return nil, fmt.Errorf("没有 Cookie 也没有 AccessToken，无法查询会话")
	}

	path := r.cfg.SessionPath
	if path == "" {
		path = "/api/auth/session"
	}

	// 注意：Prism 的会话不在 /api/auth/session 上，但那个端点对
	// ChatGPT 侧仍然有效；真正常用的是 prism_oai_access_token，
	// 它已经存在于 Cookie 串里，无需换发（见 FromAccountConfig）。
	hdr := map[string]string{
		"Accept":          "application/json",
		"Cache-Control":   "no-cache",
		"User-Agent":      r.up.UserAgent,
		"Referer":         r.up.Referer,
		"Origin":          r.up.Origin,
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
	}
	if ck != "" {
		hdr["Cookie"] = ck
	}
	if cur.AccessToken != "" {
		hdr["Authorization"] = "Bearer " + strings.TrimPrefix(cur.AccessToken, "Bearer ")
	}

	req, err := r.client.NewRequest(ctx, http.MethodGet, path, nil, hdr)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询会话: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取会话响应: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Op: "session", Status: resp.StatusCode, Body: truncate(string(body), 512)}
	}

	var sr SessionResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("解析会话响应: %w", err)
	}

	next := cur.Clone()
	next.Source = "session"
	next.UpdatedAt = time.Now()

	if tok := firstNonEmpty(sr.AccessToken, sr.AccessTokenSnake); tok != "" {
		next.AccessToken = tok
		next.applyJWT(tok)
	}
	if id := firstNonEmpty(sr.Account.ID, sr.AccountIDSnake); id != "" {
		next.AccountID = id
	}
	if p := firstNonEmpty(sr.Account.PlanType, sr.PlanSnake); p != "" {
		next.Plan = p
	}
	if e := firstNonEmpty(sr.User.Email, sr.EmailSnake); e != "" {
		next.Email = e
	}
	if sr.User.ID != "" {
		next.UserID = sr.User.ID
	}
	if sr.Expires != "" {
		if t, err := time.Parse(time.RFC3339, sr.Expires); err == nil {
			// 这个是"会话"过期时间，通常晚于 accessToken 的 exp；
			// 只有在没有解析出 JWT exp 时才拿来兜底。
			if next.ExpiresAt.IsZero() {
				next.ExpiresAt = t
			}
		}
	}

	// 把响应里刷新的 Cookie 回写，保持会话延续。
	for _, c := range resp.Cookies() {
		next.CookieHeader = MergeCookie(next.CookieHeader, c)
	}

	if !next.Usable() {
		return nil, fmt.Errorf("会话接口未返回可用的 accessToken（可能登录态已失效）")
	}
	return next, nil
}

// OAuthResponse 是 /oauth/token 的返回。
type OAuthResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// RefreshOAuth 用 refresh_token 换新的 access_token。
func (r *Refresher) RefreshOAuth(ctx context.Context, cur *Credential) (*Credential, error) {
	if cur == nil || cur.RefreshToken == "" {
		return nil, fmt.Errorf("没有 refresh_token")
	}
	url := r.cfg.OAuthTokenURL
	if url == "" {
		url = "https://auth.openai.com/oauth/token"
	}
	clientID := r.cfg.OAuthClientID
	if clientID == "" {
		clientID = config.DefaultOAuthClientID
	}
	// refresh_token 与签发它的 client 绑定：OAuth 导入的账号记录了自己的
	// oauth_client_id，刷新时逐账号覆盖，避免全局混用导致
	// invalid_grant（不同 client 的 refresh_token 不互通）。
	if v := strings.TrimSpace(cur.OAuthClientID); v != "" {
		clientID = v
	}

	payload := map[string]string{
		"client_id":     clientID,
		"grant_type":    "refresh_token",
		"refresh_token": cur.RefreshToken,
	}
	if r.cfg.OAuthScope != "" {
		payload["scope"] = r.cfg.OAuthScope
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", r.up.UserAgent)

	resp, err := r.client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OAuth 刷新: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 OAuth 响应: %w", err)
	}

	var or OAuthResponse
	if err := json.Unmarshal(body, &or); err != nil {
		return nil, fmt.Errorf("解析 OAuth 响应: %w", err)
	}
	if or.Error != "" || resp.StatusCode != http.StatusOK {
		return nil, &APIError{
			Op:     "oauth_refresh",
			Status: resp.StatusCode,
			Body:   truncate(fmt.Sprintf("%s %s", or.Error, or.ErrorDesc), 512),
		}
	}
	if or.AccessToken == "" {
		return nil, fmt.Errorf("OAuth 未返回 access_token")
	}

	next := cur.Clone()
	next.Source = "oauth"
	next.AccessToken = or.AccessToken
	next.UpdatedAt = time.Now()
	if or.RefreshToken != "" {
		// 上游可能轮换 refresh_token，必须跟进，否则下次刷新会失败。
		next.RefreshToken = or.RefreshToken
	}
	if or.ExpiresIn > 0 {
		next.ExpiresAt = time.Now().Add(time.Duration(or.ExpiresIn) * time.Second)
	}
	next.applyJWT(or.AccessToken)
	if next.ExpiresAt.IsZero() && or.ExpiresIn <= 0 {
		next.ExpiresAt = time.Now().Add(30 * time.Minute)
	}
	// id_token 里有更完整的账号信息。
	if or.IDToken != "" {
		saved := next.ExpiresAt
		next.applyJWT(or.IDToken)
		next.ExpiresAt = saved
	}
	return next, nil
}

// Refresh 按优先级自动选择刷新路径。
func (r *Refresher) Refresh(ctx context.Context, cur *Credential) (*Credential, error) {
	if cur == nil {
		return nil, fmt.Errorf("凭据为空")
	}
	var firstErr error

	if cur.RefreshToken != "" {
		next, err := r.RefreshOAuth(ctx, cur)
		if err == nil {
			return next, nil
		}
		firstErr = err
		// refresh_token 被吊销（invalid_grant）时继续尝试 session 路径，
		// 因为 session cookie 往往还活着。
	}

	if cur.SessionToken != "" || HasSessionCookie(cur.CookieHeader) {
		next, err := r.FetchSession(ctx, cur)
		if err == nil {
			return next, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	if cur.AccessToken != "" {
		next, err := r.FetchSession(ctx, cur)
		if err == nil {
			return next, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	if firstErr == nil {
		firstErr = fmt.Errorf("凭据不含任何可刷新材料（refresh_token / session cookie / access_token 均缺失）")
	}
	return nil, firstErr
}

// APIError 表示上游返回了非 2xx。
type APIError struct {
	Op     string
	Status int
	Body   string
	// RetryAfter 是上游 Retry-After 头解析出的等待时长（可能为 0）。
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s 失败: HTTP %d (retry-after=%s): %s", e.Op, e.Status, e.RetryAfter, e.Body)
	}
	return fmt.Sprintf("%s 失败: HTTP %d: %s", e.Op, e.Status, e.Body)
}

// IsAuthError 判断是否属于"认证失效"，需要换号或刷新。
func IsAuthError(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Status {
	case http.StatusUnauthorized:
		return true
	case http.StatusForbidden:
		// Sentinel 风控校验失败（"Request verification failed.
		// Please try again."）不是凭据失效 —— 账号本身是好的，
		// 同账号重试即恢复（2026-10-03 Codex 风暴实测：连续 403
		// 被当成认证失效 → 60s 冷却 → 单账号池全灭 60s，而沙箱
		// 重试 3s 后就绪）。它应按"请求级可重试故障"处理。
		if strings.Contains(ae.Body, "Request verification failed") {
			return false
		}
		return true
	}
	return false
}

// IsRateLimited 判断是否被限流/风控。
func IsRateLimited(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusTooManyRequests
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
