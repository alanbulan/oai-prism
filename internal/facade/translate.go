package facade

import (
	"encoding/json"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 本文件把各家的 API 形态翻译成上游期望的 input 数组。
//
// 上游的 input 是"条目数组"，每个条目形如：
//
//	{"type":"message","role":"user"|"assistant","content":[{"type":"input_text","text":"..."}]}
//
// 三个必须照做的细节（都是从真实报文与前端源码确认的）：
//
//  1. 用户内容用 input_text，助手历史用 output_text。传错不会报错，
//     但模型会"看不见"这段内容 —— 属于静默失效，最难排查的一类。
//  2. 上游没有独立的 system / instructions 字段，input 数组里的 system 角色
//     就是它的 Context：服务端把**最后一条 system** 与最后一条 user 拼成
//     "Context:…User request:…"，其余条目全部丢弃（见 upstream_input.go）。
//     所以全部 system 必须合并成一条，也不能折成 user（那会顶替掉真正的提问）。
//  3. tools 不知道该放哪（真实前端请求体里没有它，工具是沙箱侧提供的），
//     因此默认塞进 metadata，属于**待验证**的处理。

// translateChatMessages 把 OpenAI messages 转成上游 input 条目。
//
// 全部 system / developer 合并成最前面的唯一一条，折叠的历史挂在它末尾；
// 调用方没给 system 时用 defaultSystem 兜底，两者都没有而又有历史时，
// 就单独为历史建一条 system —— 否则历史无处安放，整段丢失。
//
// promptLimit 是上游单条提示词的字节上限（0 不限）：历史放得下就全留，
// 放不下从最旧的开始裁（见 compress.go）。
func translateChatMessages(msgs []ChatMessage, defaultSystem string, promptLimit int) []prism.InputItem {
	items := make([]prism.InputItem, 0, len(msgs)+1)

	// 找到最后一条用户消息。
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		r := strings.ToLower(strings.TrimSpace(msgs[i].Role))
		if r == "user" || r == "" {
			lastUserIdx = i
			break
		}
	}

	var sysParts []string
	for _, m := range msgs {
		if isSystemRole(m.Role) {
			if t := strings.TrimSpace(m.Content.Text()); t != "" {
				sysParts = append(sysParts, t)
			}
		}
	}
	sysText := strings.Join(sysParts, "\n\n")
	if sysText == "" {
		sysText = strings.TrimSpace(defaultSystem)
	}

	// 最后一条用户消息之前的往轮对话折进 system：上游只读 system 与最后一条 user，
	// 中间条目全部丢弃。
	var history []historyEntry
	for i := 0; i < lastUserIdx; i++ {
		m := msgs[i]
		if isSystemRole(m.Role) {
			continue
		}
		txt := m.Content.Text()
		if strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			for _, tc := range m.ToolCalls {
				txt += "\n[tool_call] " + tc.Function.Name + "(" + tc.Function.Arguments + ")"
			}
		}
		history = append(history, historyEntry{speaker: speakerOf(m.Role), text: txt})
	}
	fixed := len(sysText)
	if lastUserIdx >= 0 {
		fixed += len(msgs[lastUserIdx].Content.Text())
	}
	historyText := renderHistory(history, historyBudget(promptLimit, fixed))

	if sysText = strings.TrimSpace(sysText + historyText); sysText != "" {
		items = append(items, prism.NewSystemItem(sysText))
	}

	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			// 已合并进最前面那条 system。
		case "assistant":
			items = append(items, assistantItem(m))
		case "tool", "function":
			// 工具结果对上游而言就是一段用户可见的上下文。
			//
			// 标注成 "[name result]" 而不是 "[name]"：只写工具名会让模型
			// 分不清这是"工具回传的结果"还是"用户提到的一个名字"。
			// 这个写法借鉴自 PrismOpenAIProxy，它比我的原实现更清楚。
			text := m.Content.Text()
			if m.Name != "" {
				text = "[" + m.Name + " result]\n" + text
			}
			items = append(items, prism.NewUserItem(text))
		case "user", "":
			items = append(items, prism.InputItem{
				Type:    "message",
				Role:    "user",
				Content: toInputContent(m.Content, true),
			})
		default:
			items = append(items, prism.InputItem{
				Type:    "message",
				Role:    role,
				Content: toInputContent(m.Content, true),
			})
		}
	}
	return items
}

// assistantItem 构造助手条目。
//
// 带 tool_calls 的消息不能只发正文：上游需要看到"它调用了什么、参数是什么"，
// 否则多轮工具编排会断链。这里把调用序列化成一段文本附在正文之后。
func assistantItem(m ChatMessage) prism.InputItem {
	body := m.Content.Text()
	if len(m.ToolCalls) > 0 {
		var sb strings.Builder
		sb.WriteString(body)
		for _, tc := range m.ToolCalls {
			sb.WriteString("\n[tool_call] ")
			sb.WriteString(tc.Function.Name)
			sb.WriteString("(")
			sb.WriteString(tc.Function.Arguments)
			sb.WriteString(")")
		}
		body = sb.String()
	}
	return prism.NewAssistantItem(body)
}

// toInputContent 归一化内容块。
//
// 上游输入端 input 数组中的所有文本块均为 input_text（与 PrismOpenAIProxy 对齐）；
// 非文本块（如 input_image）保留其类型名。
func toInputContent(s StringOrArray, isUser bool) []prism.InputContent {
	blockType := prism.BlockInputText
	if s.IsZero() {
		return []prism.InputContent{{Type: blockType, Text: ""}}
	}

	raw := s.Raw()
	if len(raw) > 0 && raw[0] == '"' {
		return []prism.InputContent{{Type: blockType, Text: s.Text()}}
	}

	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// 不是数组也不是字符串：退化成纯文本，保证不丢内容。
		return []prism.InputContent{{Type: blockType, Text: s.Text()}}
	}

	out := make([]prism.InputContent, 0, len(blocks))
	for _, b := range blocks {
		typ, _ := b["type"].(string)
		switch typ {
		case "text", "input_text", "output_text":
			text, _ := b["text"].(string)
			out = append(out, prism.InputContent{Type: blockType, Text: text})
		case "image_url", "input_image", "image":
			url, detail := imageURLAndDetail(b)
			if url == "" {
				// 拿不到 URL 就退化成文本 —— 至少不能把内容整个丢掉。
				if t := prism.FlattenContent(b); t != "" {
					out = append(out, prism.InputContent{Type: blockType, Text: t})
				}
				continue
			}
			out = append(out, prism.InputContent{
				Type:     "input_image",
				ImageURL: url,
				Detail:   detail,
			})
		case "refusal":
			text, _ := b["refusal"].(string)
			out = append(out, prism.InputContent{Type: "refusal", Text: text})
		default:
			text, _ := b["text"].(string)
			if text == "" {
				text = prism.FlattenContent(b)
			}
			out = append(out, prism.InputContent{Type: blockType, Text: text})
		}
	}
	if len(out) == 0 {
		out = append(out, prism.InputContent{Type: blockType, Text: s.Text()})
	}
	return out
}

// imageURLAndDetail 从图像块里取出 URL 与精细度，兼容三种写法。
//
//	{"type":"image_url","image_url":{"url":"…","detail":"high"}}   OpenAI 标准
//	{"type":"image_url","image_url":"…"}                            简化写法
//	{"type":"input_image","image_url":"…","detail":"auto"}          Responses 风格
//
// 必须覆盖这些形态：图像 URL 若取不到，我们就会发一个**空的 input_image**
// 给上游 —— 上游不报错，模型只是"看不见图"，属于最难定位的一类静默失效。
//
// detail 缺省回填 "auto"（对照 PrismOpenAIProxy transform.mjs:61,65）：
// 上游若按精细度做分档计费/裁剪，留空等于交给它自己猜，行为不可预期。
func imageURLAndDetail(b map[string]any) (string, string) {
	var url, detail string
	switch v := b["image_url"].(type) {
	case string:
		url = v
		detail, _ = b["detail"].(string)
	case map[string]any:
		url, _ = v["url"].(string)
		detail, _ = v["detail"].(string)
		if detail == "" {
			// 有些客户端把 detail 放在块这一层。
			detail, _ = b["detail"].(string)
		}
	}
	if url == "" {
		return "", ""
	}
	if detail == "" {
		detail = "auto"
	}
	return url, detail
}

// toolsMetadata 把工具定义塞进 metadata。
//
// 不放请求体顶层的原因：真实前端请求体里根本没有 tools 字段
// （工具由沙箱侧提供），贸然加顶层字段有被 400 拒绝的风险；
// metadata 是开放容器（前端往里塞过 JSON 字符串），容错度更高。
//
// **待验证**：拿到真实凭据后应当用一次抓包确认工具该放哪里。
func toolsMetadata(tools []ChatTool) []any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			out = append(out, map[string]any{"type": t.Type})
			continue
		}
		fn := map[string]any{
			"name":        t.Function.Name,
			"description": t.Function.Description,
		}
		if len(t.Function.Parameters) > 0 {
			var params map[string]any
			if err := json.Unmarshal(t.Function.Parameters, &params); err == nil {
				fn["parameters"] = params
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// translateAnthropicMessages 把 Anthropic messages 转成上游 input 条目。
func translateAnthropicMessages(msgs []AnthropicMessage, defaultSystem string, promptLimit int) []prism.InputItem {
	chat := make([]ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "assistant" {
			role = "user"
		}
		chat = append(chat, ChatMessage{Role: role, Content: m.Content})
	}
	return translateChatMessages(chat, defaultSystem, promptLimit)
}

// messagesFromResponsesInput 解析 Responses API 的 input 字段。
//
// input 支持三种形态：
//
//	"一段纯文本"
//	[{"role":"user","content":"..."}]
//	[{"type":"message","role":"user","content":[{"type":"input_text","text":"..."}]}]
func messagesFromResponsesInput(raw json.RawMessage, defaultSystem string, promptLimit int) []prism.InputItem {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return []prism.InputItem{prism.NewUserItem(s)}
		}
		return nil
	}

	if trimmed[0] == '{' {
		var one ChatMessage
		if err := json.Unmarshal(raw, &one); err == nil && one.Role != "" {
			return translateChatMessages([]ChatMessage{one}, defaultSystem, promptLimit)
		}
		return nil
	}

	// 先按 chat 消息数组试。
	var items []ChatMessage
	if err := json.Unmarshal(raw, &items); err == nil && len(items) > 0 && items[0].Role != "" {
		return translateChatMessages(items, defaultSystem, promptLimit)
	}

	// 再按带 type 的内容块数组试。
	//
	// 注意跳过 additional_tools / function_call / function_call_output 等
	// 非消息条目：Codex CLI 的 input 数组里混着工具声明与工具结果，
	// 把它们当消息翻译会产生空消息或把工具输出伪装成用户发言。
	var blocks []struct {
		Type    string        `json:"type"`
		Role    string        `json:"role"`
		Content StringOrArray `json:"content"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		chat := make([]ChatMessage, 0, len(blocks))
		for _, b := range blocks {
			switch b.Type {
			case "", "message":
			default:
				// additional_tools / function_call / custom_tool_call_output 等
				// 在 tool-bridge 模式单独处理；这里只保留真正的消息条目。
				continue
			}
			role := b.Role
			if role == "" {
				role = "user"
			}
			chat = append(chat, ChatMessage{Role: role, Content: b.Content})
		}
		if len(chat) > 0 {
			return translateChatMessages(chat, defaultSystem, promptLimit)
		}
	}
	return nil
}
