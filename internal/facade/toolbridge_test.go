package facade

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// TestExtractExecBlock 覆盖围栏提取的三种形态。
func TestExtractExecBlock(t *testing.T) {
	// 1) 正常块
	js, ok := extractExecBlock("前言\n```codex-exec\nawait tools.exec_command({cmd:\"ls\"})\n```\n后记")
	if !ok || !strings.Contains(js, "tools.exec_command") {
		t.Fatalf("正常块提取失败: ok=%v js=%q", ok, js)
	}
	if strings.Contains(js, "前言") {
		t.Errorf("不应包含块外内容: %q", js)
	}

	// 2) 没有块
	if _, ok := extractExecBlock("纯文本回复，没有任何围栏"); ok {
		t.Fatal("纯文本不应提取出块")
	}

	// 3) 未闭合（流式截断场景）：取剩余内容
	js, ok = extractExecBlock("```codex-exec\ntext('hi')")
	if !ok || !strings.Contains(js, "text('hi')") {
		t.Fatalf("未闭合块应尽力提取: ok=%v js=%q", ok, js)
	}

	// 4) 不认别的围栏名（普通代码块不是工具调用）
	if _, ok := extractExecBlock("```js\nawait tools.exec_command()\n```"); ok {
		t.Fatal("普通 js 围栏不应被当成工具调用")
	}
}

// TestEnsureExecJS 覆盖"模型输出裸 shell 而非 JS"的自动包装。
//
// 实测背景：模型经常无视"输出 JS"的要求，直接把 bash 命令写进围栏，
// 进了 V8 就是 SyntaxError，然后陷入死循环。代理层兜底是必须的。
func TestEnsureExecJS(t *testing.T) {
	// 1) 裸命令 → 包装成 exec_command
	wrapped := ensureExecJS("printf '%s' 'X' > f.txt")
	if !strings.Contains(wrapped, "tools.exec_command") {
		t.Errorf("裸命令应被包装: %q", wrapped)
	}
	if !strings.Contains(wrapped, `printf '%s' 'X' > f.txt`) {
		t.Errorf("命令内容应原样保留: %q", wrapped)
	}

	// 2) 多行命令也整体保留
	multi := "set -eu\nprintf 'a' > f\ncat f"
	wrapped = ensureExecJS(multi)
	if !strings.Contains(wrapped, "cat f") {
		t.Errorf("多行命令应整体保留: %q", wrapped)
	}

	// 3) 已经是 JS → 原样透传
	js := "const r = await tools.exec_command({cmd: 'ls'});\ntext(r);"
	if got := ensureExecJS(js); got != js {
		t.Errorf("JS 应原样透传: %q", got)
	}
}

// TestBridgeEnabled：Codex CLI 有两条工具声明路径（见 BridgeEnabled 注释）。
// 早期只认路径 A，导致走路径 B 的客户端桥静默失效（模型退回上游沙箱执行，
// 本地拿不到文件）。这里把两条路径与"不该误伤"的场景都钉住。
func TestBridgeEnabled(t *testing.T) {
	// 路径 A：additional_tools 条目
	withTools := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"additional_tools","tools":[]}]`),
	}
	if !BridgeEnabled(withTools) {
		t.Fatal("含 additional_tools 应启用桥")
	}

	// 路径 A 续：已有 custom_tool_call 往返（Codex 独有形状）
	withCustom := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"custom_tool_call","name":"exec","input":"..."}]`),
	}
	if !BridgeEnabled(withCustom) {
		t.Fatal("含 custom_tool_call 应启用桥")
	}

	// 路径 B：标准 tools 字段 + Codex 独有工具特征
	withStandardTools := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`),
		"tools": json.RawMessage(`[{"type":"custom","name":"exec","description":"run a command"}]`),
	}
	if !BridgeEnabled(withStandardTools) {
		t.Fatal("顶层 tools 含 exec 应启用桥（路径 B）")
	}
	// 关键回归：新版 CLI 的工具名是 exec_command（不是 exec）——
	// 早期全名匹配 `"name":"exec"` 会漏判，这里钉住。
	withExecCommand := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user"}]`),
		"tools": json.RawMessage(`[{"type":"custom","name":"exec_command","description":"Runs a command"},{"type":"custom","name":"write_stdin"}]`),
	}
	if !BridgeEnabled(withExecCommand) {
		t.Fatal("顶层 tools 含 exec_command 应启用桥（路径 B，新版命名）")
	}

	withApplyPatch := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user"}]`),
		"tools": json.RawMessage(`[{"type":"custom","name":"apply_patch"}]`),
	}
	if !BridgeEnabled(withApplyPatch) {
		t.Fatal("顶层 tools 含 apply_patch 应启用桥（路径 B）")
	}

	// 不该误伤：普通 API 调用方的 function 工具
	plainFunctions := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","role":"user"}]`),
		"tools": json.RawMessage(`[{"type":"function","name":"get_weather","parameters":{}}]`),
	}
	if BridgeEnabled(plainFunctions) {
		t.Fatal("普通 function 工具不应启用桥（否则破坏正常 function calling）")
	}
	// 不该误伤：空 tools / null / 纯文本
	for name, raw := range map[string]map[string]json.RawMessage{
		"空 tools":   {"input": json.RawMessage(`[{"type":"message"}]`), "tools": json.RawMessage(`[]`)},
		"null":      {"input": json.RawMessage(`[{"type":"message"}]`), "tools": json.RawMessage(`null`)},
		"纯文本 input": {"input": json.RawMessage(`"你好"`)},
		"无 input":   {},
	} {
		if BridgeEnabled(raw) {
			t.Fatalf("%s 不应启用桥", name)
		}
	}
}

// TestExecToolName：工具名必须从请求里动态提取。
//
// 背景：CLI v0.154 工具名是 exec，v0.159 改成 exec_command。
// 名字用错时客户端直接拒绝（"unsupported custom tool call: exec"），
// 模型看不到执行结果，反复要求用户重发内容 —— 表现得像上下文丢失。
func TestExecToolName(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]json.RawMessage
		want string
	}{
		{
			"新版 CLI（exec_command）",
			map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"custom","name":"exec_command"},{"name":"write_stdin"}]`)},
			"exec_command",
		},
		{
			"旧版 CLI（exec）",
			map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"custom","name":"exec"}]`)},
			"exec",
		},
		{
			"路径 A（additional_tools 内含名字）",
			map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"additional_tools","tools":[{"name":"exec"}]}]`)},
			"exec",
		},
		{
			"都不认识时兜底 exec",
			map[string]json.RawMessage{"input": json.RawMessage(`"hi"`)},
			"exec",
		},
	}
	for _, c := range cases {
		if got := ExecToolName(c.raw); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestExecToolKind：区分 custom（v0.154）与 function（v0.159）两种工具形状。
// 回错形状时客户端静默不执行，模型陷入"执行被中止"的循环。
func TestExecToolKind(t *testing.T) {
	funcTool := map[string]json.RawMessage{
		"tools": json.RawMessage(`[{"type":"function","name":"exec_command","parameters":{}}]`),
	}
	if got := ExecToolKind(funcTool); got != "function" {
		t.Errorf("function 工具判定错误: got %q", got)
	}
	customTool := map[string]json.RawMessage{
		"tools": json.RawMessage(`[{"type":"custom","name":"exec","format":{}}]`),
	}
	if got := ExecToolKind(customTool); got != "custom" {
		t.Errorf("custom 工具判定错误: got %q", got)
	}
}

// TestToFunctionArguments：模型输出的 JS 源码要能转成 function 工具要的 JSON。
func TestToFunctionArguments(t *testing.T) {
	// JS 形状 -> JSON
	args := toFunctionArguments(`const out = await tools.exec_command({ cmd: "Set-Content -Path a.txt -Value 'hi'" });`)
	var m map[string]string
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatalf("JS 转换结果不是合法 JSON: %s", args)
	}
	if m["cmd"] != "Set-Content -Path a.txt -Value 'hi'" {
		t.Errorf("cmd 提取错误: %q", m["cmd"])
	}
	// 已是 JSON -> 原样
	args2 := toFunctionArguments(`{"cmd":"echo hi"}`)
	if err := json.Unmarshal([]byte(args2), &m); err != nil || m["cmd"] != "echo hi" {
		t.Errorf("JSON 直通失败: %s", args2)
	}
	// 带转义换行的 JS 字符串（JS 源码里是 \n 两个字符）
	args3 := toFunctionArguments("const o = await tools.exec_command({ cmd: \"a\\nb\" });")
	if err := json.Unmarshal([]byte(args3), &m); err != nil {
		t.Fatalf("转义场景失败: %s", args3)
	}
	if m["cmd"] != "a\nb" && m["cmd"] != `a\nb` {
		t.Logf("换行处理: %q（可接受）", m["cmd"])
	}

	// 关键回归测试（用户本次诊断 Bug）：const cmd = String.raw`...` + { cmd, max_output_tokens: 16000 }
	userCase := "const cmd = String.raw`\nWrite-Output 'Hello Diagnosis'\nGet-Process\n`;\nconst out = await tools.exec_command({ cmd, max_output_tokens: 16000 });\ntext(out);"
	args4 := toFunctionArguments(userCase)
	if err := json.Unmarshal([]byte(args4), &m); err != nil {
		t.Fatalf("用户诊断场景失败: %s", args4)
	}
	if strings.Contains(m["cmd"], "tools.exec_command") || strings.Contains(m["cmd"], "const out =") {
		t.Fatalf("提取结果绝不能包含 JS 胶水代码: %q", m["cmd"])
	}
	if !strings.Contains(m["cmd"], "Write-Output 'Hello Diagnosis'") || !strings.Contains(m["cmd"], "Get-Process") {
		t.Fatalf("未能正确提取变量中的命令内容: %q", m["cmd"])
	}
}

// TestHasPriorToolResult：已有执行结果时不应再注入"你什么都没执行"的纠错。
//
// 实测：任务已跑起来、模型正常收尾输出纯文本时，纠错会把模型带偏，
// 它转而去要求用户"把原始内容再发一遍"。
func TestHasPriorToolResult(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"function_call_output", `[{"type":"function_call_output","call_id":"c1","output":"ok"}]`, true},
		{"custom_tool_call_output", `[{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}]`, true},
		{"首轮无结果", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"create a.txt"}]}]`, false},
		{"空 input", ``, false},
	}
	for _, c := range cases {
		raw := map[string]json.RawMessage{}
		if c.raw != "" {
			raw["input"] = json.RawMessage(c.raw)
		}
		if got := hasPriorToolResult(raw); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBridgeResultTextArray：内容数组形态的工具结果必须提取 text。
//
// CLI 的结果是 [{"type":"input_text","text":...}]，不提取的话模型读到的是
// 一坨转义 JSON，读不懂就以为"没有输出"。
func TestBridgeResultTextArray(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"echo hi\"}"},
		{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"Script completed"},{"type":"input_text","text":"{\"chunk_id\":\"x\"}"}]}
	]`)
	items := bridgeInputItems(raw, "sys")
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "Script completed") {
		t.Error("内容数组未提取 text（模型会读到一坨转义 JSON）")
	}
	// 泄漏的特征是**转义形式**（数组被当作文本再序列化一遍），
	// 而不是正常的 input_text 类型名。
	if strings.Contains(s, `\"input_text\"`) {
		t.Error("原始 content 数组结构泄漏进了上下文（模型会读到转义 JSON）")
	}
}

// TestReplayCallText：上一轮工具调用的回放不能是空块。
//
// 背景：function_call 的参数在 arguments（JSON），custom_tool_call 在 input
// （JS 源码）。只读 input 会让 CLI v0.159 的历史回放变成空块，模型回看时
// 以为自己的命令丢了，转而要求用户重发内容。
func TestReplayCallText(t *testing.T) {
	// JSON 形态（function_call）-> 渲染回 JS
	got := replayCallText(`{"cmd":"Set-Content -Path 'a.txt' -Value 'hi'"}`)
	if !strings.Contains(got, "tools.exec_command") || !strings.Contains(got, "Set-Content") {
		t.Errorf("JSON 参数未渲染成 JS: %s", got)
	}
	idx := strings.Index(got, "{ cmd: ")
	if idx < 0 {
		t.Fatalf("缺少 cmd 参数: %s", got)
	}
	rest := strings.TrimSuffix(strings.TrimSpace(got[idx+len("{ cmd: "):]), "});")
	var s string
	if err := json.Unmarshal([]byte(rest), &s); err != nil {
		t.Errorf("渲染出的 cmd 不是合法 JSON 字符串: %v (%s)", err, rest)
	} else if s != "Set-Content -Path 'a.txt' -Value 'hi'" {
		t.Errorf("cmd 内容错误: %q", s)
	}
	// JS 形态（custom_tool_call）-> 原样
	js := `const out = await tools.exec_command({ cmd: "echo hi" });`
	if replayCallText(js) != js {
		t.Errorf("JS 应原样回放: %s", replayCallText(js))
	}
	if replayCallText("") != "" {
		t.Error("空参数应返回空（不伪造内容）")
	}
}

// TestBridgeInputReplayFunctionCall 是本次线上事故的直接回归。
//
// 真实请求里工具调用是
//
//	{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"...\"}"}
//
// 若只读 input 字段，回放块是空的 —— 模型看到自己"什么都没发过"，
// 于是回复"请把原始命令/内容再发一遍"。
func TestBridgeInputReplayFunctionCall(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"Set-Content -Path 'a.txt' -Value 'hi'\"}"},
		{"type":"function_call_output","call_id":"c1","output":"Added a.txt (+1 -0)"}
	]`)
	items := bridgeInputItems(raw, "sys")
	b, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, "Set-Content") {
		t.Error("function_call 的 arguments 未回放进上下文（历史上会变成空块）")
	}
	if !strings.Contains(s, "tools.exec_command") {
		t.Error("回放未渲染成桥约定的 JS 形态")
	}
	if strings.Contains(s, "codex-exec\\n\\n```") {
		t.Error("出现了空的 codex-exec 回放块")
	}
	if !strings.Contains(s, "Added a.txt") {
		t.Error("function_call_output 的结果未回放进上下文")
	}
}

// TestIsShellSyntaxError：区分"语法错"与"逻辑错"。
//
// 语法错要给出换语法的指引；逻辑错（文件不存在、权限不足）原样回放即可。
func TestIsShellSyntaxError(t *testing.T) {
	yes := []string{
		"Failed (exit 1)\n2 | cat > a.html <<'HTML'\n  | 重定向运算符后缺少文件规范。",
		"The '<' operator is reserved for future use.",
		"ParserError: Unexpected token 'HTML'",
		"bash: syntax error near unexpected token `newline'",
		"warning: here-document delimited by end-of-file",
	}
	for _, s := range yes {
		if !isShellSyntaxError(s) {
			t.Errorf("应判定为语法错: %s", s)
		}
	}
	no := []string{
		"ENOENT: no such file or directory, open 'a.txt'",
		"Permission denied",
		"Added pelican-bicycle.html (+168 -0)",
	}
	for _, s := range no {
		if isShellSyntaxError(s) {
			t.Errorf("不应判定为语法错: %s", s)
		}
	}
}

// TestBridgeInputItems 覆盖 Codex input 的翻译：
// 消息保留、工具调用回放为 assistant、工具结果回放为 user。
func TestBridgeInputItems(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"additional_tools","tools":[]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"be helpful"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"make a file"}]},
		{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"await tools.exec_command()"},
		{"type":"custom_tool_call_output","call_id":"c1","output":"[CLIENT RESULT]\nfile written\n[/CLIENT RESULT]"},
		{"type":"reasoning","summary":[]}
	]`)
	items := bridgeInputItems(raw, "base")
	if len(items) == 0 {
		t.Fatal("翻译结果为空")
	}

	all := ""
	for _, it := range items {
		for _, c := range it.Content {
			all += c.Text + "\n"
		}
	}
	if !strings.Contains(all, "bridge") || !strings.Contains(all, "codex-exec") {
		t.Error("桥指令必须注入（含 codex-exec 契约）")
	}
	if !strings.Contains(all, "be helpful") {
		t.Error("developer 消息应保留")
	}
	if !strings.Contains(all, "make a file") {
		t.Error("user 任务应保留")
	}
	if !strings.Contains(all, "await tools.exec_command()") {
		t.Error("custom_tool_call 应回放为 assistant 文本（上游需要自己的操作记忆）")
	}
	if !strings.Contains(all, "file written") {
		t.Error("custom_tool_call_output 应回放为 user 文本")
	}
	if strings.Contains(all, "additional_tools") {
		t.Error("additional_tools 不应出现在发给上游的文本里")
	}
}

// TestCustomToolCallItemJSON 校验 custom_tool_call 条目形状
// 与 Codex CLI 源码 ResponseItem::CustomToolCall 反序列化需求对齐
// （codex-rs/protocol/src/models.rs: call_id/name/input 为必填）。
func TestCustomToolCallItemJSON(t *testing.T) {
	item := customToolCallItemJSON("ctc_1", "await tools.exec_command()", "exec_command")
	var m map[string]any
	if err := json.Unmarshal([]byte(item), &m); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	for _, k := range []string{"id", "type", "status", "call_id", "name", "input"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少必填字段 %s: %s", k, item)
		}
	}
	// name 必须与传入一致（动态工具名：CLI 版本间 exec -> exec_command）
	if m["type"] != "custom_tool_call" || m["name"] != "exec_command" {
		t.Errorf("type/name 错误: %s", item)
	}
	if m["input"] != "await tools.exec_command()" {
		t.Errorf("input 应为 JS 源码字符串: %s", item)
	}
}

// foldInputHistory 回归：上游只认「首 system + 最后一条消息」，
// 桥模式多轮历史必须折叠进 system（否则 Codex 跨轮失忆，2026-10-03）。
func TestFoldInputHistory(t *testing.T) {
	mk := func(role, text string) prism.InputItem {
		it := prism.NewUserItem(text)
		it.Role = role
		return it
	}
	items := []prism.InputItem{
		prism.NewSystemItem("bridge-prompt"),
		mk("user", "任务一"),
		mk("assistant", "```codex-exec\nconst out = await tools.exec_command({cmd:'ls'});\n```"),
		mk("user", "[CLIENT RESULT] file-list"),
		mk("user", "任务二"),
		prism.NewSystemItem("tail-reminder"),
	}
	got := foldInputHistory(items, 0)
	if len(got) != len(items) {
		t.Fatalf("期望完整保留所有 %d 条消息条目，实际得到 %d", len(items), len(got))
	}
	sys := got[0].Content[0].Text
	if !strings.Contains(sys, "[Previous Conversation History]") ||
		!strings.Contains(sys, "任务一") ||
		!strings.Contains(sys, "[CLIENT RESULT]") {
		t.Fatalf("折叠历史缺失关键内容:\n%s", sys)
	}
	if !strings.Contains(sys, "bridge-prompt") {
		t.Fatalf("桥指令丢失:\n%s", sys)
	}
	if got[4].Role != "user" || got[4].Content[0].Text != "任务二" {
		t.Fatalf("最后一条用户消息应原样保留: %+v", got[4])
	}
	if got[len(got)-1].Content[0].Text != "tail-reminder" {
		t.Fatalf("尾部提醒应保留在末位: %+v", got[len(got)-1])
	}

	// 短输入（system + user）不折叠。
	two := []prism.InputItem{prism.NewSystemItem("s"), mk("user", "hi")}
	if got2 := foldInputHistory(two, 0); len(got2) != 2 {
		t.Fatalf("两条输入不应折叠")
	}
}

// TestFoldInputHistory_FiltersStaticInstructions 验证客户端注入的 AGENTS.md 静态指令
// 会被自动剔除，不作为用户提问污染 [Previous Conversation History]。
func TestFoldInputHistory_FiltersStaticInstructions(t *testing.T) {
	mk := func(role, text string) prism.InputItem {
		it := prism.NewUserItem(text)
		it.Role = role
		return it
	}
	items := []prism.InputItem{
		prism.NewSystemItem("bridge-prompt"),
		mk("user", "# AGENTS.md instructions for C:\\Users\\test\n\n<INSTRUCTIONS>\nDO NOT send optional context\n</INSTRUCTIONS>"),
		mk("user", "请帮我写一个网页"),
		mk("assistant", "```codex-exec\nconst out = await tools.exec_command({ cmd: \"echo done\" });\n```"),
		mk("user", "[CLIENT RESULT]\nProcess exited with code 0\n[/CLIENT RESULT]"),
		mk("assistant", "已创建完成。"),
		mk("user", "文件保存在哪里？"),
		prism.NewSystemItem("tail-reminder"),
	}
	got := foldInputHistory(items, 0)
	if len(got) != len(items) {
		t.Fatalf("期望完整保留所有 %d 条消息条目，得到 %d", len(items), len(got))
	}
	sys := got[0].Content[0].Text
	if strings.Contains(sys, "# AGENTS.md") {
		t.Fatalf("静态 AGENTS.md 不应混入历史: %s", sys)
	}
	if !strings.Contains(sys, "请帮我写一个网页") || !strings.Contains(sys, "echo done") {
		t.Fatalf("真实历史丢失: %s", sys)
	}
	if got[6].Content[0].Text != "文件保存在哪里？" {
		t.Fatalf("当轮用户提问不匹配: %+v", got[6])
	}
}

// TestBridgeInputItems_And_FoldInputHistory_MultiTurnToolExecution 验证真实 Codex CLI 多轮工具调用的完整输入翻译与全量保留
func TestBridgeInputItems_And_FoldInputHistory_MultiTurnToolExecution(t *testing.T) {
	rawJSON := `[
		{"type":"message","role":"developer","content":"You are Codex..."},
		{"type":"message","role":"user","content":"用 HTML 实现一个 SVG，绘制鹈鹕骑自行车的场景，输出到本地文件"},
		{"type":"custom_tool_call","name":"exec_command","call_id":"call_123","input":"const out = await tools.exec_command({ cmd: \"python -c 'Path(\\\"pelican-bicycle.html\\\").write_text(...)'\" });"},
		{"type":"custom_tool_call_output","call_id":"call_123","output":"(no output)"},
		{"type":"message","role":"assistant","content":"The command exited successfully with no output. What would you like to do next?"},
		{"type":"message","role":"user","content":"你文件输出的路径在哪里？"}
	]`

	items := bridgeInputItems([]byte(rawJSON), "default-sys")
	if len(items) == 0 {
		t.Fatalf("bridgeInputItems 解析失败")
	}

	folded := foldInputHistory(items, 0)
	if len(folded) != len(items) {
		t.Fatalf("期望完整保留所有 %d 条消息条目，实际得到 %d 条", len(items), len(folded))
	}

	sysText := folded[0].Content[0].Text
	if !strings.Contains(sysText, "pelican-bicycle.html") {
		t.Fatalf("历史折叠应完整保留上一轮工具调用的文件名 pelican-bicycle.html:\n%s", sysText)
	}
	if !strings.Contains(sysText, "(no output)") {
		t.Fatalf("历史折叠应完整保留上一轮工具调用结果 (no output):\n%s", sysText)
	}
	// 确认中间真实消息未被丢弃
	hasPelican := false
	for _, it := range folded {
		for _, c := range it.Content {
			if strings.Contains(c.Text, "用 HTML 实现一个 SVG") {
				hasPelican = true
				break
			}
		}
	}
	if !hasPelican {
		t.Fatalf("真实 User 提问在消息流中丢失")
	}
}

func TestExtractIncrementalInput(t *testing.T) {
	// Case 1: 首轮单消息
	turn1 := []prism.InputItem{
		prism.NewSystemItem("sys"),
		prism.NewUserItem("u1"),
	}
	inc1 := extractIncrementalInput(turn1)
	if len(inc1) != 2 || inc1[1].Content[0].Text != "u1" {
		t.Fatalf("Case 1 预期原样返回, got: %v", inc1)
	}

	// Case 2: 工具结果回传
	turn2 := []prism.InputItem{
		prism.NewSystemItem("sys"),
		prism.NewUserItem("u1"),
		prism.NewAssistantItem("tool-call"),
		prism.NewUserItem("[CLIENT RESULT] ok"),
	}
	inc2 := extractIncrementalInput(turn2)
	if len(inc2) != 2 {
		t.Fatalf("Case 2 预期 2 项(sys + toolResult), got len: %d", len(inc2))
	}
	if inc2[0].Role != "system" || inc2[1].Content[0].Text != "[CLIENT RESULT] ok" {
		t.Fatalf("Case 2 内容不匹配: %v", inc2)
	}

	// Case 3: 第二轮追问
	turn3 := []prism.InputItem{
		prism.NewSystemItem("sys"),
		prism.NewUserItem("u1"),
		prism.NewAssistantItem("tool-call"),
		prism.NewUserItem("[CLIENT RESULT] ok"),
		prism.NewAssistantItem("文件已生成"),
		prism.NewUserItem("你知道我刚才让你干什么了吗？"),
	}
	inc3 := extractIncrementalInput(turn3)
	if len(inc3) != 2 {
		t.Fatalf("Case 3 预期 2 项(sys + u2), got len: %d", len(inc3))
	}
	if inc3[0].Role != "system" || inc3[1].Content[0].Text != "你知道我刚才让你干什么了吗？" {
		t.Fatalf("Case 3 内容不匹配: %v", inc3)
	}
}

func TestIsFauxSandboxCompletion(t *testing.T) {
	// 命中场景 1：口头声称已创建
	if !IsFauxSandboxCompletion("已创建 `pelican-bicycle.html:1`，浏览器打开即可查看。") {
		t.Errorf("未能识别口头宣称已创建")
	}

	// 命中场景 2：英文 created file
	if !IsFauxSandboxCompletion("I have created file `main.py` in the workspace.") {
		t.Errorf("未能识别英文 created file")
	}

	// 正常回答不应误判
	if IsFauxSandboxCompletion("请问你需要使用什么前端框架？比如 React 或 Vue？") {
		t.Errorf("普通问答不应误判为假完成")
	}
}

func TestIsSystemIgnoredFile(t *testing.T) {
	if !isSystemIgnoredFile("AGENTS.md") {
		t.Errorf("AGENTS.md 必须被判定为系统忽略文件")
	}
	if !isSystemIgnoredFile("/codex_workspace/AGENTS.md") {
		t.Errorf("沙箱路径 /codex_workspace/AGENTS.md 必须被判定为系统忽略文件")
	}
	if !isSystemIgnoredFile("README.md") {
		t.Errorf("README.md 必须被判定为系统忽略文件")
	}
	if !isSystemIgnoredFile(".git/config") {
		t.Errorf(".git/config 必须被判定为系统忽略文件")
	}
	if isSystemIgnoredFile("src/index.html") {
		t.Errorf("正常用户文件 src/index.html 绝不能被忽略")
	}
	if isSystemIgnoredFile("pelican-bicycle.html") {
		t.Errorf("正常用户文件 pelican-bicycle.html 绝不能被忽略")
	}
}

func TestSessionChainFilterNewDeltaFiles(t *testing.T) {
	key := "test-dedup-" + t.Name()
	diffBytes, _ := json.Marshal("--- /dev/null\n+++ src/app.html\n@@ -0,0 +1 @@\n+test")
	files := []prism.CodexDeltaFile{
		{
			FilePath: "AGENTS.md",
			Status:   "added",
			Diff:     json.RawMessage(`"system file"`),
		},
		{
			FilePath: "src/app.html",
			Status:   "added",
			Diff:     json.RawMessage(diffBytes),
		},
	}

	// 第 1 轮：过滤掉 AGENTS.md，返回 src/app.html
	filtered1 := sessionChainFilterNewDeltaFiles(key, files)
	if len(filtered1) != 1 || filtered1[0].FilePath != "src/app.html" {
		t.Fatalf("第 1 轮应只保留 src/app.html，实际得到 %v", filtered1)
	}

	// 第 2 轮：上游继续返回相同的 files，应当全部去重过滤，返回空切片
	filtered2 := sessionChainFilterNewDeltaFiles(key, files)
	if len(filtered2) != 0 {
		t.Fatalf("第 2 轮未改变的文件必须全部被去重，实际得到 %v", filtered2)
	}
}

func TestSynthesizeDeltaFilesExecJS(t *testing.T) {
	diffBytes, _ := json.Marshal("--- /dev/null\n+++ src/app.html\n@@ -0,0 +1,2 @@\n+<h1>Hello</h1>\n+<svg></svg>")
	files := []prism.CodexDeltaFile{
		{
			FilePath: "src/app.html",
			Status:   "added",
			Diff:     json.RawMessage(diffBytes),
		},
		{
			FilePath: "old.txt",
			Status:   "deleted",
		},
	}

	// Windows 测试
	winJS := SynthesizeDeltaFilesExecJS(files, true)
	if !strings.Contains(winJS, "WriteAllBytes") || !strings.Contains(winJS, "CreateDirectory") {
		t.Errorf("Windows JS 应当包含 WriteAllBytes 和 CreateDirectory: %s", winJS)
	}
	if !strings.Contains(winJS, "[IO.File]::Delete") {
		t.Errorf("Windows JS 应当处理删除文件: %s", winJS)
	}

	// POSIX 测试
	posixJS := SynthesizeDeltaFilesExecJS(files, false)
	if !strings.Contains(posixJS, "base64 --decode") || !strings.Contains(posixJS, "mkdir -p") {
		t.Errorf("POSIX JS 应当包含 base64 --decode 和 mkdir -p: %s", posixJS)
	}
	if !strings.Contains(posixJS, "rm -f") {
		t.Errorf("POSIX JS 应当处理删除文件: %s", posixJS)
	}
}

func TestSafeTruncateOutput(t *testing.T) {
	shortStr := "hello world"
	if got := safeTruncateOutput(shortStr, 100); got != shortStr {
		t.Errorf("短文本不应截断: %s", got)
	}

	longStr := strings.Repeat("A", 1000) + strings.Repeat("B", 1000)
	got := safeTruncateOutput(longStr, 200)
	if len([]rune(got)) >= 2000 {
		t.Errorf("超长文本未被成功截断")
	}
	if !strings.Contains(got, "输出过长，已智能保留首尾") {
		t.Errorf("截断提示缺失: %s", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("A", 100)) {
		t.Errorf("未正确保留首部")
	}
	if !strings.HasSuffix(got, strings.Repeat("B", 100)) {
		t.Errorf("未正确保留尾部")
	}
}

func TestSessionChainLookup(t *testing.T) {
	sessionChainBind("test-sess-key", "conv-uuid-1", "proj-uuid-1")
	sessionChainRecord("test-sess-key", &RunResult{
		ProjectID:      "proj-uuid-1",
		ConversationID: "conv-uuid-1",
		ResponseID:     "resp_12345",
	}, "test-model")

	// 1. 通过原始 key 查找
	proj, conv, prev, _, _ := sessionChainLookup("test-sess-key", "", "")
	if proj != "proj-uuid-1" || conv != "conv-uuid-1" || prev != "resp_12345" {
		t.Fatalf("通过 key 查找失败: proj=%s conv=%s prev=%s", proj, conv, prev)
	}

	// 2. 通过 previousResponseId 穿透查找（key 改变时）
	proj2, conv2, prev2, _, _ := sessionChainLookup("different-key", "resp_12345", "")
	if proj2 != "proj-uuid-1" || conv2 != "conv-uuid-1" || prev2 != "resp_12345" {
		t.Fatalf("通过 previousResponseId 穿透查找失败: proj=%s conv=%s prev=%s", proj2, conv2, prev2)
	}

	// 3. 通过 conversationId 查找
	proj3, conv3, prev3, _, _ := sessionChainLookup("", "", "conv-uuid-1")
	if proj3 != "proj-uuid-1" || conv3 != "conv-uuid-1" || prev3 != "resp_12345" {
		t.Fatalf("通过 conversationId 查找失败: proj=%s conv=%s prev=%s", proj3, conv3, prev3)
	}
}

func TestGenerateLocalTitle(t *testing.T) {
	raw := json.RawMessage(`[
		{"role":"system","content":"You are a coding agent"},
		{"role":"user","content":"# AGENTS.md instructions\n\n<INSTRUCTIONS>..."},
		{"role":"user","content":"用 HTML 实现一个 SVG，绘制鹈鹕骑自行车的场景，输出到本地文件空间\n\n[LOCAL_EXECUTION_REMINDER]..."},
		{"role":"user","content":"Generate a concise, single-line task title of at most 36 characters for the following task:"}
	]`)
	got := generateLocalTitle(raw)
	if !strings.Contains(got, "鹈鹕骑自行车") {
		t.Fatalf("预期从用户任务提取标题，实际得到: %s", got)
	}
	var parsed struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil || parsed.Title == "" {
		t.Fatalf("生成的标题必须是合法 JSON 且包含 title 字段: %s", got)
	}
}

func TestSplitOversizedPowerShellCommands(t *testing.T) {
	// 1. 小内容不拆分
	shortJS := `const out = await tools.exec_command({ cmd: "$c = @'\nsmall\n'@; Set-Content -LiteralPath 'test.html' -Value $c -NoNewline" });`
	if got := splitOversizedPowerShellCommands(shortJS); got != shortJS {
		t.Fatalf("短脚本不应被修改: %s", got)
	}

	// 2. 超长（>25KB）自动分块
	hugeContent := strings.Repeat("<div>SVG content</div>\n", 2000) // ~46,000 字符
	longJS := fmt.Sprintf("const out = await tools.exec_command({ cmd: `$c = @'\n%s\n'@; Set-Content -LiteralPath 'pelican-bicycle.html' -Value $c -NoNewline -Encoding UTF8` });", hugeContent)
	got := splitOversizedPowerShellCommands(longJS)
	if !strings.Contains(got, "Set-Content") || !strings.Contains(got, "Add-Content") {
		t.Fatalf("超长脚本必须被拆分为 Set-Content + Add-Content: %s", got[:200])
	}
	if !strings.Contains(got, "pelican-bicycle.html") {
		t.Fatalf("拆分后应保留目标路径: %s", got[:200])
	}
	if strings.Count(got, "await tools.exec_command") < 2 {
		t.Fatalf("超长脚本应被拆分为多个 exec_command: %s", got)
	}
}
