package facade

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/sse"
)

// handleChatCompletions 实现 POST /v1/chat/completions。
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	req, rawFields, err := decodeJSON[ChatRequest](body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	// effort 三级回落：顶层字段 > metadata.reasoning_effort > 模型映射表。
	effort := req.ReasoningEffort
	if strings.TrimSpace(effort) == "" {
		effort = metadataEffort(rawFields)
	}
	model, resolvedEffort := h.resolveModel(req.Model, effort)
	effort = resolvedEffort
	accountID, projectID := applyHeaderOverrides(r, &model, &effort)

	runReq := &RunRequest{
		Model:        model,
		Effort:       effort,
		UserID:       req.User,
		Input:        translateChatMessages(req.Messages, h.cfg.Facade.DefaultSystemPrompt, h.cfg.Facade.PromptByteLimit()),
		Metadata:     mergeMetadata(clientMetadata(rawFields), metadataWith("tools", toolsMetadata(req.Tools))),
		StickyKey:    conversationKey(r, rawFields, req.Messages),
		AccountID:    accountID,
		ProjectID:    projectID,
		API:          "chat",
		ExtraHeaders: extractSentinelToken(r),
	}
	runReq.Extra = passthroughFields(rawFields, chatKnownFields)
	// 原生续接：会话历史由上游保管，续接轮次只发增量（见 native.go）。
	if conv := chatConversation(req.Messages, h.cfg.Facade.DefaultSystemPrompt); conv != nil {
		h.attachNative(runReq, &nativeTurn{key: runReq.StickyKey, strong: isStrongSessionKey(runReq.StickyKey), conv: conv})
	}

	// 超过上游单条上限：流开始之前就以 400 context_length_exceeded 回绝（见 context_limit.go）。
	if err := h.runner.checkPromptSize(runReq); writeContextTooLarge(w, err) {
		middleware.RecordLogError(r, "chat 提示词超限: %v", err)
		return
	}

	started := time.Now()
	id := newID("chatcmpl-")
	created := started.Unix()

	if req.Stream {
		h.streamChat(w, r, runReq, id, created, req.Model, req.StreamOptions, req.Tools)
		return
	}
	h.syncChat(w, r, runReq, id, created, req.Model, req.Tools)
}

// setConversationHeader 回传上游会话 ID。
//
// 双通道：响应头（服务可提前读）+ 响应体字段（客户端 SDK 更容易拿到）。
// 命名与 PrismOpenAIProxy 一致（x-prism-conversation-id），兼容其客户端生态。
func setConversationHeader(w http.ResponseWriter, conversationID string) {
	if conversationID != "" {
		w.Header().Set("x-prism-conversation-id", conversationID)
	}
}

// chatKnownFields 是从请求体里"已消费"的字段集合。
// 其余字段会被原样透传给上游——这是反代的重要性质：
// 上游加了新参数、客户端立刻就能用上，不需要我们发版。
//
// conversation_id 必须在这里：它是会话键（见 conversationIDFrom），若再当"未知字段"
// 透传，请求体顶层会多出一个 conversation_id（与上游字段命名不符，属于污染，可能被拒）。
var chatKnownFields = map[string]struct{}{
	"model": {}, "messages": {}, "stream": {}, "stream_options": {},
	"conversation_id": {}, "conversationId": {},
	"max_tokens": {}, "max_completion_tokens": {}, "temperature": {},
	"top_p": {}, "n": {}, "stop": {}, "presence_penalty": {},
	"frequency_penalty": {}, "seed": {}, "user": {}, "logprobs": {},
	"top_logprobs": {}, "response_format": {}, "tools": {}, "tool_choice": {},
	"reasoning_effort": {}, "metadata": {},
}

// passthroughFields 挑出未识别的字段。
//
// 注意排除掉我们已知但故意不改写的字段：把它们塞回 Extra 会与
// 显式构造的字段重复，上游可能因为重复键报错。
func passthroughFields(raw map[string]json.RawMessage, known map[string]struct{}) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]any)
	for k, v := range raw {
		if _, ok := known[k]; ok {
			continue
		}
		var any1 any
		if err := json.Unmarshal(v, &any1); err != nil {
			continue
		}
		out[k] = any1
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// streamChat 处理流式返回。
func (h *Handler) streamChat(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id string, created int64, publicModel string, so *StreamOptions, declaredTools []ChatTool) {
	// 流式下响应头必须在首帧之前写好：只能给出续接中的上游会话（见 streamConversationID），
	// 新建会话的 ID 随结束帧给出。
	setConversationHeader(w, streamConversationID(runReq))

	sw, err := sse.New(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()

	includeUsage := so != nil && so.IncludeUsage

	// 复用同一块缓冲：整个流式过程只在这里分配一次。
	buf := make([]byte, 0, 2048)

	// 首帧必须是 role，否则部分严格客户端会认为响应格式非法。
	buf = AppendChatChunk(buf[:0], ChatChunkSpec{
		ID: id, Created: created, Model: publicModel, Role: "assistant",
	})
	if err := sw.WriteData(buf); err != nil {
		return
	}
	defer keepAlive(sw, streamKeepAliveInterval, sseCommentFrame)()

	emit := func(d Delta) error {
		// 思维链与正文分成两个 chunk：混在一起会让客户端
		// 把推理过程当正文渲染出来。
		if d.Reasoning != "" {
			buf = AppendChatChunk(buf[:0], ChatChunkSpec{
				ID: id, Created: created, Model: publicModel, Reasoning: d.Reasoning,
			})
			if err := sw.WriteData(buf); err != nil {
				return err
			}
		}
		if d.Text != "" {
			buf = AppendChatChunk(buf[:0], ChatChunkSpec{
				ID: id, Created: created, Model: publicModel, Content: d.Text,
			})
			if err := sw.WriteData(buf); err != nil {
				return err
			}
		}
		return nil
	}

	res, runErr := h.runner.Run(r.Context(), runReq, emit)
	bindLogResult(r, res)

	if runErr != nil {
		// 响应头已经发出去了，没法再改状态码。
		// 按 SSE 的惯例发一个错误载荷，再以 [DONE] 收尾，
		// 这样客户端至少能拿到一个明确的失败信号而不是超时。
		if !errors.Is(runErr, context.Canceled) {
			h.log.Warn("流式生成中断", "id", id, "err", runErr)
			// HTTP 200 已发出，把失败原因记进请求日志（异常错误摘要）。
			middleware.RecordLogError(r, "chat 流式失败: %v", runErr)
			buf = append(buf[:0], `{"error":{"message":`...)
			buf = sse.AppendJSONString(buf, runErr.Error())
			if errors.Is(runErr, ErrContextTooLarge) {
				buf = append(buf, `,"type":"invalid_request_error","code":"context_length_exceeded"}}`...)
			} else {
				buf = append(buf, `,"type":"upstream_error"}}`...)
			}
			_ = sw.WriteData(buf)
		} else {
			middleware.RecordLogAbort(r)
		}
		_ = sw.Done()
		return
	}

	convID := ""
	if res != nil {
		convID = res.ConversationID
	}

	var toolCalls []ToolCall
	if res != nil && len(res.DeltaFiles) > 0 {
		toolCalls = MapDeltaFilesToToolCalls(res.DeltaFiles, declaredTools)
		h.applyLocalWorkspace(r, res.DeltaFiles)
	}

	fin := finishReason(toolCalls)

	// 如果有工具调用，推一帧带 tool_calls 的增量
	if len(toolCalls) > 0 {
		buf = AppendChatChunk(buf[:0], ChatChunkSpec{
			ID: id, Created: created, Model: publicModel, ToolCalls: toolCalls,
			ConversationID: convID,
		})
		if err := sw.WriteData(buf); err != nil {
			return
		}
	}

	// 结束帧：finish_reason 用 fin (tool_calls 或 stop)
	buf = AppendChatChunk(buf[:0], ChatChunkSpec{
		ID: id, Created: created, Model: publicModel, HasFinish: true, Finish: fin,
		ConversationID: convID,
	})
	if err := sw.WriteData(buf); err != nil {
		return
	}

	if includeUsage && res != nil {
		usage := res.Usage
		if usage == nil {
			usage = outputOnlyUsage(res)
		}
		buf = AppendChatChunk(buf[:0], ChatChunkSpec{
			ID: id, Created: created, Model: publicModel, EmptyChoices: true, Usage: usage,
			ConversationID: convID,
		})
		_ = sw.WriteData(buf)
	}

	_ = sw.Done()
}

// syncChat 处理非流式返回。
func (h *Handler) syncChat(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id string, created int64, publicModel string, declaredTools []ChatTool) {
	res, err := h.runner.Run(r.Context(), runReq, nil)
	bindLogResult(r, res)
	if err != nil {
		if writeContextTooLarge(w, err) {
			middleware.RecordLogError(r, "chat 同步失败: %v", err)
			return
		}
		status, typ, msg := mapError(err)
		middleware.RecordLogError(r, "chat 同步失败: %s", msg)
		writeError(w, status, typ, msg)
		return
	}

	var toolCalls []ToolCall
	if res != nil && len(res.DeltaFiles) > 0 {
		toolCalls = MapDeltaFilesToToolCalls(res.DeltaFiles, declaredTools)
		h.applyLocalWorkspace(r, res.DeltaFiles)
	}

	fin := finishReason(toolCalls)
	msg := ChatMessage{
		Role:             "assistant",
		Content:          stringContent(res.Text),
		ReasoningContent: res.Reasoning,
		ToolCalls:        toolCalls,
	}

	resp := ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   publicModel,
		Choices: []ChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: fin,
		}},
	}
	if res.ConversationID != "" {
		resp.PrismConversationID = res.ConversationID
		setConversationHeader(w, res.ConversationID)
	}
	usage := res.Usage
	if usage == nil {
		// runner 成功路径总会给出用量；这里只是防御，避免客户端拿到 null 崩掉。
		usage = outputOnlyUsage(res)
	}
	resp.Usage = newChatUsage(usage)
	writeJSON(w, http.StatusOK, resp)
}

// applyLocalWorkspace 按 X-Local-Workspace 把上游产物写到**网关所在机器**的目录。
//
// 这等于让调用方指定网关主机上的写入位置，所以三重门槛：
// 配置显式开启（facade.local_workspace_write）、请求来自本机、路径逐个校验。
func (h *Handler) applyLocalWorkspace(r *http.Request, files []prism.CodexDeltaFile) {
	localWorkspace := strings.TrimSpace(r.Header.Get("X-Local-Workspace"))
	if localWorkspace == "" {
		return
	}
	if !h.cfg.Facade.LocalWorkspaceWrite || !middleware.IsLocalRequest(r) {
		h.log.Warn("忽略 X-Local-Workspace：未开启 facade.local_workspace_write 或请求不是来自本机",
			"remote", r.RemoteAddr)
		return
	}
	if err := ApplyLocalWorkspaceFiles(localWorkspace, files); err != nil {
		h.log.Warn("本地工作区文件写入异常", "path", localWorkspace, "err", err)
		return
	}
	h.log.Info("已将文件变更同步写入本地工作区", "path", localWorkspace, "files", len(files))
}

// stringContent 把纯文本包成 StringOrArray。
func stringContent(s string) StringOrArray {
	b, err := json.Marshal(s)
	if err != nil {
		return StringOrArray{raw: []byte(`""`)}
	}
	return StringOrArray{raw: b}
}

// finishReason 决定 finish_reason：只看真正发出去的工具调用。
//
// 不能看 DeltaFiles —— 沙箱每轮都把 Prism 写进工作区的 AGENTS.md 报成新增文件，
// 它被映射层滤掉后 tool_calls 为空，却会让 finish_reason 变成 "tool_calls"；
// 按 OpenAI 语义，客户端会去执行一个不存在的工具调用（2026-10-04 实测命中）。
func finishReason(toolCalls []ToolCall) string {
	if len(toolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// mapError 把内部错误映射成 HTTP 状态码。
//
// 注意一个容易搞错的点：上游返回 401 不应该原样透传成 401——
// 那会让客户端以为自己的 API Key 有问题并反复重试。
// 对调用方而言这是"网关的上游凭据挂了"，属于 502。
func mapError(err error) (int, string, string) {
	switch {
	case errors.Is(err, context.Canceled):
		// 499 是 Nginx 约定的"客户端主动断开"。
		return 499, "client_closed_request", "客户端提前断开了连接"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout", "等待上游响应超时"
	case errors.Is(err, ErrPollTimeout):
		return http.StatusGatewayTimeout, "timeout", err.Error()
	}

	var ae *creds.APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return http.StatusBadGateway, "upstream_auth_error",
				"上游拒绝了这个账号的凭据。请检查 secrets/accounts.json 中的 cookies / access_token 是否已失效。" +
					"原始错误：" + ae.Body
		case http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "rate_limit_exceeded",
				"上游限流：所有账号都在冷却中，请稍后重试。原始错误：" + ae.Body
		default:
			return http.StatusBadGateway, "upstream_error",
				"上游返回错误：HTTP " + itoaSafe(ae.Status) + " " + ae.Body
		}
	}
	return http.StatusBadGateway, "upstream_error", err.Error()
}

func itoaSafe(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [12]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
