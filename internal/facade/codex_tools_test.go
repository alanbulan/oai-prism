package facade

import (
	"encoding/json"
	"strings"
	"testing"
)

// 三种 Codex 形态的工具声明（取自 2026-10-05 Codex 0.160 抓包，测试 MCP 服务器 probe-kit，有删节）。
const (
	codexDirectTools = `[
		{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
		{"type":"function","name":"view_image","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
		{"type":"namespace","name":"multi_agent_v1","description":"Tools for spawning and managing sub-agents.","tools":[{"type":"function","name":"spawn_agent"}]},
		{"type":"namespace","name":"mcp__probe_kit","description":"Tools in the mcp__probe_kit namespace.","tools":[
			{"type":"function","name":"add_numbers","description":"Add two numbers and return the sum.","strict":false,"parameters":{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}},
			{"type":"function","name":"lookup_codeword","description":"Look up the secret project codeword for a topic.","strict":false,"parameters":{"type":"object","properties":{"topic":{"type":"string","description":"Topic to look up"}},"required":["topic"]}}]},
		{"type":"web_search","external_web_access":true}
	]`
	codexSearchTools = `[
		{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
		{"type":"tool_search","execution":"client","description":"# Tool discovery\n\nSearches over deferred tool metadata.\n\nYou have access to tools from the following sources:\n- Multi-agent tools: Spawn and manage sub-agents.\n- probe-kit\nSome of the tools may not have been provided to you upfront.","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"]}}
	]`
)

func codexCodeModeInput(execDesc string) string {
	return `[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","description":"","tools":[
		{"type":"custom","name":"exec","description":` + mustJSONString(execDesc) + `}]}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`
}

func mustJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestParseCodexClientTools_Direct(t *testing.T) {
	ct := parseCodexClientTools(map[string]json.RawMessage{"tools": json.RawMessage(codexDirectTools), "input": json.RawMessage(`[]`)})
	if len(ct.mcp) != 2 || ct.deferred || ct.search {
		t.Fatalf("应认出两个 MCP 工具: %+v", ct)
	}
	if ns, name := ct.resolve("mcp__probe_kit__lookup_codeword"); ns != "mcp__probe_kit" || name != "lookup_codeword" {
		t.Fatalf("拆 namespace: %q %q", ns, name)
	}
	if len(ct.funcs) != 1 || ct.funcs[0] != "view_image" {
		t.Fatalf("其余 function 工具: %v", ct.funcs)
	}
	ct.function = true
	sec := ct.promptSection()
	for _, want := range []string{"tools.mcp__probe_kit__lookup_codeword", "topic (string, required)", "tools.mcp__probe_kit__add_numbers", "own block", "view_image"} {
		if !strings.Contains(sec, want) {
			t.Errorf("提示词缺 %q:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "spawn_agent") || strings.Contains(sec, "exec_command,") {
		t.Errorf("不该列出子代理与执行类工具:\n%s", sec)
	}
}

func TestParseCodexClientTools_ToolSearch(t *testing.T) {
	input := `[{"type":"tool_search_call","call_id":"s1","execution":"client","arguments":{"query":"codeword"}},
		{"type":"tool_search_output","call_id":"s1","status":"completed","execution":"client","tools":[
			{"type":"namespace","name":"mcp__probe_kit","description":"","tools":[{"type":"function","name":"lookup_codeword","description":"Look up the codeword.","defer_loading":true,"parameters":{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}}]}]}]`
	ct := parseCodexClientTools(map[string]json.RawMessage{"tools": json.RawMessage(codexSearchTools), "input": json.RawMessage(input)})
	if !ct.search || len(ct.sources) != 2 || ct.sources[1] != "probe-kit" {
		t.Fatalf("tool_search 与来源: %+v", ct)
	}
	if len(ct.mcp) != 1 || ct.mcp[0].Namespace != "mcp__probe_kit" {
		t.Fatalf("搜出来的工具应记下: %+v", ct.mcp)
	}
	sec := ct.promptSection()
	if !strings.Contains(sec, "tools.tool_search(") || !strings.Contains(sec, "probe-kit") {
		t.Fatalf("应教模型先搜索:\n%s", sec)
	}
	// 还没搜过的工具按名字拆
	if ns, name := (&codexClientTools{}).resolve("mcp__other_srv__do_it"); ns != "mcp__other_srv" || name != "do_it" {
		t.Fatalf("按名字拆: %q %q", ns, name)
	}
}

func TestParseCodexClientTools_CodeMode(t *testing.T) {
	desc := "Run JavaScript code\n" + codexDeferredMark + " from this description.\n\n### `apply_patch`\nEdit files.\n\n" +
		"### `mcp__docs__search`\nSearch the docs.\n\nexec tool declaration:\n```ts\ndeclare const tools: { mcp__docs__search(args: { query: string; }): Promise<CallToolResult>; };\n```\n\n## clock\n"
	raw := map[string]json.RawMessage{"input": json.RawMessage(codexCodeModeInput(desc))}
	ct := parseCodexClientTools(raw)
	if !ct.deferred || len(ct.mcp) != 1 || ct.mcp[0].Ident != "mcp__docs__search" || !strings.Contains(ct.mcp[0].Decl, "query: string") {
		t.Fatalf("code mode: %+v", ct)
	}
	sec := ct.promptSection()
	if !strings.Contains(sec, "ALL_TOOLS") || !strings.Contains(sec, "tools.mcp__docs__search") || strings.Contains(sec, "own block") {
		t.Fatalf("code mode 提示词:\n%s", sec)
	}
	// 没有 MCP 也没有延迟加载：不加这一段（system 不变）
	if s := parseCodexClientTools(map[string]json.RawMessage{"input": json.RawMessage(codexCodeModeInput("Run JS"))}).promptSection(); s != "" {
		t.Fatalf("没有额外工具时不该有这一段: %q", s)
	}
}

// function 形态：块里的 MCP 调用、tool_search、view_image 各成一条调用，exec_command 照旧合并。
func TestFunctionCalls_ClientTools(t *testing.T) {
	ct := parseCodexClientTools(map[string]json.RawMessage{"tools": json.RawMessage(codexDirectTools), "input": json.RawMessage(`[]`)})
	js := `const r = await tools.mcp__probe_kit__lookup_codeword({ topic: "alpha" });
text(r.content[0].text);
text(await tools.tool_search({ query: "codeword", limit: 3 }));
text(await tools.exec_command({ cmd: "Get-ChildItem" }));
text(await tools.exec_command({ cmd: "Get-Content -LiteralPath 'a.txt' -Raw" }));
text(await tools.view_image({ path: "shot.png" }));`
	calls := functionCalls(js, true, "exec_command", ct)
	if len(calls) != 4 {
		t.Fatalf("应是 MCP、搜索、合并的 exec、view_image 四条: %+v", calls)
	}
	if c := calls[0]; c.namespace != "mcp__probe_kit" || c.name != "lookup_codeword" || c.args != `{"topic":"alpha"}` {
		t.Fatalf("MCP 调用: %+v", c)
	}
	if c := calls[1]; !c.search || c.args != `{"limit":3,"query":"codeword"}` {
		t.Fatalf("tool_search: %+v", c)
	}
	var exec map[string]string
	_ = json.Unmarshal([]byte(calls[2].args), &exec)
	if calls[2].name != "exec_command" || !strings.Contains(exec["cmd"], "Get-ChildItem\nGet-Content") {
		t.Fatalf("exec 应合并成一条: %+v", calls[2])
	}
	if c := calls[3]; c.name != "view_image" || c.namespace != "" || c.args != `{"path":"shot.png"}` {
		t.Fatalf("view_image: %+v", c)
	}

	item := functionCallItemJSON("c1", calls[0].name, calls[0].args, calls[0].namespace)
	var m map[string]any
	if json.Unmarshal([]byte(item), &m) != nil || m["namespace"] != "mcp__probe_kit" || m["name"] != "lookup_codeword" {
		t.Fatalf("function_call 条目: %s", item)
	}
	if s := toolSearchCallItemJSON("c2", calls[1].args); !strings.Contains(s, `"type":"tool_search_call"`) || !strings.Contains(s, `"execution":"client"`) || !strings.Contains(s, `"arguments":{"limit":3`) {
		t.Fatalf("tool_search_call 条目: %s", s)
	}
}

// 历史里的 MCP 调用、搜索、带图片的输出按桥的写法回放给上游。
func TestBridgeInputItems_ClientToolHistory(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"找暗号"}]},
		{"type":"tool_search_call","call_id":"s1","execution":"client","arguments":{"query":"codeword","limit":8}},
		{"type":"tool_search_output","call_id":"s1","status":"completed","execution":"client","tools":[
			{"type":"namespace","name":"mcp__probe_kit","description":"","tools":[{"type":"function","name":"lookup_codeword","description":"Look up the codeword.","parameters":{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}}]}]},
		{"type":"function_call","call_id":"m1","namespace":"mcp__probe_kit","name":"lookup_codeword","arguments":"{\"topic\":\"alpha\"}"},
		{"type":"function_call_output","call_id":"m1","output":[{"type":"input_text","text":"codeword for alpha: ORCHID-5521"}]},
		{"type":"function_call","call_id":"v1","name":"view_image","arguments":"{\"path\":\"a.png\"}"},
		{"type":"function_call_output","call_id":"v1","output":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}
	]`)
	items := bridgeInputItems(raw, "")
	var all []string
	for _, it := range items[1:] {
		all = append(all, it.Role+": "+itemText(it))
	}
	text := strings.Join(all, "\n")
	for _, want := range []string{
		"text(await tools.tool_search({\"limit\":8,\"query\":\"codeword\"}));",
		"[CLIENT RESULT call_id=s1 tool=tool_search]",
		"tools.mcp__probe_kit__lookup_codeword",
		"text(await tools.mcp__probe_kit__lookup_codeword({\"topic\":\"alpha\"}));",
		"[CLIENT RESULT call_id=m1 tool=mcp__probe_kit__lookup_codeword]\ncodeword for alpha: ORCHID-5521",
		"[CLIENT RESULT call_id=v1 tool=view_image]\n[image attached to this message]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("缺 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "iVBOR") {
		t.Error("图片 base64 不该当成文字")
	}
	last := items[len(items)-1]
	if n := len(last.Content); n < 2 || last.Content[n-1].Type != "input_image" {
		t.Fatalf("图片应作为图片块附在结果后: %+v", last.Content)
	}
}

// 大的工具输出整段到上游（早先 6000 字符就截，读一个 15 KB 的源文件中间就没了）。
func TestBridgeInputItems_LargeOutputKept(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 800; i++ {
		sb.WriteString("def helper(x):\n    return x * 2  # padding padding\n")
	}
	body := sb.String() + "MAGIC_TOKEN = \"QUARTZ-7781\"\n" + sb.String()
	raw, _ := json.Marshal([]map[string]any{
		{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": `{"cmd":"Get-Content big.py -Raw"}`},
		{"type": "function_call_output", "call_id": "c1", "output": body},
	})
	items := bridgeInputItems(raw, "")
	got := itemText(items[len(items)-1])
	if len(body) < 60<<10 && !strings.Contains(got, "QUARTZ-7781") {
		t.Fatalf("%d 字节的输出应完整保留", len(body))
	}
	huge := strings.Repeat("line of output\n", 10000)
	raw, _ = json.Marshal([]map[string]any{{"type": "function_call_output", "call_id": "c2", "output": huge}})
	got = itemText(bridgeInputItems(raw, "")[1])
	if len(got) > codexResultMax+2000 || !strings.Contains(got, "omitted from the middle") {
		t.Fatalf("超过上限应截中间并说明（%d 字节）", len(got))
	}
}

func TestLooksLikeExecJS(t *testing.T) {
	for _, s := range []string{`text(JSON.stringify(ALL_TOOLS.map(t => t.name)))`, `await tools.exec_command({cmd:"ls"})`} {
		if !looksLikeExecJS(s) || ensureExecJS(s) != s {
			t.Errorf("%q 是 JS", s)
		}
	}
	if looksLikeExecJS("Get-ChildItem -Recurse") {
		t.Error("shell 命令不是 JS")
	}
}
