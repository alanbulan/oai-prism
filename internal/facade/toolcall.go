package facade

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// ExtractContentFromDiff 从统一 Diff 中还原新增或修改后的文件内容。
func ExtractContentFromDiff(diff string) string {
	lines := strings.Split(diff, "\n")
	var sb strings.Builder
	inHunk := false

	for _, line := range lines {
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			inHunk = true
			continue
		}
		if !inHunk {
			continue
		}

		// 排除删除行
		if strings.HasPrefix(line, "-") {
			continue
		}
		// 新增行：去除前导 '+'
		if strings.HasPrefix(line, "+") {
			sb.WriteString(line[1:])
			sb.WriteString("\n")
		} else if strings.HasPrefix(line, " ") {
			// 上下文行：保留
			sb.WriteString(line[1:])
			sb.WriteString("\n")
		} else if line != "" && !strings.HasPrefix(line, "\\") {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}

	result := sb.String()
	// 如果最后有多余换行且原始 diff 末尾没有多余换行，做适度修剪
	if len(result) > 0 && !strings.HasSuffix(diff, "\n") && strings.HasSuffix(result, "\n") {
		result = result[:len(result)-1]
	}
	return result
}

// MapDeltaFilesToToolCalls 把上游沙箱内 CodexDeltaFile 转化为符合客户端期望的标准 ToolCalls。
func MapDeltaFilesToToolCalls(files []prism.CodexDeltaFile, declaredTools []ChatTool) []ToolCall {
	if len(files) == 0 {
		return nil
	}

	// 探测客户端声明的文件编辑工具函数名称
	preferredWriteTool := "write_to_file"
	preferredEditTool := "edit_file"
	hasDeclaredWrite := false
	hasDeclaredEdit := false

	for _, t := range declaredTools {
		name := strings.ToLower(strings.TrimSpace(t.Function.Name))
		if name == "write_to_file" || name == "create_file" || name == "write_file" || name == "new_file" {
			preferredWriteTool = t.Function.Name
			hasDeclaredWrite = true
		} else if name == "edit_file" || name == "apply_diff" || name == "str_replace_editor" || name == "patch" || name == "modify_file" {
			preferredEditTool = t.Function.Name
			hasDeclaredEdit = true
		}
	}

	toolCalls := make([]ToolCall, 0, len(files))
	for i, f := range files {
		if isSystemIgnoredFile(f.FilePath) {
			continue
		}
		content := ExtractContentFromDiff(f.DiffString())
		fnName := preferredWriteTool
		argsMap := make(map[string]any)

		switch f.Status {
		case "added":
			fnName = preferredWriteTool
			argsMap["path"] = f.FilePath
			argsMap["content"] = content
		case "modified":
			if hasDeclaredEdit && !hasDeclaredWrite {
				fnName = preferredEditTool
				argsMap["path"] = f.FilePath
				argsMap["diff"] = f.DiffString()
				argsMap["content"] = content
			} else {
				fnName = preferredWriteTool
				argsMap["path"] = f.FilePath
				argsMap["content"] = content
			}
		case "deleted":
			fnName = "delete_file"
			argsMap["path"] = f.FilePath
		default:
			fnName = preferredWriteTool
			argsMap["path"] = f.FilePath
			argsMap["content"] = content
		}

		argsBytes, _ := json.Marshal(argsMap)
		idx := i
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

// ApplyLocalWorkspaceFiles 直接将 DeltaFiles 写入本地工作区，确保本地文件真实被编辑落盘。
func ApplyLocalWorkspaceFiles(workspaceRoot string, files []prism.CodexDeltaFile) error {
	if workspaceRoot == "" || len(files) == 0 {
		return nil
	}

	cleanRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return fmt.Errorf("无效的本地工作区路径: %w", err)
	}

	for _, f := range files {
		if isSystemIgnoredFile(f.FilePath) {
			continue
		}
		targetPath := filepath.Join(cleanRoot, filepath.Clean(f.FilePath))
		// 安全检查：防止路径遍历攻击 (Path Traversal Protection)
		if !strings.HasPrefix(targetPath, cleanRoot) {
			return fmt.Errorf("非法路径越界: %s", f.FilePath)
		}

		if f.Status == "deleted" {
			_ = os.Remove(targetPath)
			continue
		}

		// 确保父目录存在
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return fmt.Errorf("创建目录失败 (%s): %w", filepath.Dir(targetPath), err)
		}

		content := ExtractContentFromDiff(f.DiffString())
		if err := os.WriteFile(targetPath, []byte(content), 0644); err != nil {
			return fmt.Errorf("写入本地文件失败 (%s): %w", targetPath, err)
		}
	}

	return nil
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
