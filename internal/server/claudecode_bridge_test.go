package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

// Claude Code 工具桥的端到端回归（见 internal/facade/anthropic_bridge.go）：上游把调用写成
// ```local-tool 块，网关转成 tool_use 交给客户端执行；结果（tool_result）回来时翻译成
// [CLIENT RESULT] 接着同一个上游会话。

func ccTools() []any {
	tool := func(name, desc string, required []string, props map[string]any) map[string]any {
		return map[string]any{"name": name, "description": desc,
			"input_schema": map[string]any{"type": "object", "properties": props, "required": required}}
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	return []any{
		tool("Bash", "Executes a given bash command.", []string{"command"}, map[string]any{"command": str("The command to execute")}),
		tool("Read", "Reads a file from the local filesystem.", []string{"file_path"}, map[string]any{"file_path": str("The absolute path to the file to read")}),
		tool("Write", "Writes a file to the local filesystem.", []string{"file_path", "content"}, map[string]any{"file_path": str("The absolute path"), "content": str("The content to write")}),
		tool("Edit", "Performs exact string replacements in files.", []string{"file_path", "old_string", "new_string"},
			map[string]any{"file_path": str("The absolute path"), "old_string": str("The text to replace"), "new_string": str("The replacement")}),
	}
}

const ccSystem = "You are Claude Code, Anthropic's official CLI for Claude.\n\n# Environment\n - Primary working directory: C:\\work\\demo\n - Platform: win32"

var ccHdr = map[string]string{
	"Content-Type":             "application/json",
	"User-Agent":               "claude-cli/2.1.289 (external, cli)",
	"X-Claude-Code-Session-Id": "6f1c2a52-cc-bridge-test",
}

func ccBody(t *testing.T, stream bool, msgs ...any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model": "claude-sonnet-4-5", "max_tokens": 32000, "stream": stream,
		"system": []any{map[string]any{"type": "text", "text": ccSystem}}, "tools": ccTools(), "messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func ccUser(blocks ...any) map[string]any { return map[string]any{"role": "user", "content": blocks} }
func ccText(s string) map[string]any      { return map[string]any{"type": "text", "text": s} }
func ccResult(id, out string, isErr bool) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": out, "is_error": isErr}
}

// ccBlock 是 Anthropic 流里拼出来的一个内容块。
type ccBlock struct {
	Type, ID, Name, Text string
}

// ccStream 拼出 Anthropic 流里的内容块与 stop_reason。
func ccStream(t *testing.T, out string) ([]ccBlock, string) {
	t.Helper()
	var blocks []ccBlock
	stop := ""
	for _, line := range strings.Split(out, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type, ID, Name string
			} `json:"content_block"`
			Delta struct {
				Type, Text, Thinking string
				PartialJSON          string `json:"partial_json"`
				StopReason           string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("事件不是 JSON: %q", data)
		}
		switch ev.Type {
		case "content_block_start":
			if ev.Index != len(blocks) {
				t.Fatalf("块序号应连续: %d（已有 %d 个）", ev.Index, len(blocks))
			}
			blocks = append(blocks, ccBlock{Type: ev.ContentBlock.Type, ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name})
		case "content_block_delta":
			blocks[ev.Index].Text += ev.Delta.Text + ev.Delta.Thinking + ev.Delta.PartialJSON
		case "message_delta":
			stop = ev.Delta.StopReason
		}
	}
	return blocks, stop
}

func ccPost(t *testing.T, url, body string, hdr map[string]string) string {
	t.Helper()
	code, out := doLocal(t, http.MethodPost, url+"/v1/messages", body, hdr)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %.400s", code, out)
	}
	return out
}

func setReply(up *fakeUpstream, reply string) {
	up.mu.Lock()
	up.replyParts = []string{reply}
	up.mu.Unlock()
}

// 一个完整的工具回合：上游的调用块 → tool_use；客户端的结果 → 同一上游会话里的 [CLIENT RESULT]。
func TestE2E_ClaudeCodeBridge_ToolLoop(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)

	setReply(up, "我先把文件写好。\n\n```local-tool\n"+
		`{"name": "Write", "input": {"file_path": "C:\\work\\demo\\a.txt", "content": "hello\n<b class=\"x\">world</b>"}}`+"\n```\n")
	first := ccUser(ccText("<system-reminder>\nCLAUDE.md 内容\n</system-reminder>"), ccText("在当前目录写一个 a.txt"))
	blocks, stop := ccStream(t, ccPost(t, ts.URL, ccBody(t, true, first), ccHdr))
	if stop != "tool_use" || len(blocks) != 2 || blocks[0].Type != "text" || blocks[0].Text != "我先把文件写好。" || blocks[1].Type != "tool_use" || blocks[1].Name != "Write" {
		t.Fatalf("应回正文 + 一个 Write 调用（stop_reason=tool_use）: stop=%q %+v", stop, blocks)
	}
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(blocks[1].Text), &in); err != nil || in.FilePath != `C:\work\demo\a.txt` || in.Content != "hello\n<b class=\"x\">world</b>" {
		t.Fatalf("调用参数应原样转给客户端: %v %q", err, blocks[1].Text)
	}
	callID := blocks[1].ID
	if !strings.HasPrefix(callID, "toolu_") {
		t.Fatalf("tool_use ID 形态不对: %q", callID)
	}

	sys, user := requireSystemUser(t, upstreamInput(t, up, 0))
	for _, want := range []string{"<client_tool_bridge>", "### Write", "- file_path (string, required)", `C:\work\demo`} {
		if !strings.Contains(sys, want) {
			t.Fatalf("首轮 system 应含 %q: %.600s", want, sys)
		}
	}
	if !strings.Contains(user, "写一个 a.txt") || !strings.Contains(user, "[CLIENT_TOOL_REMINDER]") || !strings.Contains(user, "CLAUDE.md 内容") {
		t.Fatalf("本轮消息应带上提醒与客户端的 system-reminder: %q", user)
	}
	cid := startConv(t, up, 0)
	if cid == "" {
		t.Fatal("首轮应登记上游会话")
	}

	// 客户端执行完回传结果（Claude Code 原样回放上一轮的正文与 tool_use）
	setReply(up, "写好了。")
	assistant := map[string]any{"role": "assistant", "content": []any{
		ccText("我先把文件写好。"),
		map[string]any{"type": "tool_use", "id": callID, "name": "Write", "input": json.RawMessage(blocks[1].Text)},
	}}
	turn2 := []any{first, map[string]any{"role": "system", "content": "# Environment update\n - shell: bash"}, assistant,
		ccUser(ccResult(callID, "File created successfully at: C:\\work\\demo\\a.txt", false)),
		map[string]any{"role": "system", "content": []any{ccText("<total_tokens>14999985 tokens left</total_tokens>")}}}
	blocks, stop = ccStream(t, ccPost(t, ts.URL, ccBody(t, true, turn2...), ccHdr))
	if stop != "end_turn" || len(blocks) != 1 || blocks[0].Text != "写好了。" {
		t.Fatalf("纯文本回复应照常结束: stop=%q %+v", stop, blocks)
	}
	if got := startConv(t, up, 1); got != cid {
		t.Fatalf("工具结果应接着同一个上游会话: %q vs %q", got, cid)
	}
	sys, user = requireSystemUser(t, upstreamInput(t, up, 1))
	if !strings.Contains(user, "[CLIENT RESULT tool=Write id="+callID+"]") || !strings.Contains(user, "File created successfully") {
		t.Fatalf("增量应是翻译后的工具结果: %q", user)
	}
	if strings.Contains(user, "写一个 a.txt") || strings.Contains(user, "total_tokens") {
		t.Fatalf("增量不应重发往轮内容或计数提示: %q", user)
	}
	// 对话中间新出现的 system 消息让 system 变了：重发一次完整的
	if !strings.Contains(sys, "<client_tool_bridge>") || !strings.Contains(sys, "# Environment update") || strings.Contains(sys, "total_tokens") {
		t.Fatalf("system 应重发并含对话中间的 system 消息（不含计数提示）: %.300s", sys)
	}

	// 同一会话里的旁路请求（fork：主对话前缀 + 一条额外消息）追加进了上游会话；
	// 主对话下一轮仍接着同一个会话，并提醒上游忽略那几条。
	setReply(up, "跑一下测试")
	base := append(append([]any{}, turn2[:4]...), map[string]any{"role": "assistant", "content": []any{ccText("写好了。")}})
	ccPost(t, ts.URL, ccBody(t, true, append(append([]any{}, base...), ccUser(ccText("[SUGGESTION MODE: Suggest what the user might naturally type next.]")))...), ccHdr)
	setReply(up, "a.txt 里是 hello world")
	ccPost(t, ts.URL, ccBody(t, true, append(append([]any{}, base...), ccUser(ccText("读一下 a.txt")))...), ccHdr)
	if startConv(t, up, 2) != cid || startConv(t, up, 3) != cid {
		t.Fatalf("旁路请求与主对话都应在同一个上游会话: %q %q vs %q", startConv(t, up, 2), startConv(t, up, 3), cid)
	}
	if _, user = requireSystemUser(t, upstreamInput(t, up, 3)); !strings.Contains(user, "Disregard them") || !strings.Contains(user, "读一下 a.txt") {
		t.Fatalf("主对话下一轮应提醒上游忽略旁路的那条: %q", user)
	}
}

// 没有会话 ID 的客户端（弱键）：助手条目要连原文一起比，靠 tool_use ID 里的原文指纹对上。
func TestE2E_ClaudeCodeBridge_WeakKeyContinues(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	setReply(up, "```local-tool\n{\"name\": \"Bash\", \"input\": {\"command\": \"ls\"}}\n```")
	first := ccUser(ccText("列出文件"))
	blocks, _ := ccStream(t, ccPost(t, ts.URL, ccBody(t, true, first), jsonHdr))
	if len(blocks) != 1 || blocks[0].Name != "Bash" {
		t.Fatalf("应只有一个 Bash 调用（没有正文块）: %+v", blocks)
	}
	setReply(up, "有 a.txt")
	assistant := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": blocks[0].ID, "name": "Bash", "input": map[string]any{"command": "ls"}}}}
	ccPost(t, ts.URL, ccBody(t, true, first, assistant, ccUser(ccResult(blocks[0].ID, "a.txt", false))), jsonHdr)
	if a, b := startConv(t, up, 0), startConv(t, up, 1); a == "" || a != b {
		t.Fatalf("弱键下工具回合也应续接同一个上游会话: %q vs %q", a, b)
	}
}

// 非流式：content 里是 tool_use 块，input 是对象。
func TestE2E_ClaudeCodeBridge_Sync(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	setReply(up, "```local-tool\n{\"name\": \"read\", \"arguments\": \"{\\\"file_path\\\": \\\"/tmp/a.txt\\\"}\"}\n```")
	var resp struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type, ID, Name string
			Input          map[string]any
		}
	}
	if err := json.Unmarshal([]byte(ccPost(t, ts.URL, ccBody(t, false, ccUser(ccText("读 a.txt"))), ccHdr)), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "tool_use" || len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" ||
		resp.Content[0].Name != "Read" || resp.Content[0].Input["file_path"] != "/tmp/a.txt" {
		t.Fatalf("应回一个 Read 调用（工具名按声明的大小写、字符串参数解开）: %+v", resp)
	}
}

// 解析不了的调用块：转成一个必然失败的调用，客户端回错后网关把纠错说明交给上游。
func TestE2E_ClaudeCodeBridge_InvalidBlockFeedback(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	setReply(up, "```local-tool\n{\"name\": \"Write\", \"input\": {\"file_path\": \"/a\", \"content\": }}\n```")
	first := ccUser(ccText("写文件"))
	blocks, stop := ccStream(t, ccPost(t, ts.URL, ccBody(t, true, first), ccHdr))
	if stop != "tool_use" || len(blocks) != 1 || blocks[0].Name != "oaiprism_invalid_tool_call" || !strings.Contains(blocks[0].Text, "Write") {
		t.Fatalf("坏块应转成纠错调用: %+v", blocks)
	}
	setReply(up, "好的")
	assistant := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": blocks[0].ID, "name": blocks[0].Name, "input": json.RawMessage(blocks[0].Text)}}}
	ccPost(t, ts.URL, ccBody(t, true, first, assistant,
		ccUser(ccResult(blocks[0].ID, "<tool_use_error>Error: No such tool available: oaiprism_invalid_tool_call</tool_use_error>", true))), ccHdr)
	if _, user := requireSystemUser(t, upstreamInput(t, up, 1)); !strings.Contains(user, "was NOT executed") || !strings.Contains(user, `"Write"`) {
		t.Fatalf("上游应收到可行动的纠错说明: %q", user)
	}
}

// 上游没按桥的格式调用、却在沙箱里写了文件：转成客户端的 Write，路径接到客户端工作目录下。
func TestE2E_ClaudeCodeBridge_SandboxFilesBecomeWrites(t *testing.T) {
	up := &fakeUpstream{t: t, replyParts: []string{"已制作完成：`plane.svg`"}, deltaFiles: sandboxFiles()}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	blocks, stop := ccStream(t, ccPost(t, ts.URL, ccBody(t, true, ccUser(ccText("画一架飞机，存成 svg"))), ccHdr))
	if stop != "tool_use" || len(blocks) != 2 || blocks[1].Name != "Write" || !strings.Contains(blocks[0].Text, "已转成本地文件操作") {
		t.Fatalf("沙箱里的文件应转成 Write 调用（AGENTS.md 不下发）: stop=%q %+v", stop, blocks)
	}
	var in struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal([]byte(blocks[1].Text), &in); err != nil || in.FilePath != `C:\work\demo\plane.svg` || !strings.HasPrefix(in.Content, "<svg") {
		t.Fatalf("应写到客户端工作目录下: %v %q", err, blocks[1].Text)
	}
}

// Claude Code 的单发旁路请求（会话标题等）：按伴生请求无状态地跑，不登记上游会话。
func TestE2E_ClaudeCodeAuxIsStateless(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	ccPost(t, ts.URL, `{"model":"claude-haiku-4-5","max_tokens":512,"messages":[{"role":"user","content":"Generate a title"}]}`, ccHdr)
	if cid := startConv(t, up, 0); cid != "" {
		t.Fatalf("旁路请求不应登记上游会话: %q", cid)
	}
}

// Anthropic 格式的图片（base64 source）：粘贴的图片与工具结果里的图片（Read 一张图）都要到上游。
// 假上游没有上传接口，登记失败后网关改为内联发送 —— 正好看得到图片确实转过去了。
func TestE2E_AnthropicImagesReachUpstream(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	img := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": data}}

	images := func(n int, up *fakeUpstream) int {
		up.mu.Lock()
		defer up.mu.Unlock()
		raw, _ := json.Marshal(up.startBodies[n]["input"])
		return strings.Count(string(raw), "data:image/png;base64,"+data)
	}

	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	ccPost(t, ts.URL, `{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[{"role":"user","content":[`+
		mustJSON(t, img)+`,{"type":"text","text":"图里是什么？"}]}]}`, jsonHdr)
	if images(0, up) != 1 {
		t.Fatal("粘贴的图片应转给上游")
	}

	setReply(up, "```local-tool\n{\"name\": \"Read\", \"input\": {\"file_path\": \"/p/a.png\"}}\n```")
	first := ccUser(ccText("看看 a.png"))
	blocks, _ := ccStream(t, ccPost(t, ts.URL, ccBody(t, true, first), ccHdr))
	assistant := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": blocks[0].ID, "name": "Read", "input": map[string]any{"file_path": "/p/a.png"}}}}
	result := map[string]any{"type": "tool_result", "tool_use_id": blocks[0].ID, "content": []any{img}}
	ccPost(t, ts.URL, ccBody(t, true, first, assistant, ccUser(result)), ccHdr)
	if images(2, up) != 1 {
		t.Fatal("Read 读到的图片应随工具结果转给上游")
	}
}

// 读大文件：system（Claude Code 自带的提示词很长）与整份文件放不下一条时，system 先单独发一轮，
// 文件内容完整到达上游，不再截掉中间、也不因超限整轮失败。
func TestE2E_ClaudeCodeBridge_BigReadSystemSeparately(t *testing.T) {
	up := &fakeUpstream{t: t}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	bigSystem := ccSystem + "\n\n" + strings.Repeat("Follow the coding guidelines carefully. ", 1600) // ≈ 64 KB
	body := func(msgs ...any) string {
		return mustJSON(t, map[string]any{
			"model": "claude-sonnet-4-5", "max_tokens": 32000, "stream": true,
			"system": []any{map[string]any{"type": "text", "text": bigSystem}}, "tools": ccTools(), "messages": msgs,
		})
	}

	setReply(up, "```local-tool\n{\"name\": \"Read\", \"input\": {\"file_path\": \"C:\\\\work\\\\demo\\\\big.py\"}}\n```")
	first := ccUser(ccText("读一下 big.py，告诉我 MAGIC 的值"))
	blocks, _ := ccStream(t, ccPost(t, ts.URL, body(first), ccHdr))
	if len(blocks) != 1 || blocks[0].Name != "Read" {
		t.Fatalf("应回一个 Read 调用: %+v", blocks)
	}
	callID := blocks[0].ID
	cid := startConv(t, up, 0)

	// 约 60 KB 的文件，MAGIC 在正中间；同一轮对话中间又来了一条 system 消息（system 变了，要重发）
	var file strings.Builder
	for i := 1; i <= 1100; i++ {
		if i == 550 {
			file.WriteString("   550\tMAGIC = \"OBSIDIAN-3309\"\n")
			continue
		}
		fmt.Fprintf(&file, "%6d\tdef helper_%d(x): return x * %d  # padding\n", i, i, i)
	}
	setReply(up, "MAGIC 是 OBSIDIAN-3309")
	turn2 := []any{first, map[string]any{"role": "system", "content": "# Environment update\n - shell: pwsh"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": callID, "name": "Read", "input": map[string]any{"file_path": `C:\work\demo\big.py`}}}},
		ccUser(ccResult(callID, file.String(), false))}
	blocks, stop := ccStream(t, ccPost(t, ts.URL, body(turn2...), ccHdr))
	if stop != "end_turn" || len(blocks) != 1 || blocks[0].Text != "MAGIC 是 OBSIDIAN-3309" {
		t.Fatalf("这一轮应正常完成: stop=%q %+v", stop, blocks)
	}
	if startConv(t, up, 1) != cid || startConv(t, up, 2) != cid {
		t.Fatal("都应在同一个上游会话里")
	}
	sys, user := requireSystemUser(t, upstreamInput(t, up, 1))
	if !strings.Contains(sys, "Follow the coding guidelines") || !strings.Contains(sys, "# Environment update") || !strings.Contains(user, "Reply with exactly OK") {
		t.Fatalf("先单独发一轮新的完整 system: %.200q / %q", sys, user)
	}
	sys, user = requireSystemUser(t, upstreamInput(t, up, 2))
	if strings.Contains(sys, "Follow the coding guidelines") || !strings.Contains(user, `MAGIC = \"OBSIDIAN-3309\"`) && !strings.Contains(user, `MAGIC = "OBSIDIAN-3309"`) {
		t.Fatalf("本轮只带精简 system 与完整的文件: %.200q", sys)
	}
	if strings.Contains(user, "omitted") || len(sys)+len(user) > 96<<10 {
		t.Fatalf("文件不应被截，且本轮不超上限（%d 字节）", len(sys)+len(user))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
