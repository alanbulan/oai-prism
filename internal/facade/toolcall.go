package facade

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 上游沙箱的文件变更（DeltaFiles）→ 客户端可安全落地的文件操作。
//
// 早期实现把 diff 里所有 "+" 与上下文行拼起来当"完整文件内容"整体覆盖写入：
//   - modified 文件的 diff 只含改动附近的几行，覆盖后整个文件只剩片段；
//   - 2026-10-02 起上游改发对象形态的 diff（{version, hunks:[{original, updated}]}），
//     旧解析器认不出，还原结果是空串 —— 用户文件被清空。
//
// 现在的规则：只有 diff 确实携带了整份内容（新增文件）才整体写入；
// 其余修改一律还原成"带上下文校验的替换块"，在客户端按原文定位后替换，
// 定位不到就报错、不落盘 —— 宁可不改，也不能改坏。

// fileEdit 是一个可落地的文件操作。
type fileEdit struct {
	Path    string     // 已校验的相对路径（正斜杠分隔）
	Kind    string     // editWrite | editPatch | editDelete
	Content string     // editWrite：完整文件内容
	Hunks   []editHunk // editPatch：按顺序应用的替换块
}

const (
	editWrite  = "write"
	editPatch  = "patch"
	editDelete = "delete"
)

// editHunk 是一个替换块：在原文件第 Line 行附近找到 Old，替换成 New。
// Old 为空表示纯插入：插在第 Line 行之后（Line=0 即文件开头）。
type editHunk struct {
	Old  string
	New  string
	Line int
}

var errIgnoredFile = errors.New("系统文件，不下发")

// safeRelPath 把上游给出的路径规范化为工作区内的相对路径。
//
// 上游路径由模型间接控制，必须当作不可信输入：拒绝绝对路径、盘符/UNC、
// 冒号（Windows 备用数据流）、控制字符，以及任何逃出工作区的 ".."。
func safeRelPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("空路径")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("路径含控制字符: %q", p)
		}
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return "", fmt.Errorf("拒绝绝对路径: %s", p)
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("路径越出工作区: %s", p)
	}
	return clean, nil
}

// planDeltaFile 把一个 DeltaFile 还原成可落地的文件操作。
func planDeltaFile(f prism.CodexDeltaFile) (fileEdit, error) {
	if isSystemIgnoredFile(f.FilePath) {
		return fileEdit{}, errIgnoredFile
	}
	rel, err := safeRelPath(f.FilePath)
	if err != nil {
		return fileEdit{}, err
	}
	if f.Status == "deleted" {
		return fileEdit{Path: rel, Kind: editDelete}, nil
	}

	hunks, err := parseDeltaHunks(f.Diff)
	if err != nil {
		return fileEdit{}, err
	}
	if len(hunks) == 0 {
		return fileEdit{}, errors.New("diff 为空")
	}

	// 整份内容：全部是纯插入且从文件开头起（新增文件的形态）。
	whole := true
	for _, h := range hunks {
		if h.Old != "" || h.Line != 0 {
			whole = false
			break
		}
	}
	if whole && (f.Status == "added" || len(hunks) == 1) {
		var sb strings.Builder
		for _, h := range hunks {
			sb.WriteString(h.New)
		}
		return fileEdit{Path: rel, Kind: editWrite, Content: sb.String()}, nil
	}
	if f.Status == "added" {
		return fileEdit{}, errors.New("新增文件的 diff 不含完整内容")
	}
	return fileEdit{Path: rel, Kind: editPatch, Hunks: hunks}, nil
}

// parseDeltaHunks 解析两种 diff 形态：统一 diff 字符串，或 {version, hunks:[{original, updated, location}]} 对象。
func parseDeltaHunks(raw json.RawMessage) ([]editHunk, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return parseUnifiedDiff(s)
	}
	var obj struct {
		Hunks []struct {
			Original string `json:"original"`
			Updated  string `json:"updated"`
			Location struct {
				OriginalStartLine int `json:"originalStartLine"`
				OriginalLineCount int `json:"originalLineCount"`
			} `json:"location"`
		} `json:"hunks"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Hunks == nil {
		return nil, errors.New("无法识别的 diff 形态")
	}
	out := make([]editHunk, 0, len(obj.Hunks))
	for _, h := range obj.Hunks {
		line := h.Location.OriginalStartLine
		if h.Original == "" && h.Location.OriginalLineCount == 0 {
			// 纯插入：originalStartLine 表示"插在这一行之后"。
			out = append(out, editHunk{New: h.Updated, Line: line})
			continue
		}
		out = append(out, editHunk{Old: h.Original, New: h.Updated, Line: line})
	}
	return out, nil
}

// parseUnifiedDiff 把统一 diff 解析成替换块：上下文行与 "-" 行组成 Old，上下文行与 "+" 行组成 New。
//
// 按 hunk 头里的行数计数确定每个 hunk 的边界：多文件 diff 中紧跟其后的
// "--- a/next" 文件头不会被误当成删除行。
func parseUnifiedDiff(diff string) ([]editHunk, error) {
	lines := strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n")
	var (
		out            []editHunk
		inHunk         bool
		oldStart       int
		oldCount       int
		remOld, remNew int
		lastKind       byte
		oldBuf, newBuf strings.Builder
	)
	flush := func() {
		if !inHunk {
			return
		}
		h := editHunk{Old: oldBuf.String(), New: newBuf.String(), Line: oldStart}
		if oldCount == 0 {
			h.Old = "" // 纯插入：插在 oldStart 行之后
		}
		out = append(out, h)
		inHunk = false
		oldBuf.Reset()
		newBuf.Reset()
	}
	trimLast := func(s string) string { return strings.TrimSuffix(s, "\n") }

	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			flush()
			var newCount int
			var err error
			oldStart, oldCount, newCount, err = parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			remOld, remNew = oldCount, newCount
			inHunk, lastKind = true, 0
			continue
		}
		// "\ No newline at end of file"：去掉上一行补上的换行（可能紧跟在已结束的 hunk 之后）。
		if strings.HasPrefix(line, `\`) {
			if inHunk {
				o, n := oldBuf.String(), newBuf.String()
				if lastKind == '-' || lastKind == ' ' {
					o = trimLast(o)
				}
				if lastKind == '+' || lastKind == ' ' {
					n = trimLast(n)
				}
				oldBuf.Reset()
				oldBuf.WriteString(o)
				newBuf.Reset()
				newBuf.WriteString(n)
			} else if len(out) > 0 {
				h := &out[len(out)-1]
				if lastKind == '-' || lastKind == ' ' {
					h.Old = trimLast(h.Old)
				}
				if lastKind == '+' || lastKind == ' ' {
					h.New = trimLast(h.New)
				}
			}
			continue
		}
		if !inHunk {
			continue // 文件头（diff --git / index / --- / +++）或 hunk 之间的杂项
		}
		switch {
		case strings.HasPrefix(line, "-") && remOld > 0:
			oldBuf.WriteString(line[1:] + "\n")
			remOld--
			lastKind = '-'
		case strings.HasPrefix(line, "+") && remNew > 0:
			newBuf.WriteString(line[1:] + "\n")
			remNew--
			lastKind = '+'
		case (strings.HasPrefix(line, " ") || line == "") && remOld > 0 && remNew > 0:
			// 有些生成器会把空上下文行的前导空格吃掉。
			ctx := strings.TrimPrefix(line, " ")
			oldBuf.WriteString(ctx + "\n")
			newBuf.WriteString(ctx + "\n")
			remOld--
			remNew--
			lastKind = ' '
		default:
			if line == "" {
				continue // 末尾换行产生的空串
			}
			return nil, fmt.Errorf("diff 行与 hunk 头的行数不符: %q", line)
		}
		if remOld == 0 && remNew == 0 {
			flush()
		}
	}
	if inHunk {
		return nil, errors.New("diff 被截断：hunk 行数不足")
	}
	if len(out) == 0 && strings.TrimSpace(diff) != "" {
		return nil, errors.New("diff 中没有可解析的 hunk")
	}
	return out, nil
}

// parseHunkHeader 解析 "@@ -a,b +c,d @@"，返回原文件起始行、原行数与新行数（省略行数即 1）。
func parseHunkHeader(h string) (oldStart, oldCount, newCount int, err error) {
	f := strings.Fields(h)
	if len(f) < 3 || !strings.HasPrefix(f[1], "-") || !strings.HasPrefix(f[2], "+") {
		return 0, 0, 0, fmt.Errorf("无法解析 hunk 头: %s", h)
	}
	parse := func(spec string) (int, int, bool) {
		start, count := spec, "1"
		if i := strings.IndexByte(spec, ','); i >= 0 {
			start, count = spec[:i], spec[i+1:]
		}
		a, err1 := strconv.Atoi(start)
		b, err2 := strconv.Atoi(count)
		return a, b, err1 == nil && err2 == nil
	}
	a, b, ok1 := parse(f[1][1:])
	_, d, ok2 := parse(f[2][1:])
	if !ok1 || !ok2 {
		return 0, 0, 0, fmt.Errorf("无法解析 hunk 头: %s", h)
	}
	return a, b, d, nil
}

// applyHunks 在文本上依次应用替换块（与客户端脚本的算法完全一致）。
//
// 定位规则：Old 在全文中的所有出现位置里，取起始行最接近预期行
// （hunk.Line 加上前面各块造成的行数偏移）的那一处；一处都没有就报错。
// 换行风格（CRLF/LF）按原文保留。
func applyHunks(text string, hunks []editHunk) (string, error) {
	crlf := strings.Contains(text, "\r\n")
	t := strings.ReplaceAll(text, "\r\n", "\n")
	shift := 0
	for _, h := range hunks {
		o := strings.ReplaceAll(h.Old, "\r\n", "\n")
		n := strings.ReplaceAll(h.New, "\r\n", "\n")
		want := h.Line + shift
		if o == "" {
			pos := 0
			for k := 0; k < want; k++ {
				nx := strings.IndexByte(t[pos:], '\n')
				if nx < 0 {
					pos = len(t)
					break
				}
				pos += nx + 1
			}
			t = t[:pos] + n + t[pos:]
		} else {
			best, bestDist := -1, 0
			for i := strings.Index(t, o); i >= 0; {
				ln := strings.Count(t[:i], "\n") + 1
				d := ln - want
				if d < 0 {
					d = -d
				}
				if best < 0 || d < bestDist {
					best, bestDist = i, d
				}
				nx := strings.Index(t[i+1:], o)
				if nx < 0 {
					break
				}
				i += nx + 1
			}
			if best < 0 {
				return "", errors.New("补丁上下文与本地文件不匹配")
			}
			t = t[:best] + n + t[best+len(o):]
		}
		shift += strings.Count(n, "\n") - strings.Count(o, "\n")
	}
	if crlf {
		t = strings.ReplaceAll(t, "\n", "\r\n")
	}
	return t, nil
}

// MapDeltaFilesToToolCalls 把上游沙箱内 CodexDeltaFile 转化为符合客户端期望的标准 ToolCalls。
//
// 修改类变更没有完整内容，只能交给客户端声明过的编辑工具（附 diff 与替换块）；
// 客户端只声明了写文件工具时跳过这类变更 —— 用片段整体覆盖会毁掉用户文件。
func MapDeltaFilesToToolCalls(files []prism.CodexDeltaFile, declaredTools []ChatTool) []ToolCall {
	if len(files) == 0 {
		return nil
	}

	// 探测客户端声明的文件编辑工具函数名称
	preferredWriteTool := "write_to_file"
	preferredEditTool := "edit_file"
	hasDeclaredEdit := false

	for _, t := range declaredTools {
		name := strings.ToLower(strings.TrimSpace(t.Function.Name))
		if name == "write_to_file" || name == "create_file" || name == "write_file" || name == "new_file" {
			preferredWriteTool = t.Function.Name
		} else if name == "edit_file" || name == "apply_diff" || name == "str_replace_editor" || name == "patch" || name == "modify_file" {
			preferredEditTool = t.Function.Name
			hasDeclaredEdit = true
		}
	}

	toolCalls := make([]ToolCall, 0, len(files))
	for _, f := range files {
		plan, err := planDeltaFile(f)
		if err != nil {
			continue
		}
		var fnName string
		argsMap := map[string]any{"path": plan.Path}
		switch plan.Kind {
		case editWrite:
			fnName = preferredWriteTool
			argsMap["content"] = plan.Content
		case editDelete:
			fnName = "delete_file"
		case editPatch:
			if !hasDeclaredEdit {
				continue
			}
			fnName = preferredEditTool
			argsMap["diff"] = f.DiffString()
			edits := make([]map[string]any, 0, len(plan.Hunks))
			for _, h := range plan.Hunks {
				edits = append(edits, map[string]any{"old_text": h.Old, "new_text": h.New, "line": h.Line})
			}
			argsMap["edits"] = edits
		}

		argsBytes, _ := json.Marshal(argsMap)
		idx := len(toolCalls)
		toolCalls = append(toolCalls, ToolCall{
			ID:    newCallID(),
			Type:  "function",
			Index: &idx,
			Function: ToolCallFunc{
				Name:      fnName,
				Arguments: string(argsBytes),
			},
		})
	}

	return toolCalls
}

// ApplyLocalWorkspaceFiles 直接将 DeltaFiles 落到网关所在机器的工作区目录。
//
// 只应在 facade.local_workspace_write 开启且请求来自本机时调用（见 chat.go）。
// 路径经 safeRelPath 校验，并用 filepath.Rel 二次确认落点在工作区内 ——
// 早期用字符串前缀判断，"<root>-evil" 这类兄弟目录能直接绕过。
// 返回第一个错误，但会尽量处理完其余文件。
func ApplyLocalWorkspaceFiles(workspaceRoot string, files []prism.CodexDeltaFile) error {
	if workspaceRoot == "" || len(files) == 0 {
		return nil
	}

	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return fmt.Errorf("无效的本地工作区路径: %w", err)
	}
	if err := ValidateWorkspaceExists(root); err != nil {
		return err
	}

	var firstErr error
	keep := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}
	for _, f := range files {
		plan, err := planDeltaFile(f)
		if errors.Is(err, errIgnoredFile) {
			continue
		}
		if err != nil {
			keep(fmt.Errorf("%s: %w", f.FilePath, err))
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(plan.Path))
		if rel, err := filepath.Rel(root, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			keep(fmt.Errorf("非法路径越界: %s", f.FilePath))
			continue
		}

		switch plan.Kind {
		case editDelete:
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				keep(err)
			}
		case editWrite:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				keep(fmt.Errorf("创建目录失败 (%s): %w", filepath.Dir(target), err))
				continue
			}
			if err := os.WriteFile(target, []byte(plan.Content), 0o644); err != nil {
				keep(fmt.Errorf("写入本地文件失败 (%s): %w", target, err))
			}
		case editPatch:
			cur, err := os.ReadFile(target)
			if err != nil {
				keep(fmt.Errorf("读取待修改文件失败 (%s): %w", target, err))
				continue
			}
			next, err := applyHunks(string(cur), plan.Hunks)
			if err != nil {
				keep(fmt.Errorf("%s: %w", plan.Path, err))
				continue
			}
			if err := os.WriteFile(target, []byte(next), 0o644); err != nil {
				keep(fmt.Errorf("写入本地文件失败 (%s): %w", target, err))
			}
		}
	}
	return firstErr
}

// newCallID 生成标准 OpenAI 工具调用句柄 (例如 call_1234567890abcdef)。
func newCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}

// ValidateWorkspaceExists 检验本地工作区路径是否存在且为目录。
func ValidateWorkspaceExists(workspaceRoot string) error {
	if workspaceRoot == "" {
		return errors.New("workspace root cannot be empty")
	}
	info, err := os.Stat(workspaceRoot)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是有效的目录", workspaceRoot)
	}
	return nil
}
