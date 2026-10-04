package facade

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 模型写文件时常把 apply_patch 当成 shell 命令：
//
//	apply_patch <<'PATCH'
//	*** Begin Patch
//	…
//	*** End Patch
//	PATCH
//
// 这在 bash 下由 Codex 拦截执行，但 Windows 的 exec_command 跑在 PowerShell 里 ——
// PowerShell 没有 heredoc，直接报"重定向运算符后缺少文件规范"。这里在交给客户端之前改写：
//   - 客户端有原生的 apply_patch（code mode 的 tools.apply_patch）：改成调用它，任何系统都可靠；
//   - 没有、且客户端是 Windows：把补丁翻译成 PowerShell 文件操作（复用上游文件变更落盘的那套命令）。

// hasNativeApplyPatch 判断客户端在 exec 脚本里提供了 tools.apply_patch（Codex code mode 的嵌套工具声明）。
func hasNativeApplyPatch(raw map[string]json.RawMessage) bool {
	hay := string(raw["input"]) + string(raw["tools"])
	return strings.Contains(hay, "apply_patch(input: string)")
}

var heredocTermRe = regexp.MustCompile(`^['"]?[A-Za-z_][A-Za-z0-9_]*['"]?$`)

// shellApplyPatch 从"把 apply_patch 当 shell 命令"的写法里取出补丁正文。
func shellApplyPatch(cmd string) (string, bool) {
	t := strings.TrimSpace(strings.ReplaceAll(cmd, "\r\n", "\n"))
	if !strings.HasPrefix(t, "apply_patch") {
		return "", false
	}
	const begin, end = "*** Begin Patch", "*** End Patch"
	b, e := strings.Index(t, begin), strings.LastIndex(t, end)
	if b < 0 || e < b {
		return "", false
	}
	// 补丁前只能是 heredoc 起始或引号，补丁后只能是 heredoc 结束符或引号
	head := strings.TrimSpace(strings.TrimPrefix(t[:b], "apply_patch"))
	if head != "" && !strings.HasPrefix(head, "<<") && head != `"` && head != "'" {
		return "", false
	}
	if tail := strings.TrimSpace(t[e+len(end):]); tail != "" && tail != `"` && tail != "'" && !heredocTermRe.MatchString(tail) {
		return "", false
	}
	return t[b:e+len(end)] + "\n", true
}

// rewriteApplyPatch 改写交给客户端的 exec 内容里"shell 形式的 apply_patch"；不涉及时原样返回。
// candidate 是 codex-exec 围栏内容（JS 或裸命令）。返回 JS（custom 工具）或裸命令（function 工具）。
func rewriteApplyPatch(candidate string, nativePatch, isWindows, functionTool bool) string {
	cmd := candidate
	if strings.Contains(candidate, "tools.") || strings.Contains(candidate, "await") {
		c, ok := singleExecCmd(candidate)
		if !ok {
			return candidate
		}
		cmd = c
	}
	patch, ok := shellApplyPatch(cmd)
	if !ok {
		return candidate
	}
	if nativePatch && !functionTool {
		var sb strings.Builder
		sb.WriteString("const __out = await tools.apply_patch(")
		writeJSONString(&sb, patch)
		sb.WriteString(");\ntext(__out);")
		return sb.String()
	}
	if !isWindows {
		return candidate // bash 下 Codex 自己会拦截 apply_patch heredoc
	}
	cmds, err := applyPatchPowerShell(patch)
	if err != nil {
		return candidate
	}
	if functionTool {
		return strings.Join(cmds, "\n")
	}
	var sb strings.Builder
	for i, c := range cmds {
		fmt.Fprintf(&sb, "const __out%d = await tools.exec_command({ cmd: ", i)
		writeJSONString(&sb, c)
		fmt.Fprintf(&sb, " });\ntext(__out%d);\n", i)
	}
	return strings.TrimSpace(sb.String())
}

// patchOp 是补丁里的一个文件操作。
type patchOp struct {
	edit   fileEdit
	moveTo string
}

// parseApplyPatch 解析 Codex 的补丁格式（*** Add / Delete / Update File，@@ 分隔的替换块）。
func parseApplyPatch(patch string) ([]patchOp, error) {
	lines := strings.Split(strings.TrimRight(patch, "\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "*** Begin Patch" || strings.TrimSpace(lines[len(lines)-1]) != "*** End Patch" {
		return nil, errors.New("补丁缺少 Begin/End 标记")
	}
	lines = lines[1 : len(lines)-1]
	var ops []patchOp
	var cur *patchOp
	var hunk *editHunk
	flushHunk := func() {
		if cur != nil && hunk != nil && (hunk.Old != "" || hunk.New != "") {
			cur.edit.Hunks = append(cur.edit.Hunks, *hunk)
		}
		hunk = nil
	}
	flush := func() {
		flushHunk()
		if cur != nil {
			ops = append(ops, *cur)
		}
		cur = nil
	}
	header := func(l, prefix string) (string, bool) {
		if !strings.HasPrefix(l, prefix) {
			return "", false
		}
		return strings.TrimSpace(strings.TrimPrefix(l, prefix)), true
	}
	for _, l := range lines {
		l = strings.TrimSuffix(l, "\r")
		if p, ok := header(l, "*** Add File: "); ok {
			flush()
			cur = &patchOp{edit: fileEdit{Path: p, Kind: editWrite}}
			continue
		}
		if p, ok := header(l, "*** Delete File: "); ok {
			flush()
			cur = &patchOp{edit: fileEdit{Path: p, Kind: editDelete}}
			continue
		}
		if p, ok := header(l, "*** Update File: "); ok {
			flush()
			cur = &patchOp{edit: fileEdit{Path: p, Kind: editPatch}}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("补丁行不属于任何文件: %q", l)
		}
		switch cur.edit.Kind {
		case editWrite:
			if !strings.HasPrefix(l, "+") {
				return nil, fmt.Errorf("新增文件的行必须以 + 开头: %q", l)
			}
			cur.edit.Content += l[1:] + "\n"
		case editDelete:
			if strings.TrimSpace(l) != "" {
				return nil, fmt.Errorf("删除文件后不该有内容: %q", l)
			}
		case editPatch:
			if p, ok := header(l, "*** Move to: "); ok {
				cur.moveTo = p
				continue
			}
			if l == "*** End of File" {
				continue
			}
			if strings.HasPrefix(l, "@@") {
				flushHunk()
				hunk = &editHunk{}
				continue
			}
			if hunk == nil {
				hunk = &editHunk{}
			}
			switch {
			case l == "" || strings.HasPrefix(l, " "):
				s := strings.TrimPrefix(l, " ") + "\n"
				hunk.Old += s
				hunk.New += s
			case strings.HasPrefix(l, "-"):
				hunk.Old += l[1:] + "\n"
			case strings.HasPrefix(l, "+"):
				hunk.New += l[1:] + "\n"
			default:
				return nil, fmt.Errorf("无法识别的补丁行: %q", l)
			}
		}
	}
	flush()
	if len(ops) == 0 {
		return nil, errors.New("补丁里没有文件操作")
	}
	for i := range ops {
		rel, err := safeRelPath(ops[i].edit.Path)
		if err != nil {
			return nil, err
		}
		ops[i].edit.Path = rel
		if ops[i].moveTo != "" {
			if ops[i].moveTo, err = safeRelPath(ops[i].moveTo); err != nil {
				return nil, err
			}
		}
		if ops[i].edit.Kind == editPatch && len(ops[i].edit.Hunks) == 0 && ops[i].moveTo == "" {
			return nil, fmt.Errorf("%s 的修改为空", rel)
		}
	}
	return ops, nil
}

// applyPatchPowerShell 把补丁翻译成 PowerShell 命令（每条都在客户端当前目录下执行）。
func applyPatchPowerShell(patch string) ([]string, error) {
	ops, err := parseApplyPatch(patch)
	if err != nil {
		return nil, err
	}
	var cmds []string
	for _, op := range ops {
		e := op.edit
		switch e.Kind {
		case editWrite:
			cmds = append(cmds, writeFileCmds(e.Path, e.Content, true)...)
		case editDelete:
			cmds = append(cmds, deleteFileCmd(e.Path, true))
		case editPatch:
			if len(e.Hunks) > 0 {
				c := patchFileCmd(e.Path, e.Hunks, true)
				if len(c) > psCmdSplitThreshold {
					return nil, fmt.Errorf("%s 的补丁过大", e.Path)
				}
				cmds = append(cmds, c)
			}
			if op.moveTo != "" {
				cmds = append(cmds, moveFileCmd(e.Path, op.moveTo))
			}
		}
	}
	return cmds, nil
}

func moveFileCmd(from, to string) string {
	return "$ErrorActionPreference='Stop'; " + psFullPath(from) + "; $s=$p; " + psFullPath(to) +
		"; $d=[IO.Path]::GetDirectoryName($p); if (-not [IO.Directory]::Exists($d)) { [void][IO.Directory]::CreateDirectory($d) }; " +
		"Move-Item -LiteralPath $s -Destination $p -Force; 'moved " + strings.ReplaceAll(from, "'", "''") + " -> " + strings.ReplaceAll(to, "'", "''") + "'"
}
