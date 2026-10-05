package facade

import (
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 上游模型以智能体方式干活时，成品常常写在沙箱的文件里，回答本身只有一句"已制作完成：x.svg"
// （2026-10-05：一张多约束 SVG 插画，192 KB 全在文件里，回答 332 字节）。
//
// 声明了工具的客户端由 MapDeltaFilesToToolCalls 转成工具调用落地；没声明任何工具的客户端
// （Dashboard 调试台、普通聊天客户端）执行不了工具调用 —— 按 OpenAI 语义也不该收到 ——
// 只能把文件附在回答后面，不然用户只看到一句话、拿不到成品。

const (
	attachFileMax  = 1 << 20 // 单个文件超过这个大小只列文件名
	attachTotalMax = 4 << 20 // 一次回答附带的内容总量上限
)

// chatFileAttachments 返回附在回答后面的 Markdown（没有可附的文件时为空）。
// 同一会话里已经附过、内容没变的文件不再重复附（上游可能在后续轮次重复报告）。
func chatFileAttachments(r *http.Request, res *RunResult) string {
	if res == nil || len(res.DeltaFiles) == 0 {
		return ""
	}
	files := res.DeltaFiles
	if res.ConversationID != "" {
		files = sessionChainFilterNewDeltaFiles(scopeKey(r, "chatfiles:"+res.ConversationID), files)
	}
	return renderFileAttachments(files)
}

func renderFileAttachments(files []prism.CodexDeltaFile) string {
	var plans []fileEdit
	for _, f := range files {
		if plan, err := planDeltaFile(f); err == nil {
			plans = append(plans, plan)
		}
	}
	// 能直接看的成品（svg / html）排在前面，生成它们的脚本之类排在后面
	sort.SliceStable(plans, func(i, j int) bool { return viewable(plans[i].Path) && !viewable(plans[j].Path) })

	var sb strings.Builder
	total := 0
	for _, plan := range plans {
		if sb.Len() == 0 {
			sb.WriteString("\n\n---\n\n**生成的文件**\n")
		}
		switch plan.Kind {
		case editDelete:
			fmt.Fprintf(&sb, "\n`%s`（已删除）\n", plan.Path)
		case editWrite:
			size := len(plan.Content)
			if size > attachFileMax || total+size > attachTotalMax || strings.ContainsRune(plan.Content, 0) {
				fmt.Fprintf(&sb, "\n`%s`（%s，内容过大或不是文本，未附上）\n", plan.Path, formatSize(size))
				continue
			}
			total += size
			fmt.Fprintf(&sb, "\n`%s`（%s）\n\n", plan.Path, formatSize(size))
			writeFence(&sb, fenceLang(plan.Path), plan.Content)
		case editPatch:
			var d strings.Builder
			for i, h := range plan.Hunks {
				if i > 0 {
					d.WriteString("@@\n")
				}
				for _, l := range diffLines(h.Old) {
					d.WriteString("-" + l + "\n")
				}
				for _, l := range diffLines(h.New) {
					d.WriteString("+" + l + "\n")
				}
			}
			if total+d.Len() > attachTotalMax {
				fmt.Fprintf(&sb, "\n`%s`（修改 %d 处，改动过大未附上）\n", plan.Path, len(plan.Hunks))
				continue
			}
			total += d.Len()
			fmt.Fprintf(&sb, "\n`%s`（修改 %d 处）\n\n", plan.Path, len(plan.Hunks))
			writeFence(&sb, "diff", d.String())
		}
	}
	return sb.String()
}

func viewable(p string) bool {
	switch fenceLang(p) {
	case "svg", "html":
		return true
	}
	return false
}

func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// writeFence 写一个代码块；围栏比内容里最长的一串反引号更长，内容里的 ``` 不会提前截断它。
func writeFence(sb *strings.Builder, lang, body string) {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	sb.WriteString(fence + lang + "\n" + strings.TrimSuffix(body, "\n") + "\n" + fence + "\n")
}

// fenceLang 按扩展名给代码块标语言：svg / html 在调试台里直接渲染成图。
func fenceLang(p string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
	switch ext {
	case "htm":
		return "html"
	case "py":
		return "python"
	case "js", "mjs", "cjs":
		return "javascript"
	case "ts":
		return "typescript"
	case "md":
		return "markdown"
	case "tex", "sty", "cls":
		return "latex"
	case "yml":
		return "yaml"
	case "sh":
		return "bash"
	}
	return ext
}

func formatSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
