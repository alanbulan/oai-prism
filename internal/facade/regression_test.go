package facade

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
)

func diffRaw(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// ---------------------------- DeltaFiles → 文件操作 ----------------------------

// 修改类 diff 只含改动附近几行：绝不能被当成完整内容整体覆盖（旧实现会把文件截成片段）。
func TestPlanDeltaFile_PartialDiffBecomesPatch(t *testing.T) {
	f := prism.CodexDeltaFile{FilePath: "main.go", Status: "modified",
		Diff: diffRaw("--- a/main.go\n+++ b/main.go\n@@ -10,3 +10,3 @@\n line9\n-old\n+new\n line11\n")}
	plan, err := planDeltaFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != editPatch || len(plan.Hunks) != 1 {
		t.Fatalf("修改类变更应还原为补丁，得到 %+v", plan)
	}
	h := plan.Hunks[0]
	if h.Old != "line9\nold\nline11\n" || h.New != "line9\nnew\nline11\n" || h.Line != 10 {
		t.Fatalf("hunk 解析错误: %+v", h)
	}
}

// 2026-10-02 起的对象形态 diff：旧实现还原为空串、把用户文件清空。
func TestPlanDeltaFile_ObjectForm(t *testing.T) {
	added := prism.CodexDeltaFile{FilePath: "a.html", Status: "added",
		Diff: json.RawMessage(`{"version":1,"hunks":[{"original":"","updated":"<h1>hi</h1>\n","location":{"originalStartLine":0,"originalLineCount":0,"updatedStartLine":1,"updatedLineCount":1}}]}`)}
	plan, err := planDeltaFile(added)
	if err != nil || plan.Kind != editWrite || plan.Content != "<h1>hi</h1>\n" {
		t.Fatalf("对象形态新增文件应整体写入，得到 %+v err=%v", plan, err)
	}

	modified := prism.CodexDeltaFile{FilePath: "a.html", Status: "modified",
		Diff: json.RawMessage(`{"version":1,"hunks":[{"original":"<h1>hi</h1>\n","updated":"<h1>hello</h1>\n","location":{"originalStartLine":3,"originalLineCount":1,"updatedStartLine":3,"updatedLineCount":1}}]}`)}
	plan, err = planDeltaFile(modified)
	if err != nil || plan.Kind != editPatch || plan.Hunks[0].Old != "<h1>hi</h1>\n" || plan.Hunks[0].Line != 3 {
		t.Fatalf("对象形态修改应还原为补丁，得到 %+v err=%v", plan, err)
	}

	// 新增文件的 diff 却带上下文：不完整，必须拒绝而不是写片段。
	bad := prism.CodexDeltaFile{FilePath: "b.txt", Status: "added", Diff: diffRaw("@@ -3,2 +3,3 @@\n ctx\n+x\n ctx2\n")}
	if _, err := planDeltaFile(bad); err == nil {
		t.Fatal("不完整的新增文件 diff 应当报错")
	}
}

func TestPlanDeltaFile_RejectsUnsafePaths(t *testing.T) {
	for _, p := range []string{"../x.txt", "/etc/passwd", `C:\Windows\x.dll`, `\\server\share\x`, "a/../../x", "file.txt:stream", "a\x00b"} {
		f := prism.CodexDeltaFile{FilePath: p, Status: "added", Diff: diffRaw("@@ -0,0 +1 @@\n+x\n")}
		if _, err := planDeltaFile(f); err == nil {
			t.Errorf("不安全路径 %q 应被拒绝", p)
		}
	}
	f := prism.CodexDeltaFile{FilePath: "./sub/../ok.txt", Status: "added", Diff: diffRaw("@@ -0,0 +1 @@\n+x\n")}
	if plan, err := planDeltaFile(f); err != nil || plan.Path != "ok.txt" {
		t.Fatalf("工作区内路径应规范化通过，得到 %+v err=%v", plan, err)
	}
}

func TestApplyHunks(t *testing.T) {
	src := "a\r\nb\r\nc\r\nb\r\nd\r\n"
	// 第二个 b（第 4 行）才是目标：取最接近预期行的出现位置。
	got, err := applyHunks(src, []editHunk{{Old: "b\nd\n", New: "B\nd\n", Line: 4}})
	if err != nil || got != "a\r\nb\r\nc\r\nB\r\nd\r\n" {
		t.Fatalf("got %q err=%v", got, err)
	}
	got, err = applyHunks(src, []editHunk{{Old: "b\n", New: "X\n", Line: 4}})
	if err != nil || got != "a\r\nb\r\nc\r\nX\r\nd\r\n" {
		t.Fatalf("应按行号选择第二处出现: %q err=%v", got, err)
	}
	if _, err := applyHunks(src, []editHunk{{Old: "zzz\n", New: "y\n", Line: 1}}); err == nil {
		t.Fatal("上下文不匹配必须报错")
	}
	got, err = applyHunks("1\n2\n", []editHunk{{New: "ins\n", Line: 1}})
	if err != nil || got != "1\nins\n2\n" {
		t.Fatalf("纯插入应插在第 1 行之后: %q err=%v", got, err)
	}
}

// 文件名里的单引号曾能闭合引号、在用户机器上执行任意命令。
func TestSynthesize_QuotesPaths(t *testing.T) {
	f := prism.CodexDeltaFile{FilePath: "x'; calc; '.txt", Status: "deleted"}
	win := SynthesizeDeltaFilesExecJS([]prism.CodexDeltaFile{f}, true)
	if !strings.Contains(win, `'x''; calc; ''.txt'`) {
		t.Fatalf("PowerShell 单引号未转义: %s", win)
	}
	posix := SynthesizeDeltaFilesExecJS([]prism.CodexDeltaFile{f}, false)
	if !strings.Contains(posix, `'x'\\''; calc; '\\''.txt'`) {
		t.Fatalf("POSIX 单引号未转义: %s", posix)
	}
	escape := prism.CodexDeltaFile{FilePath: "../../.bashrc", Status: "added", Diff: diffRaw("@@ -0,0 +1 @@\n+evil\n")}
	js := SynthesizeDeltaFilesExecJS([]prism.CodexDeltaFile{escape}, false)
	if strings.Contains(js, "exec_command") || !strings.Contains(js, "跳过") {
		t.Fatalf("越界路径不应生成命令，应给出说明: %s", js)
	}
}

var reExecCmd = regexp.MustCompile(`exec_command\(\{ cmd: ("(?:[^"\\]|\\.)*") \}\)`)

func execCmds(t *testing.T, js string) []string {
	t.Helper()
	var out []string
	for _, m := range reExecCmd.FindAllStringSubmatch(js, -1) {
		var s string
		if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
			t.Fatalf("cmd 不是合法 JSON 字符串: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// 真正执行生成的脚本，验证它与 applyHunks 逐字节一致（含 CRLF、BOM 保留与失败不落盘）。
func TestSynthesize_ScriptsReallyWork(t *testing.T) {
	type shell struct {
		name    string
		windows bool
		argv    func(cmd string) []string
	}
	var shells []shell
	if runtime.GOOS == "windows" {
		for _, bin := range []string{"pwsh", "powershell"} {
			if p, err := exec.LookPath(bin); err == nil {
				p := p
				shells = append(shells, shell{bin, true, func(c string) []string { return []string{p, "-NoProfile", "-NonInteractive", "-Command", c} }})
			}
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		if _, err := exec.LookPath("python3"); err == nil {
			shells = append(shells, shell{"bash", false, func(c string) []string { return []string{p, "-c", c} }})
		}
	}
	if len(shells) == 0 {
		t.Skip("没有可用的 shell")
	}

	orig := "\xef\xbb\xbfpackage main\r\n\r\nfunc a() {\r\n\treturn 1\r\n}\r\n\r\nfunc b() {\r\n\treturn 1\r\n}\r\n"
	patch := prism.CodexDeltaFile{FilePath: "sub/it's.go", Status: "modified",
		Diff: diffRaw("--- a\n+++ b\n@@ -7,3 +7,4 @@\n func b() {\n-\treturn 1\n+\t// two\n+\treturn 2\n }\n")}
	plan, err := planDeltaFile(patch)
	if err != nil {
		t.Fatal(err)
	}
	want, err := applyHunks(orig[3:], plan.Hunks)
	if err != nil {
		t.Fatal(err)
	}
	want = "\xef\xbb\xbf" + want

	newFile := prism.CodexDeltaFile{FilePath: "new dir/hello.html", Status: "added",
		Diff: diffRaw("@@ -0,0 +1,2 @@\n+<p>\"quotes\" & 'apostrophes' 中文</p>\n+$(rm -rf /) `x`\n")}
	bad := prism.CodexDeltaFile{FilePath: "bad.txt", Status: "modified",
		Diff: diffRaw("@@ -1,1 +1,1 @@\n-not present\n+x\n")}

	for _, sh := range shells {
		t.Run(sh.name, func(t *testing.T) {
			dir := t.TempDir()
			_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
			if err := os.WriteFile(filepath.Join(dir, "sub", "it's.go"), []byte(orig), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "bad.txt"), []byte("keep me\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run := func(f prism.CodexDeltaFile) error {
				for _, c := range execCmds(t, SynthesizeDeltaFilesExecJS([]prism.CodexDeltaFile{f}, sh.windows)) {
					argv := sh.argv(c)
					cmd := exec.Command(argv[0], argv[1:]...)
					cmd.Dir = dir
					if out, err := cmd.CombinedOutput(); err != nil {
						return &exec.ExitError{ProcessState: cmd.ProcessState, Stderr: out}
					}
				}
				return nil
			}
			if err := run(patch); err != nil {
				t.Fatalf("补丁脚本执行失败: %v %s", err, err.(*exec.ExitError).Stderr)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "sub", "it's.go"))
			if string(got) != want {
				t.Fatalf("补丁结果不一致\n got: %q\nwant: %q", got, want)
			}
			if err := run(newFile); err != nil {
				t.Fatalf("写入脚本执行失败: %v", err)
			}
			got, _ = os.ReadFile(filepath.Join(dir, "new dir", "hello.html"))
			if string(got) != "<p>\"quotes\" & 'apostrophes' 中文</p>\n$(rm -rf /) `x`\n" {
				t.Fatalf("新增文件内容不一致: %q", got)
			}
			if err := run(bad); err == nil {
				t.Fatal("上下文不匹配时脚本必须失败")
			}
			got, _ = os.ReadFile(filepath.Join(dir, "bad.txt"))
			if string(got) != "keep me\n" {
				t.Fatalf("失败的补丁不得改动文件: %q", got)
			}
		})
	}
}

// 前缀判断曾被 "<root>-evil" 兄弟目录绕过。
func TestApplyLocalWorkspaceFiles_Containment(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	_ = os.MkdirAll(root, 0o755)
	files := []prism.CodexDeltaFile{
		{FilePath: "../ws-evil/pwn.txt", Status: "added", Diff: diffRaw("@@ -0,0 +1 @@\n+pwned\n")},
		{FilePath: "ok.txt", Status: "added", Diff: diffRaw("@@ -0,0 +1 @@\n+fine\n")},
	}
	_ = ApplyLocalWorkspaceFiles(root, files)
	if _, err := os.Stat(filepath.Join(base, "ws-evil", "pwn.txt")); err == nil {
		t.Fatal("越界写入未被阻止")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "ok.txt")); string(b) != "fine\n" {
		t.Fatalf("合法文件应写入: %q", b)
	}
}

// ---------------------------- Windows 命令行超长 ----------------------------

func TestIsCommandTooLongError_NoFalsePositive(t *testing.T) {
	if isCommandTooLongError("-a---  2026/10/03  20:06   2060 index.html") {
		t.Fatal("含 2026/2060 的正常输出被误判为命令过长")
	}
	if !isCommandTooLongError("program not found (os error 206)") || !isCommandTooLongError("文件名或扩展名太长。") {
		t.Fatal("真实的命令过长错误未识别")
	}
}

func TestSplitOversized_LeavesMultiCallBlocksAlone(t *testing.T) {
	huge := strings.Repeat("x", 30000)
	js := "const a = await tools.exec_command({ cmd: \"$c = @'\n" + huge + "\n'@; Set-Content -LiteralPath 'a.txt' -Value $c\" });\n" +
		"const b = await tools.exec_command({ cmd: \"git add a.txt\" });"
	if got := splitOversizedPowerShellCommands(js); got != js {
		t.Fatal("含多次调用的块不应被改写（会丢掉其余命令）")
	}
	// 单次调用：拆分后内容逐字节不变，最后一块保留原命令的换行语义。
	// 末行含 '@ 但不在行首（行首的 '@ 本身就是结束符，不可能出现在合法内容里）。
	content := strings.Repeat("ab\r\n", 9000) + "x'@ not a terminator"
	one := "const out = await tools.exec_command({ cmd: " + jsonStr(t, "$c = @'\n"+content+"\n'@; Set-Content -LiteralPath 'big.txt' -Value $c") + " });"
	got := splitOversizedPowerShellCommands(one)
	cmds := execCmds(t, got)
	if len(cmds) < 2 {
		t.Fatalf("应拆分为多次调用: %d", len(cmds))
	}
	var rebuilt strings.Builder
	reChunk := regexp.MustCompile(strings.Replace(rePSHereWrite.String(), "Set-Content", "(?:Set|Add)-Content", 1))
	for i, c := range cmds {
		if (i == 0) != strings.Contains(c, "Set-Content") {
			t.Fatalf("第 %d 块动词错误（首块 Set-Content，其余 Add-Content）", i)
		}
		m := reChunk.FindStringSubmatch(c)
		if m == nil {
			t.Fatalf("第 %d 块不是合法的 here-string 写入: %q", i, c[:80])
		}
		rebuilt.WriteString(m[2])
		last := i == len(cmds)-1
		if strings.Contains(m[5], "-NoNewline") == last {
			t.Fatalf("第 %d 块 -NoNewline 设置错误: %q", i, m[5])
		}
	}
	if rebuilt.String() != content {
		t.Fatal("拆分后拼回的内容与原文不一致")
	}
}

func jsonStr(t *testing.T, s string) string {
	t.Helper()
	var sb strings.Builder
	writeJSONString(&sb, s)
	return sb.String()
}

// ---------------------------- 图片来源安全 ----------------------------

func TestExtractImageData_RejectsLocalAndInternal(t *testing.T) {
	self, _ := os.Executable()
	for _, u := range []string{self, "file:///" + filepath.ToSlash(self), "file://" + self, "../../secrets/accounts.json"} {
		if _, _, ok := extractImageData(context.Background(), u); ok {
			t.Errorf("本地路径 %q 不应被读取", u)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nfake"))
	}))
	defer srv.Close()
	if _, _, ok := extractImageData(context.Background(), srv.URL+"/x.png"); ok {
		t.Error("回环地址的图片链接不应被抓取（SSRF）")
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "192.168.1.1", "100.64.0.1", "::1", "fd00::1"} {
		if isPublicIP(net.ParseIP(ip)) {
			t.Errorf("%s 不应被视为公网地址", ip)
		}
	}
	if !isPublicIP(net.ParseIP("8.8.8.8")) {
		t.Error("8.8.8.8 应为公网地址")
	}
	// data URI 仍然可用，但内容必须真是图片。
	if _, _, ok := extractImageData(context.Background(), "data:image/png;base64,aGVsbG8="); ok {
		t.Error("非图片内容的 data URI 不应通过")
	}
}

// ---------------------------- 会话链隔离 ----------------------------

func TestSessionChain_TenantIsolationAndWeakKeys(t *testing.T) {
	ka := "k:aaaaaaaaaaaaaaaa|h:same-session"
	kb := "k:bbbbbbbbbbbbbbbb|h:same-session"
	sessionChainRecord(ka, &RunResult{ProjectID: "proj-A", ConversationID: "conv-A", ResponseID: "resp-A", AccountID: "acct"}, "m")
	sessionChainAppend(ka, "secret question", "secret answer")

	if p, _, _, _, _ := sessionChainLookup(kb, "", ""); p != "" {
		t.Fatalf("不同租户同名会话不应共享状态: %s", p)
	}
	if p, _, _, _, _ := sessionChainLookupT("k:bbbbbbbbbbbbbbbb", "", "resp-A", "conv-A"); p != "" {
		t.Fatalf("别名不应跨租户命中: %s", p)
	}
	if p, _, _, _, _ := sessionChainLookupT("k:aaaaaaaaaaaaaaaa", "", "resp-A", ""); p != "proj-A" {
		t.Fatalf("同租户应能凭回复句柄命中: %s", p)
	}
	if h := sessionChainHistory(kb); len(h) != 0 {
		t.Fatalf("不同租户不应读到历史: %v", h)
	}

	weak := "f:u:ABCDEF"
	sessionChainRecord(weak, &RunResult{ProjectID: "proj-W"}, "m")
	sessionChainAppend(weak, "q", "a")
	if h := sessionChainHistory(weak); len(h) != 0 {
		t.Fatalf("弱键不应注入历史（撞键即串话）: %v", h)
	}
}

// 会话键本身是 "cid:<id>" 时，重置曾把主条目当别名删掉，整段历史丢失。
func TestSessionChain_ResetKeepsPrimaryEntry(t *testing.T) {
	key := "cid:conv-reset-1"
	sessionChainRecord(key, &RunResult{ConversationID: "conv-reset-1", ResponseID: "resp-r1"}, "m")
	sessionChainAppend(key, "hello", "world")
	sessionChainResetSession(key, false)
	if h := sessionChainHistory(key); len(h) != 2 {
		t.Fatalf("重置续接句柄不应丢失历史: %v", h)
	}
	if _, _, prev, _, _ := sessionChainLookup(key, "", ""); prev != "" {
		t.Fatalf("重置后上一轮句柄应清空: %s", prev)
	}
}

func TestSessionChain_AliasesBounded(t *testing.T) {
	key := "h:alias-cap"
	for i := 0; i < chainMaxAliases*3; i++ {
		sessionChainRecord(key, &RunResult{ResponseID: "resp-cap-" + string(rune('a'+i%26)) + strings.Repeat("x", i)}, "m")
	}
	sessionChain.mu.RLock()
	n := len(sessionChain.entries[key].aliases)
	sessionChain.mu.RUnlock()
	if n > chainMaxAliases {
		t.Fatalf("别名数量 %d 超过上限 %d", n, chainMaxAliases)
	}
}

// 只记录终态 ResponseID：RequestID 当续接键会被上游静默忽略。
func TestSessionChain_NoRequestIDFallback(t *testing.T) {
	key := "h:no-reqid"
	sessionChainRecord(key, &RunResult{RequestID: "req-123"}, "m")
	if _, _, prev, _, _ := sessionChainLookup(key, "", ""); prev != "" {
		t.Fatalf("RequestID 不应被记为上一轮句柄: %s", prev)
	}
	if resReqID(&RunResult{RequestID: "req-1"}) != "" {
		t.Fatal("resReqID 不应回落到 RequestID")
	}
}

func TestScopeKeyAddsTenant(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(HeaderSession, "s1")
	r = r.WithContext(middleware.WithTenant(r.Context(), "k:0123456789abcdef"))
	key := conversationKey(r, nil, nil)
	if key != "k:0123456789abcdef|h:s1" || !isStrongSessionKey(key) || tenantOfKey(key) != "k:0123456789abcdef" {
		t.Fatalf("租户前缀错误: %s", key)
	}
	if isStrongSessionKey("k:0123456789abcdef|f:u:XYZ") || isStrongSessionKey("u:alice") {
		t.Fatal("指纹与 user 字段应为弱键")
	}
}

// ---------------------------- 续接句柄与账号绑定 ----------------------------

func TestDropContinuation(t *testing.T) {
	req := &RunRequest{
		ProjectID: "p", ConversationID: "c", PreviousResponseID: "r", BoundAccountID: "acct-A",
		Metadata: map[string]any{"codex_listen_snapshot": "{}", "keep": 1},
	}
	req.MarkProjectFromChain()
	req.dropContinuation()
	if req.ProjectID != "" || req.ConversationID != "" || req.PreviousResponseID != "" || req.BoundAccountID != "" {
		t.Fatalf("续接句柄未清空: %+v", req)
	}
	if _, ok := req.Metadata["codex_listen_snapshot"]; ok || req.Metadata["keep"] != 1 {
		t.Fatalf("snapshot 应删除、其余 metadata 保留: %v", req.Metadata)
	}
	// 调用方显式指定的项目不受影响。
	req2 := &RunRequest{ProjectID: "explicit"}
	req2.dropContinuation()
	if req2.ProjectID != "explicit" {
		t.Fatal("显式指定的项目不应被清除")
	}
}

func TestFoldInputHistoryDoesNotMutateCaller(t *testing.T) {
	items := []prism.InputItem{prism.NewSystemItem("sys"), prism.NewUserItem("q1"), prism.NewAssistantItem("a1"), prism.NewUserItem("q2")}
	_ = foldInputHistory(items)
	if items[0].Content[0].Text != "sys" {
		t.Fatalf("foldInputHistory 改写了调用方的 input: %q", items[0].Content[0].Text)
	}
	_ = injectChainHistory(items, []ChatMessage{{Role: "user", Content: stringContent("x")}})
	if items[0].Content[0].Text != "sys" {
		t.Fatalf("injectChainHistory 改写了调用方的 input: %q", items[0].Content[0].Text)
	}
}

func TestIsCodexAuxRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("User-Agent", "openai-python/1.0")
	raw := map[string]json.RawMessage{"input": json.RawMessage(`"hi"`)}
	if isCodexAuxRequest(r, raw, false, false) {
		t.Fatal("普通 SDK 的短请求不应被当作伴生请求")
	}
	r.Header.Set("User-Agent", "codex_cli_rs/0.160.0 (Windows 10.0.26300; x86_64)")
	if !isCodexAuxRequest(r, raw, false, false) {
		t.Fatal("Codex 的无工具短请求应识别为伴生请求")
	}
	if isCodexAuxRequest(r, raw, false, true) {
		t.Fatal("带工具的请求不是伴生请求")
	}
}
