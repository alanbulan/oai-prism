package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseActionResult(t *testing.T) {
	cases := map[string]string{
		"0:{\"a\":\"$@1\",\"f\":\"\",\"b\":\"x\"}\n1:\"cdx1_abc\"\n": `"cdx1_abc"`,
		"0:{\"a\":\"cdx1_inline\",\"f\":\"\"}\n":                     `"cdx1_inline"`,
		"0:{\"a\":null,\"f\":\"\"}\n":                                `null`,
	}
	for in, want := range cases {
		got, err := parseActionResult([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("%q → %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := parseActionResult([]byte("<html>oops</html>")); err == nil {
		t.Error("不是 RSC 流时应报错")
	}
}

// 前端重新构建后 Action ID 会变：旧 ID 回 404 时从首页脚本里找出新 ID 并重试。
func TestServerAction_RediscoversStaleID(t *testing.T) {
	const fresh = "7fabcdef0123456789abcdef0123456789abcdef01"
	var calls, pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			pages.Add(1)
			fmt.Fprint(w, `<html><script src="/_next/static/chunks/a.js"></script><script src="/_next/static/chunks/b.js"></script></html>`)
		case r.URL.Path == "/_next/static/chunks/a.js":
			fmt.Fprint(w, `let x=(0,u.createServerReference)("11aa",u.callServer,void 0,u.findSourceMapURL,"other");`)
		case r.URL.Path == "/_next/static/chunks/b.js":
			fmt.Fprintf(w, `TT=(0,uZ.createServerReference)(%q,uZ.callServer,void 0,uZ.findSourceMapURL,"createProjectConversation");`, fresh)
		case r.Method == http.MethodPost && r.URL.Path == "/":
			calls.Add(1)
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("Next-Action") != fresh {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, "Server action not found.")
				return
			}
			var args []string
			if json.Unmarshal(body, &args) != nil || len(args) != 1 || r.Header.Get("Accept") != "text/x-component" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, "0:{\"a\":\"$@1\",\"f\":\"\",\"b\":\"b\"}\n1:%q\n", "cdx1_"+args[0])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	raw, err := c.ServerAction(context.Background(), Principal{}, ActionCreateProjectConversation, "proj-1")
	if err != nil || string(raw) != `"cdx1_proj-1"` {
		t.Fatalf("应重新发现并成功: %s %v", raw, err)
	}
	if calls.Load() != 2 || pages.Load() != 1 {
		t.Fatalf("应先用旧 ID 失败一次、发现后重试: calls=%d pages=%d", calls.Load(), pages.Load())
	}
	// 发现的 ID 会被记住，下次直接用。
	if _, err := c.ServerAction(context.Background(), Principal{}, ActionCreateProjectConversation, "proj-2"); err != nil || pages.Load() != 1 {
		t.Fatalf("第二次应直接用新 ID: %v pages=%d", err, pages.Load())
	}
}

func TestServerAction_DiscoveryFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `<html>no scripts</html>`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := newTestClient(t, srv.URL).ServerAction(context.Background(), Principal{}, ActionCreateProjectConversation, "p")
	if err == nil || !strings.Contains(err.Error(), "createProjectConversation") {
		t.Fatalf("找不到时应报错并点名: %v", err)
	}
}
