package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/logx"
	"github.com/oai-prism/oaiprism/internal/tokens"
)

// fakeUpstream 模拟 prism.openai.com 的真实协议。
//
// 这里的每个形状都不是编的，而是**按实测报文 1:1 复刻**的：
//
//	POST /api/llm/response_with_tools_start
//	  body  {input:[...], previousResponseId, metadata, conversationId}
//	  resp  {status:"started", request_id, turn_state}
//	        {status:"completed", response:{status:"success"|"error", payload:{...}}}
//
//	POST /api/llm/response_with_tools_status
//	  body  {request_id, turn_state}          ← turn_state 必须原样回传
//	  resp  {status:"pending", turn_state}    ← 每次都是新的令牌
//	        {status:"completed", response:{...}}
//
//	POST /api/llm/response_with_tools_stop
//	  body  {request_id, conversation_id, turn_state}
type fakeUpstream struct {
	t *testing.T

	mu           sync.Mutex
	projectCount int
	projectFails bool
	startBodies  []map[string]any
	statusBodies []map[string]any
	stopBodies   []map[string]any
	lastAuth     string
	lastOrigin   string
	lastReferer  string
	reqHeaders   []http.Header // 每个上游请求的头（按到达顺序）

	// gens 按 request_id 保存每个请求自己的轮询状态。
	//
	// 必须按请求隔离：真实上游每个 request_id 有独立的状态机，
	// 用一个全局计数器会让并发测试互相踩，误报成"客户端没回传最新令牌"。
	gens map[string]*genState
	seq  int

	// reasoningInPending：生成中的帧也带（到目前为止的）思考摘要，终态帧是完整的。
	reasoningInPending bool
	// pollDelay：每次轮询先等这么久再回（模拟上游迟迟不出结果）。
	pollDelay time.Duration
	// liveProgress：第 n 次 pending 轮询带的 codex_live_progress（nil = 不带）。
	liveProgress func(n int) map[string]any
	// finalReasoning：终态帧 reasoning 条目的逐条 summary（空 = 默认的"先想一下"）。
	finalReasoning []string
	// deltaFiles：终态 payload 的 codexDeltaFiles（上游沙箱里的文件变更）。
	deltaFiles []any

	// conversationID 让测试可以要求上游回一个非空会话 ID。
	// 默认空（真实上游新建会话时它就是 null）。
	conversationID string

	// finalOnlyPayload 模拟"只有终态帧才带正文"的可能形态。
	//
	// 真实前端的轮询间隔是 5 秒，且在 pending 分支只读 turn_state、
	// 完全不看 response —— 这暗示 pending 帧里可能没有正文。
	// 但这一点无法在没有凭据的情况下证实，所以两种形态都要能work：
	//   false（默认）-> pending 帧带累计正文，前缀差分能还原出增量（真流式）
	//   true         -> 只有终态才有正文（只能等生成完再一次给出）
	finalOnlyPayload bool

	// noUsage 模拟真实上游：终态 payload 里没有 usage（抓包实证），网关须自行计数。
	noUsage bool

	// sandbox 打开沙箱链路（申请、资源令牌、Y-Sweet 凭证、同步状态）。默认关闭：
	// 关闭时申请沙箱 404，网关不带沙箱继续，其余测试不受影响。
	sandbox     bool
	sandboxSeq  int
	deadSandbox map[string]bool // 已被"回收"的沙箱令牌：代理对它们回 502（空响应体，与实测一致）
	deadAll     bool            // 沙箱服务整体故障：任何沙箱都回 502

	// replyParts 覆盖默认的逐字生成内容（见 parts）。
	replyParts []string

	// 会话登记（Server Action createProjectConversation）：convs 是登记过的会话 ID，
	// actionFails 让登记失败，goneConvs 里的会话在 start 时回 conversation_too_large。
	convs       map[string]bool
	convSeq     int
	actionFails bool
	goneConvs   map[string]bool
}

// sandboxAlive 报告请求所带的沙箱令牌是否仍可用；不可用时直接回 502。
func (f *fakeUpstream) sandboxAlive(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	dead := f.deadAll || f.deadSandbox[r.Header.Get("X-Crixet-Sandbox-Token")]
	f.mu.Unlock()
	if dead {
		w.WriteHeader(http.StatusBadGateway)
		return false
	}
	return true
}

func (f *fakeUpstream) sandboxRoutes(mux *http.ServeMux) {
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/backend/1/new", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sandboxSeq++
		tok := fmt.Sprintf("sbtok-%d", f.sandboxSeq)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"url": "https://prism.test/s/sandboxes/proxy", "token": tok})
	})
	mux.HandleFunc("/api/projects/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sandbox/resources-token") {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"access_token": "rt", "expires_at": time.Now().Add(time.Hour).Unix(), "max_age_seconds": 3600})
	})
	mux.HandleFunc("/api/y", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"docId": "doc", "url": "wss://y.test/d/doc/ws", "token": "yt"})
	})
	for _, p := range []string{"/s/sandboxes/proxy/resources-token", "/s/sandboxes/proxy/token"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if f.sandboxAlive(w, r) {
				writeJSON(w, map[string]any{"success": true})
			}
		})
	}
	mux.HandleFunc("/s/sandboxes/proxy/wait-for-sync", func(w http.ResponseWriter, r *http.Request) {
		if f.sandboxAlive(w, r) {
			http.NotFound(w, r) // 404 = 该沙箱无需同步（与真实前端判定一致）
		}
	})
}

// genState 是单个生成请求的状态。
type genState struct {
	polls int
	seq   int // 下次应当收到的 turn_state.seq
}

// parts 是假上游"逐字生成"的内容。
func (f *fakeUpstream) parts() []string {
	if len(f.replyParts) > 0 {
		return f.replyParts
	}
	return []string{"你好", "，这是", "一段流式回答。", "（完）"}
}

// payloadOutput 构造 response.payload.output。
func (f *fakeUpstream) payloadOutput(text string, withReasoning bool) map[string]any {
	reasoning := ""
	if withReasoning {
		reasoning = "先想一下"
	}
	return f.payloadWith(text, reasoning)
}

// payloadWith 同 payloadOutput，思考摘要由调用方给（空则不带 reasoning 条目）。
func (f *fakeUpstream) payloadWith(text, reasoning string) map[string]any {
	output := []any{}
	if reasoning != "" {
		summary := []any{map[string]any{"type": "summary_text", "text": reasoning}}
		if reasoning == "先想一下" && len(f.finalReasoning) > 0 {
			summary = summary[:0]
			for _, s := range f.finalReasoning {
				summary = append(summary, map[string]any{"type": "summary_text", "text": s})
			}
		}
		output = append(output, map[string]any{"type": "reasoning", "summary": summary})
	}
	output = append(output, map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "output_text", "text": text},
		},
	})
	if f.noUsage {
		return map[string]any{"output": output}
	}
	return map[string]any{
		"output": output,
		"usage":  map[string]any{"input_tokens": 12, "output_tokens": 40},
	}
}

func (f *fakeUpstream) handler() http.Handler {
	mux := http.NewServeMux()
	if f.sandbox {
		f.sandboxRoutes(mux)
	}

	// --- 会话登记：Next.js Server Action（POST 页面路径 + Next-Action 头，回 RSC 流）---
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.Method != http.MethodPost || r.Header.Get("Next-Action") == "" {
			http.NotFound(w, r)
			return
		}
		var args []string
		_ = json.NewDecoder(r.Body).Decode(&args)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.actionFails || len(args) != 1 || args[0] == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.convSeq++
		cid := fmt.Sprintf("cdx1_%04d", f.convSeq)
		if f.convs == nil {
			f.convs = map[string]bool{}
		}
		f.convs[cid] = true
		w.Header().Set("Content-Type", "text/x-component")
		fmt.Fprintf(w, "0:{\"a\":\"$@1\",\"f\":\"\",\"b\":\"build\"}\n1:%q\n", cid)
	})

	// --- 认证 ---
	mux.HandleFunc("/api/auth/session", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastAuth = r.Header.Get("Authorization")
		f.lastOrigin = r.Header.Get("Origin")
		f.lastReferer = r.Header.Get("Referer")
		f.mu.Unlock()

		if strings.Contains(r.Header.Get("Cookie"), "expired") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"session expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"accessToken":"refreshed-token",
			"expires":"2030-01-01T00:00:00Z",
			"user":{"id":"u1","email":"tester@example.com"},
			"account":{"id":"acc-1","planType":"plus"}
		}`))
	})

	// --- 项目 ---
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		if f.projectFails {
			f.mu.Unlock()
			// 上游维护窗口中真实的返回形态。
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"Service Unavailable"}`))
			return
		}
		f.projectCount++
		n := f.projectCount
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"uuid":"proj-%d","id":"proj-%d"}`, n, n)
	})

	mux.HandleFunc("/api/project-access", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"has_access":true,"access":"write"}`))
	})

	mux.HandleFunc("/api/codex/conversation-history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[]}`))
	})

	// --- 生成：start ---
	mux.HandleFunc("/api/llm/response_with_tools_start", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		f.startBodies = append(f.startBodies, body)
		f.mu.Unlock()

		// 复刻上游最直白的校验：input 必须是数组。
		if _, ok := body["input"].([]any); !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","message":"input must be an array"}`))
			return
		}

		if strings.Contains(r.Header.Get("Authorization"), "bad-token") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid token"}}`))
			return
		}

		// 复刻"HTTP 200 + response.status=error"这个反直觉的失败表达。
		model := ""
		if meta, _ := body["metadata"].(map[string]any); meta != nil {
			model, _ = meta["model"].(string)
		}
		if model == "failing-model" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"completed","request_id":"req-err",` +
				`"response":{"status":"error","payload":{"reason":"unknown","message":"User not found"}}}`))
			return
		}
		// 复刻单条消息超限（2026-10-04 实测文案）。
		if model == "too-large-model" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"completed","request_id":"req-big",` +
				`"response":{"status":"error","payload":{"reason":"unknown","message":"This request is too large to send. Shorten your message or selected text and try again."}}}`))
			return
		}
		if cid, _ := body["conversationId"].(string); cid != "" {
			f.mu.Lock()
			gone := f.goneConvs[cid]
			f.mu.Unlock()
			if gone {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"completed","request_id":"req-gone",` +
					`"response":{"status":"error","payload":{"reason":"conversation_too_large","message":"conversation_too_large"}}}`))
				return
			}
		}
		// 复刻"start 直接返回终态"（短回答或命中缓存）。
		if model == "instant-model" {
			w.Header().Set("Content-Type", "application/json")
			out, _ := json.Marshal(map[string]any{
				"status":     "completed",
				"request_id": "req-instant",
				"response": map[string]any{
					"status":  "success",
					"payload": f.payloadOutput("立即返回", true),
				},
			})
			_, _ = w.Write(out)
			return
		}

		f.mu.Lock()
		f.seq++
		rid := fmt.Sprintf("req-%d", f.seq)
		if f.gens == nil {
			f.gens = map[string]*genState{}
		}
		f.gens[rid] = &genState{polls: 0, seq: 1}
		f.mu.Unlock()

		started := map[string]any{
			"status":          "started",
			"request_id":      rid,
			"conversation_id": nil,
			"turn_state":      map[string]any{"seq": 1},
		}
		if f.conversationID != "" {
			started["conversation_id"] = f.conversationID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(started)
	})

	// --- 生成：status ---
	mux.HandleFunc("/api/llm/response_with_tools_status", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.pollDelay > 0 {
			select { // 模拟上游迟迟不出结果（客户端可能先断开）
			case <-time.After(f.pollDelay):
			case <-r.Context().Done():
				return
			}
		}

		f.mu.Lock()
		f.statusBodies = append(f.statusBodies, body)
		f.mu.Unlock()

		// 复刻上游的字段校验顺序（request_id 先，turn_state 后）。
		rid, _ := body["request_id"].(string)
		if rid == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","message":"request_id is required"}`))
			return
		}
		ts, _ := body["turn_state"].(map[string]any)
		if ts == nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","message":"turn_state is required"}`))
			return
		}

		f.mu.Lock()
		st := f.gens[rid]
		if st == nil {
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","message":"unknown request_id"}`))
			return
		}
		// 必须回传服务端下发的最新令牌，否则说明客户端在自造状态。
		if got, _ := ts["seq"].(float64); int(got) != st.seq {
			f.mu.Unlock()
			f.t.Errorf("turn_state 不是服务端下发的最新值：收到 seq=%v，应为 %d（request=%s）",
				got, st.seq, rid)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","message":"stale turn_state"}`))
			return
		}
		st.polls++
		n := st.polls
		f.mu.Unlock()

		parts := f.parts()
		upto := n
		if upto > len(parts) {
			upto = len(parts)
		}
		text := strings.Join(parts[:upto], "")

		w.Header().Set("Content-Type", "application/json")

		if upto < len(parts) {
			resp := map[string]any{
				"status":     "pending",
				"request_id": rid,
				"turn_state": map[string]any{"seq": n + 1},
			}
			// 非 finalOnly 模式下 pending 帧带累计正文 ——
			// 这样前缀差分才有东西可差，也是"真流式"的前提。
			if f.liveProgress != nil {
				if lp := f.liveProgress(n); lp != nil {
					resp["codex_live_progress"] = lp
				}
			}
			if !f.finalOnlyPayload {
				payload := f.payloadOutput(text, false)
				if f.reasoningInPending {
					payload = f.payloadWith(text, "先想") // 生成中的帧带到目前为止的思考摘要
				}
				resp["response"] = map[string]any{
					"status":  "success",
					"payload": payload,
				}
			}
			f.mu.Lock()
			st.seq = n + 1
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		final := f.payloadOutput(text, true)
		if f.deltaFiles != nil {
			final["codexDeltaFiles"] = f.deltaFiles
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "completed",
			"request_id": rid,
			"turn_state": map[string]any{"seq": n + 1},
			"response": map[string]any{
				"status":  "success",
				"payload": final,
			},
		})
	})

	// --- 生成：stop ---
	mux.HandleFunc("/api/llm/response_with_tools_stop", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.stopBodies = append(f.stopBodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"stopped","request_id":"req-1"}`))
	})

	// --- 沙箱 ---
	mux.HandleFunc("/s/sandboxes/proxy/render", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-1","status":"queued"}`))
	})
	mux.HandleFunc("/s/sandboxes/proxy/render-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","url":"https://example.com/out.pdf"}`))
	})

	// --- 回显端点：验证凭据注入与头清洗 ---
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__Secure-next-auth.session-token", Value: "LEAKED"})
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"gotAuth":       r.Header.Get("Authorization") != "",
			"gotCookie":     r.Header.Get("Cookie"),
			"body":          buf.String(),
			"origin":        r.Header.Get("Origin"),
			"referer":       r.Header.Get("Referer"),
			"userAgent":     r.Header.Get("User-Agent"),
			"clientHdrKept": r.Header.Get("X-Client-Only"),
		})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqHeaders = append(f.reqHeaders, r.Header.Clone())
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
}

// newTestServer 起一个完整代理实例，指向假上游。
func newTestServer(t *testing.T, up *fakeUpstream, accounts []config.AccountConfig, tune func(*config.Config)) (*httptest.Server, *fakeUpstream) {
	t.Helper()
	ts, up, _ := newTestServerWithSrv(t, up, accounts, tune)
	return ts, up
}

// newTestServerWithSrv 同 newTestServer，另外返回 *Server，供需要直接操作账号池的测试使用。
func newTestServerWithSrv(t *testing.T, up *fakeUpstream, accounts []config.AccountConfig, tune func(*config.Config)) (*httptest.Server, *fakeUpstream, *Server) {
	t.Helper()

	upstream := httptest.NewServer(up.handler())
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.Upstream.BaseURL = upstream.URL
	cfg.Upstream.MaxRetries = 0 // 测试要确定性，不要内部重试干扰断言
	cfg.Upstream.ForceHTTP2 = false
	cfg.Upstream.RetryBackoff = time.Millisecond
	cfg.Creds.Mode = "static"
	cfg.Creds.File = filepath.Join(t.TempDir(), "accounts.json")
	cfg.Creds.AutoRefresh = false
	cfg.Pool.Strategy = "least_inflight"
	cfg.Pool.HealthCheck = false
	cfg.Pool.Cooldown = time.Second
	cfg.Facade.PollInterval = 5 * time.Millisecond
	cfg.Facade.PollWaitMs = 0
	cfg.Facade.UseStatusWait = false
	cfg.Facade.MaxPollTimeout = 10 * time.Second
	cfg.Capture.Enabled = false
	cfg.Metrics.Enabled = true
	cfg.Creds.Accounts = accounts

	if tune != nil {
		tune(cfg)
	}

	log := logx.Setup("error", "text")
	srv, err := New(cfg, log)
	if err != nil {
		t.Fatalf("初始化服务失败: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close()
	})
	return ts, up, srv
}

func goodAccount() []config.AccountConfig {
	return []config.AccountConfig{{ID: "main", AccessToken: "good-token", MaxConcurrency: 8}}
}

// ---------------------------- 正常路径 ----------------------------

func TestE2E_ChatCompletionsNonStream(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{
		"model":"gpt-5",
		"messages":[
			{"role":"system","content":"你是一个严谨的数学助手。"},
			{"role":"user","content":"解释一下拉格朗日中值定理"}
		]
	}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, b)
	}

	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	if out.Object != "chat.completion" {
		t.Errorf("object = %q", out.Object)
	}
	if out.Model != "gpt-5" {
		t.Errorf("model 应当回显对外模型名，得到 %q", out.Model)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices 数量 = %d", len(out.Choices))
	}
	want := "你好，这是一段流式回答。（完）"
	if out.Choices[0].Message.Content != want {
		t.Errorf("内容错误\n got = %q\nwant = %q", out.Choices[0].Message.Content, want)
	}
	if out.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q", out.Choices[0].Message.Role)
	}
	if out.Choices[0].Message.ReasoningContent != "先想一下" {
		t.Errorf("思维链未透传: %q", out.Choices[0].Message.ReasoningContent)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", out.Choices[0].FinishReason)
	}
	if out.Usage.CompletionTokens == 0 {
		t.Error("usage 透传失败")
	}

	// —— 上游收到的请求必须符合实测协议 ——
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.startBodies) == 0 {
		t.Fatal("上游未收到 start 请求")
	}
	sb := up.startBodies[0]

	input, ok := sb["input"].([]any)
	if !ok {
		t.Fatalf("input 必须是数组，实际 %T（上游会回 input must be an array）", sb["input"])
	}
	if len(input) != 2 {
		t.Fatalf("input 条目数 = %d，want 2（system + user）", len(input))
	}
	first, _ := input[0].(map[string]any)
	// 上游 input 数组接受 system 角色，必须保持 —— 折成 user 会把
	// "指令"降级成"用户发言"，从而削弱模型对系统提示的服从。
	if first["role"] != "system" {
		t.Errorf("system 消息必须保持 system 角色，得到 %+v", first)
	}
	content, _ := first["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("第一条 input 缺少 content: %+v", first)
	}
	c0, _ := content[0].(map[string]any)
	if c0["type"] != "input_text" {
		t.Errorf("用户内容块类型应为 input_text，得到 %v", c0["type"])
	}
	if !strings.Contains(fmt.Sprint(c0["text"]), "严谨的数学助手") {
		t.Errorf("system 内容丢失: %v", c0["text"])
	}

	// 模型参数必须在 metadata 里，而不是请求体顶层。
	if _, exists := sb["model"]; exists {
		t.Error("model 不应出现在请求体顶层（实测它在 metadata 内）")
	}
	meta, ok := sb["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 metadata: %+v", sb)
	}
	if meta["model"] == nil {
		t.Error("metadata.model 缺失")
	}
	if meta["frontend_origin"] == nil {
		t.Error("metadata.frontend_origin 缺失（缺它容易被风控识别为脚本）")
	}
	if meta["projectId"] == nil {
		t.Error("metadata.projectId 缺失")
	}
	if sb["stream"] != nil {
		t.Error("不应发送 stream 字段（真实报文里没有它）")
	}

	// turn_state 必须被原样回传（假上游内部已断言，这里再确认请求里带上了）。
	if len(up.statusBodies) == 0 {
		t.Fatal("上游未收到 status 轮询请求")
	}
	if up.statusBodies[0]["turn_state"] == nil {
		t.Error("status 请求必须带上 turn_state")
	}

	// 最关键的回归断言。
}

func TestE2E_ChatCompletionsStream(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("缺少 X-Accel-Buffering，Nginx 会缓冲整个流")
	}

	var (
		frames   []string
		content  strings.Builder
		reason   strings.Builder
		sawRole  bool
		sawDone  bool
		finish   string
		sawUsage bool
	)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		frames = append(frames, payload)
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk 不是合法 JSON: %v\n%s", err, payload)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %q", chunk.Object)
		}
		if chunk.Usage != nil {
			sawUsage = true
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if chunk.Choices[0].Delta.Role == "assistant" {
			sawRole = true
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
		reason.WriteString(chunk.Choices[0].Delta.ReasoningContent)
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	if !sawRole {
		t.Error("缺少首帧 role=assistant，严格客户端会报格式错误")
	}
	if !sawDone {
		t.Error("缺少 [DONE] 终止帧")
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q", finish)
	}
	if !sawUsage {
		t.Error("include_usage 为 true 时应当返回 usage 帧")
	}
	if got, want := content.String(), "你好，这是一段流式回答。（完）"; got != want {
		t.Errorf("流式拼接内容错误\n got = %q\nwant = %q", got, want)
	}
	// 思维链必须单独成帧，不能混进 content。
	if reason.String() != "先想一下" {
		t.Errorf("思维链未独立输出: %q", reason.String())
	}
	if strings.Contains(content.String(), "先想一下") {
		t.Error("思维链混进了正文，客户端会把推理过程当答案渲染")
	}
	// 必须真的分多帧，否则说明流式退化成了整体返回。
	if len(frames) < 4 {
		t.Errorf("帧数过少(%d)，流式可能退化成了一次性返回", len(frames))
	}
}

func TestE2E_ProjectReuse(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"同一个会话的问题"}]}`
	for i := 0; i < 3; i++ {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = readBody(resp)
		resp.Body.Close()
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if up.projectCount != 1 {
		t.Fatalf("同一会话应当只建 1 个项目，实际 %d 个", up.projectCount)
	}
	if len(up.startBodies) != 3 {
		t.Fatalf("start 调用次数 = %d", len(up.startBodies))
	}
	first := fmt.Sprint(up.startBodies[0]["metadata"].(map[string]any)["projectId"])
	for i, b := range up.startBodies {
		pid := fmt.Sprint(b["metadata"].(map[string]any)["projectId"])
		if pid != first {
			t.Fatalf("第 %d 次用了不同项目: %s != %s", i, pid, first)
		}
	}
}

func TestE2E_ProjectIsolationBetweenSessions(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	for _, q := range []string{"会话A的问题", "会话B的问题"} {
		body := fmt.Sprintf(`{"model":"gpt-5","messages":[{"role":"user","content":%q}]}`, q)
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = readBody(resp)
		resp.Body.Close()
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if up.projectCount != 2 {
		t.Fatalf("不同会话应各自建项目，实际 %d", up.projectCount)
	}
}

// TestE2E_ProjectFailureDegradesGracefully 覆盖上游维护窗口。
//
// 实测：上游维护时 POST /api/projects 返回 503，但推理端点未必同样不可用。
// 因此建项目失败不该直接判死，应当降级为"无项目上下文"继续尝试。
func TestE2E_ProjectFailureDegradesGracefully(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t, projectFails: true}, goodAccount(), nil)

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"维护期间也要能用"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("建项目失败时应降级继续，而不是整体失败：%d %s", resp.StatusCode, b)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.startBodies) == 0 {
		t.Fatal("降级后仍应发起 start 请求")
	}
	meta, _ := up.startBodies[0]["metadata"].(map[string]any)
	if meta != nil {
		if _, has := meta["projectId"]; has {
			t.Error("项目创建失败时不应伪造 projectId")
		}
	}
}

// ---------------------------- 失败与容错 ----------------------------

// TestE2E_NestedErrorIsNotSuccess 是本项目最重要的一条测试。
//
// 上游用 HTTP 200 + status:"completed" + response.status:"error" 表达失败。
// 只看 HTTP 状态码、或只看顶层 status 的实现，会把失败当成
// "成功但内容为空"，客户端拿到一个空回答还以为一切正常。
func TestE2E_NestedErrorIsNotSuccess(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"failing-model","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("上游的嵌套 error 被误判成了成功 —— 这是最危险的一类 bug")
	}
	b, _ := readBody(resp)
	if !strings.Contains(b, "User not found") {
		t.Errorf("上游错误信息未透出: %s", b)
	}
	if !strings.Contains(b, "unknown") {
		t.Errorf("上游错误原因未透出: %s", b)
	}
}

func TestE2E_NestedErrorInStream(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"failing-model","stream":true,"messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	b, _ := readBody(resp)
	if !strings.Contains(b, "User not found") {
		t.Fatalf("流式下上游错误未透出: %s", b)
	}
	if !strings.Contains(b, "[DONE]") {
		t.Error("出错后仍应发送 [DONE]，否则客户端会一直等")
	}
}

func TestE2E_AccountFailover(t *testing.T) {
	accounts := []config.AccountConfig{
		{ID: "broken", AccessToken: "bad-token", MaxConcurrency: 8},
		{ID: "working", AccessToken: "good-token", MaxConcurrency: 8},
	}
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, accounts, nil)

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"测试容错"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("应当自动换号成功，实际 %d: %s", resp.StatusCode, b)
	}

	resp2, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		b, _ := readBody(resp2)
		t.Fatalf("第二个请求失败: %d %s", resp2.StatusCode, b)
	}
}

func TestE2E_NoAccountsReturnsClearError(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, nil, nil)

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"x"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("无账号时应当返回 502，实际 %d", resp.StatusCode)
	}
	b, _ := readBody(resp)
	if !strings.Contains(b, "账号") {
		t.Errorf("错误信息应提示账号问题: %s", b)
	}
	if !strings.Contains(strings.ToLower(b), "accounts.json") {
		t.Errorf("错误信息应指出凭据文件位置: %s", b)
	}
}

func TestE2E_ValidationErrors(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"非法 JSON", `{`, http.StatusBadRequest},
		{"空 messages", `{"model":"gpt-5","messages":[]}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("状态码 = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

// TestE2E_StopOnClientDisconnect 验证"客户端中途断开就通知上游停止"。
//
// 这条链路直接关系到成本：不通知的话上游会继续跑完并扣额度。
//
// 时序很关键 —— 必须在**生成已经开始之后**才断开，
// 否则根本没有 request_id 可停（那属于"还没开始就放弃"，不是这条链路）。
// 所以这里先等到上游收到 start 请求，再硬断开。
//
// 用裸 TCP 而不是 http.Client：Body.Close() 有时会让连接留在池里，
// 断开不一定被立即感知；裸连接 Close 是确定性的硬断开。
func TestE2E_StopOnClientDisconnect(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) {
		c.Facade.PollInterval = 100 * time.Millisecond
		c.Facade.MaxPollTimeout = 10 * time.Second
	})

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"会被中断"}]}`
	req := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	// 先读一点，确认服务端已经在输出。
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("未收到任何响应数据: %v", err)
	}

	// 等到上游确实收到了 start（生成已经开始），这才是有意义的中断点。
	startDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(startDeadline) {
		up.mu.Lock()
		n := len(up.startBodies)
		up.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	up.mu.Lock()
	started := len(up.startBodies) > 0
	up.mu.Unlock()
	if !started {
		t.Fatal("上游始终没收到 start 请求，测试前提不成立")
	}

	// 硬断开。
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		up.mu.Lock()
		n := len(up.stopBodies)
		up.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.stopBodies) == 0 {
		t.Fatal("客户端中途断开后未通知上游停止，会让这次生成白跑完并消耗额度")
	}
	sb := up.stopBodies[0]
	if sb["request_id"] == nil {
		t.Errorf("stop 请求缺少 request_id: %+v", sb)
	}
	if sb["turn_state"] == nil {
		t.Errorf("stop 请求缺少 turn_state: %+v", sb)
	}
}

// TestE2E_AbortBeforeStartIsClean 覆盖"还没开始生成就断开"。
//
// 这种情况下没有 request_id 可停，因此不该去调 stop；
// 但也不该留下误导性日志或把取消当成"降级继续"。
func TestE2E_AbortBeforeStartIsClean(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), func(c *config.Config) {
		c.Facade.PollInterval = 100 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Content-Type", "application/json")

	// 发起后立刻取消，赶在 start 之前。
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	// 只要求不 panic、不挂死；错误类型是取消相关即可。
	if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
		// 也可能刚好赶在前面完成，容忍。
		t.Log("请求在取消前已完成")
	}
}

// TestE2E_StreamWithFinalOnlyPayload 覆盖"只有终态帧才带正文"的形态。
//
// 真实前端每 5 秒轮询一次，且 pending 分支只读 turn_state、不看 response，
// 这暗示 pending 帧里可能没有正文。没有凭据无法证实，所以两种形态都要能work：
// 这种情况下我们只能等生成结束再一次性给出，但结果必须完整。
func TestE2E_StreamWithFinalOnlyPayload(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t, finalOnlyPayload: true}, goodAccount(), nil)

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var content strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatal(err)
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	// 内容必须完整 —— 只是分片粒度退化了，正确性不能退化。
	if got, want := content.String(), "你好，这是一段流式回答。（完）"; got != want {
		t.Fatalf("内容错误\n got = %q\nwant = %q", got, want)
	}
}

// TestE2E_InstantCompletion 覆盖"start 直接返回终态"（短回答 / 命中缓存）。
func TestE2E_InstantCompletion(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"instant-model","messages":[{"role":"user","content":"短问题"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 %d: %s", resp.StatusCode, b)
	}
	b, _ := readBody(resp)
	if !strings.Contains(b, "立即返回") {
		t.Fatalf("start 直接返回的内容没被取到: %s", b)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.statusBodies) != 0 {
		t.Fatalf("已经拿到终态就不该再轮询，实际轮询了 %d 次", len(up.statusBodies))
	}
}

// ---------------------------- 其它 API 形态 ----------------------------

func TestE2E_AnthropicMessages(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{
		"model":"gpt-5","max_tokens":1024,
		"system":"你是助手",
		"messages":[{"role":"user","content":"你好"}]
	}`
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "message" || out.Role != "assistant" || out.StopReason != "end_turn" {
		t.Fatalf("Anthropic 响应结构错误: %+v", out)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "你好，这是一段流式回答。（完）" {
		t.Fatalf("内容错误: %+v", out.Content)
	}

	// Anthropic 的 system 必须折进 input。
	up.mu.Lock()
	defer up.mu.Unlock()
	input, _ := up.startBodies[0]["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("system 应折成前置条目，input 条目数 = %d", len(input))
	}
	first, _ := input[0].(map[string]any)
	c0 := first["content"].([]any)[0].(map[string]any)
	if !strings.Contains(fmt.Sprint(c0["text"]), "你是助手") {
		t.Errorf("Anthropic system 丢失: %v", c0["text"])
	}
}

func TestE2E_AnthropicStreamEventOrder(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var events []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"message_start", "content_block_start",
		"content_block_delta", "content_block_delta", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if len(events) != len(want) {
		t.Fatalf("事件序列长度不符\n got = %v\nwant = %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("第 %d 个事件是 %q，应为 %q\n完整: %v", i, events[i], want[i], events)
		}
	}
}

func TestE2E_ResponsesAPI(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","instructions":"简洁作答","input":"你好"}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("Responses 结构错误: %+v", out)
	}
	// 上游的思考摘要在前（reasoning 条目），正文消息在后
	if len(out.Output) != 2 || out.Output[0].Type != "reasoning" || len(out.Output[0].Summary) != 1 ||
		out.Output[0].Summary[0].Type != "summary_text" || out.Output[0].Summary[0].Text != "先想一下" ||
		out.Output[1].Type != "message" || len(out.Output[1].Content) != 1 {
		t.Fatalf("output 结构错误: %+v", out.Output)
	}
	if out.Output[1].Content[0].Text != "你好，这是一段流式回答。（完）" {
		t.Fatalf("内容错误: %q", out.Output[1].Content[0].Text)
	}

	// instructions 也要折进 input。
	up.mu.Lock()
	defer up.mu.Unlock()
	input, _ := up.startBodies[0]["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("instructions 应折成前置条目，input 条目数 = %d", len(input))
	}
}

func TestE2E_Models(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out ModelList
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) == 0 {
		t.Fatalf("模型列表错误: %+v", out)
	}
	var found bool
	for _, m := range out.Data {
		if m.ID == config.DefaultPrismModel {
			found = true
		}
	}
	if !found {
		t.Fatalf("默认模型 %s 应当出现在列表里", config.DefaultPrismModel)
	}
}

// ---------------------------- 原样反代 ----------------------------

func TestE2E_RawProxy_InjectsCredentialsAndStripsSetCookie(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), func(c *config.Config) {
		c.RawProxy.AllowPaths = append(c.RawProxy.AllowPaths, "/api/echo")
	})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/prism/api/echo", strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-own-token")
	req.Header.Set("Cookie", "client-own-cookie=1")
	req.Header.Set("X-Client-Only", "keep-me")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 %d: %s", resp.StatusCode, b)
	}

	// 最重要的安全断言：上游的 Set-Cookie 绝不能透给调用方。
	for _, c := range resp.Cookies() {
		if c.Value == "LEAKED" {
			t.Fatal("严重：上游的会话 Cookie 被透传给调用方，等于泄漏账号")
		}
	}

	var echo struct {
		GotAuth    bool   `json:"gotAuth"`
		GotCookie  string `json:"gotCookie"`
		Origin     string `json:"origin"`
		ClientHdrX string `json:"clientHdrKept"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&echo); err != nil {
		t.Fatal(err)
	}
	if !echo.GotAuth {
		t.Error("未注入 Authorization")
	}
	if strings.Contains(echo.GotCookie, "client-own-cookie") {
		t.Error("客户端自带的 Cookie 应当被丢弃")
	}
	if echo.Origin == "" {
		t.Error("未注入 Origin，容易被风控识别为脚本")
	}
	if echo.ClientHdrX != "keep-me" {
		t.Error("自定义请求头应当透传")
	}
}

func TestE2E_RawProxy_PathWhitelist(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	resp, err := http.Get(ts.URL + "/prism/api/some/random/endpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("白名单外的路径应当 403，实际 %d", resp.StatusCode)
	}
}

func TestE2E_RawProxy_AccountHeaderReported(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), func(c *config.Config) {
		c.RawProxy.AllowPaths = append(c.RawProxy.AllowPaths, "/api/auth/session")
	})

	resp, err := http.Get(ts.URL + "/prism/api/auth/session")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Oaiprism-Account"); got != "main" {
		t.Fatalf("X-Oaiprism-Account = %q, want main", got)
	}
}

// ---------------------------- 运维端点 ----------------------------

func TestE2E_OpsEndpoints(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz = %d", resp.StatusCode)
		}
	})

	t.Run("readyz", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("readyz = %d", resp.StatusCode)
		}
	})

	t.Run("metrics", func(t *testing.T) {
		resp, _ := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"x"}]}`))
		if resp != nil {
			resp.Body.Close()
		}

		resp, err := http.Get(ts.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := readBody(resp)
		for _, want := range []string{
			"oaiprism_http_requests_total",
			"oaiprism_facade_runs_total",
			"oaiprism_poll_rounds_total",
			"oaiprism_upstream_requests_total",
			"oaiprism_uptime_seconds",
		} {
			if !strings.Contains(b, want) {
				t.Errorf("指标缺失: %s", want)
			}
		}
		// 标签基数必须归一化：不能出现带具体 ID 的路径标签。
		if strings.Contains(b, "req-1") || strings.Contains(b, "proj-") {
			t.Error("指标里出现了未归一化的标签值，会导致基数爆炸")
		}
	})

	t.Run("accounts", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/admin/accounts")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := readBody(resp)
		if !strings.Contains(b, "\"main\"") {
			t.Fatalf("账号列表缺少 main: %s", b)
		}
		if strings.Contains(b, "good-token") {
			t.Fatal("严重：账号列表泄漏了 access token")
		}
	})
}

func TestE2E_RequestIDPropagated(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	req.Header.Set("X-Request-Id", "trace-me-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got != "trace-me-123" {
		t.Fatalf("请求 ID 未透传: %q", got)
	}

	resp2, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.Header.Get("X-Request-Id") == "" {
		t.Fatal("未自动生成请求 ID")
	}
}

func TestE2E_APIKeyAuth(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), func(c *config.Config) {
		c.Facade.APIKeys = []string{"sk-test-123"}
	})

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"x"}]}`

	t.Run("无 key 被拒", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("应当 401，实际 %d", resp.StatusCode)
		}
	})

	t.Run("错误 key 被拒", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("应当 401，实际 %d", resp.StatusCode)
		}
	})

	t.Run("正确 key 放行", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test-123")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := readBody(resp)
			t.Fatalf("应当 200，实际 %d: %s", resp.StatusCode, b)
		}
	})

	t.Run("x-api-key 形式也支持", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("x-api-key", "sk-test-123")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("应当 200，实际 %d", resp.StatusCode)
		}
	})
}

// TestE2E_ConcurrentStreams 并发压一遍，主要目的是在 -race 下暴露数据竞争。
func TestE2E_ConcurrentStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过")
	}
	ts, _ := newTestServer(t, &fakeUpstream{t: t},
		[]config.AccountConfig{
			{ID: "a", AccessToken: "t1", MaxConcurrency: 32},
			{ID: "b", AccessToken: "t2", MaxConcurrency: 32},
		}, nil)

	const n = 20
	var wg sync.WaitGroup
	var failures atomic.Int64

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"并发-%d"}]}`, i)
			resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			if err != nil {
				failures.Add(1)
				return
			}
			defer resp.Body.Close()
			b, _ := readBody(resp)
			if resp.StatusCode != http.StatusOK || !strings.Contains(b, "[DONE]") {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if f := failures.Load(); f != 0 {
		t.Fatalf("%d/%d 个并发流失败", f, n)
	}
}

// ---------------------------- 调用方身份透传 ----------------------------

// TestE2E_UserReachesUpstream 端到端验证 user 字段真的到达上游。
//
// 背景：facade 的请求结构里早就有 `user` 的 json tag，但没有任何代码
// 把它往下送 —— 解析了、丢掉了，客户端和上游都不知情。单元测试
// （buildStartPayload）测不到这一层，必须在集成层面断言。
func TestE2E_UserReachesUpstream(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","user":"u-e2e-1","messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, b)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.startBodies) == 0 {
		t.Fatal("上游未收到 start 请求")
	}
	meta, ok := up.startBodies[0]["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 metadata: %+v", up.startBodies[0])
	}
	if meta["userId"] != "u-e2e-1" {
		t.Errorf("metadata.userId = %v, want u-e2e-1（user 未透传）", meta["userId"])
	}
}

// TestE2E_NoUserSendsNoUserID 覆盖反面：没传 user 时不要发空字段。
//
// 上游可能把空 userId 当作"匿名请求"从而施加更严的限流，
// 所以"发空值"比"不发"更糟。
func TestE2E_NoUserSendsNoUserID(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, b)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	meta, ok := up.startBodies[0]["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 metadata: %+v", up.startBodies[0])
	}
	if v, bad := meta["userId"]; bad {
		t.Errorf("未传 user 时不应出现 userId，得到 %v", v)
	}
}

// ---------------------------- 客户端 metadata 透传 ----------------------------

// TestE2E_ClientMetadataPassesThrough 端到端验证 metadata 真的到达上游，
// 且保留键无法被客户端覆盖。
//
// 背景：此前 metadata 被列进"已消费字段"，却又没进 RunRequest.Metadata，
// 结果是客户端传的上下文被整个丢弃（静默失效）。
// 单元测试测不到"中间一层忘了传"，所以必须有这一条。
func TestE2E_ClientMetadataPassesThrough(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","metadata":{"trace_id":"t-42","model":"evil-model"},
	          "messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, b)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	meta, ok := up.startBodies[0]["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 metadata: %+v", up.startBodies[0])
	}
	if meta["trace_id"] != "t-42" {
		t.Errorf("客户端 metadata 未透传: %+v", meta)
	}
	if meta["model"] == "evil-model" {
		t.Errorf("客户端不应能覆盖 model: %+v", meta)
	}
}

// ---------------------------- 会话 ID 回传 ----------------------------

// TestE2E_ConversationIDReturned 验证上游会话 ID 能回到客户端。
func TestE2E_ConversationIDReturned(t *testing.T) {
	up := &fakeUpstream{t: t, conversationID: "conv-e2e-1"}
	ts, up := newTestServer(t, up, goodAccount(), nil)

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, b)
	}

	if v := resp.Header.Get("x-prism-conversation-id"); v != "conv-e2e-1" {
		t.Errorf("响应头 x-prism-conversation-id = %q", v)
	}
	var out struct {
		PrismConversationID string `json:"prism_conversation_id"`
	}
	raw, _ := readBody(resp)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if out.PrismConversationID != "conv-e2e-1" {
		t.Errorf("prism_conversation_id = %q", out.PrismConversationID)
	}
}

// TestE2E_ConversationIDAccepted 验证客户端带回来的会话 ID 能找回原来的上游会话。
//
// 网关在响应里回传上游会话 ID；只发本轮消息的客户端把它带回来，就续接同一个会话。
// 不认识的会话 ID 不透传给上游（没登记的会话上游不续接，自造的直接 403）。
func TestE2E_ConversationIDAccepted(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"记住暗号：BACK-12"}]}`)
	cid := startConv(t, up, 0)

	body := fmt.Sprintf(`{"model":"gpt-5","conversation_id":%q,"messages":[{"role":"user","content":"继续"}]}`, cid)
	postLocal(t, ts.URL+"/v1/chat/completions", body)
	_, user := requireSystemUser(t, upstreamInput(t, up, 1))
	if startConv(t, up, 1) != cid || user != "继续" {
		t.Fatalf("带回的会话 ID 应续接同一个上游会话、只发本轮: cid=%q user=%q", startConv(t, up, 1), user)
	}

	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","conversation_id":"conv-unknown","messages":[{"role":"user","content":"你好"}]}`)
	up.mu.Lock()
	defer up.mu.Unlock()
	if got := up.startBodies[2]["conversationId"]; got == "conv-unknown" {
		t.Errorf("不认识的会话 ID 不应透传给上游: %+v", up.startBodies[2])
	}
	// 客户端用的 snake_case 不能被当"未知字段"原样塞进请求体顶层。
	if _, bad := up.startBodies[2]["conversation_id"]; bad {
		t.Errorf("conversation_id 已被消费，不该出现在请求体顶层: %+v", up.startBodies[2])
	}
}

// TestE2E_APIKeyAuth_ExemptProbes 验证配置 API Key 时探针端点匿名可达，业务路由被拦截。
func TestE2E_APIKeyAuth_ExemptProbes(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), func(c *config.Config) {
		c.Facade.APIKeys = []string{"sk-e2e-secret"}
	})

	// 匿名访问探针端点应成功（200 OK）
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("探针 %s 应豁免鉴权返回 200，得到 %d", path, resp.StatusCode)
		}
	}

	// 匿名访问业务端点应拦截（401 Unauthorized）
	bizBody := `{"model":"gpt-5","messages":[{"role":"user","content":"你好"}]}`
	respBiz, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(bizBody))
	if err != nil {
		t.Fatal(err)
	}
	respBiz.Body.Close()
	if respBiz.StatusCode != http.StatusUnauthorized {
		t.Errorf("未带 key 的业务端点应返回 401，得到 %d", respBiz.StatusCode)
	}

	// 带 key 访问业务端点应成功（200 OK）
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(bizBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-e2e-secret")
	respAuth, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respAuth.Body.Close()
	if respAuth.StatusCode != http.StatusOK {
		t.Errorf("带 key 的业务端点应返回 200，得到 %d", respAuth.StatusCode)
	}
}

// TestE2E_Responses_KnownFieldsNotPassedAsExtra 验证 Responses API 的已消费字段不会污染上游。
func TestE2E_Responses_KnownFieldsNotPassedAsExtra(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","input":"测试","user":"usr-123","previousResponseId":"resp-prev-1"}`
	resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, b)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	first := up.startBodies[0]
	// user 应当进入 metadata.userId，而不应出现在请求体顶层
	if _, bad := first["user"]; bad {
		t.Errorf("user 已被消费，不应作为未知顶层字段透传: %+v", first)
	}
	if _, bad := first["previous_response_id"]; bad {
		t.Errorf("previous_response_id 不应出现在 Extra: %+v", first)
	}
}

// TestE2E_Anthropic_KnownFieldsNotPassedAsExtra 验证 Anthropic API 会话字段不会作为未知字段污染上游。
func TestE2E_Anthropic_KnownFieldsNotPassedAsExtra(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],
	          "conversation_id":"conv-ant-1","previous_response_id":"prev-ant-1"}`
	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, b)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	first := up.startBodies[0]
	if _, bad := first["conversation_id"]; bad {
		t.Errorf("conversation_id 不应作为未知字段透传: %+v", first)
	}
	if _, bad := first["previous_response_id"]; bad {
		t.Errorf("previous_response_id 不应作为未知字段透传: %+v", first)
	}
}

// TestE2E_StreamChat_ConversationIDInFinalChunk 验证流式完成时结束帧携带 prism_conversation_id。
func TestE2E_StreamChat_ConversationIDInFinalChunk(t *testing.T) {
	up := &fakeUpstream{t: t, conversationID: "conv-stream-abc"}
	ts, _ := newTestServer(t, up, goodAccount(), nil)

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := readBody(resp)
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, b)
	}

	scanner := bufio.NewScanner(resp.Body)
	foundConv := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
			chunkData := strings.TrimPrefix(line, "data: ")
			var chunk map[string]any
			if err := json.Unmarshal([]byte(chunkData), &chunk); err == nil {
				if cid, ok := chunk["prism_conversation_id"].(string); ok && cid == "conv-stream-abc" {
					foundConv = true
				}
			}
		}
	}
	if !foundConv {
		t.Error("流式 chunk 中未找到 prism_conversation_id: conv-stream-abc")
	}
}

// ---------------------------- 辅助 ----------------------------

func readBody(resp *http.Response) (string, error) {
	if resp == nil || resp.Body == nil {
		return "", nil
	}
	var buf bytes.Buffer
	_, err := buf.ReadFrom(resp.Body)
	return buf.String(), err
}

// ModelList 是 /v1/models 的最小结构（测试用，避免跨包依赖）。
type ModelList struct {
	Object string `json:"object"`
	Data   []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data"`
}

func TestDashboard_Serving(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	resp, err := http.Get(ts.URL + "/dashboard/")
	if err != nil {
		t.Fatalf("GET /dashboard/: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码 = %d，想要 200", resp.StatusCode)
	}
	body, _ := readBody(resp)
	if !strings.Contains(body, "<!doctype html>") && !strings.Contains(body, "<html") {
		t.Errorf("返回内容未包含 HTML: %s", body)
	}
}

func TestAdmin_Accounts_CRUD_SQLite(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	// 1. POST /admin/accounts (创建账号持久化至 SQLite)
	newAcc := map[string]any{
		"id":              "acc_sqlite_crud",
		"name":            "SQLite动态测试账号",
		"plan":            "pro",
		"email":           "crud@example.com",
		"cookies":         "__Secure-next-auth.session-token=crud_test",
		"max_concurrency": 4,
	}
	bodyJSON, _ := json.Marshal(newAcc)
	resp, err := http.Post(ts.URL+"/admin/accounts", "application/json", bytes.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("POST /admin/accounts: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /admin/accounts 状态码 = %d, 想要 201", resp.StatusCode)
	}

	// 2. GET /admin/accounts (验证创建成功并实时进入连接池)
	respGet, err := http.Get(ts.URL + "/admin/accounts")
	if err != nil {
		t.Fatalf("GET /admin/accounts: %v", err)
	}
	defer respGet.Body.Close()
	getBody, _ := readBody(respGet)
	if !strings.Contains(getBody, "acc_sqlite_crud") {
		t.Fatalf("创建后未在 GET /admin/accounts 找到 acc_sqlite_crud: %s", getBody)
	}

	// 3. PUT /admin/accounts/acc_sqlite_crud (更新账号持久化至 SQLite)
	putAcc := map[string]any{
		"name":            "SQLite修改后名称",
		"max_concurrency": 8,
	}
	putJSON, _ := json.Marshal(putAcc)
	reqPut, _ := http.NewRequest(http.MethodPut, ts.URL+"/admin/accounts/acc_sqlite_crud", bytes.NewReader(putJSON))
	respPut, err := http.DefaultClient.Do(reqPut)
	if err != nil {
		t.Fatalf("PUT /admin/accounts/acc_sqlite_crud: %v", err)
	}
	defer respPut.Body.Close()
	if respPut.StatusCode != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d, 想要 200", respPut.StatusCode)
	}

	// 4. DELETE /admin/accounts/acc_sqlite_crud (物理删除账号并从池中剔除)
	reqDel, _ := http.NewRequest(http.MethodDelete, ts.URL+"/admin/accounts/acc_sqlite_crud", nil)
	respDel, err := http.DefaultClient.Do(reqDel)
	if err != nil {
		t.Fatalf("DELETE /admin/accounts/acc_sqlite_crud: %v", err)
	}
	defer respDel.Body.Close()
	if respDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE 状态码 = %d, 想要 200", respDel.StatusCode)
	}

	// 5. GET /admin/accounts (验证物理删除成功)
	respGet2, errGet2 := http.Get(ts.URL + "/admin/accounts")
	if errGet2 != nil {
		t.Fatal(errGet2)
	}
	defer respGet2.Body.Close()
	getBody2, _ := readBody(respGet2)
	if strings.Contains(getBody2, "acc_sqlite_crud") {
		t.Fatalf("删除后仍然存在 acc_sqlite_crud: %s", getBody2)
	}
}

func TestAdmin_Requests_And_Chat_SQLite(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	// 1. 发起一次模型推理请求，触发中间件记录真实流水。
	//    明细只记录推理入口（chat/completions 等）；/v1/models 这类
	//    探测流量不产生推理，不入明细。
	respModel, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions 失败: %v", err)
	}
	_ = respModel.Body.Close()

	// 等待 50ms 让异步 goroutine 写入 SQLite
	time.Sleep(50 * time.Millisecond)

	// 2. GET /admin/requests 验证请求流水入库
	respReq, err := http.Get(ts.URL + "/admin/requests?page=1&page_size=10")
	if err != nil {
		t.Fatalf("GET /admin/requests 失败: %v", err)
	}
	defer respReq.Body.Close()
	if respReq.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/requests 状态码 = %d, 想要 200", respReq.StatusCode)
	}
	var reqData struct {
		Total int `json:"total"`
		Items []struct {
			Path       string `json:"path"`
			StatusCode int    `json:"status_code"`
		} `json:"items"`
	}
	if err := json.NewDecoder(respReq.Body).Decode(&reqData); err != nil {
		t.Fatalf("解码 /admin/requests 响应失败: %v", err)
	}
	if reqData.Total == 0 || len(reqData.Items) == 0 {
		t.Fatalf("未能查到真实请求明细: total=%d", reqData.Total)
	}
	if reqData.Items[0].Path != "/v1/chat/completions" {
		t.Errorf("请求路径错误: %s, 期望 /v1/chat/completions", reqData.Items[0].Path)
	}

	// 3. GET /admin/statistics 验证聚合统计
	respStat, err := http.Get(ts.URL + "/admin/statistics")
	if err != nil {
		t.Fatalf("GET /admin/statistics 失败: %v", err)
	}
	defer respStat.Body.Close()
	var statData struct {
		TotalRequests int     `json:"total_requests"`
		SuccessRate   float64 `json:"success_rate"`
	}
	if err := json.NewDecoder(respStat.Body).Decode(&statData); err != nil {
		t.Fatalf("解码 /admin/statistics 失败: %v", err)
	}
	if statData.TotalRequests == 0 {
		t.Errorf("聚合请求数为 0")
	}

	// 4. Chat 会话持久化测试: POST /admin/chat/sessions
	sess := map[string]any{
		"id":               "test_sess_001",
		"title":            "调试会话测试",
		"model":            "gpt-6-astra",
		"reasoning_effort": "medium",
	}
	sessBytes, _ := json.Marshal(sess)
	respSess, err := http.Post(ts.URL+"/admin/chat/sessions", "application/json", bytes.NewReader(sessBytes))
	if err != nil {
		t.Fatalf("POST /admin/chat/sessions 失败: %v", err)
	}
	respSess.Body.Close()

	// 5. GET /admin/chat/sessions
	respList, err := http.Get(ts.URL + "/admin/chat/sessions")
	if err != nil {
		t.Fatalf("GET /admin/chat/sessions 失败: %v", err)
	}
	listBody, _ := readBody(respList)
	if !strings.Contains(listBody, "test_sess_001") {
		t.Fatalf("未能列出创建的会话: %s", listBody)
	}

	// 6. POST /admin/chat/sessions/test_sess_001/messages 存储消息
	msg := map[string]any{
		"id":      "msg_001",
		"role":    "user",
		"content": "测试消息持久化内容",
	}
	msgBytes, _ := json.Marshal(msg)
	respMsg, err := http.Post(ts.URL+"/admin/chat/sessions/test_sess_001/messages", "application/json", bytes.NewReader(msgBytes))
	if err != nil {
		t.Fatalf("POST /admin/chat/sessions/.../messages 失败: %v", err)
	}
	respMsg.Body.Close()

	// 7. GET /admin/chat/sessions/test_sess_001/messages
	respMsgList, err := http.Get(ts.URL + "/admin/chat/sessions/test_sess_001/messages")
	if err != nil {
		t.Fatalf("GET /admin/chat/sessions/.../messages 失败: %v", err)
	}
	msgListBody, _ := readBody(respMsgList)
	if !strings.Contains(msgListBody, "测试消息持久化内容") {
		t.Fatalf("未能查询到持久化消息: %s", msgListBody)
	}

	// 8. DELETE /admin/chat/sessions/test_sess_001
	reqDel, _ := http.NewRequest(http.MethodDelete, ts.URL+"/admin/chat/sessions/test_sess_001", nil)
	respDel, err := http.DefaultClient.Do(reqDel)
	if err != nil {
		t.Fatalf("DELETE /admin/chat/sessions 失败: %v", err)
	}
	respDel.Body.Close()
}

// 真实上游不回 usage：网关按实际收发内容用 o200k_base 精确计数，
// 同一组数字既返回给客户端，也写进请求流水。
func TestE2E_MeasuredUsageRecorded(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t, noUsage: true}, goodAccount(), nil)

	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"gpt-5","messages":[{"role":"user","content":"hello world"}]}`,
		map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("chat 失败: %d %s", code, out)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens            int `json:"prompt_tokens"`
			CompletionTokens        int `json:"completion_tokens"`
			TotalTokens             int `json:"total_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || len(resp.Choices) == 0 {
		t.Fatalf("解析响应失败: %v %s", err, out)
	}
	u := resp.Usage
	// 输出 = 正文 + 推理文本的精确 token 数（推理部分单列 reasoning_tokens）；
	// 输入至少含 user 消息本身（3 帧 + 角色 1 + 正文 2）+ 回复引导 3
	msg := resp.Choices[0].Message
	reasoning := tokens.Count(msg.ReasoningContent)
	if want := tokens.Count(msg.Content) + reasoning; u.CompletionTokens != want {
		t.Fatalf("completion_tokens = %d, 正文+推理精确计数为 %d", u.CompletionTokens, want)
	}
	if u.CompletionTokensDetails.ReasoningTokens != reasoning {
		t.Fatalf("reasoning_tokens = %d, want %d", u.CompletionTokensDetails.ReasoningTokens, reasoning)
	}
	if u.PromptTokens < 9 || u.TotalTokens != u.PromptTokens+u.CompletionTokens {
		t.Fatalf("usage 不自洽: %+v", u)
	}

	// 流水异步落库，轮询等待
	var logged struct {
		Items []struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"items"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, body := doLocal(t, http.MethodGet, ts.URL+"/admin/requests?page=1&page_size=1", "", nil)
		if code == http.StatusOK && json.Unmarshal([]byte(body), &logged) == nil && len(logged.Items) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("请求流水未落库: %d %s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := logged.Items[0]; got.PromptTokens != u.PromptTokens || got.CompletionTokens != u.CompletionTokens {
		t.Fatalf("流水用量 %+v 与响应 usage %+v 不一致", got, u)
	}
}

// ---------------------------- 沙箱回收 ----------------------------

// 沙箱空闲被上游回收后，代理对旧令牌一律回 502（后端签发资源令牌照常 200）。
// 网关须当场换新容器，而不是把同一个死沙箱一直用到缓存过期 ——
// 2026-10-04 实测：两个请求先后各等 8 秒后报"沙箱工作区同步未就绪"。
func TestE2E_ReclaimedSandboxIsReplaced(t *testing.T) {
	up := &fakeUpstream{t: t, sandbox: true, deadSandbox: map[string]bool{}}
	ts, _ := newTestServer(t, up, goodAccount(), nil)

	chat := func(session string) (int, string) {
		return doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
			`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": session})
	}
	if code, out := chat("s1"); code != http.StatusOK {
		t.Fatalf("首个请求失败: %d %s", code, out)
	}
	up.mu.Lock()
	if up.sandboxSeq != 1 {
		up.mu.Unlock()
		t.Fatalf("首个请求应申请 1 个沙箱，实际 %d 个", up.sandboxSeq)
	}
	up.deadSandbox["sbtok-1"] = true // 上游回收了它，网关缓存里却还在
	up.mu.Unlock()

	// 新会话 = 新项目，必须重新同步工作区，撞上死沙箱
	if code, out := chat("s2"); code != http.StatusOK {
		t.Fatalf("沙箱被回收后请求失败（应自动换新容器）: %d %s", code, out)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.sandboxSeq != 2 {
		t.Fatalf("应重新申请 1 个沙箱（共 2 个），实际 %d 个", up.sandboxSeq)
	}
	last, _ := json.Marshal(up.startBodies[len(up.startBodies)-1])
	if !strings.Contains(string(last), "sbtok-2") || strings.Contains(string(last), "sbtok-1") {
		t.Fatalf("start 应改用新沙箱: %s", last)
	}
}

// 沙箱服务整体故障：只换一次容器就放弃（不无限申请），不发 start（否则上游 122 秒后 504），
// 且请求流水记下实际使用的账号 —— 失败请求此前一律显示"未分配"。
func TestE2E_SandboxDownFailsFastWithAccountLogged(t *testing.T) {
	up := &fakeUpstream{t: t, sandbox: true, deadAll: true}
	ts, _ := newTestServer(t, up, goodAccount(), nil)

	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Content-Type": "application/json"})
	if code == http.StatusOK {
		t.Fatalf("沙箱服务整体故障时不应成功: %s", out)
	}
	up.mu.Lock()
	acquired, starts := up.sandboxSeq, len(up.startBodies)
	up.mu.Unlock()
	if acquired != 2 {
		t.Fatalf("应只换一次容器（共申请 2 个），实际 %d 个", acquired)
	}
	if starts != 0 {
		t.Fatalf("工作区未就绪不应发起 start，实际 %d 次", starts)
	}

	var logged struct {
		Items []struct {
			AccountID    string `json:"account_id"`
			ErrorMessage string `json:"error_message"`
		} `json:"items"`
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		code, body := doLocal(t, http.MethodGet, ts.URL+"/admin/requests?page=1&page_size=1", "", nil)
		if code == http.StatusOK && json.Unmarshal([]byte(body), &logged) == nil && len(logged.Items) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("请求流水未落库: %d %s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := logged.Items[0]; got.AccountID != "main" || got.ErrorMessage == "" {
		t.Fatalf("失败请求的流水应记下账号与原因，得到 %+v", got)
	}
}
