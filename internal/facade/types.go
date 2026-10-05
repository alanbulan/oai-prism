package facade

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ---------------------------- OpenAI Chat Completions ----------------------------

// ChatRequest 是 POST /v1/chat/completions 的请求体。
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`

	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	MaxTokens           *int     `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	N                   *int     `json:"n,omitempty"`
	Stop                any      `json:"stop,omitempty"`
	PresencePenalty     *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64 `json:"frequency_penalty,omitempty"`
	Seed                *int64   `json:"seed,omitempty"`
	User                string   `json:"user,omitempty"`
	Logprobs            *bool    `json:"logprobs,omitempty"`
	TopLogprobs         *int     `json:"top_logprobs,omitempty"`
	ResponseFormat      any      `json:"response_format,omitempty"`

	Tools      []ChatTool `json:"tools,omitempty"`
	ToolChoice any        `json:"tool_choice,omitempty"`

	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	Metadata map[string]any `json:"metadata,omitempty"`
}

// StreamOptions 控制流式细节。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// StringOrArray 表示 OpenAI 里既可能是字符串、也可能是内容块数组的字段。
//
// 这是必需的：新版 SDK 一律发数组形态（为了支持多模态），
// 老 SDK 与 curl 用法发字符串。用 any 会丢类型信息，
// 用 string 会直接解析失败——两者都不可接受。
type StringOrArray struct {
	raw json.RawMessage
}

// UnmarshalJSON 保留原始形态。
func (s *StringOrArray) UnmarshalJSON(b []byte) error {
	s.raw = append(s.raw[:0], b...)
	return nil
}

// MarshalJSON 原样回写。
func (s StringOrArray) MarshalJSON() ([]byte, error) {
	if len(s.raw) == 0 {
		return []byte("null"), nil
	}
	return s.raw, nil
}

// IsZero 判断是否未设置。
func (s StringOrArray) IsZero() bool { return len(s.raw) == 0 }

// Raw 返回原始 JSON。
func (s StringOrArray) Raw() json.RawMessage { return s.raw }

// Text 把内容拉平成纯文本。
func (s StringOrArray) Text() string {
	if len(s.raw) == 0 {
		return ""
	}
	if s.raw[0] == '"' {
		var v string
		if err := json.Unmarshal(s.raw, &v); err == nil {
			return v
		}
		return ""
	}
	// 数组：逐个块取 text 字段。
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(s.raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	return ""
}

// ChatMessage 是一条消息。
type ChatMessage struct {
	Role    string        `json:"role"`
	Content StringOrArray `json:"content,omitempty"`
	Name    string        `json:"name,omitempty"`

	// 工具调用相关（assistant 侧发起 / tool 侧应答）。
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`

	// 有些客户端用 reasoning_content 回传上一轮的思维链。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ToolCall 是一次函数调用。
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Index    *int         `json:"index,omitempty"`
	Function ToolCallFunc `json:"function"`
}

// ToolCallFunc 是函数名与参数。
type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatTool 是工具定义。
type ChatTool struct {
	Type     string       `json:"type"`
	Function ToolFuncSpec `json:"function"`
}

// ToolFuncSpec 描述函数签名。
type ToolFuncSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ChatResponse 是非流式返回。
type ChatResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	Choices           []ChatChoice `json:"choices"`
	Usage             *ChatUsage   `json:"usage,omitempty"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	// PrismConversationID 是上游会话 ID，非流式时回传给客户端用于多轮延续。
	// 非标准字段（OpenAI 协议没有它），配套响应头 x-prism-conversation-id。
	PrismConversationID string `json:"prism_conversation_id,omitempty"`
}

// ChatChoice 是一条候选。
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
	Logprobs     any         `json:"logprobs"`
}

// ChatUsage 是 token 用量。
type ChatUsage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails 与 OpenAI 同名结构对齐。网关不做提示缓存，cached_tokens 恒为 0。
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// CompletionTokensDetails 中 reasoning_tokens 已含在 completion_tokens 内。
type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ---------------------------- Anthropic Messages ----------------------------

// AnthropicRequest 是 POST /v1/messages 的请求体。
type AnthropicRequest struct {
	Model      string             `json:"model"`
	Messages   []AnthropicMessage `json:"messages"`
	MaxTokens  int                `json:"max_tokens"`
	System     StringOrArray      `json:"system,omitempty"`
	Stream     bool               `json:"stream,omitempty"`
	Tools      []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice any                `json:"tool_choice,omitempty"`

	Temperature *float64       `json:"temperature,omitempty"`
	TopP        *float64       `json:"top_p,omitempty"`
	TopK        *int           `json:"top_k,omitempty"`
	StopSeqs    []string       `json:"stop_sequences,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`

	// Thinking 是扩展思考开关：开了（enabled / adaptive）才回思考块，与 Anthropic 一致 ——
	// 很多脚本直接取 content[0].text，没开却多出一个思考块会把它们弄坏。
	Thinking *AnthropicThinking `json:"thinking,omitempty"`
}

// AnthropicThinking 是请求里的 thinking 配置。
type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	// Display 是思考内容要不要给客户端："omitted" 表示不要（Claude Code 的 -p 文本输出），
	// 交互 / 流式输出时是 "updates"。
	Display string `json:"display,omitempty"`
}

// wantsThinking 判断客户端是否开启了扩展思考、且要看思考内容。
func (r *AnthropicRequest) wantsThinking() bool {
	return r.Thinking != nil && (r.Thinking.Type == "enabled" || r.Thinking.Type == "adaptive") &&
		r.Thinking.Display != "omitted"
}

// AnthropicMessage 是一条消息。
type AnthropicMessage struct {
	Role    string        `json:"role"`
	Content StringOrArray `json:"content,omitempty"`
}

// AnthropicTool 是工具定义（Anthropic 用的字段名与 OpenAI 不同）。
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// AnthropicResponse 是 /v1/messages 的非流式返回。
type AnthropicResponse struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Model        string             `json:"model"`
	Content      []AnthropicContent `json:"content"`
	StopReason   string             `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        AnthropicUsage     `json:"usage"`
}

// AnthropicContent 是内容块。
type AnthropicContent struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// AnthropicUsage 是用量。
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ---------------------------- OpenAI Responses ----------------------------

// ResponsesRequest 是 POST /v1/responses 的请求体。
type ResponsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input,omitempty"`
	Instructions string          `json:"instructions,omitempty"`
	Stream       bool            `json:"stream,omitempty"`

	MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`

	Tools      []ChatTool `json:"tools,omitempty"`
	ToolChoice any        `json:"tool_choice,omitempty"`

	Reasoning *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`

	Metadata map[string]any `json:"metadata,omitempty"`

	// User 是调用方身份（OpenAI 官方字段）。
	User string `json:"user,omitempty"`

	// 会话延续用。
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	Store              *bool  `json:"store,omitempty"`
}

// ResponsesResponse 是非流式返回。
type ResponsesResponse struct {
	ID        string          `json:"id"`
	Object    string          `json:"object"`
	CreatedAt int64           `json:"created_at"`
	Status    string          `json:"status"`
	Model     string          `json:"model"`
	Output    []ResponsesItem `json:"output"`
	Usage     *ResponsesUsage `json:"usage,omitempty"`
	Error     *ErrorPayload   `json:"error,omitempty"`
	// ConversationID 是 Responses API 的标准字段，映射上游会话 ID。
	// 下一轮带回来即可延续会话（配合 previous_response_id）。
	ConversationID string `json:"conversation_id,omitempty"`
}

// ResponsesItem 是输出条目。
type ResponsesItem struct {
	Type    string             `json:"type"`
	ID      string             `json:"id,omitempty"`
	Role    string             `json:"role,omitempty"`
	Status  string             `json:"status,omitempty"`
	Content []ResponsesContent `json:"content,omitempty"`
	Summary []ResponsesSummary `json:"summary,omitempty"` // reasoning 条目的思考摘要
}

// ResponsesSummary 是 reasoning 条目的摘要块（type 为 summary_text）。
type ResponsesSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResponsesContent 是输出内容块。
type ResponsesContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

// ResponsesUsage 是用量。
type ResponsesUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	InputTokensDetails  ResponsesInputTokenDetails  `json:"input_tokens_details"`
	OutputTokens        int                         `json:"output_tokens"`
	OutputTokensDetails ResponsesOutputTokenDetails `json:"output_tokens_details"`
	TotalTokens         int                         `json:"total_tokens"`
}

// ResponsesInputTokenDetails 与 OpenAI 同名结构对齐（Codex CLI 会读取）。
type ResponsesInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// ResponsesOutputTokenDetails 中 reasoning_tokens 已含在 output_tokens 内。
type ResponsesOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ---------------------------- 错误 ----------------------------

// ErrorPayload 是 OpenAI 风格错误体。
type ErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// ErrorResponse 是错误包装。
type ErrorResponse struct {
	Error ErrorPayload `json:"error"`
}

// ---------------------------- 解析工具 ----------------------------

// decodeJSON 解析请求体，同时保留未知字段。
//
// 一遍 json.Unmarshal 到 map 拿未知字段，一遍到结构体拿已知字段。
// 代价是两次解析，但换来"任何未来新增字段都能原样透传给上游"，
// 对反代来说这个性质比那点 CPU 重要得多。
func decodeJSON[T any](body []byte) (*T, map[string]json.RawMessage, error) {
	var typed T
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&typed); err != nil {
		return nil, nil, err
	}
	var raw map[string]json.RawMessage
	// 这里忽略错误：能进到这一步说明 JSON 结构本身是合法的。
	_ = json.Unmarshal(body, &raw)
	return &typed, raw, nil
}
