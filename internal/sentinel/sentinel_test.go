package sentinel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSDK 模仿真实 sdk.js 的骨架：按 currentScript 推出 frame 地址、插隐藏 iframe、
// 等 load 后 postMessage 要 sentinel/req 的结果，再拼出 token。
const fakeSDK = `var SentinelSDK = (function () {
  const src = document.currentScript.src;
  const version = new URL(src).pathname.split("/")[2];
  const base = new URL(src).origin + "/backend-api/sentinel/";
  const frame = document.createElement("iframe");
  frame.style.display = "none";
  frame.src = new URL("frame.html?sv=" + version, base).href;
  let loaded = false, id = 0;
  const pending = new Map();
  frame.addEventListener("load", () => { loaded = true; });
  document.body.appendChild(frame);
  window.addEventListener("message", (e) => {
    if (e.source !== frame.contentWindow) return;
    const done = pending.get(e.data.requestId);
    if (done) { pending.delete(e.data.requestId); done(e.data.result); }
  });
  function ask(type, flow, extra) {
    return new Promise((resolve) => {
      const go = () => { const rid = "req_" + (++id); pending.set(rid, resolve); frame.contentWindow.postMessage(Object.assign({ type, flow, requestId: rid }, extra), "*"); };
      loaded ? go() : frame.addEventListener("load", go);
    });
  }
  async function token(flow) {
    const p = "gAAAAAC" + btoa(JSON.stringify([screen.width, navigator.userAgent, Math.random()]));
    const r = await ask("token", flow, { p });
    if (typeof r === "string") return r;
    await new Promise((res) => setTimeout(res, 5));
    return JSON.stringify({ p: "gAAAAAB" + btoa(String(performance.now())), t: btoa(version), c: r.cachedChatReq.token, flow });
  }
  return { token };
})();`

type fakeDoer struct {
	mu      sync.Mutex
	reqs    []Request
	failReq bool
	reqN    int
}

func (d *fakeDoer) Do(_ context.Context, r Request) (int, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reqs = append(d.reqs, r)
	switch {
	case strings.HasSuffix(r.URL, "/backend-api/sentinel/frame.html"):
		return 200, []byte(`<!DOCTYPE html><html><body><script src='https://sentinel.openai.com/sentinel/v123/sdk.js'></script></body></html>`), nil
	case r.URL == "https://sentinel.openai.com/sentinel/v123/sdk.js":
		return 200, []byte(fakeSDK), nil
	case strings.HasSuffix(r.URL, "/backend-api/sentinel/req"):
		if d.failReq {
			return 0, nil, errors.New("网络断了")
		}
		d.reqN++
		return 200, []byte(`{"persona":"chatgpt-noauth","token":"C` + string(rune('0'+d.reqN)) + `"}`), nil
	case r.URL == "https://prism.openai.com/":
		return 200, []byte(`<html><script src="/_next/static/chunks/a.js"></script><script src="/_next/static/chunks/b.js"></script><script src="/_next/static/chunks/c.js"></script></html>`), nil
	}
	return 404, nil, nil
}

func (d *fakeDoer) find(suffix string) []Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Request
	for _, r := range d.reqs {
		if strings.HasSuffix(r.URL, suffix) {
			out = append(out, r)
		}
	}
	return out
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func header(r Request, k string) string {
	for _, kv := range r.Header {
		if strings.EqualFold(kv[0], k) {
			return kv[1]
		}
	}
	return ""
}

func TestSignerToken(t *testing.T) {
	d := &fakeDoer{}
	s, err := New(Options{Doer: d, Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tok, err := s.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(tok), &m); err != nil {
		t.Fatalf("token 不是 JSON: %s", tok)
	}
	if m["c"] != "C1" || m["flow"] != DefaultFlow || m["t"] != base64.StdEncoding.EncodeToString([]byte("v123")) {
		t.Fatalf("token 内容不对: %v", m)
	}
	reqs := d.find("/backend-api/sentinel/req")
	if len(reqs) != 1 {
		t.Fatalf("sentinel/req 应发 1 次，实际 %d", len(reqs))
	}
	r := reqs[0]
	var body map[string]string
	_ = json.Unmarshal(r.Body, &body)
	if !strings.HasPrefix(body["p"], "gAAAAAC") || body["flow"] != DefaultFlow {
		t.Fatalf("sentinel/req 请求体不对: %s", r.Body)
	}
	if header(r, "Origin") != "https://sentinel.openai.com" ||
		header(r, "Referer") != "https://sentinel.openai.com/backend-api/sentinel/frame.html?sv=v123" ||
		header(r, "Content-Type") != "text/plain;charset=UTF-8" {
		t.Fatalf("sentinel/req 请求头不像 iframe 发的: %v", r.Header)
	}

	// 第二个 token：页面复用，sdk.js 不再下载
	if _, err := s.Token(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(d.find("/sdk.js")); n != 1 {
		t.Fatalf("sdk.js 应只下载 1 次，实际 %d", n)
	}
	if st := s.Stats(); st.Signed != 2 || st.Failed != 0 {
		t.Fatalf("统计不对: %+v", st)
	}
}

func TestSignerErrorPayload(t *testing.T) {
	d := &fakeDoer{failReq: true}
	s, _ := New(Options{Doer: d, Logger: quietLog()})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.Token(ctx)
	var te *TokenError
	if !errors.As(err, &te) || !strings.Contains(te.Payload, `"e":`) || !strings.Contains(te.Payload, `"flow":"prism_inference"`) {
		t.Fatalf("sentinel/req 失败时应得到 SDK 式错误载荷，实际 %v", err)
	}
	if len(d.find("/backend-api/sentinel/req")) != 3 {
		t.Fatalf("frame 应重试 3 次")
	}
}

// evalPage 在一个只载入了空 SDK 的页面里求值一段 JS。
func evalPage(t *testing.T, js string) string {
	t.Helper()
	p := DefaultProfile()
	p.Page.Scripts = []string{"https://prism.openai.com/_next/static/chunks/a.js"}
	pg, err := newPage(p, `var SentinelSDK = { token: function () { return "x"; } };`, "https://sentinel.openai.com/sentinel/v1/sdk.js", newFrame(&fakeDoer{}, "https://sentinel.openai.com", "v1", quietLog()), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer pg.close()
	out := make(chan string, 1)
	pg.post(func() {
		v, err := pg.rt.RunString(js)
		if err != nil {
			out <- "ERR " + err.Error()
			return
		}
		out <- v.String()
	})
	return <-out
}

func TestPageLooksLikeChrome(t *testing.T) {
	p := DefaultProfile()
	cases := []struct{ js, want string }{
		{`navigator.webdriver`, "false"},
		{`navigator.userAgent === "` + p.UserAgent() + `"`, "true"},
		{`Object.prototype.toString.call(navigator)`, "[object Navigator]"},
		{`String(navigator.getBattery)`, "function getBattery() { [native code] }"},
		{`Function.prototype.toString.call(Function.prototype.toString)`, "function toString() { [native code] }"},
		{`Object.keys(Object.getPrototypeOf(navigator)).length === ` + itoa(len(p.Navigator.Proto)), "true"},
		{`navigator.languages.join(",")`, strings.Join(p.Navigator.Languages, ",")},
		{`(() => { try { undefined.x } catch (e) { return String(e) } })()`, "TypeError: Cannot read properties of undefined (reading 'x')"},
		{`new Date(0).toString()`, "Thu Jan 01 1970 08:00:00 GMT+0800 (中国标准时间)"},
		{`new Date(0).getHours()`, "8"},
		{`(() => { for (let i = 0; i < 200; i++) { const x = Math.random() * 2 ** 52; if (x !== Math.floor(x)) return false } return true })()`, "true"},
		{`(() => { const d = document.createElement("div"); d.style.fontSize = "15px"; d.innerText = "abc"; document.body.appendChild(d); const r = d.getBoundingClientRect(); return r.width + "x" + r.height })()`, "24.1875x22.5"},
		{`(() => { const d = document.createElement("div"); d.style.fontSize = "15px"; d.innerText = "U\u031bm"; return d.getBoundingClientRect().width })()`, "25.3125"}, // U + U+031B 合成更宽的 Ư
		{`document.currentScript === null && document.scripts.length > 0`, "true"},
		{`Object.keys(document)[0]`, "location"},
		{`Object.keys(history).join(",")`, "pushState,replaceState"},
		{`new URL("frame.html?sv=1", "https://sentinel.openai.com/backend-api/sentinel/").href`, "https://sentinel.openai.com/backend-api/sentinel/frame.html?sv=1"},
		{`(() => { try { atob("!") } catch (e) { return String(e) } })()`, "InvalidCharacterError: Failed to execute 'atob' on 'Window': The string to be decoded is not correctly encoded."},
		{`btoa("中")`, "ERR"},
		{`String.fromCharCode(...new TextEncoder().encode("é"))`, "Ã©"},
		{`typeof performance.now() === "number" && performance.now() > 0 && performance.timeOrigin > 1e12`, "true"},
		{`Object.keys(window).includes("__sentinelBoot")`, "false"},
		{`Object.keys(window).slice(0, 3).join(",")`, "window,self,document"},
		{`Object.keys(localStorage).length === ` + itoa(len(p.Page.LocalStorage)), "true"},
		{`"ai" in window`, "false"},
	}
	for _, c := range cases {
		got := evalPage(t, c.js)
		if c.want == "ERR" {
			if !strings.HasPrefix(got, "ERR") {
				t.Errorf("%s 应抛错，实际 %q", c.js, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s\n  得到 %q\n  期望 %q", c.js, got, c.want)
		}
	}
}

func TestV8Random(t *testing.T) {
	r := newV8Random()
	for i := 0; i < 1000; i++ {
		x := r.next()
		if x < 0 || x >= 1 || math.Mod(x*(1<<52), 1) != 0 {
			t.Fatalf("不是 [0,1) 内 2^-52 的整数倍: %v", x)
		}
	}
}

func TestProfileHelpers(t *testing.T) {
	p := DefaultProfile()
	if !strings.Contains(p.SecCHUA(), `"Google Chrome";v="`) || p.Platform() != `"Windows"` {
		t.Fatalf("sec-ch-ua 拼错: %s %s", p.SecCHUA(), p.Platform())
	}
	if p.AcceptLanguage() != "zh-CN,zh;q=0.9" {
		t.Fatalf("Accept-Language 拼错: %s", p.AcceptLanguage())
	}
	if !isErrorPayload(`{"e":"x","flow":"f"}`) || isErrorPayload(`{"p":"a","t":"b","c":"c","flow":"f"}`) {
		t.Fatal("isErrorPayload 判断错")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
