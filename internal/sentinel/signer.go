// Package sentinel 用纯 Go 签发 Prism 写请求需要的 OpenAI-Sentinel-Token。
//
// 做法：在 goja（纯 Go 的 JS 引擎）里搭一个"Chrome 打开 Prism 首页"的页面环境，
// 原样执行 sentinel.openai.com 下发的 sdk.js，调用 SentinelSDK.token(flow)。
// PoW、dx 虚拟机、指纹采集都是 SDK 自己的代码在跑，我们只提供它读到的浏览器：
// 取值来自 Profile（一台真实浏览器的快照）。SDK 本该经隐藏 iframe 发的
// sentinel/req 由 Go 用 Chrome TLS 指纹代发（见 frame.go）。
//
// 一个 Signer 相当于一个一直开着的浏览器标签页：SDK 的状态（requirements PoW 缓存、
// 5 秒后的预取）都在页面里延续；页面每隔 PageTTL 重开一次，像用户刷新了网页。
package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Request 是 Signer 需要发出的 HTTP 请求（调用方用 Chrome 指纹的客户端发，
// 并补上 User-Agent / sec-ch-ua / Accept-Language 等浏览器通用头）。
type Request struct {
	Method string
	URL    string
	Header [][2]string
	Body   []byte
}

// Doer 发出请求并读完响应体。
type Doer interface {
	Do(ctx context.Context, r Request) (status int, body []byte, err error)
}

// Options 配置 Signer。
type Options struct {
	Doer     Doer
	Profile  *Profile // nil 用内置指纹
	Flow     string   // 默认 prism_inference（Prism 前端的 PRISM_SENTINEL_FLOW）
	PageURL  string   // 页面地址，用来取线上脚本列表；默认 Profile.Page.Href，"-" 表示不取
	CacheDir string   // sdk.js 的磁盘缓存目录；空则只缓存在内存
	PageTTL  time.Duration
	Logger   *slog.Logger
}

const (
	DefaultFlow    = "prism_inference"
	defaultPageTTL = 6 * time.Hour
	tokenTimeout   = 30 * time.Second
	maxSoftFails   = 3
)

// Signer 签发 Sentinel token。并发安全；同一时刻只签一个（Prism 前端也是串行的）。
type Signer struct {
	o       Options
	profile *Profile
	log     *slog.Logger

	mu      sync.Mutex
	pg      *page
	sdk     map[string]string // 版本 → sdk.js 源码
	fails   int
	signed  int64
	failed  int64
	lastErr string
}

// Stats 是签发器的运行状况。
type Stats struct {
	Signed    int64     `json:"signed"`
	Failed    int64     `json:"failed"`
	LastError string    `json:"last_error,omitempty"`
	PageSince time.Time `json:"page_since,omitzero"`
}

// TokenError 表示 SDK 产出的是错误载荷（{"e":...}）而不是 token。
type TokenError struct{ Payload string }

func (e *TokenError) Error() string {
	return "Sentinel SDK 返回错误载荷: " + clip(e.Payload, 200)
}

// New 创建签发器（不联网；第一次 Token 时才打开页面）。
func New(o Options) (*Signer, error) {
	if o.Doer == nil {
		return nil, errors.New("sentinel: 缺少 Doer")
	}
	p := o.Profile
	if p == nil {
		p = DefaultProfile()
	}
	if o.Flow == "" {
		o.Flow = DefaultFlow
	}
	if o.PageTTL <= 0 {
		o.PageTTL = defaultPageTTL
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Signer{o: o, profile: p, log: o.Logger, sdk: map[string]string{}}, nil
}

// Profile 返回签发器扮演的浏览器指纹（出站请求头要与之一致）。
func (s *Signer) Profile() *Profile { return s.profile }

// Token 签一个一次性的 OpenAI-Sentinel-Token。
func (s *Signer) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, err := s.token(ctx)
	if err != nil {
		s.failed++
		s.lastErr = err.Error()
		return "", err
	}
	s.signed++
	return tok, nil
}

func (s *Signer) token(ctx context.Context) (string, error) {
	pg, err := s.ensurePage(ctx)
	if err != nil {
		return "", err
	}
	tctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	tok, err := pg.token(tctx, s.o.Flow)
	if err != nil {
		if ctx.Err() == nil {
			s.dropPage("签发出错: " + err.Error())
		}
		return "", err
	}
	if isErrorPayload(tok) {
		// SDK 自己报的错（多半是 sentinel/req 没取到）：连续几次就重开页面。
		if s.fails++; s.fails >= maxSoftFails {
			s.dropPage("连续错误载荷")
		}
		return "", &TokenError{Payload: tok}
	}
	s.fails = 0
	return tok, nil
}

// Stats 返回运行状况。
func (s *Signer) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Signed: s.signed, Failed: s.failed, LastError: s.lastErr}
	if s.pg != nil {
		st.PageSince = s.pg.born
	}
	return st
}

// Close 关掉页面。
func (s *Signer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropPage("")
}

func (s *Signer) dropPage(reason string) {
	if s.pg == nil {
		return
	}
	if reason != "" {
		s.log.Info("Sentinel 页面重开", "reason", reason)
	}
	s.pg.close()
	s.pg = nil
	s.fails = 0
}

func (s *Signer) ensurePage(ctx context.Context) (*page, error) {
	if s.pg != nil && time.Since(s.pg.born) < s.o.PageTTL {
		return s.pg, nil
	}
	s.dropPage("")
	origin := strings.TrimRight(s.profile.SentinelOrigin, "/")
	pageURL := s.profile.Page.Href

	// 1) frame.html 指明当前 sdk.js 版本（浏览器打开 iframe 时就是这么拿到的）
	status, html, err := s.o.Doer.Do(ctx, Request{Method: "GET", URL: origin + "/backend-api/sentinel/frame.html", Header: [][2]string{
		{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"},
		{"Referer", pageURL},
		{"Sec-Fetch-Site", "same-site"},
		{"Sec-Fetch-Mode", "navigate"},
		{"Sec-Fetch-Dest", "iframe"},
		{"Upgrade-Insecure-Requests", "1"},
	}})
	if err != nil || status != 200 {
		return nil, fmt.Errorf("取 sentinel frame.html 失败: HTTP %d %v", status, err)
	}
	sdkURL, version := sdkFromFrame(string(html), origin)
	if sdkURL == "" {
		return nil, fmt.Errorf("frame.html 里找不到 sdk.js: %s", clip(string(html), 160))
	}
	src, err := s.loadSDK(ctx, sdkURL, version, origin)
	if err != nil {
		return nil, err
	}

	// 2) 页面脚本列表尽量用线上最新的（指纹里会抽一个脚本地址）
	prof := s.profile.clone()
	if scripts := s.liveScripts(ctx, pageURL); len(scripts) > 0 {
		prof.Page.Scripts = scripts
	}
	prof.Page.Scripts = append(prof.Page.Scripts, origin+"/backend-api/sentinel/sdk.js", sdkURL)

	fr := newFrame(s.o.Doer, origin, version, s.log)
	pg, err := newPage(prof, src, sdkURL, fr, s.log)
	if err != nil {
		return nil, err
	}
	s.pg = pg
	s.log.Info("Sentinel 页面已打开", "sdk", version, "scripts", len(prof.Page.Scripts))
	return pg, nil
}

var (
	sdkSrcRe    = regexp.MustCompile(`src=['"]([^'"]*/sentinel/([^/'"]+)/sdk\.js)['"]`)
	scriptSrcRe = regexp.MustCompile(`<script[^>]*\ssrc="([^"]+)"`)
)

func sdkFromFrame(html, origin string) (url, version string) {
	m := sdkSrcRe.FindStringSubmatch(html)
	if m == nil {
		return "", ""
	}
	url = m[1]
	if strings.HasPrefix(url, "/") {
		url = origin + url
	}
	return url, m[2]
}

func (s *Signer) loadSDK(ctx context.Context, sdkURL, version, origin string) (string, error) {
	if src, ok := s.sdk[version]; ok {
		return src, nil
	}
	cache := ""
	if s.o.CacheDir != "" && regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(version) {
		cache = filepath.Join(s.o.CacheDir, "sentinel-sdk-"+version+".js")
		if b, err := os.ReadFile(cache); err == nil && len(b) > 0 {
			s.sdk[version] = string(b)
			return string(b), nil
		}
	}
	status, body, err := s.o.Doer.Do(ctx, Request{Method: "GET", URL: sdkURL, Header: [][2]string{
		{"Accept", "*/*"},
		{"Referer", origin + "/backend-api/sentinel/frame.html?sv=" + version},
		{"Sec-Fetch-Site", "same-origin"},
		{"Sec-Fetch-Mode", "no-cors"},
		{"Sec-Fetch-Dest", "script"},
	}})
	if err != nil || status != 200 || len(body) == 0 {
		return "", fmt.Errorf("下载 sdk.js 失败: HTTP %d %v", status, err)
	}
	s.sdk[version] = string(body)
	if cache != "" {
		if err := os.MkdirAll(s.o.CacheDir, 0o755); err == nil {
			_ = os.WriteFile(cache, body, 0o644)
		}
	}
	return string(body), nil
}

// liveScripts 取页面 HTML 里的 <script src>（Prism 每次发版 chunk 名都会变）。失败返回 nil。
func (s *Signer) liveScripts(ctx context.Context, pageURL string) []string {
	if s.o.PageURL == "-" {
		return nil
	}
	if s.o.PageURL != "" {
		pageURL = s.o.PageURL
	}
	status, body, err := s.o.Doer.Do(ctx, Request{Method: "GET", URL: pageURL, Header: [][2]string{
		{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"},
		{"Sec-Fetch-Site", "none"},
		{"Sec-Fetch-Mode", "navigate"},
		{"Sec-Fetch-User", "?1"},
		{"Sec-Fetch-Dest", "document"},
		{"Upgrade-Insecure-Requests", "1"},
	}})
	if err != nil || status != 200 {
		return nil
	}
	base := pageURL
	if i := strings.Index(base[8:], "/"); i >= 0 {
		base = base[:8+i]
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range scriptSrcRe.FindAllStringSubmatch(string(body), -1) {
		u := strings.ReplaceAll(m[1], "&amp;", "&")
		if strings.HasPrefix(u, "/") {
			u = base + u
		}
		if strings.Contains(u, "/sentinel/") || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	if len(out) < 3 {
		return nil
	}
	return out
}

func isErrorPayload(tok string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(tok), &m) != nil {
		return true
	}
	if _, ok := m["e"]; ok {
		return true
	}
	_, hasP := m["p"]
	_, hasC := m["c"]
	return !hasP || !hasC
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
