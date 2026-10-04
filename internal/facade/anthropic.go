package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/sse"
	"github.com/oai-prism/oaiprism/internal/tokens"
)

// handleAnthropicMessages 实现 POST /v1/messages（Anthropic 协议）。
//
// 支持这个端点的实际价值：市面上大量客户端（各种 CLI、IDE 插件）
// 只实现了 Anthropic 协议，或者用 Anthropic 协议效果更好。
// 在门面层做一次协议翻译的成本很低，但可用面扩大一倍。
func (h *Handler) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	req, rawFields, err := decodeJSON[AnthropicRequest](body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	model, effort := h.resolveModel(req.Model, "")
	accountID, projectID := applyHeaderOverrides(r, &model, &effort)

	// Anthropic 把 system 放在顶层字段：交给翻译层当 system，与折叠的历史
	// 合成唯一一条（上游只读最后一条 system；单独前插一条的话，客户端自带的
	// 多轮历史无处安放，整段丢失）。
	sys := req.System.Text()
	if sys == "" {
		sys = h.cfg.Facade.DefaultSystemPrompt
	}
	input := translateAnthropicMessages(req.Messages, sys)

	runReq := &RunRequest{
		Model:     model,
		Effort:    effort,
		UserID:    anthropicUserID(rawFields),
		Input:     input,
		Metadata:  mergeMetadata(clientMetadata(rawFields), metadataWith("tools", anthropicToolsMetadata(req.Tools))),
		StickyKey: anthropicConversationKey(r, rawFields, req.Messages),
		AccountID: accountID,
		ProjectID: projectID,
		API:       "messages",
	}
	// 客户端显式句柄透传（真实前端从不发这两个字段 —— 10-01 抓包
	// 92 条 start 全部 cid=N prev=N；上游对带 previous_response_id 的
	// 请求把 input 当增量 → 上下文丢失）。
	runReq.PreviousResponseID = previousResponseIDFrom(r, rawFields)
	runReq.ConversationID = conversationIDFrom(r, rawFields)
	// 历史注入：与 chat.go 同款 —— 上游不代管历史，单条消息客户端
	// 把链缓存的历史拼进 input（对齐真实前端"全量回传"行为）。
	if !historyCarriesContextA(req.Messages) {
		if hist := sessionChainHistory(runReq.StickyKey); len(hist) > 0 {
			runReq.Input = injectChainHistory(runReq.Input, hist)
			runReq.PreviousResponseID = ""
		}
	}
	runReq.Extra = passthroughFields(rawFields, anthropicKnownFields)

	id := newID("msg_")

	if req.Stream {
		h.streamAnthropic(w, r, runReq, id, req.Model)
		return
	}
	h.syncAnthropic(w, r, runReq, id, req.Model)
}

var anthropicKnownFields = map[string]struct{}{
	"model": {}, "messages": {}, "max_tokens": {}, "system": {}, "stream": {},
	"tools": {}, "tool_choice": {}, "temperature": {}, "top_p": {}, "top_k": {},
	"stop_sequences": {}, "metadata": {},
	// 会话延续与前一轮响应 ID：已由 conversationIDFrom / previousResponseIDFrom 消费，不作为未知字段透传。
	"conversation_id": {}, "conversationId": {},
	"previous_response_id": {}, "previousResponseId": {},
}

// anthropicUserID 取 Anthropic 侧的调用方身份：metadata.user_id。
//
// 它有两个用处，别只顾一个：
//  1. 会话粘性（见 anthropicConversationKey）
//  2. 透传给上游 metadata（滥用追踪 / 配额归属）
//
// Anthropic 协议本身没有顶层 user 字段，这是官方约定的位置。
func anthropicUserID(body map[string]json.RawMessage) string {
	raw, ok := body["metadata"]
	if !ok {
		return ""
	}
	var md struct {
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &md); err != nil {
		return ""
	}
	return md.UserID
}

func anthropicConversationKey(r *http.Request, body map[string]json.RawMessage, msgs []AnthropicMessage) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderSession)); v != "" {
		return scopeKey(r, "h:"+v)
	}
	// Anthropic 没有 user 字段，用 metadata.user_id 兜底。
	if uid := anthropicUserID(body); uid != "" {
		return scopeKey(r, "u:"+uid)
	}
	conv := make([]ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		conv = append(conv, ChatMessage{Role: m.Role, Content: m.Content})
	}
	return scopeKey(r, conversationKeyBase(r, nil, conv))
}

func (h *Handler) streamAnthropic(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id, publicModel string) {
	// 流式头必须早于首帧，只能回显客户端带回来的会话 ID（见 streamChat 注释）。
	setConversationHeader(w, runReq.ConversationID)

	sw, err := sse.New(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()

	buf := make([]byte, 0, 2048)

	// message_start 必须先发：input_tokens 此时就能精确算出（就是本轮要发的上下文），
	// output_tokens 留到 message_delta 再给。
	buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{
		Type: "message_start", MessageID: id, Model: publicModel,
		Usage: &prism.Usage{InputTokens: countInputTokens(runReq.Input)},
	})
	if err := sw.WriteRaw(buf); err != nil {
		return
	}
	buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{Type: "content_block_start"})
	if err := sw.WriteRaw(buf); err != nil {
		return
	}

	emit := func(d Delta) error {
		if d.Text == "" {
			return nil
		}
		buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{Type: "content_block_delta", Text: d.Text})
		return sw.WriteRaw(buf)
	}

	res, runErr := h.runner.Run(r.Context(), runReq, emit)
	bindLogResult(r, res)
	// 成功回复才记历史（同 chat.go）。
	if runErr == nil {
		sessionChainPut(runReq.StickyKey, chainConv(res), resReqID(res), resAccount(res), runReq.Model)
		if res != nil && res.Text != "" {
			sessionChainAppend(runReq.StickyKey, lastUserText(runReq.Input), res.Text)
		}
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		middleware.RecordLogError(r, "anthropic 流式失败: %v", runErr)
		buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{Type: "error", Text: runErr.Error()})
		_ = sw.WriteRaw(buf)
		return
	}

	buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{Type: "content_block_stop"})
	_ = sw.WriteRaw(buf)

	var usage *prism.Usage
	if res != nil {
		usage = res.Usage
	}
	buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{
		Type: "message_delta", StopReason: "end_turn", Usage: usage,
	})
	_ = sw.WriteRaw(buf)

	buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{Type: "message_stop"})
	_ = sw.WriteRaw(buf)
}

func (h *Handler) syncAnthropic(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id, publicModel string) {
	res, err := h.runner.Run(r.Context(), runReq, nil)
	bindLogResult(r, res)
	// 成功回复才记历史（同 chat.go）。
	if err == nil {
		sessionChainPut(runReq.StickyKey, chainConv(res), resReqID(res), resAccount(res), runReq.Model)
		if res != nil && res.Text != "" {
			sessionChainAppend(runReq.StickyKey, lastUserText(runReq.Input), res.Text)
		}
	}
	if err != nil {
		status, typ, msg := mapError(err)
		middleware.RecordLogError(r, "anthropic 同步失败: %s", msg)
		writeError(w, status, typ, msg)
		return
	}

	text := ""
	if res != nil {
		text = res.Text
		setConversationHeader(w, res.ConversationID)
	}
	resp := AnthropicResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      publicModel,
		Content:    []AnthropicContent{{Type: "text", Text: text}},
		StopReason: "end_turn",
	}
	if res != nil && res.Usage != nil {
		resp.Usage = AnthropicUsage{
			InputTokens:  res.Usage.InputTokens,
			OutputTokens: res.Usage.OutputTokens,
		}
	} else {
		resp.Usage = AnthropicUsage{OutputTokens: tokens.Count(text)}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------- 别名端点 ----------------------------

// handleCompletions 把老的 /v1/completions 映射到 chat。
//
// 很多老工具链仍然在调它，做一次转换比让用户改代码更省事。
func (h *Handler) handleCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var legacy struct {
		Model  string `json:"model"`
		Prompt any    `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON")
		return
	}

	prompt := ""
	switch p := legacy.Prompt.(type) {
	case string:
		prompt = p
	case []any:
		for _, e := range p {
			if s, ok := e.(string); ok {
				prompt += s
			}
		}
	}

	// 转成 chat 形态后复用同一个 handler：只维护一条推理路径，
	// 避免"两套入口两套 bug"。
	chatBody, err := json.Marshal(ChatRequest{
		Model:    legacy.Model,
		Messages: []ChatMessage{{Role: "user", Content: stringContent(prompt)}},
		Stream:   legacy.Stream,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "转换请求体失败")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(chatBody))
	r.ContentLength = int64(len(chatBody))
	h.handleChatCompletions(w, r)
}
