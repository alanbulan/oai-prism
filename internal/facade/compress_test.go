package facade

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// TestTranslateChatMessages_ShortHistoryKeptVerbatim 是 2026-10-04 实测缺陷的回归：
// 旧实现超过 6 轮就把早期消息截成前 197 个字，一段只有约 480 tokens 的对话
// 就答不出首轮第 234 字处给的暗号。放得下就必须原样全留。
func TestTranslateChatMessages_ShortHistoryKeptVerbatim(t *testing.T) {
	first := strings.Repeat("项目背景说明。", 33) + "部署暗号是 TANGO-731。"
	msgs := []ChatMessage{
		{Role: "user", Content: stringContent(first)},
		{Role: "assistant", Content: stringContent("已记住。")},
	}
	for k := 1; k <= 6; k++ {
		msgs = append(msgs,
			ChatMessage{Role: "user", Content: stringContent(fmt.Sprintf("第 %d 轮闲聊，只回复 OK。", k))},
			ChatMessage{Role: "assistant", Content: stringContent("OK")})
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: stringContent("部署暗号是什么？")})

	sys := textOfItem(canonicalUpstreamInput(translateChatMessages(msgs, "", 96<<10))[0])
	if !strings.Contains(sys, first) {
		t.Fatal("首轮消息应原样保留（含暗号）")
	}
	if strings.Contains(sys, "omitted") {
		t.Fatal("放得下时不应省略任何历史")
	}
}

// TestTranslateChatMessages_TrimsOldestToBudget：放不下时从最旧的开始丢，
// 注明省略，并且整条提示词不超过上限。
func TestTranslateChatMessages_TrimsOldestToBudget(t *testing.T) {
	var msgs []ChatMessage
	for k := 1; k <= 40; k++ {
		msgs = append(msgs,
			ChatMessage{Role: "user", Content: stringContent(fmt.Sprintf("Q%02d %s", k, strings.Repeat("x", 400)))},
			ChatMessage{Role: "assistant", Content: stringContent(fmt.Sprintf("A%02d %s", k, strings.Repeat("y", 400)))})
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: stringContent("最后的问题")})

	const limit = 12000
	items := canonicalUpstreamInput(translateChatMessages(msgs, "系统规则", limit))
	sys := textOfItem(items[0])
	if !strings.Contains(sys, historyOmittedMark) {
		t.Fatal("裁剪后应注明省略了多少")
	}
	if strings.Contains(sys, "Q01 ") || !strings.Contains(sys, "A40 ") || !strings.HasPrefix(sys, "系统规则") {
		t.Fatalf("应丢最旧、留最新、保住 system: %.200q", sys)
	}
	if n := promptBytes(items) + len(platformNotice) + 2; n > limit {
		t.Fatalf("连同平台声明共 %d 字节，超过上限 %d", n, limit)
	}
}

func TestRenderHistory(t *testing.T) {
	entries := []historyEntry{{speaker: "User", text: "一"}, {speaker: "Assistant", text: "二"}, {speaker: "User", text: "  "}}
	if got := renderHistory(entries, 0); got != historyHeader+"User: 一\nAssistant: 二" {
		t.Fatalf("不限预算时应全量渲染（跳过空消息）: %q", got)
	}
	if renderHistory(entries, -1) != "" || renderHistory(nil, 0) != "" {
		t.Fatal("预算为负或没有内容时应为空")
	}
}

// TestRenderHistory_DropsBulkyBeforeUserMessages：超限时先丢大块的工具输出与助手回复，
// 用户自己说的话（要求、暗号）最后才动；丢掉的位置原地注明，整段不超预算。
func TestRenderHistory_DropsBulkyBeforeUserMessages(t *testing.T) {
	entries := []historyEntry{{speaker: "User", text: "记住暗号：OLIVE-88"}}
	for k := 1; k <= 6; k++ {
		entries = append(entries,
			historyEntry{speaker: "Assistant", text: fmt.Sprintf("CALL-%d", k)},
			historyEntry{speaker: "User", text: fmt.Sprintf("[CLIENT RESULT] OUT-%d %s", k, strings.Repeat("长", 2000))})
	}
	entries = append(entries, historyEntry{speaker: "User", text: "接下来读 doc7"})

	const budget = 20000
	got := renderHistory(entries, budget)
	if len(got) > budget || !utf8.ValidString(got) {
		t.Fatalf("超出预算: %d 字节", len(got))
	}
	for _, want := range []string{"OLIVE-88", "接下来读 doc7", "CALL-1", "OUT-6 ", historyOmittedMark} {
		if !strings.Contains(got, want) {
			t.Errorf("应保留 %q", want)
		}
	}
	if strings.Contains(got, "OUT-1 ") {
		t.Error("最旧的工具输出应先丢")
	}

	// 全是用户长消息时退回按时间从最旧的丢。
	var users []historyEntry
	for k := 1; k <= 10; k++ {
		users = append(users, historyEntry{speaker: "User", text: fmt.Sprintf("U%02d %s", k, strings.Repeat("x", 3000))})
	}
	got = renderHistory(users, 12000)
	if len(got) > 12000 || strings.Contains(got, "U01 ") || !strings.Contains(got, "U10 ") {
		t.Fatalf("应从最旧的用户消息丢起（%d 字节）", len(got))
	}
}

// TestBridgeInputItems_DropsCodexBaseInstructions：Codex 0.160 把 21.7 KB 的基础
// 提示词作为 developer 消息发来，与多代理说明一起不转发；权限、skills、用户
// 自定义的 developer 指令照常进 system。
func TestBridgeInputItems_DropsCodexBaseInstructions(t *testing.T) {
	msg := func(role, text string) map[string]any {
		return map[string]any{"type": "message", "role": role,
			"content": []map[string]string{{"type": "input_text", "text": text}}}
	}
	raw, _ := json.Marshal([]any{
		map[string]any{"type": "additional_tools", "role": "developer", "tools": []any{}},
		msg("developer", "You are Codex, an agent based on GPT-6. "+strings.Repeat("base ", 1000)),
		msg("developer", "<skills_instructions>\n## Skills\nSKILL-LIST\n</skills_instructions>"),
		msg("developer", "<multi_agent_role>You are `/root`, the primary agent</multi_agent_role>"),
		msg("developer", "<multi_agent_mode>no delegation</multi_agent_mode>"),
		msg("developer", "<permissions instructions>workspace-write</permissions instructions>"),
		msg("developer", "用户自定义：回答用中文"),
		msg("user", "你好"),
	})
	sys := textOfItem(bridgeInputItems(raw, "")[0])
	for _, gone := range []string{"You are Codex", "base base", "<multi_agent_role>", "<multi_agent_mode>"} {
		if strings.Contains(sys, gone) {
			t.Errorf("不应转发 %q", gone)
		}
	}
	for _, kept := range []string{"SKILL-LIST", "workspace-write", "用户自定义：回答用中文"} {
		if !strings.Contains(sys, kept) {
			t.Errorf("应保留 %q", kept)
		}
	}
}

// TestFoldInputHistory_PostCompactionShape：Codex 压缩后的替换历史只有 user 消息与
// 摘要、没有 assistant，照样要折进 system，摘要不能丢。
func TestFoldInputHistory_PostCompactionShape(t *testing.T) {
	items := []prism.InputItem{
		prism.NewSystemItem("bridge"),
		prism.NewUserItem("记住暗号：COBALT-5521"),
		prism.NewUserItem("Another language model started to solve this problem… 摘要：MARKER-222"),
		prism.NewUserItem("暗号是什么？"),
	}
	out := canonicalUpstreamInput(foldInputHistory(items, 0))
	sys := textOfItem(out[0])
	if !strings.Contains(sys, "COBALT-5521") || !strings.Contains(sys, "MARKER-222") {
		t.Fatalf("保留的用户消息与摘要都应折进 system: %q", sys)
	}
	if textOfItem(out[1]) != "暗号是什么？" {
		t.Fatalf("最后一条 user 应是本轮提问: %q", textOfItem(out[1]))
	}
}

// TestFoldInputHistory_CompactionBudget：压缩请求超限时裁掉最旧的历史，整条不超上限。
func TestFoldInputHistory_CompactionBudget(t *testing.T) {
	items := []prism.InputItem{prism.NewSystemItem(strings.Repeat("s", 2000))}
	for k := 1; k <= 30; k++ {
		items = append(items, prism.NewAssistantItem(fmt.Sprintf("CALL-%02d", k)),
			prism.NewUserItem(fmt.Sprintf("[CLIENT RESULT] OUT-%02d %s", k, strings.Repeat("o", 1000))))
	}
	items = append(items, prism.NewUserItem("You are performing a CONTEXT CHECKPOINT COMPACTION."))

	const limit = 12000
	out := canonicalUpstreamInput(foldInputHistory(items, limit))
	sys := textOfItem(out[0])
	if n := promptBytes(out); n > limit {
		t.Fatalf("裁剪后 %d 字节，超过上限 %d", n, limit)
	}
	if strings.Contains(sys, "OUT-01 ") || !strings.Contains(sys, "OUT-30 ") || !strings.Contains(sys, "omitted") {
		t.Fatal("应丢最旧、留最新并注明省略")
	}
	if !historyTrimmed(out) || historyTrimmed(canonicalUpstreamInput(foldInputHistory(items, 0))) {
		t.Fatal("historyTrimmed 判断有误")
	}
	if full := canonicalUpstreamInput(foldInputHistory(items, 0)); promptBytes(full) <= limit {
		t.Fatal("用例本身应超限")
	}
}

func TestCodexRequestKind(t *testing.T) {
	meta := `{"request_kind":"compaction","compaction":{"trigger":"auto","strategy":"memento"}}`
	cm, _ := json.Marshal(map[string]string{"x-codex-turn-metadata": meta})
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	if got := codexRequestKind(r, map[string]json.RawMessage{"client_metadata": cm}); got != "compaction" {
		t.Fatalf("应从 client_metadata 取到 compaction，得到 %q", got)
	}
	r.Header.Set("X-Codex-Turn-Metadata", `{"request_kind":"turn"}`)
	if got := codexRequestKind(r, nil); got != "turn" {
		t.Fatalf("应从请求头取到 turn，得到 %q", got)
	}
	if got := codexRequestKind(httptest.NewRequest("POST", "/", nil), nil); got != "" {
		t.Fatalf("没有元数据时应为空，得到 %q", got)
	}
}

func TestAnthropicTooLongMessage(t *testing.T) {
	err := &contextTooLargeError{Bytes: 200000, Limit: 100000, Tokens: 50000}
	if got := anthropicTooLongMessage(err); got != "prompt is too long: 50000 tokens > 25000 maximum" {
		t.Fatalf("文案须与 Anthropic 官方一致: %q", got)
	}
	if !strings.Contains(err.Error(), "200000 字节") || responsesErrorCode(err) != "context_length_exceeded" {
		t.Fatal("错误信息与 Responses 错误码不对")
	}
}
