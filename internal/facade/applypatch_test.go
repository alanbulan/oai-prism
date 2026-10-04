package facade

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/prism"
)

const samplePatch = "*** Begin Patch\n*** Add File: web/a.html\n+<p>`${x}` 中文</p>\n+second\n*** Update File: b.txt\n@@\n keep\n-old\n+new\n*** Delete File: c.txt\n*** End Patch\n"

// 2026-10-04 用户在 Windows 上遇到的原样：模型把 apply_patch 当 shell 命令，PowerShell 报语法错误。
func TestRewriteShellApplyPatch(t *testing.T) {
	heredoc := "apply_patch <<'PATCH'\n" + samplePatch + "PATCH"
	viaJS := `const out = await tools.exec_command({ cmd: ` + jsonString(heredoc) + ` });` + "\ntext(out);"

	for _, in := range []string{heredoc, viaJS, "apply_patch \"" + samplePatch + "\""} {
		got := rewriteApplyPatch(in, true, true, false)
		if !strings.HasPrefix(got, "const __out = await tools.apply_patch(") {
			t.Fatalf("有原生 apply_patch 时应改调它:\n%s", got)
		}
		arg := strings.TrimSuffix(strings.TrimPrefix(got, "const __out = await tools.apply_patch("), ");\ntext(__out);")
		var patch string
		if err := json.Unmarshal([]byte(arg), &patch); err != nil || patch != samplePatch {
			t.Fatalf("补丁正文应原样（JSON 转义保住反引号与 ${）: %q %v", patch, err)
		}
	}

	// 没有原生工具：Windows 翻译成 PowerShell 文件操作，bash 原样交给客户端拦截
	ps := rewriteApplyPatch(heredoc, false, true, false)
	if strings.Contains(ps, "<<") || strings.Count(ps, "tools.exec_command") != 3 || !strings.Contains(ps, "WriteAllBytes") || !strings.Contains(ps, "patched b.txt") || !strings.Contains(ps, "deleted c.txt") {
		t.Fatalf("Windows 应翻译成三条 PowerShell 命令:\n%s", ps)
	}
	if fn := rewriteApplyPatch(heredoc, true, true, true); strings.Contains(fn, "tools.") || strings.Count(fn, "\n") < 2 {
		t.Fatalf("function 形态的工具只能给裸命令:\n%s", fn)
	}
	if got := rewriteApplyPatch(heredoc, false, false, false); got != heredoc {
		t.Fatal("bash 客户端不该改写")
	}

	// 不相干的命令原样返回
	for _, in := range []string{"Get-ChildItem", `text(await tools.exec_command({ cmd: "ls" }));`, "apply_patch_helper --x", "echo apply_patch <<'X'"} {
		if got := rewriteApplyPatch(in, true, true, false); got != in {
			t.Fatalf("%q 不该被改写成 %q", in, got)
		}
	}
}

func TestParseApplyPatch(t *testing.T) {
	ops, err := parseApplyPatch(samplePatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 || ops[0].edit.Kind != editWrite || ops[0].edit.Content != "<p>`${x}` 中文</p>\nsecond\n" {
		t.Fatalf("Add File 解析不对: %+v", ops)
	}
	if h := ops[1].edit.Hunks; len(h) != 1 || h[0].Old != "keep\nold\n" || h[0].New != "keep\nnew\n" {
		t.Fatalf("Update File 解析不对: %+v", h)
	}
	if ops[2].edit.Kind != editDelete || ops[2].edit.Path != "c.txt" {
		t.Fatalf("Delete File 解析不对: %+v", ops[2])
	}
	mv, err := parseApplyPatch("*** Begin Patch\n*** Update File: a.txt\n*** Move to: dir/b.txt\n*** End Patch")
	if err != nil || mv[0].moveTo != "dir/b.txt" {
		t.Fatalf("Move to: %+v %v", mv, err)
	}
	for _, bad := range []string{
		"*** Begin Patch\n*** Add File: ../x\n+a\n*** End Patch",
		"*** Begin Patch\n*** Add File: C:/x\n+a\n*** End Patch",
		"*** Begin Patch\n*** Add File: a\nno plus\n*** End Patch",
		"*** Begin Patch\n*** Update File: a\n*** End Patch",
		"*** Begin Patch\n*** End Patch",
	} {
		if _, err := parseApplyPatch(bad); err == nil {
			t.Fatalf("应拒绝: %q", bad)
		}
	}
}

func TestBridgePromptPatchRecipe(t *testing.T) {
	native := bridgePrompt(true)
	if !strings.Contains(native, "tools.apply_patch(patch)") || !strings.Contains(native, "NEVER run apply_patch as a shell command") {
		t.Fatal("有原生 apply_patch 时应推荐 tools.apply_patch")
	}
	if strings.Contains(native, "behaves identically on every OS") {
		t.Fatal("不能再说 heredoc 在各系统上都一样")
	}
	if legacy := bridgePrompt(false); !strings.Contains(legacy, "On Windows NEVER use apply_patch heredocs") {
		t.Fatal("没有原生工具时要说明 heredoc 只适用于 bash")
	}
	if !strings.Contains(native, "/prism-uploads/") {
		t.Fatal("要说明附件在 /prism-uploads/ 下、用内置工具打开")
	}
	raw := map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"additional_tools","tools":"declare const tools: { apply_patch(input: string): Promise<unknown>; };"}]`)}
	if !hasNativeApplyPatch(raw) || hasNativeApplyPatch(map[string]json.RawMessage{"input": json.RawMessage(`[]`)}) {
		t.Fatal("原生 apply_patch 检测不对")
	}
}

// Codex 的图片标签带本机路径，上游模型会拿它去沙箱里找文件。
func TestStripLocalImagePath(t *testing.T) {
	c := []prism.InputContent{
		{Type: prism.BlockInputText, Text: `<image name=[Image #1] path="F:\Media\Pictures\ChatGPT Image 2026年9月9日 08_44_31.png">`},
	}
	stripLocalImagePath(c)
	if c[0].Text != `<image name=[Image #1]>` {
		t.Fatalf("得到 %q", c[0].Text)
	}
	other := []prism.InputContent{{Type: prism.BlockInputText, Text: `see path="x" here`}}
	stripLocalImagePath(other)
	if other[0].Text != `see path="x" here` {
		t.Fatal("只改 <image> 标签")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
