package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"

	"github.com/oai-prism/oaiprism/internal/sentinel"
)

func TestRequiresProof(t *testing.T) {
	sa := http.Header{"Next-Action": {"abc"}}
	cases := []struct {
		method, path string
		h            http.Header
		want         bool
	}{
		{"POST", "/api/projects", nil, true},
		{"GET", "/api/llm/response_with_tools_status", nil, true}, // 前端连轮询都带
		{"OPTIONS", "/api/projects", nil, false},
		{"GET", "/api/auth/session", nil, false},
		{"GET", "/api/auth/session/", nil, false},
		{"POST", "/api/user-events", nil, false},
		{"GET", "/api/ff/flags", nil, false},
		{"GET", "/api/sandbox/proxy/sandbox-resources/x", nil, false},
		{"GET", "/api/sandbox/proxy/token", nil, true},
		{"POST", "/", sa, true},
		{"POST", "/", nil, false},
		{"GET", "/s/sandboxes/proxy/token", nil, true},
		{"GET", "/s/codex_v2_abc/x", nil, true},
		{"GET", "/auth/session", nil, false},
	}
	for _, c := range cases {
		h := c.h
		if h == nil {
			h = http.Header{}
		}
		if got := RequiresProof(c.method, c.path, h); got != c.want {
			t.Errorf("%s %s next-action=%q: got %v want %v", c.method, c.path, h.Get("Next-Action"), got, c.want)
		}
	}
}

type stubDoer struct{}

func (stubDoer) Do(_ context.Context, r sentinel.Request) (int, []byte, error) {
	switch {
	case strings.HasSuffix(r.URL, "/frame.html"):
		return 200, []byte(`<script src='https://sentinel.openai.com/sentinel/v1/sdk.js'></script>`), nil
	case strings.HasSuffix(r.URL, "/sdk.js"):
		return 200, []byte(`var SentinelSDK = { token: function (flow) { return JSON.stringify({ p: "P", t: "T", c: "C", flow: flow }); } };`), nil
	}
	return 404, nil, nil
}

func newTestTransport(t *testing.T) *Transport {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := sentinel.New(sentinel.Options{Doer: stubDoer{}, PageURL: "-", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := New(Options{Signer: s, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.Close)
	// 预置会话，避免测试去访问真实的 /auth/session
	ss := tr.sessionFor("AT1")
	ss.token, ss.at = "ST1", time.Now()
	return tr
}

func TestHeaderForPrismWrite(t *testing.T) {
	tr := newTestTransport(t)
	req, _ := http.NewRequest("POST", "https://prism.openai.com/api/projects", strings.NewReader("{}"))
	req.Header.Set("Cookie", "cf_clearance=x; prism_oai_access_token=AT1; prism_session_token=OLD; oai-did=d")
	req.Header.Set("Authorization", "Bearer AT1")
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	req.Header.Set("sec-ch-ua", `"Chromium";v="131"`)
	req.Header.Set("Origin", "https://prism.openai.com")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	h, err := tr.header(req)
	if err != nil {
		t.Fatal(err)
	}
	p := tr.profile
	checks := map[string]string{
		"Cookie":          "prism_oai_access_token=AT1; prism_session_token=ST1",
		"Authorization":   "",
		"User-Agent":      p.UserAgent(),
		"sec-ch-ua":       p.SecCHUA(),
		"Origin":          "https://prism.openai.com",
		"Priority":        "u=1, i",
		SentinelHeader:    `{"p":"P","t":"T","c":"C","flow":"prism_inference"}`,
		"Accept-Language": p.AcceptLanguage(),
	}
	for k, want := range checks {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestHeaderNoProofAndOtherHosts(t *testing.T) {
	tr := newTestTransport(t)
	req, _ := http.NewRequest("GET", "https://prism.openai.com/api/auth/session", nil)
	req.Header.Set("Cookie", "prism_oai_access_token=AT1")
	h, _ := tr.header(req)
	if h.Get(SentinelHeader) != "" {
		t.Fatal("豁免端点不该带 Sentinel token")
	}
	req, _ = http.NewRequest("POST", "https://auth.openai.com/oauth/token", nil)
	req.Header.Set("Cookie", "prism_oai_access_token=AT1")
	h, _ = tr.header(req)
	if h.Get("Cookie") != "" || h.Get(SentinelHeader) != "" {
		t.Fatal("其他域名不该带 Prism 的 Cookie 与 Sentinel token")
	}
	// Y-Sweet 文档接口：保留文档令牌，不拼账号 Cookie
	req, _ = http.NewRequest("POST", "https://prism.openai.com/y/d/doc1/update", nil)
	req.Header.Set("Authorization", "Bearer ysweet-token")
	h, _ = tr.header(req)
	if h.Get("Authorization") != "Bearer ysweet-token" || h.Get("Cookie") != "" || h.Get(SentinelHeader) != "" {
		t.Fatalf("Y-Sweet 请求头不对: %v", h)
	}
}

func TestToResponseDecompresses(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"ok":true}`))
	_ = zw.Close()
	fr := &fhttp.Response{
		StatusCode: 200, Status: "200 OK", Proto: "HTTP/2.0", ProtoMajor: 2,
		Header:        fhttp.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"31"}, "Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(buf.Bytes())),
		ContentLength: int64(buf.Len()),
	}
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "prism.openai.com"}}
	r := toResponse(fr, req)
	b, _ := io.ReadAll(r.Body)
	if string(b) != `{"ok":true}` || r.Header.Get("Content-Encoding") != "" || r.ContentLength != -1 {
		t.Fatalf("没有解压或头没清理: %q %v %d", b, r.Header, r.ContentLength)
	}
}

func TestIsPrism(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://prism.openai.com":   true,
		"https://PRISM.openai.com/x": true,
		"http://prism.openai.com":    false,
		"http://127.0.0.1:8790":      false,
		"https://example.com":        false,
	} {
		u, _ := url.Parse(raw)
		if IsPrism(u) != want {
			t.Errorf("IsPrism(%s) != %v", raw, want)
		}
	}
}
