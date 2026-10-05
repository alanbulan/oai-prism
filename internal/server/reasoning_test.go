package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 上游的思考摘要透传到三种下游：Chat 的 reasoning_content、Responses 的 reasoning 条目
// （Codex 显示思考）、Anthropic 的 thinking 块（Claude Code 开了 thinking 时）。

var jsonHdr = map[string]string{"Content-Type": "application/json"}

// sseData 按顺序取出 SSE 流里每个 data 行的 JSON（跳过 [DONE]）。
func sseData(t *testing.T, out string) []map[string]any {
	t.Helper()
	var evs []map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatalf("data 不是合法 JSON: %v\n%s", err, data)
		}
		evs = append(evs, v)
	}
	return evs
}

func TestE2E_ChatReasoningDeltaOnce(t *testing.T) {
	// 生成中的帧就带着（到目前为止的）思考摘要：只发新增部分，不随每次正文增量重复全文
	ts, _ := newTestServer(t, &fakeUpstream{t: t, reasoningInPending: true}, goodAccount(), nil)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"你好"}]}`, jsonHdr)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, out)
	}
	var reason []string
	for _, ev := range sseData(t, out) {
		for _, c := range ev["choices"].([]any) {
			if r, _ := c.(map[string]any)["delta"].(map[string]any)["reasoning_content"].(string); r != "" {
				reason = append(reason, r)
			}
		}
	}
	if strings.Join(reason, "") != "先想一下" || len(reason) != 2 {
		t.Fatalf("思考应分两段各发一次（先想 / 一下），实际 %q", reason)
	}
}

// responsesItems 取出 Responses 流里 output_item.added / done 的 (index, type) 序列与 completed 的 output。
func responsesItems(t *testing.T, out string) (seq []string, completed []map[string]any, summary string) {
	t.Helper()
	for _, ev := range sseData(t, out) {
		switch ev["type"] {
		case "response.output_item.added", "response.output_item.done":
			item := ev["item"].(map[string]any)
			seq = append(seq, strings.TrimPrefix(ev["type"].(string), "response.output_item.")+
				":"+jsonNum(ev["output_index"])+":"+item["type"].(string))
		case "response.reasoning_summary_text.delta":
			summary += ev["delta"].(string)
		case "response.completed":
			for _, it := range ev["response"].(map[string]any)["output"].([]any) {
				completed = append(completed, it.(map[string]any))
			}
		}
	}
	return seq, completed, summary
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func itemTypes(items []map[string]any) string {
	var ts []string
	for _, it := range items {
		ts = append(ts, it["type"].(string))
	}
	return strings.Join(ts, ",")
}

func TestE2E_ResponsesStreamReasoningItem(t *testing.T) {
	body := `{"model":"gpt-5","stream":true,"input":"你好"}`

	// 思考与正文一起到（上游只在终态给结果）：reasoning 条目在前
	ts, _ := newTestServer(t, &fakeUpstream{t: t, finalOnlyPayload: true}, goodAccount(), nil)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", body, jsonHdr)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, out)
	}
	seq, completed, summary := responsesItems(t, out)
	if want := "added:0:reasoning,done:0:reasoning,added:1:message,done:1:message"; strings.Join(seq, ",") != want {
		t.Fatalf("条目顺序\n got %s\nwant %s", strings.Join(seq, ","), want)
	}
	if summary != "先想一下" || itemTypes(completed) != "reasoning,message" {
		t.Fatalf("summary=%q completed=%s", summary, itemTypes(completed))
	}
	if s := completed[0]["summary"].([]any)[0].(map[string]any); s["type"] != "summary_text" || s["text"] != "先想一下" {
		t.Fatalf("completed 里的 reasoning 条目不对: %v", completed[0])
	}

	// 正文先流式到、思考最后才到：正文条目关闭后补一个 reasoning 条目，index 依次递增
	ts, _ = newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	_, out = doLocal(t, http.MethodPost, ts.URL+"/v1/responses", body, jsonHdr)
	seq, completed, summary = responsesItems(t, out)
	if want := "added:0:message,done:0:message,added:1:reasoning,done:1:reasoning"; strings.Join(seq, ",") != want {
		t.Fatalf("条目顺序\n got %s\nwant %s", strings.Join(seq, ","), want)
	}
	if summary != "先想一下" || itemTypes(completed) != "message,reasoning" {
		t.Fatalf("summary=%q completed=%s", summary, itemTypes(completed))
	}
}

func TestE2E_ResponsesBridgeReasoningFirst(t *testing.T) {
	// Codex（工具桥）：思考条目排在工具调用前面；同步响应同样
	reply := "```codex-exec\ntext(await tools.exec_command({cmd: \"Get-ChildItem\"}));\n```"
	hdr := map[string]string{"Content-Type": "application/json", "User-Agent": "codex_cli_rs/0.158.0 (Windows 10.0.26300; x86_64)"}

	ts, _ := newTestServer(t, &fakeUpstream{t: t, replyParts: []string{reply}}, goodAccount(), nil)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", fnBody(t, true), hdr)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, out)
	}
	seq, completed, summary := responsesItems(t, out)
	if want := "added:0:reasoning,done:0:reasoning,added:1:function_call,done:1:function_call"; strings.Join(seq, ",") != want {
		t.Fatalf("条目顺序\n got %s\nwant %s", strings.Join(seq, ","), want)
	}
	if summary != "先想一下" || itemTypes(completed) != "reasoning,function_call" {
		t.Fatalf("summary=%q completed=%s", summary, itemTypes(completed))
	}
	if cmds := fnCmds(t, out); len(cmds) != 1 || cmds[0] != "Get-ChildItem" {
		t.Fatalf("工具调用不对: %q", cmds)
	}

	_, out = doLocal(t, http.MethodPost, ts.URL+"/v1/responses", fnBody(t, false), hdr)
	var sync struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal([]byte(out), &sync); err != nil {
		t.Fatal(err)
	}
	if itemTypes(sync.Output) != "reasoning,function_call" {
		t.Fatalf("同步 output = %s", itemTypes(sync.Output))
	}
}

// anthropicBlocks 取出 Anthropic 流里内容块的 (index, 类型) 序列，以及思考与正文的拼接结果。
func anthropicBlockSeq(t *testing.T, out string) (seq []string, thinking, text string, signed bool) {
	t.Helper()
	for _, ev := range sseData(t, out) {
		switch ev["type"] {
		case "content_block_start":
			seq = append(seq, jsonNum(ev["index"])+":"+ev["content_block"].(map[string]any)["type"].(string))
		case "content_block_delta":
			d := ev["delta"].(map[string]any)
			switch d["type"] {
			case "thinking_delta":
				thinking += d["thinking"].(string)
			case "text_delta":
				text += d["text"].(string)
			case "signature_delta":
				signed = d["signature"] != ""
			}
		}
	}
	return seq, thinking, text, signed
}

func TestE2E_AnthropicThinkingBlocks(t *testing.T) {
	// Claude Code 的真实请求形状：thinking 带 display，另有 context_management（2026-10-05 抓包）
	const on = `{"model":"claude-x","max_tokens":256,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024,"display":"updates"},` +
		`"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},"messages":[{"role":"user","content":"你好"}]}`
	const off = `{"model":"claude-x","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"你好"}]}`
	const answer = "你好，这是一段流式回答。（完）"

	// 开了 thinking，思考与正文一起到：思考块在前（index 0，带签名），正文块 index 1
	up := &fakeUpstream{t: t, finalOnlyPayload: true}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/messages", on, jsonHdr)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, out)
	}
	seq, thinking, text, signed := anthropicBlockSeq(t, out)
	if strings.Join(seq, ",") != "0:thinking,1:text" || thinking != "先想一下" || text != answer || !signed {
		t.Fatalf("seq=%v thinking=%q text=%q signed=%v", seq, thinking, text, signed)
	}
	up.mu.Lock()
	sent, _ := json.Marshal(up.startBodies)
	up.mu.Unlock()
	if strings.Contains(string(sent), "budget_tokens") || strings.Contains(string(sent), "clear_thinking") {
		t.Fatalf("thinking / context_management 是 Anthropic 协议字段，透传给上游会被整轮拒绝: %s", sent)
	}
	// claude-* 模型名上游不认识：换成默认模型，回给客户端的仍是原名
	if strings.Contains(string(sent), "claude-x") || !strings.Contains(out, `"model":"claude-x"`) {
		t.Fatalf("模型名映射不对：上游收到 %s", sent)
	}

	// 正文先流式到、思考最后才到：思考块排在正文块之后
	ts, _ = newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	_, out = doLocal(t, http.MethodPost, ts.URL+"/v1/messages", on, jsonHdr)
	if seq, thinking, text, _ = anthropicBlockSeq(t, out); strings.Join(seq, ",") != "0:text,1:thinking" || thinking != "先想一下" || text != answer {
		t.Fatalf("seq=%v thinking=%q text=%q", seq, thinking, text)
	}

	// 没开 thinking，或开了但 display 为 omitted（Claude Code -p 文本输出）：只有正文块
	for _, body := range []string{off, strings.Replace(on, `"updates"`, `"omitted"`, 1)} {
		_, out = doLocal(t, http.MethodPost, ts.URL+"/v1/messages", body, jsonHdr)
		if seq, thinking, text, _ = anthropicBlockSeq(t, out); strings.Join(seq, ",") != "0:text" || thinking != "" || text != answer {
			t.Fatalf("seq=%v thinking=%q text=%q", seq, thinking, text)
		}
	}

	// 同步：开了 thinking 时 content[0] 是思考块
	for _, c := range []struct {
		body, want string
	}{{strings.Replace(on, `"stream":true,`, "", 1), "thinking,text"}, {strings.Replace(off, `"stream":true,`, "", 1), "text"}} {
		_, out = doLocal(t, http.MethodPost, ts.URL+"/v1/messages", c.body, jsonHdr)
		var resp struct {
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got := itemTypes(resp.Content); got != c.want {
			t.Fatalf("同步 content = %s, want %s", got, c.want)
		}
		if c.want == "thinking,text" && (resp.Content[0]["thinking"] != "先想一下" || resp.Content[0]["signature"] == "") {
			t.Fatalf("思考块不对: %v", resp.Content[0])
		}
	}
}
