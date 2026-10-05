package facade

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/prism"
)

func deltaFile(path, status string, diff any) prism.CodexDeltaFile {
	raw, _ := json.Marshal(diff)
	return prism.CodexDeltaFile{FilePath: path, Status: status, Diff: raw}
}

// 2026-10-05 实测形状：{version, hunks:[{original, updated}]}，新增文件是一个 original 为空的块。
func TestRenderFileAttachments(t *testing.T) {
	svg := "<svg xmlns=\"http://www.w3.org/2000/svg\"><text>```</text></svg>\n"
	out := renderFileAttachments([]prism.CodexDeltaFile{
		deltaFile("AGENTS.md", "added", map[string]any{"version": 1, "hunks": []any{map[string]any{"original": "", "updated": "Prism 的说明"}}}),
		deltaFile("cloud_qin_29.svg", "added", map[string]any{"version": 1, "hunks": []any{map[string]any{"original": "", "updated": svg}}}),
		deltaFile("notes.txt", "modified", map[string]any{"version": 1, "hunks": []any{map[string]any{
			"original": "旧的一行\n", "updated": "新的一行\n", "location": map[string]any{"originalStartLine": 3, "originalLineCount": 1}}}}),
		deltaFile("tmp.log", "deleted", nil),
	})
	for _, want := range []string{
		"**生成的文件**",
		"`cloud_qin_29.svg`（" + formatSize(len(svg)) + "）\n\n````svg\n<svg",
		"</svg>\n````\n",
		"`notes.txt`（修改 1 处）\n\n```diff\n-旧的一行\n+新的一行\n```",
		"`tmp.log`（已删除）",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "AGENTS.md") || strings.Contains(out, "Prism 的说明") {
		t.Errorf("Prism 自己写进工作区的 AGENTS.md 不该附上:\n%s", out)
	}
	if renderFileAttachments([]prism.CodexDeltaFile{
		deltaFile("AGENTS.md", "added", map[string]any{"version": 1, "hunks": []any{map[string]any{"original": "", "updated": "x"}}}),
	}) != "" {
		t.Error("只有系统文件时不该附任何内容")
	}
}

func TestRenderFileAttachments_TooLarge(t *testing.T) {
	big := strings.Repeat("a", attachFileMax+1)
	out := renderFileAttachments([]prism.CodexDeltaFile{
		deltaFile("big.svg", "added", map[string]any{"version": 1, "hunks": []any{map[string]any{"original": "", "updated": big}}}),
	})
	if !strings.Contains(out, "`big.svg`（1.0 MB，内容过大或不是文本，未附上）") || strings.Contains(out, "aaaa") {
		t.Fatalf("超大文件只列文件名:\n%.300s", out)
	}
}
