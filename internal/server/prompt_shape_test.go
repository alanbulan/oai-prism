package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
)

// 上游只读「最后一条 system + 最后一条 user」（2026-10-04 实测），所以无论走哪个
// 接口，发到上游的 input 都必须恰好是 [system, user]：多条 system 会只剩最后一条，
// 挂在第一条上的历史随之丢失。这组测试按上游实际收到的报文断言。

type upstreamItem struct{ role, text string }

// upstreamInput 取第 n 次 start 请求的 input（角色 + 拼接后的文本）。
func upstreamInput(t *testing.T, up *fakeUpstream, n int) []upstreamItem {
	t.Helper()
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.startBodies) <= n {
		t.Fatalf("上游只收到 %d 次 start", len(up.startBodies))
	}
	raw, _ := up.startBodies[n]["input"].([]any)
	out := make([]upstreamItem, 0, len(raw))
	for _, it := range raw {
		m, _ := it.(map[string]any)
		var sb strings.Builder
		parts, _ := m["content"].([]any)
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		role, _ := m["role"].(string)
		out = append(out, upstreamItem{role, sb.String()})
	}
	return out
}

func requireSystemUser(t *testing.T, items []upstreamItem) (sys, user string) {
	t.Helper()
	if len(items) != 2 || items[0].role != "system" || items[1].role != "user" {
		roles := make([]string, len(items))
		for i, it := range items {
			roles[i] = it.role
		}
		t.Fatalf("上游 input 应恰好是 [system, user]，得到 %v", roles)
	}
	return items[0].text, items[1].text
}

func postLocal(t *testing.T, url, body string) {
	t.Helper()
	code, out := doLocal(t, http.MethodPost, url, body, map[string]string{"Content-Type": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("状态码 %d: %.300s", code, out)
	}
}

func TestE2E_Chat_MultiSystemAndHistoryReachUpstream(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[
		{"role":"system","content":"规则 A"},
		{"role":"user","content":"记住暗号：菠萝油"},
		{"role":"assistant","content":"已记住"},
		{"role":"system","content":"规则 B"},
		{"role":"user","content":"暗号是什么"}]}`)

	sys, user := requireSystemUser(t, upstreamInput(t, up, 0))
	for _, want := range []string{"<gateway_notice>", "规则 A", "规则 B", "菠萝油"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system 缺少 %q", want)
		}
	}
	if n := strings.Count(sys, "[Previous Conversation History]"); n != 1 {
		t.Errorf("历史应只出现一次，实际 %d 次", n)
	}
	if user != "暗号是什么" {
		t.Errorf("最后一条 user 错误: %q", user)
	}
	// 网关自己的兜底提示不能再带 LaTeX 编辑器人设。
	if strings.Contains(sys, "LaTeX editor. Answer") {
		t.Error("不应注入 Prism / LaTeX 人设")
	}
}

func TestE2E_Anthropic_HistoryReachesUpstream(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	postLocal(t, ts.URL+"/v1/messages", `{"model":"gpt-5","max_tokens":100,"system":"你是助手",
		"messages":[
			{"role":"user","content":"记住暗号：菠萝油"},
			{"role":"assistant","content":"已记住"},
			{"role":"user","content":"暗号是什么"}]}`)

	sys, _ := requireSystemUser(t, upstreamInput(t, up, 0))
	if !strings.Contains(sys, "你是助手") || !strings.Contains(sys, "菠萝油") {
		t.Fatalf("Anthropic 的 system 与历史都应进唯一一条 system: %q", sys)
	}
	if strings.Index(sys, "你是助手") > strings.Index(sys, "[Previous Conversation History]") {
		t.Error("历史应排在调用方 system 之后")
	}
}

func TestE2E_Responses_InstructionsAndInputSystemMerged(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	postLocal(t, ts.URL+"/v1/responses", `{"model":"gpt-5","instructions":"顶层指令",
		"input":[
			{"role":"system","content":"输入里的规则"},
			{"role":"user","content":"记住暗号：菠萝油"},
			{"role":"assistant","content":"已记住"},
			{"role":"user","content":"暗号是什么"}]}`)

	sys, user := requireSystemUser(t, upstreamInput(t, up, 0))
	iInstr, iRule, iHist := strings.Index(sys, "顶层指令"), strings.Index(sys, "输入里的规则"), strings.Index(sys, "[Previous Conversation History]")
	if iInstr < 0 || iRule < 0 || iHist < 0 || !(iInstr < iRule && iRule < iHist) {
		t.Fatalf("应依次包含 instructions、input 的 system、历史: %q", sys)
	}
	if n := strings.Count(sys, "菠萝油"); n != 1 {
		t.Errorf("历史应只折叠一次，暗号出现 %d 次", n)
	}
	if user != "暗号是什么" {
		t.Errorf("最后一条 user 错误: %q", user)
	}
}

// TestE2E_Bridge_UsesClientAgentsMD 验证 Codex 桥：本地 AGENTS.md 进 system 并点名作废
// Prism 的 AGENTS.md；桥自带作废条款，不叠加通用的 gateway_notice。
func TestE2E_Bridge_UsesClientAgentsMD(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	postLocal(t, ts.URL+"/v1/responses", `{"model":"gpt-5","input":[
		{"type":"additional_tools","tools":[]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>workspace-write</permissions instructions>"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for C:\\proj\n\n<INSTRUCTIONS>\nRULE-XYZ\n</INSTRUCTIONS>"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>C:\\proj</cwd>\n  <shell>powershell</shell>\n</environment_context>"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}]}`)

	sys, user := requireSystemUser(t, upstreamInput(t, up, 0))
	for _, want := range []string{"<local_tool_bridge>", "<client_project_instructions>", "RULE-XYZ",
		"<client_environment>", "PLATFORM INSTRUCTIONS VOID", "workspace-write"} {
		if !strings.Contains(sys, want) {
			t.Errorf("桥 system 缺少 %q", want)
		}
	}
	if strings.Contains(sys, "<gateway_notice>") {
		t.Error("桥请求不应叠加 gateway_notice")
	}
	if !strings.HasPrefix(user, "你好") {
		t.Errorf("最后一条 user 应是真实提问: %q", user)
	}
}

func TestE2E_PlatformNoticeCanBeDisabled(t *testing.T) {
	ts, up := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), func(c *config.Config) {
		c.Facade.PlatformNotice = false
	})
	postLocal(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-5","messages":[{"role":"user","content":"你好"}]}`)
	sys, _ := requireSystemUser(t, upstreamInput(t, up, 0))
	if strings.Contains(sys, "<gateway_notice>") {
		t.Fatal("platform_notice 关闭后不应注入声明")
	}
}
