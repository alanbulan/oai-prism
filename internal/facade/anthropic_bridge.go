package facade

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 本文件实现「Claude Code 工具桥」：与 Codex 工具桥（toolbridge.go）同一个思路 ——
// 上游当大脑，本地 Claude Code 当手脚。
//
// 上游是云端沙箱里的 Codex 智能体，不认客户端在请求里声明的工具（Claude Code 的
// Bash / Read / Write / Edit …），只会用沙箱自带的工具干活，客户端只拿到最终文本。
// 此前 /v1/messages 把工具定义当元数据记下就完了：Claude Code 永远收不到 tool_use，
// 模型在沙箱里"写好"的文件用户本地一个也没有。
//
// 桥：system 里列出客户端声明的工具，要求上游把每次调用写成 ```local-tool 围栏
// （JSON：name + input）；网关解析成 Anthropic 的 tool_use 块（stop_reason=tool_use），
// Claude Code 在本地执行（走它自己的权限确认），结果（tool_result）回来时翻译成
// [CLIENT RESULT] 文本交给上游继续。与 Codex 桥不同，调用直接对应客户端的工具，
// 不经过 JS / shell 包装：读写文件用 Read / Write / Edit，跑命令用 Bash。

const (
	localToolFence = "```local-tool"

	// invalidToolName 是解析不了的调用块转成的 tool_use 名字。客户端没有这个工具，
	// 必然回一条出错的结果 —— 网关在下一轮把它翻译成"你的调用块不是合法 JSON"
	// （见 renderToolResult），上游据此重发，而不是整轮静默丢失。
	invalidToolName = "oaiprism_invalid_tool_call"

	// toolCatalogBudget 是工具清单在 system 里的字节预算：上游单条提示词约 100 KiB 封顶，
	// Claude Code 带着二三十个工具（接了 MCP 还会更多），原样列出的 JSON Schema 就有五六十 KB。
	toolCatalogBudget = 24 << 10

	// toolResultsBudget 是一条消息里全部工具结果的字节预算（按条数均分，每条至少 toolResultMin）。
	// 增量轮次里它们就是发给上游的那条消息，超出单条上限整轮都发不出去。
	toolResultsBudget = 40 << 10
	toolResultMin     = 4 << 10

	// replayValueMax 是回放历史里工具参数单个字符串的上限（Write 的整份文件内容之类）。
	// 回放只在新建上游会话、折叠历史时用得上，原文在上游会话里。
	replayValueMax = 2000

	// toolUseFPPrefix 开头的 tool_use ID 里带着这轮上游原文的指纹（见 bridgeToolUseIDs）。
	toolUseFPPrefix = "toolu_op"
)

// claudeBridgeHead 是桥指令的固定部分（工具清单接在后面）。
const claudeBridgeHead = "<client_tool_bridge>\n" +
	"You are the reasoning engine of a LOCAL agent client (such as Claude Code) running on the user's own computer. " +
	"The client executes tools on the user's machine and reports the results back to you; you decide what to do next.\n\n" +
	"[THE REMOTE SANDBOX IS NOT THE USER'S MACHINE]\n" +
	"1. Your built-in tools (shell, apply_patch, file creation/editing such as 'createNewFile' / 'updateFile', file reads) run in a remote temporary container. " +
	"The user cannot see it and nothing you do there reaches their computer: files written there are lost. Do NOT use them for the user's task.\n" +
	"2. Everything that must happen on the user's machine - running commands, reading, searching, creating or editing files, fetching URLs - is done ONLY by calling the client tools listed in <client_tools>.\n" +
	`3. The remote container's AGENTS.md (it begins "` + prismAgentsMDHead + `") and its <environment_context> (bash, /codex_workspace/...) describe that container, not the user's machine: ignore them. ` +
	"The user's real environment (OS, shell, working directory, project instructions) is described by the client's own instructions further below.\n" +
	`4. Exception: files the user attached are uploaded into the remote project under /prism-uploads/ and appear as "[project file: /prism-uploads/<name>]". ` +
	"Open those with your built-in read-only tools (path relative to your working directory, e.g. prism-uploads/<name>) when you need them; " +
	"this is part of reading the user's message and is allowed even when the user asks you not to use tools.\n\n" +
	"[HOW TO CALL A CLIENT TOOL]\n" +
	"Write one fenced block per call, exactly like this:\n" +
	localToolFence + "\n" +
	`{"name": "Read", "input": {"file_path": "/absolute/path/to/file"}}` + "\n" +
	"```\n" +
	`- The block holds one JSON object: "name" is a tool name from <client_tools>, "input" is an object with that tool's parameters.` + "\n" +
	`- It must be strict JSON: inside strings escape " as \", \ as \\ and newlines as \n (Windows paths: "C:\\Users\\me\\file.txt"). Write complete file contents - never abbreviate.` + "\n" +
	"- Independent calls may go in one reply, one block each; they run in order and all results come back together. A call that needs an earlier call's result must wait for your next reply.\n" +
	`- After the block(s), END your reply. Never predict or invent results: the client runs the calls and its next message gives you each result as "[CLIENT RESULT tool=<name> id=<id>] ... [/CLIENT RESULT]" (with ERROR after the id when it failed).` + "\n" +
	"- Text outside the blocks is shown to the user: keep it to a short note on what you are doing.\n" +
	"- When the task is done, or you need the user's input, reply in plain text with no block.\n" +
	"- Never say you created, edited or ran something unless a client tool did it and its result confirmed it.\n" +
	"</client_tool_bridge>"

// clientToolReminder 接在本轮消息末尾（只在本轮出现，进了历史就没有了）。
const clientToolReminder = "\n\n[CLIENT_TOOL_REMINDER] Act on the user's machine only through " + localToolFence +
	" blocks (strict JSON, one call per block) calling the client tools from your instructions; your remote sandbox tools do not reach the user. " +
	"After the blocks, end your reply and wait for the [CLIENT RESULT]."

// anthropicToolChoice 是请求里的 tool_choice。
type anthropicToolChoice struct {
	Type                   string `json:"type"` // auto / any / tool / none
	Name                   string `json:"name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
}

func parseAnthropicToolChoice(v any) anthropicToolChoice {
	var c anthropicToolChoice
	switch t := v.(type) {
	case nil:
	case string:
		c.Type = t
	default:
		if b, err := json.Marshal(t); err == nil {
			_ = json.Unmarshal(b, &c)
		}
	}
	return c
}

// anthropicBridgeTools 返回桥要列给上游的客户端工具；为空表示本轮不走桥
// （没声明工具，或 tool_choice 是 none）。服务端工具（web_search_20250305 这类带 type、
// 没有 input_schema 的）由 Anthropic 自己执行，客户端跑不了，不列。
func anthropicBridgeTools(req *AnthropicRequest) []AnthropicTool {
	if parseAnthropicToolChoice(req.ToolChoice).Type == "none" {
		return nil
	}
	var out []AnthropicTool
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" || (t.Type != "" && t.Type != "custom" && len(t.InputSchema) == 0) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// anthropicBridgePrompt 是注入给上游的桥指令（排在客户端自己的 system 前面）。
func anthropicBridgePrompt(tools []AnthropicTool, choice anthropicToolChoice) string {
	var sb strings.Builder
	sb.WriteString(claudeBridgeHead)
	sb.WriteString("\n\n<client_tools>\n")
	sb.WriteString(renderToolCatalog(tools, toolCatalogBudget))
	sb.WriteString("</client_tools>")
	switch choice.Type {
	case "any":
		sb.WriteString("\nIn this reply you MUST call at least one client tool.")
	case "tool":
		sb.WriteString("\nIn this reply you MUST call the client tool \"" + choice.Name + "\".")
	}
	if choice.DisableParallelToolUse {
		sb.WriteString("\nCall at most one tool in this reply.")
	}
	return sb.String()
}

// coreClientTools 是 Claude Code 的核心工具：清单里说明保留得更完整。
var coreClientTools = map[string]bool{
	"Bash": true, "Read": true, "Write": true, "Edit": true, "MultiEdit": true, "Glob": true, "Grep": true,
	"LS": true, "NotebookEdit": true, "WebFetch": true, "WebSearch": true, "TodoWrite": true, "Task": true,
	"Agent": true, "Skill": true, "AskUserQuestion": true, "ExitPlanMode": true, "ToolSearch": true,
}

// catalogLevel 是工具清单的一档详略：说明字数（核心 / 其它工具）、参数说明字数、嵌套参数展开几层。
type catalogLevel struct{ core, other, param, depth int }

var catalogLevels = []catalogLevel{
	{core: 1500, other: 600, param: 200, depth: 3},
	{core: 900, other: 240, param: 120, depth: 2},
	{core: 500, other: 120, param: 60, depth: 1},
	{core: 200, other: 0, param: 0, depth: 1},
}

// renderToolCatalog 把工具定义渲染成紧凑的清单：从最详细的一档开始，放不进预算就换更简略的一档。
func renderToolCatalog(tools []AnthropicTool, budget int) string {
	out := ""
	for _, lv := range catalogLevels {
		var sb strings.Builder
		for _, t := range tools {
			renderCatalogTool(&sb, t, lv)
		}
		if out = sb.String(); len(out) <= budget {
			break
		}
	}
	return out
}

func renderCatalogTool(sb *strings.Builder, t AnthropicTool, lv catalogLevel) {
	limit := lv.other
	if coreClientTools[t.Name] {
		limit = lv.core
	}
	sb.WriteString("### " + t.Name + "\n")
	if d := clipText(t.Description, limit); d != "" {
		sb.WriteString(d + "\n")
	}
	var schema map[string]any
	if len(t.InputSchema) > 0 && json.Unmarshal(t.InputSchema, &schema) == nil {
		var ps strings.Builder
		renderSchemaProps(&ps, schema, "", 1, lv)
		if ps.Len() > 0 {
			sb.WriteString("Input:\n" + ps.String())
		} else {
			sb.WriteString("Input: {} (no parameters)\n")
		}
	}
	sb.WriteString("\n")
}

// renderSchemaProps 列出对象的字段：必填的按 required 的顺序在前，其余按名字排序（输出必须稳定 ——
// system 一变，原生续接就要重发整段）。
func renderSchemaProps(sb *strings.Builder, schema map[string]any, indent string, depth int, lv catalogLevel) {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return
	}
	required := map[string]bool{}
	var order []string
	if rq, ok := schema["required"].([]any); ok {
		for _, r := range rq {
			if s, ok := r.(string); ok && props[s] != nil && !required[s] {
				required[s] = true
				order = append(order, s)
			}
		}
	}
	var rest []string
	for k := range props {
		if !required[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range append(order, rest...) {
		p, _ := props[k].(map[string]any)
		sb.WriteString(indent + "- " + k + " (" + schemaType(p))
		if required[k] {
			sb.WriteString(", required")
		}
		sb.WriteString(")")
		if d, _ := p["description"].(string); lv.param > 0 && d != "" {
			sb.WriteString(": " + strings.Join(strings.Fields(clipText(d, lv.param)), " "))
		}
		sb.WriteString("\n")
		if depth < lv.depth {
			child := p
			if items, ok := p["items"].(map[string]any); ok {
				child = items
			}
			renderSchemaProps(sb, child, indent+"  ", depth+1, lv)
		}
	}
}

// schemaType 是参数类型的简写：string、array of object、one of "a" | "b" …
func schemaType(p map[string]any) string {
	if e, ok := p["enum"].([]any); ok && len(e) > 0 {
		var parts []string
		for i, v := range e {
			if i == 12 {
				parts = append(parts, "…")
				break
			}
			b, _ := json.Marshal(v)
			parts = append(parts, string(b))
		}
		return "one of " + strings.Join(parts, " | ")
	}
	if c, ok := p["const"]; ok {
		b, _ := json.Marshal(c)
		return "const " + string(b)
	}
	t := ""
	switch v := p["type"].(type) {
	case string:
		t = v
	case []any:
		var ts []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				ts = append(ts, s)
			}
		}
		t = strings.Join(ts, "|")
	}
	if t == "" {
		for _, key := range []string{"anyOf", "oneOf"} {
			if alts, ok := p[key].([]any); ok {
				var ts []string
				for _, a := range alts {
					if m, ok := a.(map[string]any); ok {
						ts = append(ts, schemaType(m))
					}
				}
				t = strings.Join(ts, " | ")
				break
			}
		}
	}
	if t == "array" {
		if items, ok := p["items"].(map[string]any); ok {
			t = "array of " + schemaType(items)
		}
	}
	if t == "" {
		t = "any"
	}
	return t
}

// clipText 把说明截到 n 个字符以内，尽量断在句末。
func clipText(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	cut := string([]rune(s)[:n])
	if i := strings.LastIndexAny(cut, ".。\n"); i >= len(cut)*3/5 {
		cut = cut[:i+1]
	}
	return strings.TrimSpace(cut) + " …"
}

// ---------------------------- 客户端历史 → 上游 ----------------------------

// anthropicBlock 是消息里的一个内容块（只取用得到的字段）。
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	ToolName  string          `json:"tool_name"` // tool_reference（ToolSearch 的结果）
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`

	raw json.RawMessage
}

// decodeAnthropicContent 拆出内容块；字符串内容当一个文本块。
func decodeAnthropicContent(raw json.RawMessage) []anthropicBlock {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return []anthropicBlock{{Type: "text", Text: StringOrArray{raw: raw}.Text()}}
	}
	out := make([]anthropicBlock, 0, len(items))
	for _, it := range items {
		var b anthropicBlock
		if json.Unmarshal(it, &b) != nil {
			continue
		}
		b.raw = it
		out = append(out, b)
	}
	return out
}

// anthropicChatMessages 把 Anthropic messages 换成 chat 消息（工具桥与普通请求共用，
// 两边看到的历史逐字一致）：
//   - system 角色（Claude Code 2.1 起把环境信息等放在对话中间的 system 消息里）合并进 system；
//     <total_tokens> 这类每轮都变的计数提示丢掉 —— 否则 system 每轮都变，原生续接每轮重发整段；
//   - assistant：正文 + 工具调用（按桥的围栏格式回放），思考块不要；
//   - user：正文、工具结果（[CLIENT RESULT]）合成一个文本块，图片另起图片块。
func anthropicChatMessages(msgs []AnthropicMessage) []ChatMessage {
	decoded := make([][]anthropicBlock, len(msgs))
	calls := map[string]anthropicBlock{} // tool_use ID → 调用：工具结果按它标注工具名
	for i, m := range msgs {
		decoded[i] = decodeAnthropicContent(m.Content.Raw())
		if strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			for _, b := range decoded[i] {
				if b.Type == "tool_use" && b.ID != "" {
					calls[b.ID] = b
				}
			}
		}
	}
	chat := make([]ChatMessage, 0, len(msgs))
	for i, m := range msgs {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "assistant":
			text, fp := assistantReplay(decoded[i])
			chat = append(chat, ChatMessage{Role: "assistant", Content: stringContent(text), fp: fp})
		case "system", "developer":
			if t := strings.TrimSpace(m.Content.Text()); t != "" && !reVolatileSystem.MatchString(t) {
				chat = append(chat, ChatMessage{Role: "system", Content: stringContent(t)})
			}
		default:
			chat = append(chat, ChatMessage{Role: "user", Content: userContent(decoded[i], calls)})
		}
	}
	return chat
}

// reVolatileSystem 匹配每轮都变、对上游没有意义的 system 提示（Claude Code 的剩余 token 计数）。
var reVolatileSystem = regexp.MustCompile(`^<total_tokens>[^<]*</total_tokens>$`)

// assistantReplay 把助手消息回放成文本（正文 + 工具调用围栏），并取出这轮上游原文的指纹
// （tool_use ID 里带着，见 bridgeToolUseIDs；没有时为 0）。
func assistantReplay(blocks []anthropicBlock) (string, uint64) {
	var parts []string
	var fp uint64
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		case "tool_use":
			if fp == 0 {
				fp = toolUseFingerprint(b.ID)
			}
			if b.Name == invalidToolName {
				parts = append(parts, "(an unusable "+localToolFence+" block was here)")
				continue
			}
			parts = append(parts, localToolFence+"\n"+replayToolCall(b.Name, b.Input)+"\n```")
		}
	}
	return strings.Join(parts, "\n\n"), fp
}

// replayToolCall 渲染一次调用的 JSON；过长的字符串参数（整份文件内容之类）截短。
func replayToolCall(name string, input json.RawMessage) string {
	var in any
	if len(input) > 0 {
		dec := json.NewDecoder(bytes.NewReader(input))
		dec.UseNumber()
		_ = dec.Decode(&in)
	}
	if in == nil {
		in = map[string]any{}
	}
	return marshalNoEscape(map[string]any{"name": name, "input": clipJSONStrings(in, replayValueMax)})
}

func clipJSONStrings(v any, n int) any {
	switch t := v.(type) {
	case string:
		if utf8.RuneCountInString(t) > n {
			r := []rune(t)
			return string(r[:n]) + fmt.Sprintf("…[%d more characters omitted]", len(r)-n)
		}
	case map[string]any:
		for k, x := range t {
			t[k] = clipJSONStrings(x, n)
		}
	case []any:
		for i, x := range t {
			t[i] = clipJSONStrings(x, n)
		}
	}
	return v
}

// marshalNoEscape 序列化成 JSON，不把 < > & 转成 \u003c（文件内容原样可读）。
func marshalNoEscape(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return "{}"
	}
	return strings.TrimRight(buf.String(), "\n")
}

// userContent 把 user 消息的内容块翻译成 chat 内容（JSON 数组）。
func userContent(blocks []anthropicBlock, calls map[string]anthropicBlock) StringOrArray {
	results := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			results++
		}
	}
	budget := toolResultMin
	if results > 0 {
		budget = max(toolResultMin, toolResultsBudget/results)
	}

	var texts, images []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				texts = append(texts, b.Text)
			}
		case "image":
			if u := imageSourceURL(b); u != "" {
				images = append(images, u)
			} else {
				texts = append(texts, "[image omitted: unsupported source]")
			}
		case "tool_result":
			t, imgs := renderToolResult(b, calls[b.ToolUseID], budget)
			texts = append(texts, t)
			images = append(images, imgs...)
		case "document":
			texts = append(texts, documentText(b))
		default:
			if t := strings.TrimSpace(b.Text); t != "" {
				texts = append(texts, t)
			}
		}
	}
	parts := []map[string]any{{"type": "text", "text": strings.Join(texts, "\n\n")}}
	for _, u := range images {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return stringContent(strings.Join(texts, "\n\n"))
	}
	return StringOrArray{raw: raw}
}

// imageSourceURL 把 Anthropic 的图片来源换成 URL（base64 → data URL）。
func imageSourceURL(b anthropicBlock) string {
	if b.Source == nil {
		return ""
	}
	switch b.Source.Type {
	case "base64":
		if b.Source.Data != "" && b.Source.MediaType != "" {
			return "data:" + b.Source.MediaType + ";base64," + b.Source.Data
		}
	case "url":
		return b.Source.URL
	}
	return ""
}

// documentText 是文档块的文本（纯文本文档给原文，PDF 之类转发不了，注明一句）。
func documentText(b anthropicBlock) string {
	if b.Source != nil && b.Source.Type == "text" {
		return "[document]\n" + b.Source.Data + "\n[/document]"
	}
	return "[document omitted: the gateway cannot forward this document type]"
}

// renderToolResult 把一条工具结果渲染成 [CLIENT RESULT] 文本，图片另外返回。
func renderToolResult(b anthropicBlock, call anthropicBlock, budget int) (string, []string) {
	if call.Name == invalidToolName {
		return "[CLIENT RESULT id=" + b.ToolUseID + " ERROR]\n" + invalidCallFeedback(call.Input) + "\n[/CLIENT RESULT]", nil
	}
	head := "[CLIENT RESULT"
	if call.Name != "" {
		head += " tool=" + call.Name
	}
	if b.ToolUseID != "" {
		head += " id=" + b.ToolUseID
	}
	if b.IsError {
		head += " ERROR"
	}
	head += "]"

	var texts, images []string
	for _, c := range decodeAnthropicContent(b.Content) {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "image":
			if u := imageSourceURL(c); u != "" {
				images = append(images, u)
				texts = append(texts, "[image attached to this message]")
			}
		case "tool_reference":
			texts = append(texts, "[tool loaded: "+c.ToolName+"]")
		default:
			texts = append(texts, string(c.raw))
		}
	}
	body := truncateMiddle(strings.Join(texts, "\n"), budget)
	if strings.TrimSpace(body) == "" && len(images) == 0 {
		body = "(no output)"
	}
	return head + "\n" + body + "\n[/CLIENT RESULT]", images
}

// invalidCallFeedback 是"调用块解析不了"的纠错说明（原因在那次 tool_use 的 input.error 里）。
func invalidCallFeedback(input json.RawMessage) string {
	var in struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(input, &in)
	reason := in.Error
	if reason == "" {
		reason = "it is not valid JSON"
	}
	return "Your " + localToolFence + " block was NOT executed: " + reason + ". " +
		`Re-emit the call now as one strict-JSON object {"name": "<tool>", "input": {...}} inside a ` + localToolFence +
		` fence (escape " as \" and newlines as \n inside strings). Do not ask the user to resend anything.`
}

// truncateMiddle 把文本截到 n 字节以内：保留开头三分之二与结尾三分之一，中间注明省略了多少。
func truncateMiddle(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	head := cutUTF8(s, n*2/3)
	tailStart := len(s) - n/3
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return head + fmt.Sprintf("\n…[%d bytes of output omitted by the gateway]…\n", tailStart-len(head)) + s[tailStart:]
}

// withClientToolReminder 把桥的提醒接在 user 条目最后一个文本块末尾（返回副本）。
func withClientToolReminder(it prism.InputItem) prism.InputItem {
	it.Content = append([]prism.InputContent(nil), it.Content...)
	for i := len(it.Content) - 1; i >= 0; i-- {
		if t := it.Content[i].Type; t == "" || t == prism.BlockInputText {
			it.Content[i].Text += clientToolReminder
			return it
		}
	}
	it.Content = append(it.Content, prism.InputContent{Type: prism.BlockInputText, Text: strings.TrimSpace(clientToolReminder)})
	return it
}

// addClientToolReminder 给本轮消息加上桥的提醒：全量 input 的最后一条 user 与原生续接的本轮消息。
// 提醒不进下一轮的历史，续接比对用不带它的文本。
func addClientToolReminder(input []prism.InputItem, conv *nativeConversation) {
	for i := len(input) - 1; i >= 0; i-- {
		if strings.EqualFold(input[i].Role, "user") {
			input[i] = withClientToolReminder(input[i])
			break
		}
	}
	if conv != nil {
		conv.currentText = itemText(conv.current)
		conv.current = withClientToolReminder(conv.current)
	}
}

// ---------------------------- 会话键 ----------------------------

// claudeCodeSessionID 取 Claude Code 的会话 ID：请求头 X-Claude-Code-Session-Id（2.x），
// 或 metadata.user_id 里的 session（2.1 起是 JSON {"session_id":…}，更早是 "user_…_session_<uuid>"）。
func claudeCodeSessionID(r *http.Request, body map[string]json.RawMessage) string {
	if v := strings.TrimSpace(r.Header.Get("X-Claude-Code-Session-Id")); v != "" {
		return v
	}
	uid := anthropicUserID(body)
	if strings.HasPrefix(strings.TrimSpace(uid), "{") {
		var m struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(uid), &m) == nil {
			return strings.TrimSpace(m.SessionID)
		}
	}
	if i := strings.LastIndex(uid, "_session_"); i >= 0 {
		return strings.TrimSpace(uid[i+len("_session_"):])
	}
	return ""
}

// firstUserFingerprint 是第一条 user 消息正文的短指纹。
//
// 同一个 Claude Code 会话里不止一段对话：主对话之外还有子代理（Agent 工具）和各种旁路请求，
// 它们带着同一个会话 ID。会话 ID 再加上首条消息的指纹，每段对话各绑各的上游会话。
func firstUserFingerprint(msgs []AnthropicMessage) string {
	for _, m := range msgs {
		if strings.EqualFold(strings.TrimSpace(m.Role), "user") {
			sum := sha256.Sum256([]byte(strings.TrimSpace(m.Content.Text())))
			return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:8])
		}
	}
	return ""
}

// isClaudeCodeAux 判断是否 Claude Code 的单发旁路请求（会话标题、话题判断之类）：不带工具、
// 只有一条消息。它们不属于任何一段对话，按伴生请求在专用项目里无状态地跑。
func isClaudeCodeAux(r *http.Request, req *AnthropicRequest) bool {
	return strings.Contains(strings.ToLower(r.UserAgent()), "claude-cli") && len(req.Tools) == 0 && len(req.Messages) == 1
}

// anthropicOutputSchema 取请求要求的 JSON 输出格式（output_format 或 output_config.format，
// type=json_schema），转成一句给上游的指令；上游不认这两个字段，不说它就回自由文本。
func anthropicOutputSchema(raw map[string]json.RawMessage) string {
	var format struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	if v, ok := raw["output_format"]; ok {
		_ = json.Unmarshal(v, &format)
	}
	if format.Type == "" {
		if v, ok := raw["output_config"]; ok {
			var oc struct {
				Format json.RawMessage `json:"format"`
			}
			if json.Unmarshal(v, &oc) == nil && len(oc.Format) > 0 {
				_ = json.Unmarshal(oc.Format, &format)
			}
		}
	}
	if format.Type != "json_schema" || len(format.Schema) == 0 {
		return ""
	}
	return "Reply with ONLY a JSON object (no code fence, no other text) that matches this JSON schema: " + string(format.Schema)
}

// ---------------------------- 上游回复 → 客户端 ----------------------------

// localToolCall 是从上游回复里解析出的一次客户端工具调用。
type localToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage // JSON 对象
}

// parseLocalToolCalls 拆出上游回复里的 ```local-tool 调用块，返回去掉调用块后的正文与调用（按出现顺序）。
//
// 收尾围栏取"使块内 JSON 解析得通"的第一个：文件内容里自己带 ``` 的行（写 Markdown 时常见）
// 不会把块截断。解析不了的块转成 invalidToolName 调用（input.error 写明原因）。
func parseLocalToolCalls(text string, tools []AnthropicTool) (string, []localToolCall) {
	lines := strings.SplitAfter(text, "\n")
	var prose strings.Builder
	var calls []localToolCall
	for i := 0; i < len(lines); {
		ticks, ok := localToolOpen(lines[i])
		if !ok {
			prose.WriteString(lines[i])
			i++
			continue
		}
		next := len(lines) // 下一个调用块的开头（候选收尾不越过它）
		var cands []int
		for k := i + 1; k < len(lines); k++ {
			if _, open := localToolOpen(lines[k]); open {
				next = k
				break
			}
			if isFenceClose(lines[k], ticks) {
				cands = append(cands, k)
			}
		}
		var parsed []localToolCall
		var perr error
		end := next
		if len(cands) == 0 {
			parsed, perr = decodeToolBlock(strings.Join(lines[i+1:next], ""), tools)
		}
		for n, k := range cands {
			p, err := decodeToolBlock(strings.Join(lines[i+1:k], ""), tools)
			if err == nil {
				parsed, perr, end = p, nil, k+1
				break
			}
			if n == 0 {
				perr, end = err, k+1
			}
		}
		if perr != nil {
			calls = append(calls, invalidToolCall(strings.Join(lines[i+1:end], ""), perr))
		} else {
			calls = append(calls, parsed...)
		}
		i = end
	}
	out := prose.String()
	if len(calls) > 0 {
		out = reBlankRun.ReplaceAllString(out, "\n\n")
	}
	return strings.TrimSpace(out), calls
}

var reBlankRun = regexp.MustCompile(`\n{3,}`)

// localToolOpen 判断一行是不是调用块的开头围栏（```local-tool，允许更多反引号），返回反引号数。
func localToolOpen(line string) (int, bool) {
	t := strings.TrimRight(line, "\r\n")
	t = strings.TrimLeft(t, " ")
	n := 0
	for n < len(t) && t[n] == '`' {
		n++
	}
	if n < 3 {
		return 0, false
	}
	tag := strings.ToLower(strings.TrimSpace(t[n:]))
	return n, tag == "local-tool" || tag == "local_tool"
}

// isFenceClose 判断一行是不是只有（至少 ticks 个）反引号的收尾围栏。
func isFenceClose(line string, ticks int) bool {
	t := strings.TrimSpace(line)
	return len(t) >= ticks && strings.Trim(t, "`") == ""
}

// invalidToolCall 把解析不了的调用块转成一次必然失败的调用（见 invalidToolName）。
func invalidToolCall(body string, err error) localToolCall {
	reason := "it is not valid JSON (" + err.Error() + ")"
	if m := reToolName.FindStringSubmatch(body); m != nil {
		reason = "the call to " + strconv.Quote(m[1]) + " is not valid JSON (" + err.Error() + ")"
	}
	return localToolCall{Name: invalidToolName, Input: json.RawMessage(marshalNoEscape(map[string]string{"error": reason}))}
}

var reToolName = regexp.MustCompile(`"(?:name|tool)"\s*:\s*"([^"\\]{1,64})"`)

// decodeToolBlock 解析一个调用块：严格 JSON 不行就修一遍再试（见 repairJSON）。
// 一个块里有多个对象（数组、逐行多个）时返回多次调用。
func decodeToolBlock(body string, tools []AnthropicTool) ([]localToolCall, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("the block is empty")
	}
	vals, err := decodeJSONValues(body)
	if err != nil {
		if fixed, ferr := decodeJSONValues(repairJSON(body)); ferr == nil {
			vals, err = fixed, nil
		}
	}
	if err != nil {
		return nil, err
	}
	var calls []localToolCall
	for _, v := range vals {
		cs, err := toolCallsFromJSON(v, tools)
		if err != nil {
			return nil, err
		}
		calls = append(calls, cs...)
	}
	if len(calls) == 0 {
		return nil, errors.New("the block contains no call")
	}
	return calls, nil
}

// decodeJSONValues 依次解析文本里的 JSON 值（数字保持原样）。
func decodeJSONValues(s string) ([]any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var out []any
	for {
		var v any
		err := dec.Decode(&v)
		if err != nil {
			if errors.Is(err, io.EOF) && len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		out = append(out, v)
	}
}

// toolCallsFromJSON 认几种常见写法：{"name","input"}、{"name","arguments"}（对象或 JSON 字符串）、
// {"tool","parameters"}、OpenAI 的 {"function":{"name","arguments"}}、参数直接平铺在顶层，以及它们的数组。
func toolCallsFromJSON(v any, tools []AnthropicTool) ([]localToolCall, error) {
	switch t := v.(type) {
	case []any:
		var out []localToolCall
		for _, x := range t {
			cs, err := toolCallsFromJSON(x, tools)
			if err != nil {
				return nil, err
			}
			out = append(out, cs...)
		}
		return out, nil
	case map[string]any:
		for _, k := range []string{"tool_calls", "calls"} {
			if arr, ok := t[k].([]any); ok {
				return toolCallsFromJSON(arr, tools)
			}
		}
		if fn, ok := t["function"].(map[string]any); ok {
			t = fn
		}
		name := ""
		nameKey := ""
		for _, k := range []string{"name", "tool", "tool_name", "recipient_name"} {
			if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
				name, nameKey = s, k
				break
			}
		}
		if name == "" {
			return nil, errors.New(`the call has no "name"`)
		}
		var input any
		found := false
		for _, k := range []string{"input", "arguments", "parameters", "args", "params"} {
			x, ok := t[k]
			if !ok {
				continue
			}
			found = true
			if s, isStr := x.(string); isStr {
				dec := json.NewDecoder(strings.NewReader(s))
				dec.UseNumber()
				if err := dec.Decode(&x); err != nil {
					return nil, fmt.Errorf("%q is a string that is not JSON", k)
				}
			}
			input = x
			break
		}
		if !found {
			// 参数直接平铺在顶层：{"name":"Read","file_path":"…"}
			flat := map[string]any{}
			for k, x := range t {
				if k != nameKey && k != "type" && k != "id" {
					flat[k] = x
				}
			}
			input = flat
		}
		if input == nil {
			input = map[string]any{}
		}
		if _, ok := input.(map[string]any); !ok {
			return nil, errors.New(`"input" must be a JSON object`)
		}
		return []localToolCall{{Name: normalizeToolName(name, tools), Input: json.RawMessage(marshalNoEscape(input))}}, nil
	}
	return nil, errors.New("the block must hold a JSON object")
}

func normalizeToolName(name string, tools []AnthropicTool) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "functions.")
	for _, t := range tools {
		if t.Name == name {
			return name
		}
	}
	for _, t := range tools {
		if strings.EqualFold(t.Name, name) {
			return t.Name
		}
	}
	return name
}

// repairJSON 修几类模型手写 JSON 的常见毛病（只在严格解析失败后才用）：
//   - 字符串里的原始换行、制表符等控制字符 → 转义；
//   - 非法转义（Windows 路径 "C:\Users" 里的 \U）→ 字面反斜杠；\' → '；
//   - 字符串里没转义的双引号（HTML 属性 class="a" 之类）：后面紧跟的不是 , : } ] 就当正文转义；
//   - 对象 / 数组末尾多余的逗号。
func repairJSON(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 64)
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inStr {
			switch c {
			case '"':
				inStr = true
			case ',':
				j := i + 1
				for j < len(s) && strings.IndexByte(" \t\r\n", s[j]) >= 0 {
					j++
				}
				if j < len(s) && (s[j] == '}' || s[j] == ']') {
					continue
				}
			}
			sb.WriteByte(c)
			continue
		}
		switch c {
		case '\\':
			if i+1 < len(s) {
				nx := s[i+1]
				switch {
				case nx == 'u' && isHex4(s[i+2:]):
					sb.WriteString(`\u`)
					i++
					continue
				case nx != 'u' && strings.IndexByte(`"\/bfnrt`, nx) >= 0:
					sb.WriteByte('\\')
					sb.WriteByte(nx)
					i++
					continue
				case nx == '\'':
					sb.WriteByte('\'')
					i++
					continue
				}
			}
			sb.WriteString(`\\`)
		case '"':
			j := i + 1
			for j < len(s) && strings.IndexByte(" \t\r\n", s[j]) >= 0 {
				j++
			}
			if j >= len(s) || strings.IndexByte(",:}]", s[j]) >= 0 {
				inStr = false
				sb.WriteByte('"')
			} else {
				sb.WriteString(`\"`)
			}
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&sb, `\u%04x`, c)
			} else {
				sb.WriteByte(c)
			}
		}
	}
	return sb.String()
}

func isHex4(s string) bool {
	if len(s) < 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(s[i])) {
			return false
		}
	}
	return true
}

// bridgeToolUseIDs 给这轮的工具调用分配 tool_use ID。第一个 ID 里带着这轮上游原文的指纹：
// 客户端下一轮回传的是 [正文, tool_use…]，拼不回上游原文；弱会话键要连助手条目一起比对
// （见 native.go），就从 ID 里取回原文指纹（assistantReplay → ChatMessage.fp）。
func bridgeToolUseIDs(calls []localToolCall, upstreamText string) {
	fp := entryFingerprint(historyEntry{speaker: "Assistant", text: upstreamText})
	for i := range calls {
		if i == 0 {
			calls[i].ID = fmt.Sprintf("%s%016x%s", toolUseFPPrefix, fp, randHex(4))
		} else {
			calls[i].ID = "toolu_" + randHex(12)
		}
	}
}

// toolUseFingerprint 从 bridgeToolUseIDs 分配的 ID 里取回原文指纹（不是这种 ID 时为 0）。
func toolUseFingerprint(id string) uint64 {
	if !strings.HasPrefix(id, toolUseFPPrefix) || len(id) < len(toolUseFPPrefix)+16 {
		return 0
	}
	v, err := strconv.ParseUint(id[len(toolUseFPPrefix):len(toolUseFPPrefix)+16], 16, 64)
	if err != nil {
		return 0
	}
	return v
}

// ---------------------------- 沙箱文件兜底 ----------------------------

// reClientCwd 从客户端的 system 里取工作目录（Claude Code 的环境段落）。
var reClientCwd = regexp.MustCompile(`(?m)^[ \t]*-?[ \t]*(?:Primary working directory|Working directory):[ \t]*(\S.*?)[ \t]*$`)

func clientWorkingDir(system string) string {
	if m := reClientCwd.FindStringSubmatch(system); m != nil {
		return strings.Trim(m[1], "`")
	}
	return ""
}

// joinClientPath 把沙箱里的相对路径接到客户端工作目录下（Windows 目录用反斜杠）。
func joinClientPath(cwd, rel string) string {
	if strings.Contains(cwd, `\`) || (len(cwd) >= 2 && cwd[1] == ':') {
		return strings.TrimRight(cwd, `\/`) + `\` + strings.ReplaceAll(rel, "/", `\`)
	}
	return path.Join(cwd, rel)
}

func toolNamed(tools []AnthropicTool, name string) string {
	for _, t := range tools {
		if t.Name == name {
			return name
		}
	}
	return ""
}

// deltaFilesToClientTools 把上游写进沙箱的文件转成客户端的 Write / Edit 调用（上游没按桥的格式
// 调用工具、却在沙箱里写了文件时的兜底），返回调用与给用户看的说明。
// 新文件整份写入；修改按替换块逐个 Edit（客户端会校验原文，对不上就报错，不会改坏文件）；
// 删除不自动执行。
func deltaFilesToClientTools(files []prism.CodexDeltaFile, cwd string, tools []AnthropicTool) ([]localToolCall, string) {
	write, edit := toolNamed(tools, "Write"), toolNamed(tools, "Edit")
	if cwd == "" || write == "" {
		return nil, ""
	}
	var calls []localToolCall
	var skipped []string
	add := func(name string, input map[string]any) {
		calls = append(calls, localToolCall{Name: name, Input: json.RawMessage(marshalNoEscape(input))})
	}
	for _, f := range files {
		plan, err := planDeltaFile(f)
		if errors.Is(err, errIgnoredFile) {
			continue
		}
		if err != nil {
			skipped = append(skipped, f.FilePath+"（"+err.Error()+"）")
			continue
		}
		p := joinClientPath(cwd, plan.Path)
		switch plan.Kind {
		case editWrite:
			add(write, map[string]any{"file_path": p, "content": plan.Content})
		case editPatch:
			if edit == "" {
				skipped = append(skipped, plan.Path+"（客户端没有 Edit 工具）")
				continue
			}
			for _, hk := range plan.Hunks {
				if hk.Old == "" {
					skipped = append(skipped, plan.Path+"（一处纯插入的改动无法定位）")
					continue
				}
				add(edit, map[string]any{"file_path": p, "old_string": hk.Old, "new_string": hk.New})
			}
		case editDelete:
			skipped = append(skipped, plan.Path+"（上游删除了它，删除不自动执行）")
		}
	}
	note := ""
	if len(calls) > 0 {
		note = "[oaiprism] 上游把文件写进了云端沙箱，已转成本地文件操作交给客户端执行。"
	}
	if len(skipped) > 0 {
		note = strings.TrimSpace(note + "\n[oaiprism] 未转换的沙箱文件变更：" + strings.Join(skipped, "；"))
	}
	return calls, note
}
