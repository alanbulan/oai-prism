package facade

import (
	"encoding/json"

	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/sse"
)

// 本文件是流式响应的编码层。
//
// 全部走手写拼接 + sse.AppendJSONString，而不是 json.Marshal 结构体。
// 原因：一次 4k token 的回答会产生上千个 chunk，每个 chunk 用
// json.Marshal 就要反射遍历一次结构体、做一次 map 分配、再拷一次字节。
// 手写版本把这三件事都省掉，实测单 chunk 编码开销下降一个数量级。
//
// 复用方式：调用方持有一个 []byte，反复 append 后传给 sse.Writer。
// 为了避免每个 chunk 都重新分配，本层的所有函数都接收 dst 并返回新切片。

// ChatChunkSpec 描述一个 stream chunk 的内容。
type ChatChunkSpec struct {
	ID        string
	Created   int64
	Model     string
	Role      string
	Content   string
	Reasoning string
	Finish    string
	HasFinish bool
	ToolCalls []ToolCall
	Usage     *prism.Usage
	// EmptyChoices 用于 OpenAI 的 include_usage 收尾帧：
	// 它要求 choices 为空数组、只带 usage。
	EmptyChoices bool
	// ConversationID 是上游会话 ID（通常在结束帧携带，方便流式客户端也能获取新会话 ID）。
	ConversationID string
}

// AppendChatChunk 编码一个 OpenAI chat.completion.chunk。
func AppendChatChunk(dst []byte, s ChatChunkSpec) []byte {
	dst = append(dst, `{"id":`...)
	dst = sse.AppendJSONString(dst, s.ID)
	dst = append(dst, `,"object":"chat.completion.chunk","created":`...)
	dst = sse.AppendInt(dst, s.Created)
	dst = append(dst, `,"model":`...)
	dst = sse.AppendJSONString(dst, s.Model)
	dst = append(dst, `,"choices":[`...)

	if !s.EmptyChoices {
		dst = append(dst, `{"index":0,"delta":{`...)
		first := true
		if s.Role != "" {
			dst = append(dst, `"role":`...)
			dst = sse.AppendJSONString(dst, s.Role)
			first = false
		}
		if s.Content != "" {
			if !first {
				dst = append(dst, ',')
			}
			dst = append(dst, `"content":`...)
			dst = sse.AppendJSONString(dst, s.Content)
			first = false
		}
		if s.Reasoning != "" {
			if !first {
				dst = append(dst, ',')
			}
			// reasoning_content 是非标准但被广泛支持的扩展字段
			// （DeepSeek / vLLM / 各类客户端都认），用来承载思维链。
			dst = append(dst, `"reasoning_content":`...)
			dst = sse.AppendJSONString(dst, s.Reasoning)
			first = false
		}
		if len(s.ToolCalls) > 0 {
			if !first {
				dst = append(dst, ',')
			}
			dst = append(dst, `"tool_calls":`...)
			b, _ := json.Marshal(s.ToolCalls)
			dst = append(dst, b...)
			first = false
		}
		dst = append(dst, `},"logprobs":null,"finish_reason":`...)
		if s.HasFinish {
			dst = sse.AppendJSONString(dst, s.Finish)
		} else {
			dst = append(dst, `null`...)
		}
		dst = append(dst, '}')
	}

	dst = append(dst, ']')

	if s.Usage != nil {
		dst = append(dst, `,"usage":{"prompt_tokens":`...)
		dst = sse.AppendInt(dst, int64(s.Usage.InputTokens))
		dst = append(dst, `,"completion_tokens":`...)
		dst = sse.AppendInt(dst, int64(s.Usage.OutputTokens))
		dst = append(dst, `,"total_tokens":`...)
		dst = sse.AppendInt(dst, int64(s.Usage.TotalTokens))
		dst = append(dst, `,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":`...)
		dst = sse.AppendInt(dst, int64(s.Usage.ReasoningTokens))
		dst = append(dst, `}}`...)
	}

	if s.ConversationID != "" {
		dst = append(dst, `,"prism_conversation_id":`...)
		dst = sse.AppendJSONString(dst, s.ConversationID)
	}

	dst = append(dst, `,"system_fingerprint":"fp_oaiprism"}`...)
	return dst
}

// ---------------------------- Anthropic ----------------------------

// AnthropicEvent 是 /v1/messages 流式事件的编码输入。
type AnthropicEvent struct {
	Type       string
	MessageID  string
	Model      string
	Text       string
	StopReason string
	Usage      *prism.Usage
	Index      int
	// ErrorType 是 error 事件的 error.type（默认 api_error）。
	ErrorType string
	// Block 是内容块类型："thinking" 或正文（空）；Signature 非空时是思考块的签名增量。
	Block     string
	Signature string
}

// AppendAnthropicEvent 编码一个 Anthropic SSE 事件（含 event: 行）。
//
// Anthropic 协议要求 event 名与 body 里的 type 一致，
// 且事件种类比 OpenAI 多得多（start / block_start / delta / block_stop / ...）。
func AppendAnthropicEvent(dst []byte, e AnthropicEvent) []byte {
	dst = append(dst, "event: "...)
	dst = append(dst, e.Type...)
	dst = append(dst, "\ndata: "...)

	switch e.Type {
	case "message_start":
		dst = append(dst, `{"type":"message_start","message":{"id":`...)
		dst = sse.AppendJSONString(dst, e.MessageID)
		dst = append(dst, `,"type":"message","role":"assistant","model":`...)
		dst = sse.AppendJSONString(dst, e.Model)
		dst = append(dst, `,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":`...)
		in := 0
		if e.Usage != nil {
			in = e.Usage.InputTokens
		}
		dst = sse.AppendInt(dst, int64(in))
		dst = append(dst, `,"output_tokens":0}}}`...)

	case "content_block_start":
		dst = append(dst, `{"type":"content_block_start","index":`...)
		dst = sse.AppendInt(dst, int64(e.Index))
		if e.Block == "thinking" {
			dst = append(dst, `,"content_block":{"type":"thinking","thinking":"","signature":""}}`...)
		} else {
			dst = append(dst, `,"content_block":{"type":"text","text":""}}`...)
		}

	case "content_block_delta":
		dst = append(dst, `{"type":"content_block_delta","index":`...)
		dst = sse.AppendInt(dst, int64(e.Index))
		switch {
		case e.Signature != "":
			dst = append(dst, `,"delta":{"type":"signature_delta","signature":`...)
			dst = sse.AppendJSONString(dst, e.Signature)
		case e.Block == "thinking":
			dst = append(dst, `,"delta":{"type":"thinking_delta","thinking":`...)
			dst = sse.AppendJSONString(dst, e.Text)
		default:
			dst = append(dst, `,"delta":{"type":"text_delta","text":`...)
			dst = sse.AppendJSONString(dst, e.Text)
		}
		dst = append(dst, `}}`...)

	case "content_block_stop":
		dst = append(dst, `{"type":"content_block_stop","index":`...)
		dst = sse.AppendInt(dst, int64(e.Index))
		dst = append(dst, '}')

	case "message_delta":
		dst = append(dst, `{"type":"message_delta","delta":{"stop_reason":`...)
		if e.StopReason != "" {
			dst = sse.AppendJSONString(dst, e.StopReason)
		} else {
			dst = append(dst, `null`...)
		}
		// 新版 Anthropic API 在 message_delta 里给出完整用量（累计值）
		in, out := 0, 0
		if e.Usage != nil {
			in, out = e.Usage.InputTokens, e.Usage.OutputTokens
		}
		dst = append(dst, `,"stop_sequence":null},"usage":{"input_tokens":`...)
		dst = sse.AppendInt(dst, int64(in))
		dst = append(dst, `,"output_tokens":`...)
		dst = sse.AppendInt(dst, int64(out))
		dst = append(dst, `}}`...)

	case "message_stop":
		dst = append(dst, `{"type":"message_stop"}`...)

	case "error":
		typ := e.ErrorType
		if typ == "" {
			typ = "api_error"
		}
		dst = append(dst, `{"type":"error","error":{"type":`...)
		dst = sse.AppendJSONString(dst, typ)
		dst = append(dst, `,"message":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, `}}`...)
	}

	dst = append(dst, "\n\n"...)
	return dst
}

// ---------------------------- Responses API ----------------------------

// ResponsesEvent 是 /v1/responses 流式事件的编码输入。
type ResponsesEvent struct {
	Type       string
	ResponseID string
	Model      string
	CreatedAt  int64
	ItemID     string
	Text       string
	Status     string
	Usage      *prism.Usage

	// ItemJSON / OutputJSON 供工具桥输出非 message 形状的条目
	// （如 custom_tool_call）：直接内嵌完整 JSON，避免为每种
	// item 形状各写一个硬编码分支。
	ItemJSON   string
	OutputJSON string

	// OutputIndex 是条目在 response.output 里的位置（reasoning 条目在前时正文是 1）。
	OutputIndex int
}

// appendItemRef 写条目内事件共用的 "item_id":…,"output_index":N。
func appendItemRef(dst []byte, e ResponsesEvent) []byte {
	dst = append(dst, `,"item_id":`...)
	dst = sse.AppendJSONString(dst, e.ItemID)
	dst = append(dst, `,"output_index":`...)
	return sse.AppendInt(dst, int64(e.OutputIndex))
}

// AppendResponsesEvent 编码一个 Responses API SSE 事件。
func AppendResponsesEvent(dst []byte, e ResponsesEvent) []byte {
	dst = append(dst, "event: "...)
	dst = append(dst, e.Type...)
	dst = append(dst, "\ndata: "...)

	switch e.Type {
	case "response.created", "response.in_progress":
		dst = append(dst, `{"type":`...)
		dst = sse.AppendJSONString(dst, e.Type)
		dst = append(dst, `,"response":{"id":`...)
		dst = sse.AppendJSONString(dst, e.ResponseID)
		dst = append(dst, `,"object":"response","created_at":`...)
		dst = sse.AppendInt(dst, e.CreatedAt)
		dst = append(dst, `,"status":"in_progress","model":`...)
		dst = sse.AppendJSONString(dst, e.Model)
		dst = append(dst, `,"output":[]}}`...)

	case "response.output_item.added", "response.output_item.done":
		dst = append(dst, `{"type":`...)
		dst = sse.AppendJSONString(dst, e.Type)
		dst = append(dst, `,"output_index":`...)
		dst = sse.AppendInt(dst, int64(e.OutputIndex))
		dst = append(dst, `,"item":`...)
		switch {
		case e.ItemJSON != "":
			dst = append(dst, e.ItemJSON...)
		case e.Type == "response.output_item.added":
			dst = append(dst, `{"id":`...)
			dst = sse.AppendJSONString(dst, e.ItemID)
			dst = append(dst, `,"type":"message","status":"in_progress","role":"assistant","content":[]}`...)
		default:
			dst = append(dst, `{"id":`...)
			dst = sse.AppendJSONString(dst, e.ItemID)
			dst = append(dst, `,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":`...)
			dst = sse.AppendJSONString(dst, e.Text)
			dst = append(dst, `,"annotations":[]}]}`...)
		}
		dst = append(dst, '}')

	case "response.content_part.added":
		dst = append(dst, `{"type":"response.content_part.added"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`...)

	case "response.output_text.delta":
		dst = append(dst, `{"type":"response.output_text.delta"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"content_index":0,"delta":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, '}')

	case "response.output_text.done":
		dst = append(dst, `{"type":"response.output_text.done"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"content_index":0,"text":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, '}')

	case "response.content_part.done":
		dst = append(dst, `{"type":"response.content_part.done"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"content_index":0,"part":{"type":"output_text","text":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, `,"annotations":[]}}`...)

	// 思考摘要（reasoning 条目）：Codex 据此显示模型的思考过程。
	case "response.reasoning_summary_part.added":
		dst = append(dst, `{"type":"response.reasoning_summary_part.added"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"summary_index":0,"part":{"type":"summary_text","text":""}}`...)

	case "response.reasoning_summary_text.delta":
		dst = append(dst, `{"type":"response.reasoning_summary_text.delta"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"summary_index":0,"delta":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, '}')

	case "response.reasoning_summary_text.done":
		dst = append(dst, `{"type":"response.reasoning_summary_text.done"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"summary_index":0,"text":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, '}')

	case "response.reasoning_summary_part.done":
		dst = append(dst, `{"type":"response.reasoning_summary_part.done"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"summary_index":0,"part":{"type":"summary_text","text":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, `}}`...)

	case "response.custom_tool_call_input.done":
		dst = append(dst, `{"type":"response.custom_tool_call_input.done"`...)
		dst = appendItemRef(dst, e)
		dst = append(dst, `,"input":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, '}')

	case "response.completed":
		dst = append(dst, `{"type":"response.completed","response":{"id":`...)
		dst = sse.AppendJSONString(dst, e.ResponseID)
		dst = append(dst, `,"object":"response","created_at":`...)
		dst = sse.AppendInt(dst, e.CreatedAt)
		dst = append(dst, `,"status":"completed","model":`...)
		dst = sse.AppendJSONString(dst, e.Model)
		if e.OutputJSON != "" {
			dst = append(dst, `,"output":`...)
			dst = append(dst, e.OutputJSON...)
		} else {
			dst = append(dst, `,"output":[{"id":`...)
			dst = sse.AppendJSONString(dst, e.ItemID)
			dst = append(dst, `,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":`...)
			dst = sse.AppendJSONString(dst, e.Text)
			dst = append(dst, `,"annotations":[]}]}]`...)
		}
		if e.Usage != nil {
			dst = append(dst, `,"usage":{"input_tokens":`...)
			dst = sse.AppendInt(dst, int64(e.Usage.InputTokens))
			dst = append(dst, `,"input_tokens_details":{"cached_tokens":0},"output_tokens":`...)
			dst = sse.AppendInt(dst, int64(e.Usage.OutputTokens))
			dst = append(dst, `,"output_tokens_details":{"reasoning_tokens":`...)
			dst = sse.AppendInt(dst, int64(e.Usage.ReasoningTokens))
			dst = append(dst, `},"total_tokens":`...)
			dst = sse.AppendInt(dst, int64(e.Usage.TotalTokens))
			dst = append(dst, '}')
		}
		dst = append(dst, `}}`...)

	case "error":
		dst = append(dst, `{"type":"error","code":"server_error","message":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, '}')

	case "response.failed":
		// Responses API 的标准失败终止事件。
		//
		// 为什么必须有它：只发自定义 "error" 事件时，Codex CLI 的
		// 状态机等不到任何 *终止* 事件（它只认 completed / failed /
		// incomplete），于是流一结束就报
		// "stream closed before response.completed" —— 用户看到的
		// 是"流莫名断了"，而真正的失败原因（上游 504/沙箱未就绪等）
		// 完全丢失。实测于 Codex CLI 0.154/0.159。
		dst = append(dst, `{"type":"response.failed","response":{"id":`...)
		dst = sse.AppendJSONString(dst, e.ResponseID)
		dst = append(dst, `,"object":"response","created_at":`...)
		dst = sse.AppendInt(dst, e.CreatedAt)
		dst = append(dst, `,"status":"failed","model":`...)
		dst = sse.AppendJSONString(dst, e.Model)
		dst = append(dst, `,"output":[],"error":{"code":`...)
		code := e.Status
		if code == "" {
			code = "server_error"
		}
		dst = sse.AppendJSONString(dst, code)
		dst = append(dst, `,"message":`...)
		dst = sse.AppendJSONString(dst, e.Text)
		dst = append(dst, `}}}`...)
	}

	dst = append(dst, "\n\n"...)
	return dst
}

// jsonRawToString 用于把 json.RawMessage 安全转成字符串（仅调试用）。
func jsonRawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
