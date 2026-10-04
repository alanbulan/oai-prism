package sentinel

import (
	"container/heap"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
)

//go:embed browser.js
var browserJS string

// page 是一个"浏览器标签页"：一个 goja 运行时 + 事件循环，里面是伪造的 Prism 页面
// 环境和原样执行的 sdk.js。goja 运行时不是并发安全的，所有 JS 都在 loop 协程上跑。
type page struct {
	rt    *goja.Runtime
	api   *goja.Object
	jobs  chan func()
	quit  chan struct{}
	done  chan struct{}
	log   *slog.Logger
	frame *frame

	timers timerHeap
	nextID int64
	live   map[int64]*timer

	start  time.Time // timeOrigin 对应的时刻
	origin float64   // performance.timeOrigin（毫秒，0.1 精度）
	mono   float64   // timeOrigin 时刻的单调时钟读数（秒），见 now()
	rand   *v8Random
	born   time.Time
}

type timer struct {
	id     int64
	due    time.Time
	every  time.Duration
	fn     goja.Callable
	args   []goja.Value
	index  int
	cancel bool
}

type timerHeap []*timer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if h[i].due.Equal(h[j].due) {
		return h[i].id < h[j].id
	}
	return h[i].due.Before(h[j].due)
}
func (h timerHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *timerHeap) Push(x any)   { t := x.(*timer); t.index = len(*h); *h = append(*h, t) }
func (h *timerHeap) Pop() any {
	old := *h
	t := old[len(old)-1]
	*h = old[:len(old)-1]
	return t
}

// jsBudget 是单次进入 JS 的看门狗：sdk.js 的正常执行在百毫秒级，卡住就打断。
const jsBudget = 20 * time.Second

// newPage 搭环境、执行 sdk.js，并启动事件循环。sdkURL 是 sdk.js 的真实地址
// （document.currentScript.src —— SDK 靠它推出 sentinel 域名与版本号）。
func newPage(p *Profile, sdkSrc, sdkURL string, fr *frame, log *slog.Logger) (*page, error) {
	pg := &page{
		rt:    goja.New(),
		jobs:  make(chan func(), 64),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
		log:   log,
		frame: fr,
		live:  map[int64]*timer{},
		rand:  newV8Random(),
		born:  time.Now(),
	}
	// 页面在 SDK 初始化前几秒就打开了（真实浏览器里 performance.now() 不会从 0 起步）。
	age := 2*time.Second + time.Duration(pg.rand.next()*float64(4*time.Second))
	pg.start = time.Now().Add(-age)
	pg.origin = math.Floor(float64(pg.start.UnixMicro())/100) / 10
	pg.mono = math.Floor((3600+pg.rand.next()*86400*3)*1e4) / 1e4 // 像开机若干小时后的单调时钟

	profileJSON, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if _, err := pg.rt.RunScript("browser.js", browserJS); err != nil {
		return nil, fmt.Errorf("载入页面环境失败: %w", err)
	}
	boot, ok := goja.AssertFunction(pg.rt.Get("__sentinelBoot"))
	if !ok {
		return nil, errors.New("页面环境缺少 __sentinelBoot")
	}
	apiVal, err := boot(goja.Undefined(), pg.rt.ToValue(string(profileJSON)), pg.hostObject())
	if err != nil {
		return nil, fmt.Errorf("初始化页面环境失败: %w", err)
	}
	pg.api = apiVal.ToObject(pg.rt)
	_ = pg.rt.GlobalObject().Delete("__sentinelBoot")

	if _, err := pg.call("beforeSDK", sdkURL); err != nil {
		return nil, err
	}
	sdkSrc = pg.nativeXOR(sdkSrc)
	if _, err := pg.rt.RunScript("sdk.js", sdkSrc); err != nil {
		return nil, fmt.Errorf("执行 sdk.js 失败: %w", err)
	}
	if _, err := pg.call("afterSDK"); err != nil {
		return nil, err
	}
	if tok := pg.rt.Get("SentinelSDK"); tok == nil || goja.IsUndefined(tok.ToObject(pg.rt).Get("token")) {
		return nil, errors.New("sdk.js 执行后没有 SentinelSDK.token")
	}
	go pg.loop()
	return pg, nil
}

// sdk.js 用 r+=String.fromCharCode(...) 逐字 XOR 解 dx（约 2 万字符）。V8 的 rope 让这是线性的，
// goja 却是平方级 —— 一次签发要产生几百 MB 临时字符串。把这两个 XOR 函数换成语义相同的
// 原生实现（按 UTF-16 码元异或，密钥为空时原样返回）；函数形状对不上就保持原样（慢但正确）。
var xorFuncRe = regexp.MustCompile(`function ([\w$]+)\(t,n\)\{const e=[\w$]+;let r="";for\(let o=0;o<t\[e\(\d+\)\];o\+\+\)r\+=String\[e\(\d+\)\]\(t\[e\(\d+\)\]\(o\)\^n\[e\(\d+\)\]\(o%n\[e\(\d+\)\]\)\);return r\}`)

// xorTap 只给测试用：观察 dx 每次 XOR 的明文与密钥。
var xorTap func(plain, key string)

func (pg *page) nativeXOR(src string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "__x" + hex.EncodeToString(b)
	n := 0
	src = xorFuncRe.ReplaceAllStringFunc(src, func(m string) string {
		n++
		fn := xorFuncRe.FindStringSubmatch(m)[1]
		return "function " + fn + "(t,n){return " + name + "(\"\"+t,\"\"+n)}"
	})
	if n == 0 {
		pg.log.Debug("Sentinel sdk.js 的 XOR 函数形状变了，按原样执行")
		return src
	}
	xor := func(c goja.FunctionCall) goja.Value {
		t, _ := c.Argument(0).ToString().(goja.String)
		k, _ := c.Argument(1).ToString().(goja.String)
		if t == nil || k == nil || k.Length() == 0 {
			return c.Argument(0).ToString()
		}
		tl, kl := t.Length(), k.Length()
		key := make([]uint16, kl)
		for i := range key {
			key[i] = k.CharAt(i)
		}
		out := make([]uint16, tl)
		for i := range out {
			out[i] = t.CharAt(i) ^ key[i%kl]
		}
		if xorTap != nil {
			xorTap(t.String(), k.String())
		}
		return goja.StringFromUTF16(out)
	}
	_ = pg.rt.GlobalObject().DefineDataProperty(name, pg.rt.ToValue(xor), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_FALSE)
	return src
}

func (pg *page) call(name string, args ...any) (goja.Value, error) {
	fn, ok := goja.AssertFunction(pg.api.Get(name))
	if !ok {
		return nil, fmt.Errorf("页面环境缺少 %s", name)
	}
	vals := make([]goja.Value, len(args))
	for i, a := range args {
		vals[i] = pg.rt.ToValue(a)
	}
	return fn(pg.api, vals...)
}

// post 把一段需要碰 JS 的工作交给事件循环。页面已关闭时返回 false。
func (pg *page) post(fn func()) bool {
	select {
	case pg.jobs <- fn:
		return true
	case <-pg.done:
		return false
	}
}

func (pg *page) close() {
	select {
	case <-pg.quit:
	default:
		close(pg.quit)
	}
	<-pg.done
}

func (pg *page) loop() {
	defer close(pg.done)
	wake := time.NewTimer(time.Hour)
	defer wake.Stop()
	for {
		pg.runDue()
		var next <-chan time.Time
		if len(pg.timers) > 0 {
			d := time.Until(pg.timers[0].due)
			if d < 0 {
				d = 0
			}
			wake.Reset(d)
			next = wake.C
		}
		select {
		case fn := <-pg.jobs:
			pg.guard(fn)
		case <-next:
		case <-pg.quit:
			return
		}
		if next != nil && !wake.Stop() {
			select {
			case <-wake.C:
			default:
			}
		}
	}
}

// guard 执行一段 JS：带看门狗，吞掉 panic（一个坏挑战不该拖垮整个进程）。
func (pg *page) guard(fn func()) {
	dog := time.AfterFunc(jsBudget, func() { pg.rt.Interrupt("sentinel: JS 执行超时") })
	defer func() {
		dog.Stop()
		pg.rt.ClearInterrupt()
		if r := recover(); r != nil {
			pg.log.Warn("Sentinel 页面执行异常", "err", fmt.Sprint(r))
		}
	}()
	fn()
}

func (pg *page) runDue() {
	now := time.Now()
	for len(pg.timers) > 0 && !pg.timers[0].due.After(now) {
		t := heap.Pop(&pg.timers).(*timer)
		if t.cancel {
			delete(pg.live, t.id)
			continue
		}
		if t.every > 0 {
			t.due = now.Add(t.every)
			heap.Push(&pg.timers, t)
		} else {
			delete(pg.live, t.id)
		}
		pg.guard(func() {
			if _, err := t.fn(goja.Undefined(), t.args...); err != nil {
				pg.log.Debug("Sentinel 页面定时器报错", "err", err)
			}
		})
	}
}

// now 是 performance.now()：页面打开至今的毫秒数。Chrome 在单调时钟（秒）上按
// 100µs 粗化，再减去 timeOrigin 的读数、乘 1000，于是会带出 5132.70000000298 这种浮点尾巴。
func (pg *page) now() float64 {
	t := math.Floor((pg.mono+time.Since(pg.start).Seconds())*1e4) / 1e4
	return (t - pg.mono) * 1000
}

func (pg *page) hostObject() *goja.Object {
	rt := pg.rt
	h := rt.NewObject()
	set := func(name string, fn func(goja.FunctionCall) goja.Value) { _ = h.Set(name, fn) }
	str := func(s string) goja.Value { return rt.ToValue(s) }

	set("now", func(goja.FunctionCall) goja.Value { return rt.ToValue(pg.now()) })
	set("timeOrigin", func(goja.FunctionCall) goja.Value { return rt.ToValue(pg.origin) })
	set("random", func(goja.FunctionCall) goja.Value { return rt.ToValue(pg.rand.next()) })
	set("randomBytes", func(c goja.FunctionCall) goja.Value {
		n := int(c.Argument(0).ToInteger())
		if n < 0 || n > 65536 {
			panic(rt.NewTypeError("getRandomValues: 长度非法"))
		}
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return rt.ToValue(rt.NewArrayBuffer(b))
	})
	set("reactSuffix", func(goja.FunctionCall) goja.Value {
		const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
		b := make([]byte, 11)
		for i := range b {
			b[i] = alpha[int(pg.rand.next()*float64(len(alpha)))]
		}
		return str(string(b))
	})
	set("setTimeout", func(c goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(c.Argument(0))
		if !ok {
			return rt.ToValue(0)
		}
		ms := c.Argument(1).ToFloat()
		if math.IsNaN(ms) || ms < 0 {
			ms = 0
		}
		var args []goja.Value
		if o, ok := c.Argument(2).(*goja.Object); ok {
			n := int(o.Get("length").ToInteger())
			for i := 0; i < n; i++ {
				args = append(args, o.Get(fmt.Sprint(i)))
			}
		}
		d := time.Duration(ms * float64(time.Millisecond))
		pg.nextID++
		t := &timer{id: pg.nextID, due: time.Now().Add(d), fn: fn, args: args}
		if c.Argument(3).ToBoolean() {
			t.every = max(d, time.Millisecond)
		}
		heap.Push(&pg.timers, t)
		pg.live[t.id] = t
		return rt.ToValue(t.id)
	})
	set("clearTimeout", func(c goja.FunctionCall) goja.Value {
		if t := pg.live[c.Argument(0).ToInteger()]; t != nil {
			t.cancel = true
		}
		return goja.Undefined()
	})
	set("btoa", func(c goja.FunctionCall) goja.Value {
		s := c.Argument(0).String()
		b := make([]byte, 0, len(s))
		for _, r := range s {
			if r > 0xff {
				return goja.Null()
			}
			b = append(b, byte(r))
		}
		return str(base64.StdEncoding.EncodeToString(b))
	})
	set("atob", func(c goja.FunctionCall) goja.Value {
		b, ok := atob(c.Argument(0).String())
		if !ok {
			return goja.Null()
		}
		r := make([]rune, len(b))
		for i, x := range b {
			r[i] = rune(x)
		}
		return str(string(r))
	})
	set("utf8", func(c goja.FunctionCall) goja.Value {
		return rt.ToValue(rt.NewArrayBuffer(utf8Encode(c.Argument(0).String())))
	})
	set("utf8decode", func(c goja.FunctionCall) goja.Value {
		var b []byte
		switch x := c.Argument(0).Export().(type) {
		case goja.ArrayBuffer:
			b = x.Bytes()
		case []byte:
			b = x
		}
		return str(strings.ToValidUTF8(string(b), "�"))
	})
	set("parseURL", func(c goja.FunctionCall) goja.Value {
		out, err := parseURL(c.Argument(0).String(), c.Argument(1).String())
		if err != nil {
			panic(rt.NewTypeError("Failed to construct 'URL': Invalid URL"))
		}
		return str(out)
	})
	set("frame", func(c goja.FunctionCall) goja.Value {
		var msg frameMessage
		if err := json.Unmarshal([]byte(c.Argument(0).String()), &msg); err == nil {
			pg.frame.handle(msg, func(reply []byte) {
				pg.post(func() {
					if _, err := pg.call("deliver", string(reply)); err != nil {
						pg.log.Debug("Sentinel frame 回包投递失败", "err", err)
					}
				})
			})
		}
		return goja.Undefined()
	})
	set("log", func(c goja.FunctionCall) goja.Value {
		pg.log.Debug("Sentinel 页面", "msg", c.Argument(0).String())
		return goja.Undefined()
	})
	return h
}

// token 在页面里调一次 SentinelSDK.token(flow)，等它的 Promise 落定。
func (pg *page) token(ctx context.Context, flow string) (string, error) {
	type result struct {
		v   string
		err error
	}
	ch := make(chan result, 1)
	ok := pg.post(func() {
		v, err := pg.call("token", flow)
		if err != nil {
			ch <- result{err: err}
			return
		}
		obj, isObj := v.(*goja.Object)
		then, hasThen := goja.Callable(nil), false
		if isObj {
			then, hasThen = goja.AssertFunction(obj.Get("then"))
		}
		if !hasThen {
			ch <- result{v: v.String()}
			return
		}
		_, err = then(obj,
			pg.rt.ToValue(func(c goja.FunctionCall) goja.Value {
				ch <- result{v: c.Argument(0).String()}
				return goja.Undefined()
			}),
			pg.rt.ToValue(func(c goja.FunctionCall) goja.Value {
				ch <- result{err: fmt.Errorf("SentinelSDK.token 失败: %s", c.Argument(0).String())}
				return goja.Undefined()
			}))
		if err != nil {
			ch <- result{err: err}
		}
	})
	if !ok {
		return "", errors.New("Sentinel 页面已关闭")
	}
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-pg.done:
		return "", errors.New("Sentinel 页面已关闭")
	}
}

func atob(s string) ([]byte, bool) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\f', '\r':
			return -1
		}
		return r
	}, s)
	if len(s)%4 == 1 {
		return nil, false
	}
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	return b, err == nil
}

// utf8Encode 按 TextEncoder 的规则编码：JS 字符串是 UTF-16，孤立代理项编成 U+FFFD。
// goja 把 JS 字符串交给 Go 时已经做过同样的替换，这里只需确保输出是合法 UTF-8。
func utf8Encode(s string) []byte {
	if utf8.ValidString(s) {
		return []byte(s)
	}
	return []byte(strings.ToValidUTF8(s, "�"))
}

// parseURL 实现 new URL(u, base) 用到的字段，返回 JSON。
func parseURL(raw, base string) (string, error) {
	var u *url.URL
	var err error
	if base != "" {
		b, err2 := url.Parse(base)
		if err2 != nil || b.Scheme == "" {
			return "", errors.New("invalid base")
		}
		u, err = b.Parse(raw)
	} else {
		u, err = url.Parse(raw)
	}
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid url")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		host = strings.ToLower(u.Hostname())
		port = ""
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	search := ""
	if u.RawQuery != "" || u.ForceQuery {
		search = "?" + u.RawQuery
	}
	hash := ""
	if u.Fragment != "" {
		hash = "#" + u.EscapedFragment()
	}
	origin := scheme + "://" + host
	out, _ := json.Marshal(map[string]string{
		"href": origin + path + search + hash, "origin": origin, "protocol": scheme + ":",
		"host": host, "hostname": strings.ToLower(u.Hostname()), "port": port,
		"pathname": path, "search": search, "hash": hash,
	})
	return string(out), nil
}
