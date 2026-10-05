package facade

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/prism"
)

var bridgeTestTools = []AnthropicTool{
	{Name: "Read", InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`)},
	{Name: "Write", InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`)},
	{Name: "Edit"},
}

func inputOf(t *testing.T, c localToolCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Input, &m); err != nil {
		t.Fatalf("input 不是 JSON 对象: %q", c.Input)
	}
	return m
}

func TestParseLocalToolCalls(t *testing.T) {
	text := "先看看文件。\n\n```local-tool\n{\"name\": \"Read\", \"input\": {\"file_path\": \"/a.txt\"}}\n```\n\n" +
		"```local-tool\n{\"name\": \"write\", \"input\": {\"file_path\": \"/b.txt\", \"content\": \"x\"}}\n```\n"
	prose, calls := parseLocalToolCalls(text, bridgeTestTools)
	if prose != "先看看文件。" || len(calls) != 2 || calls[0].Name != "Read" || calls[1].Name != "Write" {
		t.Fatalf("应拆出正文与两次调用（工具名按声明的大小写）: %q %+v", prose, calls)
	}
	if inputOf(t, calls[1])["content"] != "x" {
		t.Fatalf("参数应原样保留: %s", calls[1].Input)
	}

	// 没有调用块：正文原样
	if p, c := parseLocalToolCalls("  就是一段回答\n", bridgeTestTools); p != "就是一段回答" || c != nil {
		t.Fatalf("纯文本不应有调用: %q %+v", p, c)
	}
}

// 写 Markdown 文件时内容里自己带 ``` 行（模型没把换行转义）：收尾围栏取让 JSON 解析得通的那一个。
func TestParseLocalToolCalls_FenceInsideContent(t *testing.T) {
	text := "```local-tool\n{\"name\": \"Write\", \"input\": {\"file_path\": \"/r.md\", \"content\": \"# T\n```bash\nnpm i\n```\n\"}}\n```\n完成"
	prose, calls := parseLocalToolCalls(text, bridgeTestTools)
	if len(calls) != 1 || calls[0].Name != "Write" || prose != "完成" {
		t.Fatalf("应解析成一次 Write: %q %+v", prose, calls)
	}
	if got := inputOf(t, calls[0])["content"]; got != "# T\n```bash\nnpm i\n```\n" {
		t.Fatalf("内容应完整: %q", got)
	}
}

func TestParseLocalToolCalls_Lenient(t *testing.T) {
	cases := map[string]struct {
		block string
		name  string
		check func(map[string]any) bool
	}{
		"arguments 是 JSON 字符串": {`{"name":"Read","arguments":"{\"file_path\":\"/x\"}"}`, "Read", func(m map[string]any) bool { return m["file_path"] == "/x" }},
		"参数平铺在顶层":              {`{"name":"Read","file_path":"/x"}`, "Read", func(m map[string]any) bool { return m["file_path"] == "/x" && m["name"] == nil }},
		"OpenAI function 形态":   {`{"function":{"name":"Read","arguments":{"file_path":"/x"}}}`, "Read", func(m map[string]any) bool { return m["file_path"] == "/x" }},
		"字符串里的原始换行":            {"{\"name\":\"Write\",\"input\":{\"file_path\":\"/x\",\"content\":\"a\nb\"}}", "Write", func(m map[string]any) bool { return m["content"] == "a\nb" }},
		"HTML 属性里没转义的引号":       {`{"name":"Write","input":{"file_path":"/x","content":"<div class="a">hi</div>"}}`, "Write", func(m map[string]any) bool { return m["content"] == `<div class="a">hi</div>` }},
		"Windows 路径没加倍的反斜杠":    {`{"name":"Read","input":{"file_path":"C:\Users\me\a.txt"}}`, "Read", func(m map[string]any) bool { return m["file_path"] == `C:\Users\me\a.txt` }},
		"末尾多余的逗号":              {`{"name":"Read","input":{"file_path":"/x",},}`, "Read", func(m map[string]any) bool { return m["file_path"] == "/x" }},
		"数字保持原样":               {`{"name":"Read","input":{"file_path":"/x","offset":12345678901234567890}}`, "Read", func(m map[string]any) bool { return m["offset"] == 1.2345678901234567e19 }},
	}
	for name, c := range cases {
		_, calls := parseLocalToolCalls("```local-tool\n"+c.block+"\n```", bridgeTestTools)
		if len(calls) != 1 || calls[0].Name != c.name || !c.check(inputOf(t, calls[0])) {
			t.Errorf("%s: %+v", name, calls)
		}
	}
	// 一个块里多个对象（数组 / 逐行）
	for _, block := range []string{`[{"name":"Read","input":{"file_path":"/a"}},{"name":"Read","input":{"file_path":"/b"}}]`,
		"{\"name\":\"Read\",\"input\":{\"file_path\":\"/a\"}}\n{\"name\":\"Read\",\"input\":{\"file_path\":\"/b\"}}"} {
		if _, calls := parseLocalToolCalls("```local-tool\n"+block+"\n```", bridgeTestTools); len(calls) != 2 {
			t.Errorf("应拆成两次调用: %q → %+v", block, calls)
		}
	}
}

func TestParseLocalToolCalls_Invalid(t *testing.T) {
	_, calls := parseLocalToolCalls("```local-tool\n{\"name\": \"Write\", \"input\": {\"file_path\": }}\n```", bridgeTestTools)
	if len(calls) != 1 || calls[0].Name != invalidToolName {
		t.Fatalf("坏块应转成纠错调用: %+v", calls)
	}
	if e, _ := inputOf(t, calls[0])["error"].(string); !strings.Contains(e, `"Write"`) || !strings.Contains(e, "not valid JSON") {
		t.Fatalf("纠错调用应写明原因与工具名: %q", e)
	}
	// 未闭合的块：到结尾为止
	_, calls = parseLocalToolCalls("```local-tool\n{\"name\":\"Read\",\"input\":{\"file_path\":\"/a\"}}", bridgeTestTools)
	if len(calls) != 1 || calls[0].Name != "Read" {
		t.Fatalf("未闭合的块也应解析: %+v", calls)
	}
}

func TestRenderToolCatalog(t *testing.T) {
	tools := []AnthropicTool{{Name: "Grep", Description: "Search file contents.", InputSchema: json.RawMessage(`{"type":"object",
		"properties":{"pattern":{"type":"string","description":"Regex"},"output_mode":{"type":"string","enum":["content","files_with_matches"]},
		"-i":{"type":"boolean"},"edits":{"type":"array","items":{"type":"object","properties":{"old":{"type":"string"}},"required":["old"]}}},
		"required":["pattern"]}`)}}
	out := renderToolCatalog(tools, toolCatalogBudget)
	for _, want := range []string{"### Grep\nSearch file contents.\nInput:\n- pattern (string, required): Regex\n- -i (boolean)\n",
		`- output_mode (one of "content" | "files_with_matches")`, "- edits (array of object)\n  - old (string, required)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("清单应含 %q:\n%s", want, out)
		}
	}
	if renderToolCatalog(tools, toolCatalogBudget) != out {
		t.Fatal("清单输出必须稳定（system 一变就要整段重发）")
	}

	// 放不进预算时换更简略的一档
	var many []AnthropicTool
	for i := 0; i < 80; i++ {
		many = append(many, AnthropicTool{Name: "mcp__x__tool" + strings.Repeat("y", i%7), Description: strings.Repeat("Long description. ", 80),
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","description":"` + strings.Repeat("d", 300) + `"}}}`)})
	}
	if n := len(renderToolCatalog(many, toolCatalogBudget)); n > toolCatalogBudget {
		t.Fatalf("工具很多时清单应压到预算以内: %d", n)
	}
}

func TestAnthropicChatMessages(t *testing.T) {
	var msgs []AnthropicMessage
	if err := json.Unmarshal([]byte(`[
		{"role":"user","content":[{"type":"text","text":"<system-reminder>规则</system-reminder>"},{"type":"text","text":"读 a.png"}]},
		{"role":"system","content":"# Environment\n - Primary working directory: /home/me/p"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"想","signature":"s"},{"type":"text","text":"读一下"},
			{"type":"tool_use","id":"toolu_op00000000000000ffabcdabcd","name":"Read","input":{"file_path":"/home/me/p/a.png"}},
			{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"`+strings.Repeat("x", 3000)+`"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_op00000000000000ffabcdabcd","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]},
			{"type":"tool_result","tool_use_id":"toolu_2","content":"boom","is_error":true}]},
		{"role":"system","content":[{"type":"text","text":"<total_tokens>99 tokens left</total_tokens>"}]}
	]`), &msgs); err != nil {
		t.Fatal(err)
	}
	chat := anthropicChatMessages(msgs)
	if len(chat) != 4 || chat[1].Role != "system" || chat[2].Role != "assistant" {
		t.Fatalf("对话中间的 system 保留、计数提示丢掉: %+v", chat)
	}
	if got := chat[0].Content.Text(); got != "<system-reminder>规则</system-reminder>\n\n读 a.png" {
		t.Fatalf("user 的多个文本块应分段拼接: %q", got)
	}
	a := chat[2].Content.Text()
	if strings.Contains(a, "想") || !strings.Contains(a, "读一下\n\n```local-tool\n{\"input\":{\"file_path\":\"/home/me/p/a.png\"},\"name\":\"Read\"}\n```") ||
		!strings.Contains(a, "[1000 more characters omitted]") {
		t.Fatalf("助手消息应回放成调用围栏（不带思考、长参数截短）: %q", a)
	}
	if chat[2].fp != 0xff {
		t.Fatalf("应从 tool_use ID 取回原文指纹: %x", chat[2].fp)
	}
	u := toInputContent(chat[3].Content, true)
	if len(u) != 2 || u[1].Type != "input_image" || u[1].ImageURL != "data:image/png;base64,iVBOR" {
		t.Fatalf("工具结果里的图片应作为图片块转给上游: %+v", u)
	}
	if !strings.Contains(u[0].Text, "[CLIENT RESULT tool=Read id=toolu_op00000000000000ffabcdabcd]\n[image attached to this message]") ||
		!strings.Contains(u[0].Text, "[CLIENT RESULT tool=Bash id=toolu_2 ERROR]\nboom\n[/CLIENT RESULT]") {
		t.Fatalf("工具结果应标注工具名与出错: %q", u[0].Text)
	}
}

func TestAddClientToolReminder(t *testing.T) {
	chat := []ChatMessage{{Role: "system", Content: stringContent("S")}, {Role: "user", Content: stringContent("Q")}}
	input := translateChatMessages(chat, "", 0)
	conv := chatConversation(chat, "")
	addClientToolReminder(input, conv)
	if !strings.HasSuffix(itemText(input[len(input)-1]), clientToolReminder) || !strings.HasSuffix(itemText(conv.current), clientToolReminder) {
		t.Fatal("本轮消息应带上提醒")
	}
	if es := conv.entries(); es[len(es)-1].text != "Q" {
		t.Fatalf("续接比对用不带提醒的原文: %q", es[len(es)-1].text)
	}
}

func TestClaudeCodeSessionID(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Claude-Code-Session-Id", "hdr-1")
	if got := claudeCodeSessionID(r, nil); got != "hdr-1" {
		t.Fatalf("请求头优先: %q", got)
	}
	r = httptest.NewRequest("POST", "/v1/messages", nil)
	for raw, want := range map[string]string{
		`{"user_id":"{\"device_id\":\"d\",\"session_id\":\"json-2\"}"}`: "json-2",
		`{"user_id":"user_abc_account_x_session_old-3"}`:                "old-3",
		`{"user_id":"plain"}`: "",
	} {
		body := map[string]json.RawMessage{"metadata": json.RawMessage(raw)}
		if got := claudeCodeSessionID(r, body); got != want {
			t.Errorf("%s → %q，应为 %q", raw, got, want)
		}
	}
}

func TestDeltaFilesToClientTools(t *testing.T) {
	whole := json.RawMessage(`{"version":1,"hunks":[{"original":"","updated":"<svg/>"}]}`)
	patch := json.RawMessage(`{"version":1,"hunks":[{"original":"a\n","updated":"b\n","location":{"originalStartLine":3,"originalLineCount":1}}]}`)
	files := []prism.CodexDeltaFile{
		{FilePath: "AGENTS.md", Status: "added", Diff: whole},
		{FilePath: "img/p.svg", Status: "added", Diff: whole},
		{FilePath: "main.go", Status: "modified", Diff: patch},
		{FilePath: "old.txt", Status: "deleted"},
	}
	calls, note := deltaFilesToClientTools(files, `C:\work\demo`, []AnthropicTool{{Name: "Write"}, {Name: "Edit"}})
	if len(calls) != 2 || calls[0].Name != "Write" || calls[1].Name != "Edit" {
		t.Fatalf("新文件 → Write、修改 → Edit，系统文件与删除不转: %+v", calls)
	}
	if p := inputOf(t, calls[0])["file_path"]; p != `C:\work\demo\img\p.svg` {
		t.Fatalf("路径应接到客户端工作目录下（Windows 用反斜杠）: %q", p)
	}
	if e := inputOf(t, calls[1]); e["old_string"] != "a\n" || e["new_string"] != "b\n" {
		t.Fatalf("修改应按替换块转成 Edit: %+v", e)
	}
	if !strings.Contains(note, "old.txt") {
		t.Fatalf("没转的变更应写进说明: %q", note)
	}
	if calls, _ := deltaFilesToClientTools(files, "", []AnthropicTool{{Name: "Write"}}); calls != nil {
		t.Fatal("不知道客户端工作目录时不转")
	}
	if got := joinClientPath("/home/me/p", "a/b.txt"); got != "/home/me/p/a/b.txt" {
		t.Fatalf("POSIX 路径: %q", got)
	}
	if got := clientWorkingDir("# Environment\n - Primary working directory: C:\\work\\demo\n - Platform: win32"); got != `C:\work\demo` {
		t.Fatalf("应从 system 取出工作目录: %q", got)
	}
}

func TestToolUseFingerprintRoundTrip(t *testing.T) {
	calls := []localToolCall{{Name: "Read"}, {Name: "Bash"}}
	bridgeToolUseIDs(calls, "原文")
	want := entryFingerprint(historyEntry{speaker: "Assistant", text: "原文"})
	if got := toolUseFingerprint(calls[0].ID); got != want || toolUseFingerprint(calls[1].ID) != 0 {
		t.Fatalf("第一个 ID 应带原文指纹: %s %s", calls[0].ID, calls[1].ID)
	}
}

// 上游会话末尾多出客户端没有的几条（旁路请求 / 回退）：接着用，并提醒上游忽略。
func TestNativeDelta_BranchTail(t *testing.T) {
	b := committed(&nativeTurn{strong: true, conv: nativeConv("SYS", "第二问", historyEntry{speaker: "User", text: "第一问"}, historyEntry{speaker: "Assistant", text: "答一"})}, "答二")
	fork := &nativeTurn{strong: true, conv: nativeConv("SYS", "[SUGGESTION MODE]",
		historyEntry{speaker: "User", text: "第一问"}, historyEntry{speaker: "Assistant", text: "答一"}, historyEntry{speaker: "User", text: "第二问"}, historyEntry{speaker: "Assistant", text: "答二"})}
	if rest, ok := fork.delta(b, fork.conv.entries()); !ok || len(rest) != 1 || fork.branch != 0 {
		t.Fatalf("旁路请求本身是正常追加: ok=%v %+v", ok, rest)
	}
	b.delivered = fork.fingerprints(fork.conv.entries()) // 旁路那一轮提交后

	main := &nativeTurn{strong: true, conv: nativeConv("SYS", "第三问",
		historyEntry{speaker: "User", text: "第一问"}, historyEntry{speaker: "Assistant", text: "答一"}, historyEntry{speaker: "User", text: "第二问"}, historyEntry{speaker: "Assistant", text: "答二"})}
	rest, ok := main.delta(b, main.conv.entries())
	if !ok || len(rest) != 1 || rest[0].text != "第三问" || main.branch != 1 {
		t.Fatalf("主对话应接着同一个会话、跳过旁路的一条: ok=%v branch=%d %+v", ok, main.branch, rest)
	}
	items, _, _ := main.deltaItems(b, rest, 96<<10, "")
	if u := itemText(items[1]); !strings.HasPrefix(u, "[Note: the last 1 message(s)") || !strings.HasSuffix(u, "第三问") {
		t.Fatalf("增量应先提醒忽略旁路的消息: %q", u)
	}

	// 弱键不这样接；中间被改写（不只是末尾多出来）也不接
	weak := &nativeTurn{conv: main.conv}
	if _, ok := weak.delta(b, weak.conv.entries()); ok {
		t.Fatal("弱键对不上就是另一段对话")
	}
	rewritten := &nativeTurn{strong: true, conv: nativeConv("SYS", "第三问", historyEntry{speaker: "User", text: "第一问"}, historyEntry{speaker: "User", text: "改过的第二问"})}
	if _, ok := rewritten.delta(b, rewritten.conv.entries()); ok {
		t.Fatal("历史中间被改写时不能接")
	}
}
