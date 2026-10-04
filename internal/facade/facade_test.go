package facade

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/prism"
)

// ---------------------------- 消息翻译 ----------------------------

func mustSA(t *testing.T, raw string) StringOrArray {
	t.Helper()
	var s StringOrArray
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("解析 StringOrArray(%s): %v", raw, err)
	}
	return s
}

func TestStringOrArray(t *testing.T) {
	t.Run("字符串", func(t *testing.T) {
		s := mustSA(t, `"hello"`)
		if s.Text() != "hello" {
			t.Fatalf("Text = %q", s.Text())
		}
		if s.IsZero() {
			t.Fatal("不应被判为空")
		}
	})

	t.Run("内容块数组", func(t *testing.T) {
		s := mustSA(t, `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
		if s.Text() != "ab" {
			t.Fatalf("Text = %q", s.Text())
		}
	})

	t.Run("空值", func(t *testing.T) {
		var s StringOrArray
		if !s.IsZero() || s.Text() != "" {
			t.Fatal("零值应当为空")
		}
	})

	t.Run("原样回写", func(t *testing.T) {
		s := mustSA(t, `[{"type":"image_url","image_url":{"url":"x"}}]`)
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != `[{"type":"image_url","image_url":{"url":"x"}}]` {
			t.Fatalf("回写失真: %s", b)
		}
	})
}

func TestTranslateChatMessages(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: mustSA(t, `"你是助手"`)},
		{Role: "developer", Content: mustSA(t, `"补充规则"`)},
		{Role: "user", Content: mustSA(t, `"问题"`)},
		{Role: "assistant", Content: mustSA(t, `"回答"`)},
		{Role: "tool", Content: mustSA(t, `"工具结果"`), ToolCallID: "call_1"},
	}

	out := translateChatMessages(msgs, "", 0)

	// system 与 developer 合并成最前面的唯一一条 system：上游只读最后一条
	// system 当 Context（2026-10-04 双 system 暗号实测），分开发前一条会被丢掉。
	// 也不能折成 user —— 上游会把它当成"User request"，顶替真正的提问。
	if len(out) != 4 {
		t.Fatalf("条目数 = %d, want 4: %+v", len(out), out)
	}
	sys := out[0].Content[0].Text
	if out[0].Role != "system" || !strings.Contains(sys, "你是助手") || !strings.Contains(sys, "补充规则") {
		t.Fatalf("system 与 developer 应合并成一条 system: %+v", out[0])
	}
	if strings.Index(sys, "你是助手") > strings.Index(sys, "补充规则") {
		t.Fatalf("合并后应保持原顺序: %q", sys)
	}
	if out[1].Role != "user" || out[2].Role != "assistant" || out[3].Role != "user" {
		t.Fatalf("角色顺序错误: %+v", out)
	}
	// 块类型必须正确：输入序列中无论助手还是用户文本块均为 input_text（对齐 PrismOpenAIProxy）。
	if out[2].Content[0].Type != prism.BlockInputText {
		t.Errorf("助手内容块类型应为 input_text，得到 %q", out[2].Content[0].Type)
	}
	if out[1].Content[0].Type != prism.BlockInputText {
		t.Errorf("用户内容块类型应为 input_text，得到 %q", out[1].Content[0].Type)
	}
}

// TestTranslateChatMessages_InjectsDefaultSystem 验证兜底系统提示：调用方没给 system 时补上。
func TestTranslateChatMessages_InjectsDefaultSystem(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: mustSA(t, `"问题"`)}}

	out := translateChatMessages(msgs, "你是 Prism 的助手", 0)
	if len(out) != 2 {
		t.Fatalf("条目数 = %d, want 2（兜底 system + user）", len(out))
	}
	if out[0].Role != "system" || out[0].Content[0].Text != "你是 Prism 的助手" {
		t.Fatalf("兜底 system 未注入或角色错误: %+v", out[0])
	}

	// 调用方自带 system 时不应重复注入。
	withSys := []ChatMessage{
		{Role: "system", Content: mustSA(t, `"自带"`)},
		{Role: "user", Content: mustSA(t, `"问题"`)},
	}
	out2 := translateChatMessages(withSys, "兜底", 0)
	if len(out2) != 2 || out2[0].Content[0].Text != "自带" {
		t.Fatalf("自带 system 时不应再注入兜底: %+v", out2)
	}

	// 兜底为空则不注入。
	out3 := translateChatMessages(msgs, "", 0)
	if len(out3) != 1 {
		t.Fatalf("兜底为空时不应注入: %+v", out3)
	}
}

func TestTranslateChatMessages_SystemAlwaysPreserved(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: mustSA(t, `"规则"`)},
		{Role: "user", Content: mustSA(t, `"问题"`)},
	}
	out := translateChatMessages(msgs, "", 0)
	if len(out) != 2 {
		t.Fatalf("条目数 = %d, want 2", len(out))
	}
	if out[0].Role != "system" {
		t.Fatalf("system 角色必须保留: %+v", out[0])
	}
	if !strings.Contains(out[0].Content[0].Text, "规则") {
		t.Fatalf("system 内容丢失: %+v", out[0])
	}
}

func TestToInputContent_UserBlocks(t *testing.T) {
	got := toInputContent(mustSA(t, `[{"type":"text","text":"hi"}]`), true)
	if len(got) != 1 || got[0].Type != prism.BlockInputText || got[0].Text != "hi" {
		t.Fatalf("用户内容块错误: %+v", got)
	}
}

func TestToInputContent_AssistantBlocks(t *testing.T) {
	got := toInputContent(mustSA(t, `[{"type":"output_text","text":"hi"}]`), false)
	if len(got) != 1 || got[0].Type != prism.BlockInputText {
		t.Fatalf("助手内容块错误: %+v", got)
	}
}

func TestToInputContent_KeepsImages(t *testing.T) {
	got := toInputContent(mustSA(t, `[{"type":"image_url","image_url":{"url":"http://x/1.png"}}]`), true)
	if len(got) != 1 || got[0].Type != "input_image" {
		t.Fatalf("多模态块不应被丢弃或改写: %+v", got)
	}
	// 关键：URL 必须真的带上。
	//
	// 只断言 Type 是不够的 —— 曾经就是这样漏掉了一个"发空 input_image
	// 给上游"的 bug：模型看不见图，但客户端和服务端都不报错。
	// 静默失效比报错难查得多，所以这里必须断言到内容。
	if got[0].ImageURL != "http://x/1.png" {
		t.Fatalf("图像 URL 丢失（上游会收到一个空的 input_image）: %+v", got[0])
	}
}

// TestToInputContent_ImageVariants 覆盖三种图像块写法。
//
// 客户端生态里这三种都真实存在，取不到 URL 就等于把图丢了。
func TestToInputContent_ImageVariants(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantURL    string
		wantDetail string
	}{
		{
			name:       "OpenAI 标准：image_url 是对象",
			raw:        `[{"type":"image_url","image_url":{"url":"http://a/1.png","detail":"high"}}]`,
			wantURL:    "http://a/1.png",
			wantDetail: "high",
		},
		{
			// 没写 detail 时回填 "auto"（对齐 PrismOpenAIProxy 的行为）。
			name:       "简化写法：image_url 是字符串",
			raw:        `[{"type":"image_url","image_url":"http://b/2.png"}]`,
			wantURL:    "http://b/2.png",
			wantDetail: "auto",
		},
		{
			name:       "Responses 风格：detail 在块这一层",
			raw:        `[{"type":"input_image","image_url":"http://c/3.png","detail":"low"}]`,
			wantURL:    "http://c/3.png",
			wantDetail: "low",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := toInputContent(mustSA(t, c.raw), true)
			if len(got) != 1 {
				t.Fatalf("块数 = %d, want 1: %+v", len(got), got)
			}
			if got[0].Type != "input_image" {
				t.Errorf("Type = %q, want input_image", got[0].Type)
			}
			if got[0].ImageURL != c.wantURL {
				t.Errorf("ImageURL = %q, want %q", got[0].ImageURL, c.wantURL)
			}
			if got[0].Detail != c.wantDetail {
				t.Errorf("Detail = %q, want %q", got[0].Detail, c.wantDetail)
			}
		})
	}
}

// TestToInputContent_BrokenImageFallsBackToText 验证 URL 取不到时不静默丢内容。
func TestToInputContent_BrokenImageFallsBackToText(t *testing.T) {
	got := toInputContent(mustSA(t, `[{"type":"image_url","text":"图注"}]`), true)
	if len(got) != 1 || got[0].Text != "图注" {
		t.Fatalf("取不到 URL 时应退化成文本而不是丢弃: %+v", got)
	}
}

// TestToolMessageAnnotation 固化工具结果的标注格式。
//
// 借鉴 PrismOpenAIProxy 的写法：带 " result" 后缀。
// 只写 [name] 会让模型分不清"工具回传的结果"和"用户提到的名字"。
func TestToolMessageAnnotation(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "tool", Name: "get_weather", Content: mustSA(t, `"晴, 25度"`)},
	}
	out := translateChatMessages(msgs, "", 0)
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("工具结果应作为 user 上下文: %+v", out)
	}
	text := out[0].Content[0].Text
	if !strings.Contains(text, "get_weather result") {
		t.Errorf("标注缺少 result 后缀: %q", text)
	}
	if !strings.Contains(text, "晴, 25度") {
		t.Errorf("工具结果内容丢失: %q", text)
	}
}

func TestAssistantItemWithToolCalls(t *testing.T) {
	m := ChatMessage{
		Role:    "assistant",
		Content: mustSA(t, `""`),
		ToolCalls: []ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: ToolCallFunc{Name: "get_weather", Arguments: `{"city":"上海"}`},
		}},
	}
	it := assistantItem(m)
	if it.Role != "assistant" || it.Content[0].Type != prism.BlockInputText {
		t.Fatalf("助手条目结构错误: %+v", it)
	}
	// 工具调用必须出现在文本里，否则多轮工具编排会断链。
	if !strings.Contains(it.Content[0].Text, "get_weather") ||
		!strings.Contains(it.Content[0].Text, "上海") {
		t.Fatalf("工具调用信息丢失: %q", it.Content[0].Text)
	}
}

func TestToolsMetadata(t *testing.T) {
	tools := []ChatTool{
		{
			Type: "function",
			Function: ToolFuncSpec{
				Name:        "search",
				Description: "搜索",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		},
		{Type: "web_search"},
	}
	out := toolsMetadata(tools)
	if len(out) != 2 {
		t.Fatalf("工具数 = %d", len(out))
	}
	fn := out[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Fatalf("自定义工具翻译错误: %+v", out[0])
	}
	if out[1].(map[string]any)["type"] != "web_search" {
		t.Fatalf("内置工具应原样保留: %+v", out[1])
	}
	if toolsMetadata(nil) != nil {
		t.Fatal("无工具时应返回 nil，避免塞进一个空字段")
	}
}

func TestMetadataWith(t *testing.T) {
	if metadataWith("tools", nil) != nil {
		t.Fatal("nil 值不应产生 metadata")
	}
	if metadataWith("tools", []any{}) != nil {
		t.Fatal("空数组不应产生 metadata：会变成看起来有值实则没值的伪字段")
	}
	m := metadataWith("tools", []any{1})
	if m["tools"] == nil {
		t.Fatal("有效值应当被保留")
	}
}

func TestToInputContent_KeepsImageType(t *testing.T) {
	in := `[{"type":"image_url","image_url":{"url":"http://x/1.png"}}]`
	got := toInputContent(mustSA(t, in), true)
	if len(got) != 1 || got[0].Type != "input_image" {
		t.Fatalf("多模态块不应被丢弃或改写: %+v", got)
	}
}

func TestMessagesFromResponsesInput(t *testing.T) {
	textOf := func(items []prism.InputItem) string {
		var sb strings.Builder
		for _, it := range items {
			for _, c := range it.Content {
				sb.WriteString(c.Text)
			}
		}
		return sb.String()
	}

	t.Run("纯字符串", func(t *testing.T) {
		items := messagesFromResponsesInput(json.RawMessage(`"你好"`), "", 0)
		if len(items) != 1 || textOf(items) != "你好" {
			t.Fatalf("解析错误: %+v", items)
		}
	})

	t.Run("消息数组", func(t *testing.T) {
		items := messagesFromResponsesInput(json.RawMessage(
			`[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]`), "", 0)
		if len(items) != 2 {
			t.Fatalf("条目数 = %d", len(items))
		}
	})

	t.Run("内容块数组", func(t *testing.T) {
		items := messagesFromResponsesInput(json.RawMessage(
			`[{"type":"message","role":"user","content":[{"type":"input_text","text":"块内容"}]}]`), "", 0)
		if len(items) != 1 || textOf(items) != "块内容" {
			t.Fatalf("解析错误: %+v", items)
		}
	})

	t.Run("空输入", func(t *testing.T) {
		if items := messagesFromResponsesInput(nil, "", 0); len(items) != 0 {
			t.Fatalf("应为空: %+v", items)
		}
	})
}

// ---------------------------- 编码 ----------------------------

func TestAppendChatChunk(t *testing.T) {
	got := string(AppendChatChunk(nil, ChatChunkSpec{
		ID: "chatcmpl-1", Created: 1700000000, Model: "gpt-5",
		Content: `含"引号"与
换行`,
	}))

	var parsed struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int            `json:"index"`
			Delta        map[string]any `json:"delta"`
			FinishReason any            `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, got)
	}
	if parsed.Object != "chat.completion.chunk" {
		t.Errorf("object = %q", parsed.Object)
	}
	if parsed.Created != 1700000000 {
		t.Errorf("created = %d", parsed.Created)
	}
	if parsed.Choices[0].Delta["content"] != "含\"引号\"与\n换行" {
		t.Errorf("content 转义错误: %v", parsed.Choices[0].Delta["content"])
	}
	if parsed.Choices[0].FinishReason != nil {
		t.Errorf("未完成时 finish_reason 应为 null，得到 %v", parsed.Choices[0].FinishReason)
	}
}

func TestAppendChatChunk_FinishAndUsage(t *testing.T) {
	got := string(AppendChatChunk(nil, ChatChunkSpec{
		ID: "c", Created: 1, Model: "m", HasFinish: true, Finish: "stop",
		Usage: &prism.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
	}))
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("缺少 finish_reason: %s", got)
	}
	if !strings.Contains(got, `"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,`) {
		t.Errorf("usage 编码错误: %s", got)
	}
	if !strings.Contains(got, `"completion_tokens_details":{"reasoning_tokens":0}`) {
		t.Errorf("usage 缺少 completion_tokens_details: %s", got)
	}
}

func TestAppendChatChunk_ConversationID(t *testing.T) {
	got := string(AppendChatChunk(nil, ChatChunkSpec{
		ID: "c", Created: 1, Model: "m", HasFinish: true, Finish: "stop",
		ConversationID: "conv-12345",
	}))
	if !strings.Contains(got, `"prism_conversation_id":"conv-12345"`) {
		t.Errorf("结束帧应包含 prism_conversation_id: %s", got)
	}
}

func TestAppendChatChunk_EmptyChoicesForUsageFrame(t *testing.T) {
	got := string(AppendChatChunk(nil, ChatChunkSpec{
		ID: "c", Created: 1, Model: "m", EmptyChoices: true,
		Usage: &prism.Usage{TotalTokens: 5},
	}))
	if !strings.Contains(got, `"choices":[]`) {
		t.Errorf("include_usage 收尾帧的 choices 必须是空数组: %s", got)
	}
}

func TestAppendChatChunk_Reasoning(t *testing.T) {
	got := string(AppendChatChunk(nil, ChatChunkSpec{
		ID: "c", Created: 1, Model: "m", Reasoning: "思考中",
	}))
	if !strings.Contains(got, `"reasoning_content":"思考中"`) {
		t.Errorf("思维链字段缺失: %s", got)
	}
	if strings.Contains(got, `"content"`) {
		t.Errorf("纯思维链帧不应带 content 字段（会让客户端把推理当正文渲染）: %s", got)
	}
}

func TestAppendAnthropicEvent_AllTypes(t *testing.T) {
	d := &prism.Usage{InputTokens: 5, OutputTokens: 7}
	cases := []struct {
		ev   AnthropicEvent
		want []string
	}{
		{AnthropicEvent{Type: "message_start", MessageID: "msg_1", Model: "gpt-5", Usage: d},
			[]string{"event: message_start", `"id":"msg_1"`, `"input_tokens":5`}},
		{AnthropicEvent{Type: "content_block_start"},
			[]string{"event: content_block_start", `"type":"text"`}},
		{AnthropicEvent{Type: "content_block_delta", Text: "hi"},
			[]string{"event: content_block_delta", `"text_delta"`, `"text":"hi"`}},
		{AnthropicEvent{Type: "content_block_stop"},
			[]string{"event: content_block_stop"}},
		{AnthropicEvent{Type: "message_delta", StopReason: "end_turn", Usage: d},
			[]string{"event: message_delta", `"stop_reason":"end_turn"`, `"input_tokens":5`, `"output_tokens":7`}},
		{AnthropicEvent{Type: "message_stop"},
			[]string{"event: message_stop"}},
	}
	for _, c := range cases {
		got := string(AppendAnthropicEvent(nil, c.ev))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s 事件缺少 %q\n%s", c.ev.Type, w, got)
			}
		}
		// 每个事件都必须以双换行结尾，否则 SSE 解析器会挂住。
		if !strings.HasSuffix(got, "\n\n") {
			t.Errorf("%s 事件未以空行结尾: %q", c.ev.Type, got)
		}
		// data 行必须是合法 JSON。
		data := got[strings.Index(got, "data: ")+6:]
		var v any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &v); err != nil {
			t.Errorf("%s 事件的 data 不是合法 JSON: %v\n%s", c.ev.Type, err, data)
		}
	}
}

func TestAppendResponsesEvent(t *testing.T) {
	d := &prism.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}
	for _, typ := range []string{
		"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed",
	} {
		got := string(AppendResponsesEvent(nil, ResponsesEvent{
			Type: typ, ResponseID: "resp_1", Model: "gpt-5", CreatedAt: 1,
			ItemID: "msg_1", Text: "内容", Usage: d,
		}))
		if !strings.HasPrefix(got, "event: "+typ+"\n") {
			t.Errorf("%s 的 event 行错误: %q", typ, got)
		}
		data := got[strings.Index(got, "data: ")+6:]
		var v map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &v); err != nil {
			t.Errorf("%s 的 data 不是合法 JSON: %v\n%s", typ, err, data)
		}
		if v["type"] != typ {
			t.Errorf("%s 的 body.type = %v", typ, v["type"])
		}
	}
}

// ---------------------------- 其它 ----------------------------

func TestNewID_UniqueAndSafe(t *testing.T) {
	seen := make(map[string]struct{}, 10000)
	for i := 0; i < 10000; i++ {
		id := newID("chatcmpl-")
		if !strings.HasPrefix(id, "chatcmpl-") {
			t.Fatalf("前缀丢失: %s", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("ID 重复: %s", id)
		}
		seen[id] = struct{}{}
		// 可能被放进 JSON 与 URL，字符集必须安全。
		for _, c := range id {
			if !strings.ContainsRune(idAlphabet+"-", c) {
				t.Fatalf("ID 含不安全字符 %q: %s", c, id)
			}
		}
	}
}

func TestConversationKey(t *testing.T) {
	req := mustRequest(t, nil)
	msgs := []ChatMessage{
		{Role: "system", Content: mustSA(t, `"系统提示"`)},
		{Role: "user", Content: mustSA(t, `"第一个问题"`)},
	}

	k1 := conversationKey(req, nil, msgs)
	k2 := conversationKey(req, nil, msgs)
	if k1 != k2 {
		t.Fatal("同一会话的指纹必须稳定")
	}

	// 换一条 user 消息 -> 不同会话。
	msgs2 := []ChatMessage{
		{Role: "system", Content: mustSA(t, `"系统提示"`)},
		{Role: "user", Content: mustSA(t, `"另一个问题"`)},
	}
	if conversationKey(req, nil, msgs2) == k1 {
		t.Fatal("不同会话不应产生相同指纹")
	}

	// user 字段优先。
	body := map[string]json.RawMessage{"user": json.RawMessage(`"u-42"`)}
	if got := conversationKey(req, body, msgs); got != "u:u-42" {
		t.Fatalf("user 字段未优先: %q", got)
	}

	// 显式头优先于一切。
	req2 := mustRequest(t, map[string]string{HeaderSession: "sess-1"})
	if got := conversationKey(req2, body, msgs); got != "h:sess-1" {
		t.Fatalf("显式头未优先: %q", got)
	}

	// 全都拿不到 -> 空串（上层退化为轮转分桶）。
	if got := conversationKey(req, nil, nil); got != "" {
		t.Fatalf("无信息时应返回空串，得到 %q", got)
	}
}

// 输入计数口径：每条消息 3 个帧 token + 角色名 + 正文，末尾 3 个回复引导 token
// （openai-cookbook num_tokens_from_messages 同款）；正文用 o200k_base 精确编码。
func TestCountInputTokens(t *testing.T) {
	if got := countInputTokens(nil); got != 0 {
		t.Fatalf("空输入应为 0，得到 %d", got)
	}
	items := []prism.InputItem{
		prism.NewSystemItem("hello world"), // 3 + system(1) + 2
		prism.NewUserItem("你好世界"),          // 3 + user(1) + 2
	}
	if got, want := countInputTokens(items), 6+6+3; got != want {
		t.Fatalf("countInputTokens = %d, want %d", got, want)
	}

	// 图片块按视觉规则计费（远程链接取不到尺寸，按 1024×1024 高精度 765 计）；
	// input_file 计文件名。
	img := prism.InputItem{Type: "message", Role: "user", Content: []prism.InputContent{
		{Type: "input_text", Text: "hello world"},
		{Type: "input_image", ImageURL: "https://example.com/a.png", Detail: "low"},
		{Type: "input_file", Filename: "user"},
	}}
	if got, want := countInputTokens([]prism.InputItem{img}), 3+1+2+85+1+3; got != want {
		t.Fatalf("含图片的 countInputTokens = %d, want %d", got, want)
	}

	// 异步计数与同步一致
	if got := countInputAsync(items)(); got != 15 {
		t.Fatalf("countInputAsync = %d, want 15", got)
	}
}

// 输出计数：正文 + 推理文本，推理部分单列 reasoning_tokens（已含在输出内）。
func TestMeasuredUsage(t *testing.T) {
	u := measuredUsage(15, &RunResult{Text: "Hello, world!", Reasoning: "你好世界"})
	if u.InputTokens != 15 || u.OutputTokens != 6 || u.ReasoningTokens != 2 || u.TotalTokens != 21 {
		t.Fatalf("measuredUsage = %+v", u)
	}
	cu := newChatUsage(u)
	if cu.CompletionTokensDetails == nil || cu.CompletionTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("chat usage 缺少 reasoning_tokens: %+v", cu)
	}
	if ru := newResponsesUsage(u); ru.OutputTokensDetails.ReasoningTokens != 2 || ru.TotalTokens != 21 {
		t.Fatalf("responses usage 错误: %+v", ru)
	}
}

func TestMapError(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
	}{
		{&creds.APIError{Op: "x", Status: 401}, 502}, // 上游凭据失效 -> 网关错误，不是调用方的 401
		{&creds.APIError{Op: "x", Status: 403}, 502},
		{&creds.APIError{Op: "x", Status: 429}, 429},
		{&creds.APIError{Op: "x", Status: 500}, 502},
		{ErrPollTimeout, 504},
	}
	for _, c := range cases {
		status, _, msg := mapError(c.err)
		if status != c.wantStatus {
			t.Errorf("mapError(%v) 状态码 = %d, want %d", c.err, status, c.wantStatus)
		}
		if msg == "" {
			t.Errorf("mapError(%v) 的消息不应为空", c.err)
		}
	}
}

func TestFinishReason(t *testing.T) {
	if finishReason(nil) != "stop" {
		t.Fatal("没有工具调用应给出 stop")
	}
	if finishReason([]ToolCall{{ID: "call_1"}}) != "tool_calls" {
		t.Fatal("有工具调用应给出 tool_calls")
	}
}

// TestFinishReason_IgnoredDeltaFilesStayStop 复现 2026-10-04 实测：沙箱每轮都把
// Prism 写进工作区的 AGENTS.md 报成新增文件，映射层滤掉它之后没有任何工具调用，
// finish_reason 必须是 stop —— 否则客户端会去执行一个不存在的工具调用。
func TestFinishReason_IgnoredDeltaFilesStayStop(t *testing.T) {
	files := []prism.CodexDeltaFile{{FilePath: "AGENTS.md", Status: "added"}}
	calls := MapDeltaFilesToToolCalls(files, nil)
	if len(calls) != 0 {
		t.Fatalf("AGENTS.md 不应映射成工具调用: %+v", calls)
	}
	if got := finishReason(calls); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop", got)
	}
}

// ---------------------------- 客户端 metadata 透传 ----------------------------

// rawFields 把 JSON 解成 passthrough 用的原始字段表。
func rawFields(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("测试体不是合法 JSON: %v", err)
	}
	return raw
}

// TestClientMetadata_FiltersReserved 验证保留键被挡住、普通键放行。
//
// 这是安全边界：放行 model / projectId / sandbox_* 等于让调用方冒充网关
// （换模型、挪沙箱、顶替身份）。同时普通键必须放行 —— 否则又是静默失效。
func TestClientMetadata_FiltersReserved(t *testing.T) {
	md := clientMetadata(rawFields(t, `{"metadata":{
		"trace_id":"t-1",
		"model":"evil-model",
		"projectId":"evil-project",
		"user_id":"evil-user",
		"sandbox_url":"http://evil",
		"frontend_origin":"http://evil",
		"tools":"evil-tools",
		"prism_conversation_id":"evil-conv",
		"prism_previous_response_id":"evil-prev"
	}}`))
	if _, bad := md["model"]; bad {
		t.Error("model 是保留键，不应透传")
	}
	for _, k := range []string{"projectId", "user_id", "sandbox_url", "frontend_origin", "tools", "prism_conversation_id", "prism_previous_response_id"} {
		if _, bad := md[k]; bad {
			t.Errorf("%s 是保留键，不应透传", k)
		}
	}
	if md["trace_id"] != "t-1" {
		t.Errorf("普通键应放行，得到 %+v", md)
	}
}

func TestClientMetadata_EmptyReturnsNil(t *testing.T) {
	if got := clientMetadata(rawFields(t, `{}`)); got != nil {
		t.Errorf("无 metadata 时应返回 nil，得到 %+v", got)
	}
	if got := clientMetadata(rawFields(t, `{"metadata":{"model":"x"}}`)); got != nil {
		t.Errorf("只剩保留键时应返回 nil，得到 %+v", got)
	}
	if got := clientMetadata(rawFields(t, `{"metadata":"not-an-object"}`)); got != nil {
		t.Errorf("metadata 不是对象时应返回 nil，得到 %+v", got)
	}
}

// TestMetadataEffort 覆盖 effort 的第三级回落：metadata.reasoning_effort。
func TestMetadataEffort(t *testing.T) {
	if got := metadataEffort(rawFields(t, `{"metadata":{"reasoning_effort":"xhigh"}}`)); got != "xhigh" {
		t.Errorf("metadataEffort = %q, want xhigh", got)
	}
	if got := metadataEffort(rawFields(t, `{}`)); got != "" {
		t.Errorf("无 metadata 时应为空，得到 %q", got)
	}
	if got := metadataEffort(rawFields(t, `{"metadata":{"reasoning_effort":"  "}}`)); got != "" {
		t.Errorf("空白值应视为空，得到 %q", got)
	}
}

// TestMergeMetadata_InjectedWins 验证网关注入的键不被客户端覆盖。
func TestMergeMetadata_InjectedWins(t *testing.T) {
	got := mergeMetadata(map[string]any{"a": 1}, map[string]any{"a": 2, "b": 3})
	if got["a"] != 2 {
		t.Errorf("网关注入应覆盖客户端: %+v", got)
	}
	if got["b"] != 3 {
		t.Errorf("注入的键应保留: %+v", got)
	}
}

// ---------------------------- 会话 ID 透传 ----------------------------

func TestConversationIDFrom(t *testing.T) {
	// 1) 请求头优先（兼容 PrismOpenAIProxy 客户端生态）
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("x-prism-conversation-id", "conv-header")
	if got := conversationIDFrom(r, nil); got != "conv-header" {
		t.Errorf("应从请求头取到，得到 %q", got)
	}

	// 2) 请求体 conversation_id
	if got := conversationIDFrom(r, rawFields(t, `{"conversation_id":"conv-body"}`)); got != "conv-header" {
		t.Errorf("请求头优先级应高于请求体，得到 %q", got)
	}
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := conversationIDFrom(r2, rawFields(t, `{"conversationId":"conv-camel"}`)); got != "conv-camel" {
		t.Errorf("camelCase 也应识别，得到 %q", got)
	}

	// 3) metadata 里的 prism_conversation_id（有些客户端塞在 metadata 里）
	if got := conversationIDFrom(r2, rawFields(t, `{"metadata":{"prism_conversation_id":"conv-meta"}}`)); got != "conv-meta" {
		t.Errorf("应从 metadata 取到，得到 %q", got)
	}

	// 4) 都没有时为空
	if got := conversationIDFrom(r2, rawFields(t, `{}`)); got != "" {
		t.Errorf("无来源时应为空，得到 %q", got)
	}
}

// ---------------------------- /v1/models 的 label ----------------------------

func TestHandleModels_Label(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	h.cfg.Facade.Models = map[string]config.ModelMapping{
		"gpt-5":      {Model: "gpt-5", Label: "GPT-5"},
		"gpt-5-high": {Model: "gpt-5", ReasoningEffort: "high"}, // 无 label
	}
	h.cfg.Facade.DefaultModel = "gpt-5"

	rec := httptest.NewRecorder()
	h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var out ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range out.Data {
		byID[m.ID] = m
	}
	if byID["gpt-5"].Name != "GPT-5" {
		t.Errorf("gpt-5 的 name = %q, want GPT-5", byID["gpt-5"].Name)
	}
	if byID["gpt-5-high"].Name != "" {
		t.Errorf("未配 label 的模型不应带 name，得到 %q", byID["gpt-5-high"].Name)
	}
}

// ---------------------------- 测试辅助 ----------------------------

func mustRequest(t *testing.T, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// ---------------------------- Issue #256 综合回归测试 ----------------------------

func TestIssue256_PendingJournal(t *testing.T) {
	j := NewPendingJournal()
	reqID := "req_test_256"
	convID := "cdx_conv_256"
	acctID := "acct_1"
	projectID := "proj_1"

	j.RecordStart(reqID, convID, acctID, projectID, []byte(`{"turn":1}`))

	entry, ok := j.Get(reqID)
	if !ok || entry.Status != "started" {
		t.Fatalf("Journal record start 失败: %+v", entry)
	}

	j.UpdateState(reqID, []byte(`{"turn":2}`), "pending")
	entry2, _ := j.Get(reqID)
	if entry2.Status != "pending" || string(entry2.TurnState) != `{"turn":2}` {
		t.Fatalf("Journal update state 失败: %+v", entry2)
	}

	j.MarkTerminal(reqID, "completed", "final answer", nil)
	entry3, _ := j.Get(reqID)
	if entry3.Status != "completed" || entry3.FinalText != "final answer" {
		t.Fatalf("Journal mark terminal 失败: %+v", entry3)
	}
}

func TestIssue256_ToolBridge_CallIDPreserved(t *testing.T) {
	inputJSON := `[
		{"type":"additional_tools","tools":[]},
		{"type":"message","role":"user","content":"list files"},
		{"type":"custom_tool_call","name":"exec_command","call_id":"ctc_123","input":"tools.exec_command({cmd:\"dir\"})"},
		{"type":"custom_tool_call_output","name":"exec_command","call_id":"ctc_123","output":"main.go\ngo.mod"}
	]`
	items := bridgeInputItems([]byte(inputJSON), "base sys")
	if len(items) == 0 {
		t.Fatalf("bridgeInputItems 解析失败")
	}

	// 检查最后一个用户条目是否包含 call_id 和工具名称
	var foundOutput bool
	for _, it := range items {
		for _, c := range it.Content {
			if strings.Contains(c.Text, "ctc_123") && strings.Contains(c.Text, "exec_command") {
				foundOutput = true
				break
			}
		}
	}
	if !foundOutput {
		t.Errorf("客户端结果回灌未正确包含 call_id 和 tool: %+v", items)
	}
}

func TestIssue256_SentinelTokenExtraction(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("openai-sentinel-token", "pow_token_xyz")

	hdr := extractSentinelToken(r)
	if hdr["openai-sentinel-token"] != "pow_token_xyz" {
		t.Errorf("未能正确提取 openai-sentinel-token: %v", hdr)
	}

	r2 := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r2.Header.Set("X-OpenAI-Sentinel-Token", "pow_token_upper")
	hdr2 := extractSentinelToken(r2)
	if hdr2["openai-sentinel-token"] != "pow_token_upper" {
		t.Errorf("未能正确提取 X-OpenAI-Sentinel-Token: %v", hdr2)
	}
}

func TestIssue256_ModelAliases(t *testing.T) {
	cfg := config.Default()
	h := &Handler{cfg: cfg}

	// 清单清理（2026-10-02）：别名/下线模型全部移除，prism-sol 原样透传。
	m1, _ := h.resolveModel("prism-sol", "")
	if m1 != "prism-sol" {
		t.Errorf("prism-sol 应原样透传（不再内置重定向）: got %q", m1)
	}

	// 已下线模型（astra 系）与历史别名（prism-sol / gpt-5 / 短别名等）已于
	// 2026-10-02 从对外清单整体移除（configs/config.yaml 与内置默认同步）：
	// /v1/models 不再展示，resolveModel 也不再重定向 —— 未知名原样透传，
	// 由上游决定行为。这里锁定的是"清单干净、无隐藏别名"这个契约。
	m2, _ := h.resolveModel("prism-astra", "")
	if m2 != "prism-astra" {
		t.Errorf("prism-astra 应原样透传（不再内置重定向）: got %q", m2)
	}

	m3, _ := h.resolveModel("gpt-6", "")
	if m3 != "gpt-6" {
		t.Errorf("gpt-6 应原样透传（不再内置重定向）: got %q", m3)
	}

	m4, _ := h.resolveModel("gpt-6.1-sol", "")
	if m4 != "gpt-6.1-sol" {
		t.Errorf("gpt-6.1-sol 映射错误: got %q", m4)
	}
}
