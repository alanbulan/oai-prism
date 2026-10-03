package server

// 官方 OAuth 授权导入：让用户在浏览器里走 auth.openai.com 的官方授权页登录，
// 本地回调端口接住 authorization code，后端用 PKCE 换 token 并直接入库。
//
// 流程（与 Codex CLI / sub2api 的实现一致）：
//   1. POST /admin/oauth/begin        生成 PKCE（hex verifier + S256 challenge）
//                                     与 authorize URL，确保本地回调监听器就绪
//   2. 用户浏览器打开 authorize_url 登录授权 → 跳回 redirect_uri?code=...
//   3. 回调按 state 反查会话 → 换 token 入库（监听器模式），
//      或 POST /admin/oauth/exchange 手动粘贴回调地址（兜底模式）
//   4. GET  /admin/oauth/status       前端轮询导入进度
//
// 关键设计：回调监听器是**单例**（端口只监听一次），回调按 state 反查会话 ——
// 用户多次点击"打开授权页"会产生多个并发会话，若监听器绑定单一会话，
// 后完成的会话会被先前的会话错误校验（state 不匹配）。
//
// client 取凭据配置（configs 的 oauth_client_id，默认 Codex CLI 的
// app_EMoamEEZ73f0CkXaXp7hrann）：localhost:1455 回调在该 client 白名单内
// （sub2api / Codex CLI 验证过）。Prism 自家的 client 不开放 authorize 的
// 本地回调 —— 实测返回 invalid_authorize_request，故不用。
// 换出的 refresh_token 与该 client 绑定，账号上记录 oauth_client_id，
// 刷新时逐账号使用，避免全局混用。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

const (
	oauthAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	oauthTokenURL     = "https://auth.openai.com/oauth/token"
	oauthScope        = "openid profile email offline_access"
	oauthSessionTTL   = 30 * time.Minute
)

// oauthSession 一次授权流程的全部状态。
type oauthSession struct {
	ID           string
	State        string
	CodeVerifier string
	RedirectURI  string
	ClientID     string
	CreatedAt    time.Time

	done    bool   // 已拿到 code 并完成（或失败）
	account string // 成功时：入库的账号 ID
	errMsg  string // 失败时：错误描述
}

// oauthSessionStore 管理进行中的授权会话（内存态，进程生命周期一致）。
// 主键两种：state（回调反查）与 session_id（前端轮询/手动兜底）。
type oauthSessionStore struct {
	mu      sync.Mutex
	byState map[string]*oauthSession
	byID    map[string]*oauthSession
}

var oauthSessions = &oauthSessionStore{
	byState: map[string]*oauthSession{},
	byID:    map[string]*oauthSession{},
}

func (s *oauthSessionStore) put(sess *oauthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, old := range s.byState {
		expired := time.Since(old.CreatedAt) > oauthSessionTTL
		if expired || old.ClientID == sess.ClientID {
			// 过期清理；同 client 只保留最新一个 pending —— 单活跃会话模式下
			// 回调不会串到旧会话，新会话直接覆盖旧的。
			delete(s.byState, k)
			delete(s.byID, old.ID)
		}
	}
	s.byState[sess.State] = sess
	s.byID[sess.ID] = sess
}

func (s *oauthSessionStore) getByState(state string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byState[state]
	if !ok || time.Since(sess.CreatedAt) > oauthSessionTTL {
		return nil
	}
	return sess
}

func (s *oauthSessionStore) getByID(id string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok || time.Since(sess.CreatedAt) > oauthSessionTTL {
		return nil
	}
	return sess
}

func (s *oauthSessionStore) remove(sess *oauthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byState, sess.State)
	delete(s.byID, sess.ID)
}

// ---- PKCE（OpenAI 特有：verifier 用 hex 编码，challenge 走标准 S256）----

func oauthGeneratePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, 64)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func oauthRandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// oauthBuildAuthorizeURL 组装官方授权页地址。
// codex_cli_simplified_flow 与 id_token_add_organizations 是 Codex 系
// client 的专用参数（与 Codex CLI / sub2api 一致），简化登录页并回传组织信息。
func oauthBuildAuthorizeURL(clientID, redirectURI, state, challenge string) string {
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", oauthScope)
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	return oauthAuthorizeURL + "?" + params.Encode()
}

// oauthExchange 用 code 换 token（JSON 请求，与 refresh 流程同一端点）。
func oauthExchangeToken(clientID, code, codeVerifier, redirectURI string) (*credsOAuthToken, error) {
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     clientID,
		"code":          code,
		"redirect_uri":  redirectURI,
		"code_verifier": codeVerifier,
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, strings.NewReader(string(buf)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("token 响应解析失败: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("上游拒绝: %s (%s)", out.Error, out.ErrorDescription)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("token 响应缺少 access_token")
	}
	return &credsOAuthToken{
		AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		IDToken: out.IDToken, ExpiresIn: out.ExpiresIn,
	}, nil
}

// credsOAuthToken 与 internal/creds 的 OAuth 响应字段一致。
type credsOAuthToken struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int64
}

// oauthEmailFromIDToken 从 id_token（JWT 第二段）里拿 email，拿不到就返回空。
func oauthEmailFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Email
}

// ---- 单例回调监听器：按 state 反查会话 ----

var (
	oauthCallbackOnce sync.Once
	oauthCallbackErr  error
)

// ensureOAuthCallbackListener 启动全局唯一的本地回调监听器（1455）。
// 端口被占（Codex CLI 在跑等）时返回错误，调用方降级为"网关自身回调路由"。
func (s *Server) ensureOAuthCallbackListener() (string, error) {
	oauthCallbackOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:1455")
		if err != nil {
			oauthCallbackErr = err
			return
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			state := r.URL.Query().Get("state")
			// state 必须精确匹配：它是 OAuth 回调防 CSRF 的唯一凭据。
			// 早期在匹配不上时"认领唯一进行中的会话"，等于让任意回调绕过 state 校验。
			sess := oauthSessions.getByState(state)
			if sess == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(oauthResultHTML("state 校验失败",
					"回调的 state 没有匹配到进行中的授权会话（可能已过期，或打开的是旧的授权页）。请回到控制台重新发起授权。")))
				return
			}
			s.handleLocalCallback(w, r, sess)
		})
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
	})
	if oauthCallbackErr != nil {
		return "", oauthCallbackErr
	}
	return "http://localhost:1455/auth/callback", nil
}

// writeOAuthJSON 输出管理端 JSON 结果。
func writeOAuthJSON(w http.ResponseWriter, out map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// boolPtr 返回 bool 指针（AccountConfig.Enabled 需要）。
func boolPtr(v bool) *bool { return &v }

// ---- HTTP 处理器 ----

// handleOAuthBegin 创建授权会话并返回 authorize URL。
func (s *Server) handleOAuthBegin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURI string `json:"redirect_uri"`
		ClientID    string `json:"client_id"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)

	clientID := strings.TrimSpace(body.ClientID)
	if clientID == "" {
		// 凭据配置的 oauth_client_id 是唯一真相源（与 refresh 流程同 client，
		// 换出的 token 体系与存量账号一致）
		clientID = s.cfg.Creds.OAuthClientID
	}
	if clientID == "" {
		clientID = config.DefaultOAuthClientID
	}

	// 本地回调监听器（单例）；端口被占则降级为网关自身的回调路由。
	redirectURI, err := s.ensureOAuthCallbackListener()
	if err != nil {
		redirectURI = "http://localhost:" + s.gwPort + "/admin/oauth/callback"
	}
	if u := strings.TrimSpace(body.RedirectURI); u != "" {
		redirectURI = u
	}

	verifier, challenge, err := oauthGeneratePKCE()
	if err != nil {
		writeAdminErr(w, http.StatusInternalServerError, "生成 PKCE 失败: "+err.Error())
		return
	}
	state := oauthRandomHex(16)
	sess := &oauthSession{
		ID:           "oa_" + oauthRandomHex(8),
		State:        state,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
		ClientID:     clientID,
		CreatedAt:    time.Now(),
	}
	oauthSessions.put(sess)

	writeOAuthJSON(w, map[string]any{
		"session_id":    sess.ID,
		"authorize_url": oauthBuildAuthorizeURL(clientID, redirectURI, state, challenge),
		"redirect_uri":  redirectURI,
		"client_id":     clientID,
	})
}

// handleLocalCallback 处理回调：接 code → 换 token → 入库 → 输出结果页。
func (s *Server) handleLocalCallback(w http.ResponseWriter, r *http.Request, sess *oauthSession) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.finishOAuthSession(sess, "", fmt.Errorf("授权页返回错误: %s (%s)", e, q.Get("error_description")))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(oauthResultHTML("授权被取消", q.Get("error_description"))))
		return
	}
	code := q.Get("code")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	acctID, err := s.completeOAuthLogin(sess, code)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(oauthResultHTML("换 token 失败", err.Error())))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(oauthResultHTML("授权成功", "账号 "+acctID+" 已入库并进入调度池，可关闭此页回到控制台查看。")))
}

// finishOAuthSession 记录会话终态并从反查表移除。
func (s *Server) finishOAuthSession(sess *oauthSession, accountID string, err error) {
	sess.done = true
	sess.account = accountID
	if err != nil {
		sess.errMsg = err.Error()
	}
	oauthSessions.remove(sess)
}

// completeOAuthLogin 换 token 并把账号写入 SQLite、热重载进池。
func (s *Server) completeOAuthLogin(sess *oauthSession, code string) (string, error) {
	tok, err := oauthExchangeToken(sess.ClientID, code, sess.CodeVerifier, sess.RedirectURI)
	if err != nil {
		s.finishOAuthSession(sess, "", err)
		return "", err
	}

	email := oauthEmailFromIDToken(tok.IDToken)
	now := time.Now()
	expires := now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	if tok.ExpiresIn <= 0 {
		expires = now.Add(24 * time.Hour)
	}

	acct := config.AccountConfig{
		ID:           "oauth-" + oauthRandomHex(4),
		Name:         "OAuth 导入",
		Enabled:      boolPtr(true),
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    &expires,
		Email:        email,
		Plan:         "pro",
		Tags:         []string{"oauth"},
		// refresh_token 与签发它的 client 绑定：导入用的哪个 client，
		// 这个账号的刷新也必须用哪个 —— 逐账号记录，避免全局混用。
		Headers: map[string]string{"oauth_client_id": sess.ClientID},
	}
	if email != "" {
		acct.Name = email
	}
	if s.sqlite != nil {
		if err := s.sqlite.SaveAccount(acct); err != nil {
			s.finishOAuthSession(sess, "", fmt.Errorf("入库失败: %w", err))
			return "", err
		}
		_ = s.syncPoolFromSQLite()
	}
	s.finishOAuthSession(sess, acct.ID, nil)
	return acct.ID, nil
}

// handleOAuthStatus 前端轮询导入进度。
func (s *Server) handleOAuthStatus(w http.ResponseWriter, r *http.Request) {
	sess := oauthSessions.getByState(strings.TrimSpace(r.URL.Query().Get("state")))
	if sess == nil {
		// 兼容前端只拿 session_id 轮询的用法
		sess = oauthSessions.getByID(strings.TrimSpace(r.URL.Query().Get("session_id")))
	}
	if sess == nil {
		writeAdminErr(w, http.StatusNotFound, "会话不存在或已过期")
		return
	}
	out := map[string]any{"status": "waiting", "session_id": sess.ID}
	if sess.done {
		if sess.errMsg != "" {
			out["status"] = "error"
			out["error"] = sess.errMsg
		} else {
			out["status"] = "success"
			out["account_id"] = sess.account
		}
	}
	writeOAuthJSON(w, out)
}

// handleOAuthExchange 手动兜底：用户把浏览器地址栏里的完整回调 URL（或 code）粘贴回来。
func (s *Server) handleOAuthExchange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Callback  string `json:"callback"` // 完整回调 URL 或裸 code
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeAdminErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	sess := oauthSessions.getByID(strings.TrimSpace(body.SessionID))
	if sess == nil {
		writeAdminErr(w, http.StatusNotFound, "会话不存在或已过期")
		return
	}

	code := strings.TrimSpace(body.Callback)
	if u, err := url.Parse(code); err == nil && u.Query().Get("code") != "" {
		if st := u.Query().Get("state"); st != "" && st != sess.State {
			writeAdminErr(w, http.StatusBadRequest, "state 不匹配，请粘贴本次授权的回调地址")
			return
		}
		code = u.Query().Get("code")
	}
	if code == "" {
		writeAdminErr(w, http.StatusBadRequest, "回调内容里没有 code")
		return
	}

	acctID, err := s.completeOAuthLogin(sess, code)
	if err != nil {
		writeAdminErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOAuthJSON(w, map[string]any{"status": "success", "account_id": acctID})
}

// oauthResultHTML 回调端浏览器看到的结果页。
func oauthResultHTML(title, detail string) string {
	return "<!doctype html><meta charset='utf-8'><title>" + title + "</title>" +
		"<body style='font-family:system-ui;display:flex;align-items:center;justify-content:center;height:100vh;margin:0'>" +
		"<div style='text-align:center'><h2>" + title + "</h2><p style='color:#666'>" + detail + "</p>" +
		"<p style='color:#aaa;font-size:13px'>完成后可关闭此页，回到 OAIprism 控制台。</p></div></body>"
}
