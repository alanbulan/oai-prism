package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// frame 顶替 sentinel.openai.com/backend-api/sentinel/frame.html 那个隐藏 iframe。
//
// SDK 在父页面里算 PoW 和 dx，但 sentinel/req 由 iframe 代发（同源带 cookie），
// 父页面通过 postMessage 要结果。这里按 frame.html 里同一份 sdk.js 的 iframe 分支
// 原样实现它的缓存语义：
//
//   - init：立刻请求一次并缓存（SDK 在每次出完 token 5 秒后发 init 预取下一份）；
//   - token：缓存够新（540 秒内）且是同一个 requirements token 就直接用，
//     否则现取，4 秒取不到就回 {"e":"elapsed"} 错误载荷；用完把缓存标记为已消费。
type frame struct {
	doer    Doer
	reqURL  string
	referer string
	origin  string
	log     *slog.Logger

	mu    sync.Mutex
	cache map[string]*frameCache
}

type frameCache struct {
	chatReq   json.RawMessage
	proof     string
	lastFetch time.Time
}

type frameMessage struct {
	Type      string `json:"type"`
	Flow      string `json:"flow"`
	RequestID string `json:"requestId"`
	P         string `json:"p"`
}

const (
	chatReqTTL      = 540 * time.Second
	frameTokenWait  = 4 * time.Second
	frameReqTimeout = 20 * time.Second
)

func newFrame(doer Doer, origin, version string, log *slog.Logger) *frame {
	referer := origin + "/backend-api/sentinel/frame.html"
	if version != "" {
		referer += "?sv=" + version
	}
	return &frame{
		doer: doer, origin: origin, log: log,
		reqURL:  origin + "/backend-api/sentinel/req",
		referer: referer,
		cache:   map[string]*frameCache{},
	}
}

func (f *frame) entry(flow string) *frameCache {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cache[flow]
	if c == nil {
		c = &frameCache{}
		f.cache[flow] = c
	}
	return c
}

// handle 处理一条父页面发来的消息；reply 在任意协程上被调用一次（或不调用）。
func (f *frame) handle(m frameMessage, reply func([]byte)) {
	if m.Type != "init" && m.Type != "token" {
		return
	}
	flow := m.Flow
	if flow == "" {
		flow = "__default__"
	}
	go func() {
		var result any
		if m.Type == "init" {
			result = f.fetch(flow, m.P)
		} else {
			result = f.token(flow, m.P)
		}
		out, _ := json.Marshal(map[string]any{"type": "response", "requestId": m.RequestID, "result": result})
		reply(out)
	}()
}

func (f *frame) token(flow, p string) any {
	c := f.entry(flow)
	f.mu.Lock()
	fresh := c.chatReq != nil && time.Since(c.lastFetch) <= chatReqTTL && c.proof == p
	f.mu.Unlock()
	if !fresh {
		done := make(chan any, 1)
		go func() { done <- f.fetch(flow, p) }()
		select {
		case r := <-done:
			if s, ok := r.(string); ok {
				return s
			}
		case <-time.After(frameTokenWait):
			return errorPayload(map[string]any{"e": "elapsed", "p": p}, flow)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c.lastFetch = time.Time{}
	return map[string]any{"cachedChatReq": c.chatReq, "cachedProof": c.proof}
}

// fetch 发 sentinel/req（最多 3 次），成功则更新缓存并返回 {cachedChatReq, cachedProof}，
// 失败返回 SDK 同款的错误载荷字符串。
func (f *frame) fetch(flow, p string) any {
	c := f.entry(flow)
	f.mu.Lock()
	c.proof = p
	f.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"p": p, "flow": flow})
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), frameReqTimeout)
		status, resp, err := f.doer.Do(ctx, Request{
			Method: "POST", URL: f.reqURL, Body: body,
			Header: [][2]string{
				{"Content-Type", "text/plain;charset=UTF-8"},
				{"Accept", "*/*"},
				{"Origin", f.origin},
				{"Referer", f.referer},
				{"Sec-Fetch-Site", "same-origin"},
				{"Sec-Fetch-Mode", "cors"},
				{"Sec-Fetch-Dest", "empty"},
				{"Priority", "u=1, i"},
			},
		})
		cancel()
		if err == nil && status != 200 {
			err = fmt.Errorf("sentinel/req HTTP %d", status)
		}
		if err == nil && !json.Valid(resp) {
			err = fmt.Errorf("sentinel/req 响应不是 JSON")
		}
		if err == nil {
			f.mu.Lock()
			c.chatReq = json.RawMessage(resp)
			c.lastFetch = time.Now()
			out := map[string]any{"cachedChatReq": c.chatReq, "cachedProof": c.proof}
			f.mu.Unlock()
			return out
		}
		lastErr = err
		f.log.Debug("Sentinel sentinel/req 失败", "attempt", attempt+1, "err", err)
	}
	return errorPayload(map[string]any{"e": lastErr.Error(), "p": p, "a": 2}, flow)
}

// errorPayload 复刻 SDK 的 ce()：对象追加 flow 后 JSON 化（frame 里没有 oai-did，不带 id）。
func errorPayload(m map[string]any, flow string) string {
	type kv struct {
		k string
		v any
	}
	ordered := []kv{}
	for _, k := range []string{"e", "p", "a"} {
		if v, ok := m[k]; ok {
			ordered = append(ordered, kv{k, v})
		}
	}
	ordered = append(ordered, kv{"flow", flow})
	out := []byte{'{'}
	for i, e := range ordered {
		if i > 0 {
			out = append(out, ',')
		}
		k, _ := json.Marshal(e.k)
		v, _ := json.Marshal(e.v)
		out = append(append(append(out, k...), ':'), v...)
	}
	return string(append(out, '}'))
}
