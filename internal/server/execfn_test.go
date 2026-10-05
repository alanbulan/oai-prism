package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// function 形态的 exec_command（Codex 0.158 等）：参数只有 {"cmd": …}，网关得自己从 JS 块里取命令。
// 2026-10-04 用户实测：模型写 cmd: String.raw`$ErrorActionPreference = 'Stop' …`，
// 旧的文本提取取成了 "Stop"，客户端执行了一条叫 Stop 的命令。

const fnTools = `[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`

func fnBody(t *testing.T, stream bool) string {
	t.Helper()
	body := map[string]any{"model": "gpt-5", "stream": stream, "tools": json.RawMessage(fnTools),
		"input": []any{map[string]any{"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": "在当前文件夹里画一个鹈鹕骑自行车的 SVG 动画"}}}}}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var reFnArgs = regexp.MustCompile(`"type":"function_call"[^{}]*?"arguments":("(?:[^"\\]|\\.)*")`)

// fnCmds 取出回复里所有 function_call 的 cmd。
func fnCmds(t *testing.T, out string) []string {
	t.Helper()
	var cmds []string
	seen := map[string]bool{}
	for _, m := range reFnArgs.FindAllStringSubmatch(out, -1) {
		var args string
		if err := json.Unmarshal([]byte(m[1]), &args); err != nil {
			t.Fatal(err)
		}
		var a map[string]any
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			t.Fatal(err)
		}
		if c, _ := a["cmd"].(string); !seen[c] {
			seen[c] = true
			cmds = append(cmds, c)
		}
	}
	return cmds
}

func TestE2E_FunctionExecStringRaw(t *testing.T) {
	script := "$ErrorActionPreference = 'Stop'\n$c = @'\n<svg id=\"stage\"></svg>\n'@\nSet-Content -LiteralPath 'p.html' -Value $c\n" +
		"if (-not ((Get-Content -LiteralPath 'p.html' -Raw) -match '(?s)<svg\\b.*?</svg>')) { throw 'bad' }"
	reply := "```codex-exec\ntext(await tools.exec_command({cmd: String.raw`" + script + "`}));\n```"
	hdr := map[string]string{"Content-Type": "application/json", "User-Agent": "codex_cli_rs/0.158.0 (Windows 10.0.26300; x86_64)"}

	for _, stream := range []bool{true, false} {
		ts, _ := newTestServer(t, &fakeUpstream{t: t, replyParts: []string{reply}}, goodAccount(), nil)
		code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", fnBody(t, stream), hdr)
		if code != http.StatusOK {
			t.Fatalf("stream=%v 状态码 %d: %.300s", stream, code, out)
		}
		cmds := fnCmds(t, out)
		if len(cmds) != 1 || cmds[0] != script {
			t.Fatalf("stream=%v 命令应按 JS 语义原样取出: %q", stream, cmds)
		}
	}

	// 超出 Windows 命令行上限：压缩成一条，不再是会被系统拒绝（os error 206）的超长命令
	big := "$c = @'\n" + strings.Repeat("<circle r=\"42\" fill=\"#243e3e\"/><!-- 鹈鹕 -->\n", 1200) + "'@\nAdd-Content -LiteralPath 'p.html' -Value $c"
	reply = "```codex-exec\ntext(await tools.exec_command({cmd: String.raw`" + big + "`}));\n```"
	ts, _ := newTestServer(t, &fakeUpstream{t: t, replyParts: []string{reply}}, goodAccount(), nil)
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", fnBody(t, true), hdr)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d", code)
	}
	cmds := fnCmds(t, out)
	if len(cmds) != 1 || len(cmds[0]) > 24000 || !strings.Contains(cmds[0], "GZipStream") {
		t.Fatalf("超长命令应压缩成一条: %d 条，首条 %d 字节", len(cmds), len(cmds[0]))
	}
}

// function 形态下的 MCP 工具（Codex direct mode：tools 里是 namespace mcp__<服务器>）：上游在块里写
// tools.mcp__probe_kit__lookup_codeword(...)，网关换成带 namespace 的 function_call；桥提示词里列出了这个工具。
func TestE2E_FunctionMCPCall(t *testing.T) {
	tools := `[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
		{"type":"namespace","name":"mcp__probe_kit","description":"Tools in the mcp__probe_kit namespace.","tools":[
			{"type":"function","name":"lookup_codeword","description":"Look up the secret project codeword for a topic.","parameters":{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}}]}]`
	reply := "```codex-exec\ntext(await tools.mcp__probe_kit__lookup_codeword({ topic: \"alpha\" }));\n```"
	up := &fakeUpstream{t: t, replyParts: []string{reply}}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	body, _ := json.Marshal(map[string]any{"model": "gpt-5", "stream": true, "tools": json.RawMessage(tools),
		"input": []any{map[string]any{"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": "用 probe-kit 查 alpha 的暗号"}}}}})
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", string(body),
		map[string]string{"Content-Type": "application/json", "User-Agent": "codex_exec/0.160.0 (Windows 10.0.26300; x86_64)"})
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %.300s", code, out)
	}
	if !strings.Contains(out, `"type":"function_call","status":"completed","call_id":`) ||
		!strings.Contains(out, `"namespace":"mcp__probe_kit","name":"lookup_codeword","arguments":"{\"topic\":\"alpha\"}"`) {
		t.Fatalf("应回带 namespace 的 function_call: %.800s", out)
	}
	sys, _ := requireSystemUser(t, upstreamInput(t, up, 0))
	if !strings.Contains(sys, "CLIENT MCP TOOLS") || !strings.Contains(sys, "tools.mcp__probe_kit__lookup_codeword") {
		t.Fatalf("桥提示词应列出 MCP 工具: %.400s", sys)
	}
}

// 模型拿自带工具"看"了上游的远程容器（回答里是 /codex_workspace/…，没有 codex-exec 块）：网关在同一个
// 上游会话里纠正一次，客户端只拿到纠正后的调用；下一轮工具结果照常接在同一会话后面。
func TestE2E_BridgeCorrectsRemoteContainerRead(t *testing.T) {
	up := &fakeUpstream{t: t, replyQueue: []string{
		"Working directory: `/codex_workspace/2c86b923`, OS Linux. `src/app.py` does not exist.",
		"```codex-exec\ntext(await tools.exec_command({ cmd: \"Get-Content -LiteralPath 'src/app.py' -Raw -Encoding utf8\" }));\n```",
		"app.py 里定义了 main()",
	}}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json", "User-Agent": "codex_cli_rs/0.158.0 (Windows 10.0.26300; x86_64)",
		"X-Oaiprism-Session": "codex-remote-read-1"}
	user := map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "读一下 src/app.py 说说里面有什么"}}}
	body := func(input ...any) string {
		raw, _ := json.Marshal(map[string]any{"model": "gpt-5", "stream": true, "tools": json.RawMessage(fnTools), "input": input})
		return string(raw)
	}
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", body(user), hdr)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d", code)
	}
	if cmds := fnCmds(t, out); len(cmds) != 1 || !strings.Contains(cmds[0], "Get-Content -LiteralPath 'src/app.py'") {
		t.Fatalf("客户端应拿到纠正后的读文件调用: %q", cmds)
	}
	if strings.Contains(out, "codex_workspace") {
		t.Fatal("被纠正的回答不该到客户端")
	}
	if n := startCount(up); n != 2 || startConv(t, up, 1) != startConv(t, up, 0) {
		t.Fatalf("纠正应在同一个上游会话里多发一轮: %d 次", n)
	}
	if _, u := requireSystemUser(t, upstreamInput(t, up, 1)); !strings.Contains(u, "[SYSTEM CORRECTION]") {
		t.Fatalf("第二轮应是纠正消息: %q", u)
	}

	// 下一轮：客户端回传调用与结果，增量只有结果，不带"忽略最后几条"的说明
	callID := regexp.MustCompile(`"call_id":"([^"]+)"`).FindStringSubmatch(out)[1]
	args := mustJSON(t, map[string]string{"cmd": fnCmds(t, out)[0]})
	code, _ = doLocal(t, http.MethodPost, ts.URL+"/v1/responses", body(user,
		map[string]any{"type": "function_call", "name": "exec_command", "call_id": callID, "arguments": args},
		map[string]any{"type": "function_call_output", "call_id": callID, "output": "def main():\n    pass\n"}), hdr)
	if code != http.StatusOK || startConv(t, up, 2) != startConv(t, up, 0) {
		t.Fatalf("工具结果应接着同一个上游会话（%d）", code)
	}
	if _, u := requireSystemUser(t, upstreamInput(t, up, 2)); !strings.Contains(u, "def main()") || strings.Contains(u, "Disregard") || strings.Contains(u, "读一下") {
		t.Fatalf("增量应只是工具结果: %q", u)
	}
}

// 本地读过文件之后的最终回答顺带提到远程容器：不是误读，不纠正。
func TestE2E_BridgeNoCorrectionAfterClientResult(t *testing.T) {
	up := &fakeUpstream{t: t, replyQueue: []string{"app.py 定义了 main()（这是你本机的文件，不是远程的 /codex_workspace 容器）。"}}
	ts, _ := newTestServer(t, up, goodAccount(), nil)
	hdr := map[string]string{"Content-Type": "application/json", "X-Oaiprism-Session": "codex-remote-read-2"}
	raw, _ := json.Marshal(map[string]any{"model": "gpt-5", "stream": true, "tools": json.RawMessage(fnTools), "input": []any{
		map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "读一下 src/app.py"}}},
		map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": `{"cmd":"Get-Content src/app.py -Raw"}`},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "def main():\n    pass\n"},
	}})
	code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/responses", string(raw), hdr)
	if code != http.StatusOK || !strings.Contains(out, "app.py 定义了 main()") {
		t.Fatalf("应原样给出最终回答 (%d): %.300s", code, out)
	}
	if n := startCount(up); n != 1 {
		t.Fatalf("工具结果之后的回答不该触发纠正: %d 次", n)
	}
}
