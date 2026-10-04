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
