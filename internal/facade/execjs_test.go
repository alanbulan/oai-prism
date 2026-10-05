package facade

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// bt 让测试里的 JS 能写反引号（Go 的原始字符串里放不进）。
func bt(s string) string { return strings.ReplaceAll(s, "‵", "`") }

// 2026-10-04 用户实测：cmd 写成 String.raw 模板，文本提取取成了 "Stop"。
const userScript = `$ErrorActionPreference = 'Stop'
$c = @'
<!DOCTYPE html>
<html lang="zh-CN"><body><svg id="stage"></svg>
<script>const stage = document.getElementById('stage'); const re = /\d+\.\d+/;</script>
</body></html>
'@
Add-Content -LiteralPath 'pelican-bike-3d.html' -Value $c -NoNewline -Encoding utf8
$html = Get-Content -LiteralPath 'pelican-bike-3d.html' -Raw -Encoding utf8
$match = [regex]::Match($html, '(?s)<svg\b.*?</svg>')
if (-not $match.Success) { throw '未找到 SVG 场景。' }
Write-Output 'SVG 结构验证通过。'`

func TestEvalExecJSStringRaw(t *testing.T) {
	js := bt("text(await tools.exec_command({cmd: String.raw‵" + userScript + "‵}));")
	calls, err := evalExecJS(js)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Cmd != userScript {
		t.Fatalf("String.raw 求值不对: %#v", calls)
	}
	if got, _ := singleExecCmd(js); got != userScript {
		t.Fatalf("singleExecCmd: %q", got)
	}
	// 旧的文本提取正是在这里取成了 "Stop"
	var args map[string]any
	if err := json.Unmarshal([]byte(functionCallArgs(js, true)[0]), &args); err != nil || args["cmd"] != userScript {
		t.Fatalf("function 参数不对: %v %v", args, err)
	}
}

func TestEvalExecJSForms(t *testing.T) {
	js := bt(`const dir = 'out';
const lines = ['a', "b"];
const out1 = await tools.exec_command({ cmd: 'mkdir ' + dir, workdir: 'C:\\w', max_output_tokens: 500 });
text(out1);
await tools.exec_command(‵echo ${lines.join(',')}‵);
const patch = ['*** Begin Patch', '*** Add File: x.txt', '+hi', '*** End Patch'].join('\n');
text(await tools.apply_patch(patch));
exit();
await tools.exec_command({ cmd: 'never' });`)
	calls, err := evalExecJS(js)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("应有 3 次调用（exit 之后的不算）: %#v", calls)
	}
	if calls[0].Cmd != "mkdir out" || calls[0].Args["workdir"] != `C:\w` || calls[0].Args["max_output_tokens"] != int64(500) {
		t.Fatalf("第 1 次: %#v", calls[0])
	}
	if calls[1].Cmd != "echo a,b" {
		t.Fatalf("模板插值: %q", calls[1].Cmd)
	}
	if calls[2].Tool != "apply_patch" || !strings.Contains(calls[2].Patch, "*** Add File: x.txt\n+hi") {
		t.Fatalf("apply_patch: %#v", calls[2])
	}
}

func TestEvalExecJSRejects(t *testing.T) {
	for name, js := range map[string]string{
		"语法错误":     "Get-ChildItem | Select-Object Name",
		"未定义变量":    "await tools.exec_command({ cmd: missing })",
		"cmd 非字符串": "await tools.exec_command({ cmd: 42 })",
	} {
		if _, err := evalExecJS(js); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
	start := time.Now()
	if _, err := evalExecJS("while (true) {}"); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("死循环应在预算内被打断: %v %v", err, time.Since(start))
	}
}

func TestFunctionCallArgsJoinsAndTranslates(t *testing.T) {
	cmdOf := func(args string) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(args), &m); err != nil {
			t.Fatal(err)
		}
		return m["cmd"].(string)
	}

	// 多次调用合成一条，顺序不变；第二次换了 workdir 要先切目录
	js := `await tools.exec_command({ cmd: 'mkdir a', workdir: 'C:\\w' });
await tools.exec_command({ cmd: 'dir', workdir: 'C:\\x' });`
	got := functionCallArgs(js, true)
	var m map[string]any
	_ = json.Unmarshal([]byte(got[0]), &m)
	if len(got) != 1 || m["cmd"] != "mkdir a\nSet-Location -LiteralPath 'C:\\x'\ndir" || m["workdir"] != `C:\w` {
		t.Fatalf("合并不对: %v", got)
	}

	// 裸命令、JSON 形式照旧
	if c := cmdOf(functionCallArgs("Get-ChildItem", true)[0]); c != "Get-ChildItem" {
		t.Fatalf("裸命令: %q", c)
	}
	if c := cmdOf(functionCallArgs(`{"command":"echo hi"}`, false)[0]); c != "echo hi" {
		t.Fatalf("JSON: %q", c)
	}

	// 补丁：Windows 翻译成 PowerShell，其余系统用 heredoc
	patch := "*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch"
	js = bt("await tools.apply_patch(String.raw‵" + patch + "‵)")
	if c := cmdOf(functionCallArgs(js, true)[0]); strings.Contains(c, "<<") || !strings.Contains(c, "WriteAllBytes") {
		t.Fatalf("Windows 补丁: %q", c)
	}
	if c := cmdOf(functionCallArgs(js, false)[0]); c != "apply_patch <<'OAIPRISM_PATCH'\n"+patch+"\nOAIPRISM_PATCH" {
		t.Fatalf("bash 补丁: %q", c)
	}
	shell := bt("await tools.exec_command({cmd: String.raw‵apply_patch <<'PATCH'\n" + patch + "\nPATCH‵})")
	if c := cmdOf(functionCallArgs(shell, true)[0]); strings.Contains(c, "<<") {
		t.Fatalf("shell 形式的补丁在 Windows 上应翻译: %q", c)
	}
	if c := cmdOf(functionCallArgs("await tools.apply_patch('garbage')", true)[0]); !strings.HasPrefix(c, "throw ") {
		t.Fatalf("解析不了的补丁应明确报错: %q", c)
	}
}

// bigHTML 生成压缩率接近真实页面的大文件内容。
func bigHTML(n int) string {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		sb.WriteString("<g class=\"wheel\" transform=\"rotate(" + strings.Repeat("7", i%5+1) + ")\"><circle r=\"42\" fill=\"#243e3e\"/></g><!-- 鹈鹕 -->\n")
	}
	return sb.String()
}

func randomText(n int) string {
	r := rand.New(rand.NewSource(1))
	const set = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	b := make([]byte, n)
	for i := range b {
		b[i] = set[r.Intn(len(set))]
	}
	return string(b)
}

var reB64 = regexp.MustCompile(`\$__oaiprism='([A-Za-z0-9+/=]+)'`)

func gunzipB64(t *testing.T, b64 string) string {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestFitPowerShell(t *testing.T) {
	small := "Get-ChildItem"
	if got := fitPowerShell(small); len(got) != 1 || got[0] != small {
		t.Fatalf("短命令应原样: %v", got)
	}

	script := "$c = @'\n" + bigHTML(60000) + "'@\nSet-Content -LiteralPath 'a.html' -Value $c"
	got := fitPowerShell(script)
	if len(got) != 1 || utf16Len(got[0]) > psCmdLimit {
		t.Fatalf("可压缩的大命令应压成一条: %d 条", len(got))
	}
	m := reB64.FindStringSubmatch(got[0])
	if m == nil || gunzipB64(t, m[1]) != script {
		t.Fatal("压缩内容解不回原命令")
	}

	noisy := "$c = @'\n" + randomText(70000) + "\n'@\nSet-Content -LiteralPath 'r.txt' -Value $c"
	parts := fitPowerShell(noisy)
	if len(parts) < 2 {
		t.Fatalf("压不下的命令应分段: %d", len(parts))
	}
	for i, p := range parts {
		if utf16Len(p) > psCmdLimit {
			t.Fatalf("第 %d 段超长: %d", i, utf16Len(p))
		}
	}
	if utf16Len("a😀b") != 4 {
		t.Fatal("utf16Len 应把代理对算两个单元")
	}
}

func TestFitExecJS(t *testing.T) {
	short := `await tools.exec_command({ cmd: "dir" })`
	if fitExecJS(short, true) != short {
		t.Fatal("没有超长命令时应原样")
	}
	big := bt("const r = await tools.exec_command({cmd: String.raw‵$c = @'\n" + bigHTML(40000) + "'@\nAdd-Content -LiteralPath 'a.html' -Value $c‵, workdir: 'C:\\\\w'});\ntext(r);")
	if fitExecJS(big, false) != big {
		t.Fatal("非 Windows 不处理")
	}
	got := fitExecJS(big, true)
	calls, err := evalExecJS(got)
	if err != nil || len(calls) != 1 || utf16Len(calls[0].Cmd) > psCmdLimit || calls[0].Args["workdir"] != `C:\w` {
		t.Fatalf("超长命令应改写成压缩形式: %v %#v", err, calls)
	}
}

// 在本机 PowerShell 里真跑一遍压缩与分段形式（CI 没有 pwsh 时跳过）。
func TestFitPowerShellRunsInPwsh(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("没有 pwsh")
	}
	run := func(dir, cmd string) string {
		c := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", cmd)
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("pwsh: %v\n%s", err, out)
		}
		return string(out)
	}
	check := func(dir, want string) {
		b, err := os.ReadFile(filepath.Join(dir, "out.txt"))
		if err != nil || string(b) != want {
			t.Fatalf("文件内容不对: %v (%d vs %d 字节)", err, len(b), len(want))
		}
	}
	script := func(content string) string {
		return "$ErrorActionPreference = 'Stop'\n$c = @'\n" + content + "\n'@\n" +
			"Set-Content -LiteralPath 'out.txt' -Value $c -NoNewline -Encoding utf8NoBOM\nWrite-Output ('done ' + $c.Length)"
	}

	// 压缩成一条
	dir := t.TempDir()
	content := bigHTML(50000)
	cmds := fitPowerShell(script(content))
	if len(cmds) != 1 {
		t.Fatalf("应压成一条: %d", len(cmds))
	}
	if out := run(dir, cmds[0]); !strings.Contains(out, "done ") {
		t.Fatalf("输出: %s", out)
	}
	check(dir, content)

	// 分段：顺序执行与并行执行都只跑一次、内容一致
	content = randomText(60000)
	parts := fitPowerShell(script(content))
	if len(parts) < 2 {
		t.Fatalf("应分段: %d", len(parts))
	}
	dir = t.TempDir()
	var outs []string
	for _, p := range parts {
		outs = append(outs, run(dir, p))
	}
	if !strings.Contains(outs[len(outs)-1], "done 60000") || !strings.Contains(outs[0], "staged part 1/") {
		t.Fatalf("顺序执行输出: %q", outs)
	}
	check(dir, content)

	parts = fitPowerShell(script(content)) // 新的临时目录名
	dir = t.TempDir()
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for _, p := range parts {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if strings.Contains(run(dir, p), "done 60000") {
				mu.Lock()
				done++
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	if done != 1 {
		t.Fatalf("并行执行应恰好执行一次脚本: %d", done)
	}
	check(dir, content)
}
