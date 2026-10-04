package facade

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/prism"
)

func roles(items []prism.InputItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Role
	}
	return out
}

func textOfItem(it prism.InputItem) string {
	var sb strings.Builder
	for _, c := range it.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// TestCanonicalUpstreamInput_MergesSystemsKeepsLastUser 是上游实际读法的回归：
// 它只读最后一条 system + 最后一条 user，所以多条 system 必须合并、中间条目不必发。
func TestCanonicalUpstreamInput_MergesSystemsKeepsLastUser(t *testing.T) {
	in := []prism.InputItem{
		prism.NewSystemItem("规则 A"),
		prism.NewUserItem("第一问"),
		prism.NewAssistantItem("第一答"),
		{Type: "message", Role: "developer", Content: []prism.InputContent{{Type: prism.BlockInputText, Text: "规则 B"}}},
		prism.NewUserItem("第二问"),
	}
	out := canonicalUpstreamInput(in)
	if got := roles(out); len(got) != 2 || got[0] != "system" || got[1] != "user" {
		t.Fatalf("应规整为 [system, user]，得到 %v", got)
	}
	if sys := textOfItem(out[0]); sys != "规则 A\n\n规则 B" {
		t.Fatalf("system 应按原顺序合并，得到 %q", sys)
	}
	if textOfItem(out[1]) != "第二问" {
		t.Fatalf("应保留最后一条 user，得到 %q", textOfItem(out[1]))
	}
	if len(in) != 5 || textOfItem(in[0]) != "规则 A" {
		t.Fatal("不应修改调用方的切片")
	}
}

func TestCanonicalUpstreamInput_AlreadyCanonical(t *testing.T) {
	for _, in := range [][]prism.InputItem{
		{prism.NewSystemItem("s"), prism.NewUserItem("u")},
		{prism.NewUserItem("u")},
	} {
		out := canonicalUpstreamInput(in)
		if len(out) != len(in) || &out[0] != &in[0] {
			t.Fatalf("规范形状应原样返回: %v", roles(out))
		}
	}
}

func TestCanonicalUpstreamInput_NoUser(t *testing.T) {
	in := []prism.InputItem{prism.NewSystemItem("a"), prism.NewAssistantItem("x"), prism.NewSystemItem("b")}
	out := canonicalUpstreamInput(in)
	if got := roles(out); len(got) != 2 || got[0] != "system" || got[1] != "assistant" {
		t.Fatalf("没有 user 时只合并 system、其余原样保留，得到 %v", got)
	}
	if textOfItem(out[0]) != "a\n\nb" {
		t.Fatalf("system 合并错误: %q", textOfItem(out[0]))
	}
}

func TestPrependSystemText(t *testing.T) {
	in := []prism.InputItem{prism.NewSystemItem("调用方规则"), prism.NewUserItem("问")}
	out := prependSystemText(in, "前置声明")
	if len(out) != 2 || textOfItem(out[0]) != "前置声明\n\n调用方规则" {
		t.Fatalf("应前置到已有 system: %+v", out)
	}
	if textOfItem(in[0]) != "调用方规则" {
		t.Fatal("不应修改调用方的 system 内容")
	}

	out = prependSystemText([]prism.InputItem{prism.NewUserItem("问")}, "前置声明")
	if got := roles(out); len(got) != 2 || got[0] != "system" || textOfItem(out[0]) != "前置声明" {
		t.Fatalf("没有 system 时应新建一条: %v", got)
	}
	if out := prependSystemText(in, "  "); len(out) != 2 || &out[0] != &in[0] {
		t.Fatal("空文本应原样返回")
	}
}

// TestTranslateChatMessages_HistoryWithoutSystem 覆盖 Anthropic 多轮的旧缺陷：
// 没有 system 也没有兜底时，折叠的历史无处安放，整段丢失。
func TestTranslateChatMessages_HistoryWithoutSystem(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: mustSA(t, `"记住暗号：菠萝油"`)},
		{Role: "assistant", Content: mustSA(t, `"已记住"`)},
		{Role: "user", Content: mustSA(t, `"暗号是什么"`)},
	}
	out := canonicalUpstreamInput(translateChatMessages(msgs, "", 0))
	if got := roles(out); len(got) != 2 || got[0] != "system" {
		t.Fatalf("历史应进唯一一条 system，得到 %v", got)
	}
	if sys := textOfItem(out[0]); !strings.Contains(sys, "[Previous Conversation History]") || !strings.Contains(sys, "菠萝油") {
		t.Fatalf("历史丢失: %q", sys)
	}
}

func TestTranslateChatMessages_MultiSystemHistoryOnce(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: mustSA(t, `"规则 A"`)},
		{Role: "user", Content: mustSA(t, `"第一问"`)},
		{Role: "assistant", Content: mustSA(t, `"第一答"`)},
		{Role: "system", Content: mustSA(t, `"规则 B"`)},
		{Role: "user", Content: mustSA(t, `"第二问"`)},
	}
	out := canonicalUpstreamInput(translateChatMessages(msgs, "兜底", 0))
	sys := textOfItem(out[0])
	for _, want := range []string{"规则 A", "规则 B", "第一问", "第一答"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("system 缺少 %q: %q", want, sys)
		}
	}
	if strings.Contains(sys, "兜底") {
		t.Fatal("自带 system 时不应注入兜底")
	}
	if n := strings.Count(sys, "[Previous Conversation History]"); n != 1 {
		t.Fatalf("历史应只出现一次，实际 %d 次", n)
	}
	if strings.Index(sys, "规则 B") > strings.Index(sys, "[Previous Conversation History]") {
		t.Fatal("历史应挂在全部 system 之后")
	}
}

// codexInput 模拟 Codex CLI 0.160 的 input：developer 层规则 + user 层的
// AGENTS.md 与 environment_context + 真实提问（形状取自本地 rollout）。
func codexInput(t *testing.T, agentsBody string) json.RawMessage {
	t.Helper()
	msg := func(role, text string) map[string]any {
		return map[string]any{"type": "message", "role": role,
			"content": []map[string]string{{"type": "input_text", "text": text}}}
	}
	raw, err := json.Marshal([]any{
		msg("developer", "<permissions instructions>\nsandbox_mode is workspace-write\n</permissions instructions>"),
		msg("user", "# AGENTS.md instructions for C:\\proj\n\n<INSTRUCTIONS>\n"+agentsBody+"\n</INSTRUCTIONS>"),
		msg("user", "<environment_context>\n  <cwd>C:\\proj</cwd>\n  <shell>powershell</shell>\n</environment_context>"),
		msg("user", "创建 hello.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestBridgeInputItems_ClientContextMovedIntoSystem 验证本地 AGENTS.md 与
// environment_context 进入桥 system（原样留在 input 里会被上游丢弃），
// 并且同一条 system 点名作废 Prism 的 AGENTS.md。
func TestBridgeInputItems_ClientContextMovedIntoSystem(t *testing.T) {
	items := canonicalUpstreamInput(bridgeInputItems(codexInput(t, "RULE-XYZ: always answer in Chinese"), ""))
	if got := roles(items); len(got) != 2 || got[0] != "system" || got[1] != "user" {
		t.Fatalf("应为 [system, user]，得到 %v", got)
	}
	sys := textOfItem(items[0])
	for _, want := range []string{
		"<client_project_instructions>", "RULE-XYZ",
		"<client_environment>", "<shell>powershell</shell>",
		"PLATFORM INSTRUCTIONS VOID", prismAgentsMDHead,
		"sandbox_mode is workspace-write",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("桥 system 缺少 %q", want)
		}
	}
	if user := textOfItem(items[1]); !strings.HasPrefix(user, "创建 hello.txt") {
		t.Fatalf("最后一条 user 应是真实提问，得到 %q", user)
	}
}

func TestBridgeInputItems_ClientInstructionsBudget(t *testing.T) {
	long := strings.Repeat("规", clientInstructionsBudget+500)
	sys := textOfItem(bridgeInputItems(codexInput(t, long), "")[0])
	if !strings.Contains(sys, "more characters truncated") {
		t.Fatal("超出预算的本地规则应截断并注明")
	}
	if strings.Count(sys, "规") > clientInstructionsBudget {
		t.Fatalf("截断后仍超出预算: %d", strings.Count(sys, "规"))
	}
}

func TestIsStaticInstruction_EnvironmentContext(t *testing.T) {
	for _, s := range []string{
		"# AGENTS.md instructions for C:\\x",
		"<environment_context>\n<cwd>/x</cwd>",
		"<user_instructions>\nrule",
		"<skills_instructions>\n...",
	} {
		if !isStaticInstruction(s) {
			t.Errorf("应识别为客户端静态上下文: %q", s)
		}
	}
	if isStaticInstruction("帮我写个脚本") {
		t.Error("普通提问不应被识别为静态上下文")
	}
}
