package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// 上游模型以智能体方式干活时，正文要到终态才有；进行中的转录（codex_live_progress）
// 带着思考摘要与工具调用。这些要实时转给客户端，并且算作"上游有进展"。

func TestE2E_LiveProgressStreamsReasoning(t *testing.T) {
	up := &fakeUpstream{
		t:                t,
		finalOnlyPayload: true,
		liveProgress: func(n int) map[string]any {
			switch n {
			case 1:
				return map[string]any{"transcriptCursor": 5, "lineCount": 5,
					"reasoningSummaries": []any{map[string]any{"line_index": 3, "text": "**规划** 先画飞机"}},
					"toolCalls":          []any{map[string]any{"line_index": 4, "call_id": "tool:4:0", "name": "apply_patch"}},
				}
			case 3:
				return map[string]any{"transcriptCursor": 9, "lineCount": 4,
					"reasoningSummaries": []any{map[string]any{"line_index": 7, "text": "**检查** 数一数发动机"}},
				}
			}
			return map[string]any{"transcriptCursor": 5, "lineCount": 0}
		},
		// 终态 reasoning 前两条与转录重复（空白写法不同），第三条是新的
		finalReasoning: []string{"**规划**\n先画飞机", "**检查** 数一数发动机", "**收尾** 写回答"},
	}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"画一架飞机"}]}`, jsonHdr)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, out)
	}
	var reason []string
	var content string
	reasoningAfterContent := false
	for _, ev := range sseData(t, out) {
		for _, c := range ev["choices"].([]any) {
			d, _ := c.(map[string]any)["delta"].(map[string]any)
			if r, _ := d["reasoning_content"].(string); r != "" {
				reason = append(reason, r)
				reasoningAfterContent = reasoningAfterContent || content != ""
			}
			if s, _ := d["content"].(string); s != "" {
				content += s
			}
		}
	}
	want := "**规划** 先画飞机\n\n→ 调用工具 `apply_patch`\n\n**检查** 数一数发动机\n\n**收尾** 写回答"
	if got := strings.Join(reason, ""); got != want {
		t.Fatalf("思考过程不对:\n got %q\nwant %q", got, want)
	}
	if len(reason) != 3 || reasoningAfterContent {
		t.Fatalf("转录应在生成中逐段送达（3 段，都在正文之前），实际 %d 段 %q", len(reason), reason)
	}
	if content != "你好，这是一段流式回答。（完）" {
		t.Fatalf("正文 %q", content)
	}
}

// slowAgent 模拟一轮跑得比 max_poll_timeout 还久、正文只在终态给出的上游。
func slowAgent(t *testing.T, progress bool) *fakeUpstream {
	up := &fakeUpstream{
		t:                t,
		finalOnlyPayload: true,
		pollDelay:        40 * time.Millisecond,
		replyParts:       strings.Split("一二三四五六七八九十甲乙", ""),
	}
	if progress {
		up.liveProgress = func(n int) map[string]any {
			return map[string]any{"transcriptCursor": n, "lineCount": 1}
		}
	}
	return up
}

func fastPolls(idle, hard time.Duration) func(*config.Config) {
	return func(c *config.Config) {
		c.Facade.PollInterval = 10 * time.Millisecond
		c.Facade.PollBackoffMax = 20 * time.Millisecond
		c.Facade.MaxPollTimeout = idle
		c.Facade.MaxRunTimeout = hard
	}
}

const slowBody = `{"model":"gpt-5","messages":[{"role":"user","content":"慢慢来"}]}`

func TestE2E_PollTimeoutExtendsWhileProgressing(t *testing.T) {
	// 12 次轮询 × 40ms ≈ 0.5s，比"无进展时限"200ms 长，但转录游标一直在前进
	ts, _ := newTestServer(t, slowAgent(t, true), goodAccount(), fastPolls(200*time.Millisecond, 10*time.Second))
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions", slowBody, jsonHdr)
	if code != http.StatusOK || !strings.Contains(out, "一二三四五六七八九十甲乙") {
		t.Fatalf("有进展时不应超时: %d %s", code, out)
	}
}

func TestE2E_PollTimeoutWithoutProgress(t *testing.T) {
	ts, up := newTestServer(t, slowAgent(t, false), goodAccount(), fastPolls(200*time.Millisecond, 10*time.Second))
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions", slowBody, jsonHdr)
	if code != http.StatusGatewayTimeout || !strings.Contains(out, "没有任何进展") {
		t.Fatalf("毫无进展应按 max_poll_timeout 超时: %d %s", code, out)
	}
	up.mu.Lock()
	stops := len(up.stopBodies)
	up.mu.Unlock()
	if stops == 0 {
		t.Fatal("超时放弃后应通知上游停止")
	}
}

func TestE2E_PollTimeoutRunCap(t *testing.T) {
	ts, _ := newTestServer(t, slowAgent(t, true), goodAccount(), fastPolls(200*time.Millisecond, 250*time.Millisecond))
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions", slowBody, jsonHdr)
	if code != http.StatusGatewayTimeout || !strings.Contains(out, "超过单轮生成上限") {
		t.Fatalf("一直有进展也不能超过 max_run_timeout: %d %s", code, out)
	}
}

// 成品写在上游沙箱文件里、回答只有一句话时：没声明工具的客户端（调试台）拿到附在回答后面的文件，
// 声明了工具的客户端照旧收到工具调用。
func sandboxFiles() []any {
	hunk := func(s string) map[string]any {
		return map[string]any{"version": 1, "hunks": []any{map[string]any{"original": "", "updated": s}}}
	}
	return []any{
		map[string]any{"file_path": "AGENTS.md", "status": "added", "diff": hunk("Prism 的说明")},
		map[string]any{"file_path": "plane.svg", "status": "added", "diff": hunk("<svg viewBox=\"0 0 10 10\"><rect width=\"10\" height=\"10\"/></svg>\n")},
	}
}

func TestE2E_ChatAttachesSandboxFiles(t *testing.T) {
	up := &fakeUpstream{t: t, conversationID: "cdx1_files", replyParts: []string{"已制作完成：`plane.svg`"}, deltaFiles: sandboxFiles()}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	chat := func(stream bool) (string, []map[string]any) {
		body := `{"model":"gpt-5","stream":` + map[bool]string{true: "true", false: "false"}[stream] +
			`,"messages":[{"role":"user","content":"画一架飞机，存成 svg"}]}`
		code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions", body, jsonHdr)
		if code != http.StatusOK {
			t.Fatalf("状态码 %d: %s", code, out)
		}
		if !stream {
			return out, nil
		}
		return out, sseData(t, out)
	}

	_, evs := chat(true)
	var content, finish string
	for _, ev := range evs {
		for _, c := range ev["choices"].([]any) {
			cm := c.(map[string]any)
			if d, _ := cm["delta"].(map[string]any); d != nil {
				if s, _ := d["content"].(string); s != "" {
					content += s
				}
				if d["tool_calls"] != nil {
					t.Fatalf("客户端没声明工具，不该收到 tool_calls: %v", d["tool_calls"])
				}
			}
			if f, _ := cm["finish_reason"].(string); f != "" {
				finish = f
			}
		}
	}
	if !strings.HasPrefix(content, "已制作完成：`plane.svg`") ||
		!strings.Contains(content, "`plane.svg`（") || !strings.Contains(content, "```svg\n<svg viewBox") {
		t.Fatalf("文件没附在回答后面:\n%s", content)
	}
	if strings.Contains(content, "AGENTS.md") || finish != "stop" {
		t.Fatalf("AGENTS.md 不该附上、finish_reason 应为 stop（%q）:\n%s", finish, content)
	}

	// 同一上游会话里同样的文件再报一次：不重复附
	out, _ := chat(false)
	if strings.Contains(out, "plane.svg`（") {
		t.Fatalf("同一会话里没变的文件不该重复附上:\n%s", out)
	}
}

func TestE2E_ChatWithToolsGetsToolCalls(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{"好了"}, deltaFiles: sandboxFiles()}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"gpt-5","messages":[{"role":"user","content":"画"}],
		  "tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object"}}}]}`, jsonHdr)
	if code != http.StatusOK || !strings.Contains(out, `"name":"write_file"`) || strings.Contains(out, "生成的文件") {
		t.Fatalf("声明了工具时应照旧转成工具调用、不附文件: %d %s", code, out)
	}
}

// 看不到上游沙箱的客户端：提示上游把成品直接写在回答里、别往沙箱写文件；
// 声明了工具的 Chat 客户端（沙箱文件会转成它的工具调用）不加这条。
func TestE2E_InlineDeliveryNotice(t *testing.T) {
	const rule = "This client sees ONLY your reply text"
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)

	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"画一个 svg"}]}`)
	if sys, _ := requireSystemUser(t, upstreamInput(t, up, 0)); !strings.Contains(sys, rule) || !strings.Contains(sys, "```svg") {
		t.Errorf("没声明工具的 Chat 请求应要求把成品写在回答里:\n%.600s", sys)
	}

	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"写个文件"}],
		"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object"}}}]}`)
	if sys, _ := requireSystemUser(t, upstreamInput(t, up, 1)); !strings.Contains(sys, "<gateway_notice>") || strings.Contains(sys, rule) {
		t.Errorf("声明了工具的 Chat 请求只加通用声明:\n%.600s", sys)
	}

	postLocal(t, ts.URL+"/v1/messages", `{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"画一个 svg"}]}`)
	if sys, _ := requireSystemUser(t, upstreamInput(t, up, 2)); !strings.Contains(sys, rule) {
		t.Errorf("Anthropic 接口拿不到沙箱文件，也应要求写在回答里:\n%.600s", sys)
	}
}
