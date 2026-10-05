package facade

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
)

// codex-exec 块是 JS：code mode 的客户端（exec 是 custom 工具）把它原样放进 V8 跑。
// function 形态的客户端（exec_command 是 type=function，参数只有 {"cmd": …}）跑不了 JS，
// 网关得自己把命令取出来。早先靠字符串匹配取 cmd，String.raw`…`、拼接、数组 join、
// 变量引用都会取错 —— 2026-10-04 实测 `cmd: String.raw`$ErrorActionPreference = 'Stop' …``
// 被取成了 "Stop"，客户端执行了一条叫 Stop 的命令，重试一次又取成了 "stage"。
//
// 这里用真正的 JS 引擎（goja，Sentinel 签名已在用）把块跑一遍，工具换成只记录参数的桩：
// 取到的就是 JS 语义下的真实值。桩返回空串，所以依赖上一条命令输出的分支逻辑不会如实执行 ——
// 块里这种写法极少，命令本身取对才是关键。

// execCall 是块里的一次工具调用。
type execCall struct {
	Tool  string         // exec_command | apply_patch | 其他客户端工具（MCP 工具、tool_search、view_image …）
	Cmd   string         // exec_command 的命令
	Args  map[string]any // exec_command 除 cmd 外的参数（workdir 等）；其他工具是全部参数。原样转交
	Patch string         // apply_patch 的补丁正文
}

// reToolRef 找出块里用到的工具（tools.xxx）：exec_command / apply_patch 以外的也挂上记录参数的桩，
// function 形态下换成对应的 function_call（见 codex_tools.go）。
var reToolRef = regexp.MustCompile(`\btools\.([A-Za-z_$][A-Za-z0-9_$]*)`)

// looksLikeExecJS 判断块内容是 JS（而不是直接写进围栏的 shell 命令）。
// 只调 text() / ALL_TOOLS 的块也是 JS：早先只认 tools. 与 await，
// `text(JSON.stringify(ALL_TOOLS…))` 被当成 PowerShell 命令执行（2026-10-05 实测）。
func looksLikeExecJS(s string) bool {
	for _, sig := range []string{"tools.", "await ", "text(", "ALL_TOOLS"} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

const (
	execJSBudget = time.Second // 块里只有拼字符串的胶水代码，正常远用不到
	execJSMax    = 4 << 20
)

// evalExecJS 求值块内容，返回其中的工具调用（按调用顺序）。
// 不是合法 JS、调了桩以外的工具、超时等都返回错误，调用方退回文本提取。
func evalExecJS(js string) ([]execCall, error) {
	if len(js) > execJSMax {
		return nil, errors.New("块过大")
	}
	vm := goja.New()
	var calls []execCall
	str := func(v goja.Value) (string, bool) {
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			return "", false
		}
		s, ok := v.Export().(string)
		return s, ok
	}

	tools := vm.NewObject()
	_ = tools.Set("exec_command", func(c goja.FunctionCall) goja.Value {
		arg := c.Argument(0)
		call := execCall{Tool: "exec_command"}
		if s, ok := str(arg); ok {
			call.Cmd = s
		} else if obj, ok := arg.(*goja.Object); ok {
			for _, k := range obj.Keys() {
				v := obj.Get(k)
				switch k {
				case "cmd", "command":
					s, ok := str(v)
					if !ok {
						panic(vm.NewTypeError("exec_command: cmd 必须是字符串"))
					}
					call.Cmd = s
				default:
					if call.Args == nil {
						call.Args = map[string]any{}
					}
					call.Args[k] = v.Export()
				}
			}
		}
		if strings.TrimSpace(call.Cmd) == "" {
			panic(vm.NewTypeError("exec_command: 缺少 cmd"))
		}
		calls = append(calls, call)
		return vm.ToValue("")
	})
	_ = tools.Set("apply_patch", func(c goja.FunctionCall) goja.Value {
		arg := c.Argument(0)
		patch, ok := str(arg)
		if obj, isObj := arg.(*goja.Object); !ok && isObj {
			patch, ok = str(obj.Get("input"))
		}
		if !ok || strings.TrimSpace(patch) == "" {
			panic(vm.NewTypeError("apply_patch: 补丁必须是字符串"))
		}
		calls = append(calls, execCall{Tool: "apply_patch", Patch: patch})
		return vm.ToValue("")
	})
	for _, m := range reToolRef.FindAllStringSubmatch(js, -1) {
		name := m[1]
		if name == "exec_command" || name == "apply_patch" || tools.Get(name) != nil {
			continue
		}
		_ = tools.Set(name, func(c goja.FunctionCall) goja.Value {
			calls = append(calls, execCall{Tool: name, Args: toolArgs(vm, c.Argument(0))})
			// 桩的返回值像一个空的 MCP CallToolResult：块里读 .content 不至于抛错
			return vm.ToValue(map[string]any{"content": []any{map[string]any{"type": "text", "text": ""}}, "isError": false})
		})
	}
	_ = vm.Set("tools", tools)
	_ = vm.Set("ALL_TOOLS", vm.NewArray())

	noop := func(goja.FunctionCall) goja.Value { return goja.Undefined() }
	exitVal := vm.NewObject()
	_ = vm.Set("text", noop)
	_ = vm.Set("exit", func(goja.FunctionCall) goja.Value { panic(exitVal) })
	console := vm.NewObject()
	for _, k := range []string{"log", "info", "warn", "error"} {
		_ = console.Set(k, noop)
	}
	_ = vm.Set("console", console)

	dog := time.AfterFunc(execJSBudget, func() { vm.Interrupt("exec 块求值超时") })
	defer dog.Stop()
	// 块可以直接写顶层 await：包进 async 函数里跑。
	v, err := vm.RunString("(async () => {\n" + js + "\n})()")
	if err != nil {
		return nil, err
	}
	p, ok := v.Export().(*goja.Promise)
	if !ok {
		return nil, errors.New("求值结果不是 Promise")
	}
	switch p.State() {
	case goja.PromiseStateFulfilled:
	case goja.PromiseStateRejected:
		if r := p.Result(); r == nil || !r.SameAs(exitVal) {
			return nil, fmt.Errorf("块执行出错: %v", r)
		}
	default:
		return nil, errors.New("块在等待未完成的异步操作")
	}
	return calls, nil
}

// toolArgs 把桩收到的参数换成 JSON 对象：对象原样；JSON 字符串解析；其他字符串当 query / input。
func toolArgs(vm *goja.Runtime, v goja.Value) map[string]any {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return map[string]any{}
	}
	switch x := v.Export().(type) {
	case map[string]any:
		return x
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(x), &m) == nil && m != nil {
			return m
		}
		return map[string]any{"input": x}
	}
	b, err := json.Marshal(v.Export())
	var m map[string]any
	if err == nil && json.Unmarshal(b, &m) == nil && m != nil {
		return m
	}
	panic(vm.NewTypeError("工具参数必须是对象"))
}

// fnCall 是交给 function 形态客户端的一次调用。
type fnCall struct {
	name, namespace string
	args            string // arguments（JSON 文本）
	search          bool   // tool_search_call（arguments 是对象，不是字符串）
}

// functionCalls 把块内容换成 function 形态客户端的调用：exec_command / apply_patch 合成
// exec 工具的一条命令（见 functionCallArgs）；其余工具（MCP、tool_search、view_image …）
// 各成一条 function_call，MCP 工具按 ct 拆出 namespace。顺序与块里一致。
func functionCalls(block string, isWindows bool, execName string, ct *codexClientTools) []fnCall {
	t := strings.TrimSpace(block)
	execOnly := func(args []string) []fnCall {
		out := make([]fnCall, 0, len(args))
		for _, a := range args {
			out = append(out, fnCall{name: execName, args: a})
		}
		return out
	}
	// 模型偶尔直接给 {"cmd": "..."}
	if strings.HasPrefix(t, "{") && json.Valid([]byte(t)) {
		return execOnly([]string{toFunctionArguments(t)})
	}
	calls := []execCall{{Tool: "exec_command", Cmd: t}} // 不像 JS：整块就是一条 shell 命令
	if looksLikeExecJS(t) {
		c, err := evalExecJS(t)
		if err != nil || len(c) == 0 {
			return execOnly([]string{toFunctionArguments(ensureExecJS(rewriteApplyPatch(t, false, isWindows, true)))})
		}
		calls = c
	}
	var out []fnCall
	var run []execCall
	flush := func() {
		if len(run) > 0 {
			out = append(out, execOnly(execCallArgs(run, isWindows))...)
			run = nil
		}
	}
	for _, c := range calls {
		if c.Tool == "exec_command" || c.Tool == "apply_patch" {
			run = append(run, c)
			continue
		}
		flush()
		args := marshalNoEscape(c.Args)
		if c.Tool == "tool_search" {
			out = append(out, fnCall{search: true, args: args})
			continue
		}
		ns, name := ct.resolve(c.Tool)
		out = append(out, fnCall{name: name, namespace: ns, args: args})
	}
	flush()
	return out
}

// functionCallArgs 把要交给客户端的块内容换成 function 形态 exec_command 的参数（JSON），
// 只取 exec_command / apply_patch（其余工具见 functionCalls）。
func functionCallArgs(block string, isWindows bool) []string {
	var out []string
	for _, c := range functionCalls(block, isWindows, "exec_command", nil) {
		if c.name == "exec_command" && c.namespace == "" && !c.search {
			out = append(out, c.args)
		}
	}
	return out
}

// execCallArgs 把一串 exec_command / apply_patch 调用换成 exec 工具的参数。
//
// 多次调用合成一条命令顺序执行：一次回复里发多条 function_call，Codex 会并行执行
// exec_command（supports_parallel_tool_calls），先后顺序就没了。一般返回一条；Windows 上
// 整条命令超出命令行长度上限、压缩后仍放不下时才分段（见 fitPowerShell）。
func execCallArgs(calls []execCall, isWindows bool) []string {
	script, extra := callsToScript(calls, isWindows)
	cmds := []string{script}
	if isWindows {
		cmds = fitPowerShell(script)
	}
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		args := make(map[string]any, len(extra)+1)
		for k, v := range extra {
			args[k] = v
		}
		args["cmd"] = c
		b, err := json.Marshal(args)
		if err != nil {
			b, _ = json.Marshal(map[string]string{"cmd": c})
		}
		out = append(out, string(b))
	}
	return out
}

// callsToScript 把调用序列合成一条 shell 脚本；额外参数（workdir 等）取第一次调用的，
// 之后的调用若换了 workdir，就在它前面切目录。
func callsToScript(calls []execCall, isWindows bool) (string, map[string]any) {
	var parts []string
	var extra map[string]any
	baseWD := ""
	for i, c := range calls {
		cmd := c.Cmd
		if c.Tool == "apply_patch" {
			cmd = patchShellCommand(c.Patch, isWindows)
		} else if p, ok := shellApplyPatch(cmd); ok && isWindows {
			cmd = patchShellCommand(p, true) // PowerShell 没有 heredoc
		}
		wd, _ := c.Args["workdir"].(string)
		if i == 0 {
			extra, baseWD = c.Args, wd
		} else if wd != "" && wd != baseWD {
			if isWindows {
				cmd = "Set-Location -LiteralPath " + psQuote(wd) + "\n" + cmd
			} else {
				cmd = "cd " + shQuote(wd) + " && " + cmd
			}
		}
		parts = append(parts, cmd)
	}
	return strings.Join(parts, "\n"), extra
}

// patchShellCommand 把补丁变成 shell 命令：Windows 翻译成 PowerShell 文件操作；
// 其余系统用 apply_patch heredoc（Codex 自己拦截，或走它放进 PATH 的 apply_patch）。
func patchShellCommand(patch string, isWindows bool) string {
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	if !isWindows {
		term := "OAIPRISM_PATCH"
		for strings.Contains(patch, term) {
			term += "_"
		}
		return "apply_patch <<'" + term + "'\n" + patch + term
	}
	cmds, err := applyPatchPowerShell(patch)
	if err != nil {
		return "throw " + psQuote("oaiprism: 补丁无法解析，未修改任何文件："+err.Error())
	}
	return strings.Join(cmds, "\n")
}

// fitExecJS 处理 code mode（custom 工具）块里超长的 exec_command：Windows 上任一命令超出
// 命令行长度上限时，按求值结果重写成顺序调用，超长的命令换成压缩 / 分段形式。
// 没有超长命令（绝大多数情况）原样返回，块里的 JS 逻辑不受影响。
func fitExecJS(js string, isWindows bool) string {
	if !isWindows || len(js) <= psCmdLimit { // UTF-8 字节数不小于 UTF-16 单元数
		return js
	}
	calls, err := evalExecJS(js)
	if err != nil {
		return js
	}
	over := false
	for _, c := range calls {
		over = over || (c.Tool == "exec_command" && utf16Len(c.Cmd) > psCmdLimit)
	}
	if !over {
		return js
	}
	var sb strings.Builder
	for _, c := range calls {
		if c.Tool == "apply_patch" {
			sb.WriteString("text(await tools.apply_patch(")
			writeJSONString(&sb, c.Patch)
			sb.WriteString("));\n")
			continue
		}
		for _, part := range fitPowerShell(c.Cmd) {
			args := map[string]any{}
			for k, v := range c.Args {
				args[k] = v
			}
			args["cmd"] = part
			b, err := json.Marshal(args)
			if err != nil {
				return js
			}
			sb.WriteString("text(await tools.exec_command(")
			sb.Write(b)
			sb.WriteString("));\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

const (
	// psCmdLimit：单条命令的 UTF-16 长度上限。CreateProcess 命令行最多 32,767 个 UTF-16 单元，
	// 还要放 pwsh.exe 的路径与参数，留足余量。
	psCmdLimit = 24000
	// psStageChunk：分段传输时每段的 Base64 字符数。
	psStageChunk = 20000
)

// fitPowerShell 让一条 PowerShell 命令放得进 Windows 命令行：
//   - 不超长：原样；
//   - 超长：gzip + Base64 后在客户端解压、点源执行（作用域与直接执行一致）。代码与 HTML
//     一般压到三分之一以下，36 KB 的整页 HTML 一条就放得下；
//   - 压缩后仍超长：分段。每段先写进临时目录，最后到齐的那一段负责拼接执行 ——
//     各段被并行执行（Codex 并行跑 exec_command）还是顺序执行，结果都一样。
func fitPowerShell(script string) []string {
	n := utf16Len(script)
	if n <= psCmdLimit {
		return []string{script}
	}
	b64 := gzipBase64(script)
	head := fmt.Sprintf("# oaiprism: 原命令 %d 字符，超出 Windows 命令行长度上限，压缩后传输\n", n)
	if one := head + "$__oaiprism=" + psQuote(b64) + "\n" + psRunCompressed; utf16Len(one) <= psCmdLimit {
		return []string{one}
	}

	var chunks []string
	for len(b64) > 0 {
		k := min(psStageChunk, len(b64))
		chunks = append(chunks, b64[:k])
		b64 = b64[k:]
	}
	id := make([]byte, 6)
	_, _ = rand.Read(id)
	dir := "oaiprism-" + hex.EncodeToString(id)
	total := len(chunks)
	out := make([]string, total)
	for i, c := range chunks {
		// 输出用 ASCII：管道里的 PowerShell 按控制台代码页编码，中文可能变乱码
		staged := psQuote(fmt.Sprintf("oaiprism: staged part %d/%d (the last part to arrive runs the command)", i+1, total))
		out[i] = fmt.Sprintf("# oaiprism: 原命令 %d 字符，压缩后仍超出 Windows 命令行长度上限，分 %d 段传输（第 %d 段）\n", n, total, i+1) +
			"$__d=Join-Path ([IO.Path]::GetTempPath()) " + psQuote(dir) + "; [void][IO.Directory]::CreateDirectory($__d)\n" +
			fmt.Sprintf("[IO.File]::WriteAllText((Join-Path $__d '%d.tmp'),'%s'); Move-Item -LiteralPath (Join-Path $__d '%d.tmp') -Destination (Join-Path $__d '%d.part') -Force\n", i, c, i, i) +
			fmt.Sprintf("if (@(Get-ChildItem -LiteralPath $__d -Filter '*.part').Count -lt %d) { %s; return }\n", total, staged) +
			"try { [IO.File]::Open((Join-Path $__d 'run.lock'),'CreateNew').Close() } catch { " + staged + "; return }\n" +
			fmt.Sprintf("$__oaiprism=-join (0..%d | ForEach-Object { [IO.File]::ReadAllText((Join-Path $__d \"$_.part\")) }); Remove-Item -LiteralPath $__d -Recurse -Force\n", total-1) +
			psRunCompressed
	}
	return out
}

// psRunCompressed 解压 $__oaiprism（gzip + Base64 的 UTF-8 脚本）并在当前作用域执行。
const psRunCompressed = "$__oaiprism=[IO.StreamReader]::new([IO.Compression.GZipStream]::new([IO.MemoryStream]::new([Convert]::FromBase64String($__oaiprism)),[IO.Compression.CompressionMode]::Decompress),[Text.Encoding]::UTF8).ReadToEnd()\n" +
	". ([ScriptBlock]::Create($__oaiprism))"

func gzipBase64(s string) string {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// utf16Len 是字符串的 UTF-16 长度（Windows 命令行按它计）。
func utf16Len(s string) int {
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}
