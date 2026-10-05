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

	mapped := h.anthropicModel(req.Model)
	if mapped != req.Model && r.Header.Get(HeaderModel) == "" {
		middleware.RecordLogModel(r, mapped) // 流水记实际用的模型，不是客户端发来的 claude-*
	}
	model, effort := h.resolveModel(mapped, "")
	accountID, projectID := applyHeaderOverrides(r, &model, &effort)

	// 工具桥：声明了工具的客户端（Claude Code）由上游决策、本地执行（见 anthropic_bridge.go）。
	var bt *anthropicBridgeTurn
	if tools := anthropicBridgeTools(req); len(tools) > 0 {
		bt = &anthropicBridgeTurn{tools: tools}
	}

	// Anthropic 把 system 放在顶层字段：作为最前面的一条 system，与对话中间的 system 消息、
	// 折叠的历史合成唯一一条（上游只读最后一条 system；分开发的话，客户端自带的多轮历史
	// 无处安放，整段丢失）。
	sys := req.System.Text()
	if sys == "" && bt == nil {
		sys = h.cfg.Facade.DefaultSystemPrompt
	}
	if bt != nil {
		sys = strings.TrimSpace(anthropicBridgePrompt(bt.tools, parseAnthropicToolChoice(req.ToolChoice)) + "\n\n" + sys)
	}
	if directive := anthropicOutputSchema(rawFields); directive != "" {
		sys = strings.TrimSpace(sys + "\n\n" + directive)
	}
	chat := append([]ChatMessage{{Role: "system", Content: stringContent(sys)}}, anthropicChatMessages(req.Messages)...)
	input := translateChatMessages(chat, "", h.cfg.Facade.PromptByteLimit())
	conv := chatConversation(chat, "")
	if bt != nil {
		addClientToolReminder(input, conv)
		if conv != nil {
			bt.cwd = clientWorkingDir(conv.system)
		}
	}

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
		Bridge:    bt != nil,
		IsAux:     isClaudeCodeAux(r, req),
	}
	runReq.Extra = passthroughFields(rawFields, anthropicKnownFields)
	// 原生续接：会话历史由上游保管，续接轮次只发增量（见 native.go）。
	if conv != nil && !runReq.IsAux {
		h.attachNative(runReq, &nativeTurn{key: runReq.StickyKey, strong: isStrongSessionKey(runReq.StickyKey), conv: conv})
	}
	if bt != nil {
		h.log.Debug("Claude Code 工具桥", "tools", len(bt.tools), "key", runReq.StickyKey, "cwd", bt.cwd)
	}

	// 超过上游单条上限：在 message_start 之前以 400 "prompt is too long" 回绝 ——
	// Claude Code 等客户端认这句文案，据此压缩上下文（见 context_limit.go）。
	if err := h.runner.checkPromptSize(runReq); writeAnthropicTooLarge(w, err) {
		middleware.RecordLogError(r, "anthropic 提示词超限: %v", err)
		return
	}

	id := newID("msg_")

	if req.Stream {
		h.streamAnthropic(w, r, runReq, id, req.Model, req.wantsThinking(), bt)
		return
	}
	h.syncAnthropic(w, r, runReq, id, req.Model, req.wantsThinking(), bt)
}

// anthropicBridgeTurn 是一次工具桥请求的上下文。
type anthropicBridgeTurn struct {
	tools []AnthropicTool
	cwd   string // 客户端工作目录（沙箱文件兜底转成本地路径用）
}

// anthropicBridgeOutcome 把桥模式的上游回复拆成给客户端的正文与工具调用。
//
// 上游没按桥的格式调用工具、却在沙箱里写了文件时，把文件变更转成客户端的
// Write / Edit 调用（转不了就附在正文后面），不让成品留在用户拿不到的沙箱里。
func (h *Handler) anthropicBridgeOutcome(r *http.Request, runReq *RunRequest, bt *anthropicBridgeTurn, res *RunResult) (string, []localToolCall) {
	if res == nil {
		return "", nil
	}
	prose, calls := parseLocalToolCalls(res.Text, bt.tools)
	if len(calls) == 0 && len(res.DeltaFiles) > 0 {
		key := runReq.StickyKey
		if key == "" {
			key = scopeKey(r, "cid:"+res.ConversationID)
		}
		if files := sessionChainFilterNewDeltaFiles("ccfiles:"+key, res.DeltaFiles); len(files) > 0 {
			fileCalls, note := deltaFilesToClientTools(files, bt.cwd, bt.tools)
			if len(fileCalls) > 0 {
				calls = fileCalls
				h.log.Info("Claude Code 工具桥：上游沙箱里的文件变更已转成本地工具调用", "files", len(files), "calls", len(fileCalls))
			} else {
				note = strings.TrimSpace(note + renderFileAttachments(files))
			}
			if note != "" {
				prose = strings.TrimSpace(prose + "\n\n" + note)
			}
		}
	}
	if len(calls) > 0 {
		bridgeToolUseIDs(calls, res.Text)
		names := make([]string, len(calls))
		for i, c := range calls {
			names[i] = c.Name
		}
		h.log.Info("Claude Code 工具桥：交给客户端执行", "calls", strings.Join(names, ","))
	}
	return prose, calls
}

// anthropicModel 把模型表里没有的名字换成默认模型。
//
// Claude Code 发的是 claude-sonnet-4-5 这类名字，原样交给上游会整轮回
// "Error while processing conversation (400)"，又被当成沙箱未就绪重试 10 次 —— 客户端
// 卡上五六分钟后失败（2026-10-05 实测）。回给客户端的仍是它请求的名字。
func (h *Handler) anthropicModel(requested string) string {
	f := &h.cfg.Facade
	if _, ok := f.Models[requested]; ok || requested == "" || requested == f.DefaultModel {
		return requested
	}
	for _, m := range f.Models {
		if m.Model == requested {
			return requested
		}
	}
	return f.DefaultModel
}

var anthropicKnownFields = map[string]struct{}{
	"model": {}, "messages": {}, "max_tokens": {}, "system": {}, "stream": {},
	"tools": {}, "tool_choice": {}, "temperature": {}, "top_p": {}, "top_k": {},
	"stop_sequences": {}, "metadata": {},
	// Anthropic 协议自己的开关不透传：上游不认识顶层的陌生字段，整轮回
	// "Error while processing conversation (400)"（Claude Code 每个请求都带 thinking 与
	// context_management，2026-10-05 实测因此完全用不了）。
	"thinking": {}, "context_management": {}, "container": {}, "mcp_servers": {}, "service_tier": {},
	"output_config": {}, "output_format": {}, "betas": {},
	// 会话 ID 是会话键（见 anthropicConversationKey）；previous_response_id 不适用于本协议。
	// 两者都不作为未知字段透传给上游。
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
	if cid := conversationIDFrom(r, body); cid != "" {
		return scopeKey(r, "cid:"+cid)
	}
	// Claude Code 的会话 ID 加首条消息指纹：同一会话里的主对话、子代理、旁路请求各是一段对话
	// （见 firstUserFingerprint）。
	if sid := claudeCodeSessionID(r, body); sid != "" {
		return scopeKey(r, "sid:"+sid+"/"+firstUserFingerprint(msgs))
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

func (h *Handler) streamAnthropic(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id, publicModel string, thinking bool, bt *anthropicBridgeTurn) {
	// 流式头必须早于首帧，只能给出续接中的上游会话（见 streamConversationID）。
	setConversationHeader(w, streamConversationID(runReq))

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
	defer keepAlive(sw, streamKeepAliveInterval, anthropicPingFrame)()
	// 内容块等内容到了再开：思考块要排在正文块前面。
	blocks := &anthropicBlocks{sw: sw, thinkingOn: thinking}
	emit := func(d Delta) error {
		if err := blocks.thinking(d.Reasoning); err != nil {
			return err
		}
		if bt != nil {
			// 桥模式：正文要等整段回复到齐才分得清是不是工具调用（上游本来也只在终态给正文）。
			return nil
		}
		return blocks.text(d.Text)
	}

	res, runErr := h.runner.Run(r.Context(), runReq, emit)
	bindLogResult(r, res)

	if errors.Is(runErr, context.Canceled) {
		middleware.RecordLogAbort(r) // 客户端已断开：不再往断掉的连接上写收尾事件
		return
	}
	if runErr != nil {
		middleware.RecordLogError(r, "anthropic 流式失败: %v", runErr)
		ev := AnthropicEvent{Type: "error", Text: runErr.Error()}
		if errors.Is(runErr, ErrContextTooLarge) {
			ev.ErrorType, ev.Text = "invalid_request_error", anthropicTooLongMessage(runErr)
		}
		buf = AppendAnthropicEvent(buf[:0], ev)
		_ = sw.WriteRaw(buf)
		return
	}

	stop := "end_turn"
	if bt != nil {
		prose, calls := h.anthropicBridgeOutcome(r, runReq, bt, res)
		if blocks.text(prose) != nil {
			return
		}
		for _, c := range calls {
			if blocks.toolUse(c) != nil {
				return
			}
		}
		if len(calls) > 0 {
			stop = "tool_use"
		}
	}
	if blocks.finish() != nil {
		return
	}

	var usage *prism.Usage
	if res != nil {
		usage = res.Usage
	}
	buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{
		Type: "message_delta", StopReason: stop, Usage: usage,
	})
	_ = sw.WriteRaw(buf)

	buf = AppendAnthropicEvent(buf[:0], AnthropicEvent{Type: "message_stop"})
	_ = sw.WriteRaw(buf)
}

func (h *Handler) syncAnthropic(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id, publicModel string, thinking bool, bt *anthropicBridgeTurn) {
	res, err := h.runner.Run(r.Context(), runReq, nil)
	bindLogResult(r, res)
	if err != nil {
		if writeAnthropicTooLarge(w, err) {
			middleware.RecordLogError(r, "anthropic 同步失败: %v", err)
			return
		}
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
	if bt != nil {
		prose, calls := h.anthropicBridgeOutcome(r, runReq, bt, res)
		text = prose
		resp.Content = resp.Content[:0]
		if prose != "" || len(calls) == 0 {
			resp.Content = append(resp.Content, AnthropicContent{Type: "text", Text: prose})
		}
		for _, c := range calls {
			resp.Content = append(resp.Content, AnthropicContent{Type: "tool_use", ID: c.ID, Name: c.Name, Input: c.Input})
		}
		if len(calls) > 0 {
			resp.StopReason = "tool_use"
		}
	}
	if thinking && res != nil && res.Reasoning != "" {
		resp.Content = append([]AnthropicContent{{Type: "thinking", Thinking: res.Reasoning, Signature: anthropicThinkingSignature}}, resp.Content...)
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

// anthropicThinkingSignature 是思考块的签名。真 Anthropic 的签名用来校验回传的思考没被改过；
// 这里的思考是上游给的摘要，客户端回传时网关直接忽略（只取 text），签名只是占位。
const anthropicThinkingSignature = "oaiprism-upstream-reasoning-summary"

// anthropicBlocks 按序写内容块：思考块（客户端开了 thinking 才有）在前、正文块在后，
// index 依次递增。正文已经开始后才到的思考，等正文块关闭后补成下一个块。
type anthropicBlocks struct {
	sw         *sse.Writer
	buf        []byte
	thinkingOn bool
	index      int    // 当前打开（或下一个要开）的块
	open       string // "", "thinking", "text"
	hasText    bool
	hasTool    bool
	late       strings.Builder
}

func (b *anthropicBlocks) write(e AnthropicEvent) error {
	e.Index = b.index
	b.buf = AppendAnthropicEvent(b.buf[:0], e)
	return b.sw.WriteRaw(b.buf)
}

func (b *anthropicBlocks) start(kind string) error {
	if b.open == kind {
		return nil
	}
	if err := b.stop(); err != nil {
		return err
	}
	b.open = kind
	if kind == "text" {
		b.hasText = true
	}
	return b.write(AnthropicEvent{Type: "content_block_start", Block: kind})
}

func (b *anthropicBlocks) stop() error {
	if b.open == "" {
		return nil
	}
	if b.open == "thinking" {
		if err := b.write(AnthropicEvent{Type: "content_block_delta", Block: "thinking", Signature: anthropicThinkingSignature}); err != nil {
			return err
		}
	}
	err := b.write(AnthropicEvent{Type: "content_block_stop"})
	b.open = ""
	b.index++
	return err
}

func (b *anthropicBlocks) thinking(text string) error {
	if text == "" || !b.thinkingOn {
		return nil
	}
	if b.open == "text" {
		b.late.WriteString(text)
		return nil
	}
	if err := b.start("thinking"); err != nil {
		return err
	}
	return b.write(AnthropicEvent{Type: "content_block_delta", Block: "thinking", Text: text})
}

func (b *anthropicBlocks) text(text string) error {
	if text == "" {
		return nil
	}
	if err := b.start("text"); err != nil {
		return err
	}
	return b.write(AnthropicEvent{Type: "content_block_delta", Text: text})
}

// toolUse 写一个完整的 tool_use 块（Claude Code 工具桥）：开块、参数 JSON、关块。
func (b *anthropicBlocks) toolUse(c localToolCall) error {
	if err := b.stop(); err != nil {
		return err
	}
	b.hasTool = true
	if err := b.write(AnthropicEvent{Type: "content_block_start", Block: "tool_use", ToolID: c.ID, ToolName: c.Name}); err != nil {
		return err
	}
	if err := b.write(AnthropicEvent{Type: "content_block_delta", Block: "tool_use", Text: string(c.Input)}); err != nil {
		return err
	}
	err := b.write(AnthropicEvent{Type: "content_block_stop"})
	b.index++
	return err
}

// finish 关闭仍打开的块、补发迟到的思考；正文块、工具调用都没有时补一个空正文块（空回答也要有正文块）。
func (b *anthropicBlocks) finish() error {
	if err := b.stop(); err != nil {
		return err
	}
	if late := b.late.String(); late != "" {
		b.late.Reset()
		if err := b.thinking(late); err != nil {
			return err
		}
		if err := b.stop(); err != nil {
			return err
		}
	}
	if !b.hasText && !b.hasTool {
		if err := b.start("text"); err != nil {
			return err
		}
	}
	return b.stop()
}
