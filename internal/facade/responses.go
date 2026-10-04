package facade

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/sse"
)

// responsesTurn 是一次 Responses 请求在 handler 内部流转的上下文。
type responsesTurn struct {
	id          string
	created     int64
	publicModel string

	// chainKey 是会话链的记录键：强会话键直接用；弱键（首条消息指纹等）
	// 改用"每条回复链一条"的内部键，避免撞键串话。
	chainKey string

	stream       bool
	bridge       bool
	execToolName string
	execKind     string
	isAux        bool
}

// handleResponses 实现 POST /v1/responses（OpenAI 新一代 Responses API）。
//
// 这是目前 Codex CLI / 新版官方 SDK 的首选端点，
// 不实现它会导致"用官方工具链连不上"，因此必须支持。
func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := h.readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	req, rawFields, err := decodeJSON[ResponsesRequest](body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体不是合法 JSON: "+err.Error())
		return
	}

	// effort 三级回落：reasoning.effort > metadata.reasoning_effort > 模型映射表。
	effort := ""
	if req.Reasoning != nil {
		effort = req.Reasoning.Effort
	}
	if strings.TrimSpace(effort) == "" {
		effort = metadataEffort(rawFields)
	}
	model, resolvedEffort := h.resolveModel(req.Model, effort)
	effort = resolvedEffort
	accountID, projectID := applyHeaderOverrides(r, &model, &effort)

	// 工具桥：Codex CLI 把工具声明放在 input 的 additional_tools 条目里
	// （顶层 tools 为 null）。检测到它就切换到桥模式 —— 上游当大脑，
	// 本地 CLI 当手脚，见 toolbridge.go 顶部注释。
	bridge := BridgeEnabled(rawFields)
	toolsStr := string(rawFields["tools"])
	hasTools := toolsStr != "" && toolsStr != "null" && toolsStr != "[]"
	turn := &responsesTurn{
		id:           newID("resp_"),
		created:      time.Now().Unix(),
		publicModel:  req.Model,
		stream:       req.Stream,
		bridge:       bridge,
		execToolName: ExecToolName(rawFields),
		execKind:     ExecToolKind(rawFields),
	}
	// 桥判定诊断：CLI 有两条工具声明路径（use_responses_lite 决定）——
	// true 走 input 里的 additional_tools 条目，false 走顶层 tools 字段。
	h.log.Debug("桥判定",
		"bridge", bridge,
		"tools_bytes", len(toolsStr),
		"input_bytes", len(rawFields["input"]),
		"exec_tool_name", turn.execToolName,
	)

	// Codex 的任务标题请求：网关就地生成，不打上游（见 generateLocalTitle）。
	if !bridge && !hasTools && len(rawFields["input"]) < 3000 &&
		strings.Contains(string(rawFields["input"]), "Generate a concise, single-line task title") {
		h.writeLocalTitle(w, req, rawFields, turn)
		return
	}

	input := messagesFromResponsesInput(req.Input, "")
	if bridge {
		// UA 推断的 OS 事实声明随桥指令一起进首条 system（见 osDirective）。
		input = bridgeInputItems(req.Input, osDirective(r.UserAgent()))
	} else if req.Instructions != "" {
		// instructions 就是 Responses API 的 system，保持 system 角色。
		input = append([]prism.InputItem{prism.NewSystemItem(req.Instructions)}, input...)
	} else if h.cfg.Facade.DefaultSystemPrompt != "" {
		input = append([]prism.InputItem{prism.NewSystemItem(h.cfg.Facade.DefaultSystemPrompt)}, input...)
	}
	if len(input) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input 不能为空")
		return
	}
	// 客户端是否自带多轮历史（必须在折叠之前判断，折叠后 assistant 条目已被并入 system）。
	hasClientHistory := false
	for _, it := range input {
		role := strings.ToLower(strings.TrimSpace(it.Role))
		if role == "assistant" || role == "tool" || role == "function" {
			hasClientHistory = true
			break
		}
	}

	// 稳定会话标识（必须在消息加工之前计算，避免折叠或裁剪历史导致哈希漂移）。
	stickyKey := responsesConversationKey(r, rawFields, input)
	turn.isAux = isCodexAuxRequest(r, rawFields, bridge, hasTools)

	runReq := &RunRequest{
		Model:        model,
		Effort:       effort,
		UserID:       req.User,
		Metadata:     mergeMetadata(clientMetadata(rawFields), metadataWith("tools", toolsMetadata(req.Tools))),
		StickyKey:    stickyKey,
		AccountID:    accountID,
		ProjectID:    projectID,
		API:          "responses",
		ExtraHeaders: extractSentinelToken(r),
		IsAux:        turn.isAux,
	}
	runReq.Extra = passthroughFields(rawFields, responsesKnownFields)

	convIDFromReq := conversationIDFrom(r, rawFields)
	if turn.isAux {
		// 伴生请求：不继承、不记录会话链，在专用伴生项目里跑（见 Runner.resolveProject）。
		runReq.ConversationID = convIDFromReq
		runReq.Input = input
		h.dispatchResponses(w, r, runReq, turn)
		return
	}

	// 会话链：强会话键按键继承；任何请求都可凭客户端显式带回的
	// previous_response_id / conversation_id 命中（按调用方隔离）。
	// 弱键（首条消息指纹 / user 字段）绝不按键继承 —— 两段以同一句话开头的
	// 对话会撞到同一个弱键，继承就是串话。
	tenant := middleware.Tenant(r.Context())
	strongKey := isStrongSessionKey(stickyKey)
	lookupKey := ""
	if strongKey {
		lookupKey = stickyKey
	}
	hit, found := sessionChainFind(tenant, lookupKey, req.PreviousResponseID, convIDFromReq)
	switch {
	case strongKey:
		turn.chainKey = stickyKey
	case found:
		turn.chainKey = hit.Key
	default:
		turn.chainKey = scopeKey(r, "r:"+turn.id)
	}
	h.log.Debug("会话粘性判定", "stickyKey", stickyKey, "chainKey", turn.chainKey,
		"hasClientHistory", hasClientHistory, "chainProj", hit.ProjectID, "chainPrevResp", hit.ResponseID)

	if runReq.ProjectID == "" && hit.ProjectID != "" {
		runReq.ProjectID = hit.ProjectID
		runReq.MarkProjectFromChain()
	}
	// 续接句柄：客户端显式带的照传（若是我们发出的本地 ID，换成上游真实 ID）；
	// 会话链里的只在 upstream_continuation 开启时附带。
	runReq.ConversationID = convIDFromReq
	runReq.PreviousResponseID = req.PreviousResponseID
	if found && req.PreviousResponseID != "" && hit.ResponseID != "" {
		runReq.PreviousResponseID = hit.ResponseID
	}
	if h.cfg.Facade.UpstreamContinuation {
		if runReq.ConversationID == "" {
			runReq.ConversationID = hit.ConversationID
		}
		if runReq.PreviousResponseID == "" {
			runReq.PreviousResponseID = hit.ResponseID
		}
		if len(hit.Snapshot) > 0 {
			if runReq.Metadata == nil {
				runReq.Metadata = make(map[string]any, 2)
			}
			runReq.Metadata["codex_listen_snapshot"] = string(hit.Snapshot)
		}
	}
	if found && hit.AccountID != "" {
		// 项目与续接句柄是账号私有的：租到别的账号时 Runner 会整体作废它们。
		runReq.BoundAccountID = hit.AccountID
	}
	sessionChainBind(turn.chainKey, runReq.ConversationID, runReq.ProjectID)

	// 上下文：每轮都发完整上下文。
	//
	// 2026-10-03 实测：只发"本轮增量 + previousResponseId"时第二轮必然失忆
	// （上游不会替我们按句柄拼历史）。客户端自带历史的折叠进首条 system；
	// 只发本轮消息的客户端，用本地会话链累积的历史注入。
	switch {
	case hasClientHistory:
		runReq.Input = foldInputHistory(input)
	default:
		runReq.Input = input
		if hist := sessionChainHistory(turn.chainKey); len(hist) > 0 {
			runReq.Input = injectChainHistory(input, hist)
		}
	}
	for idx, it := range runReq.Input {
		var preview string
		if len(it.Content) > 0 {
			preview = truncateRunes(it.Content[0].Text, 60)
		}
		h.log.Debug("准备发送给上游的 InputItem", "index", idx, "role", it.Role, "preview", preview)
	}

	h.dispatchResponses(w, r, runReq, turn)
}

// dispatchResponses 按 stream 字段分派。
func (h *Handler) dispatchResponses(w http.ResponseWriter, r *http.Request, runReq *RunRequest, turn *responsesTurn) {
	if turn.stream {
		h.streamResponses(w, r, runReq, turn)
		return
	}
	h.syncResponses(w, r, runReq, turn)
}

// isCodexAuxRequest 判断是否为 Codex 的伴生轻量请求（摘要、标题之类）。
//
// 只认带 Codex 特征的请求：早期判据只看"无工具 + 输入小于 3000 字节"，
// 普通 SDK 的每一个短请求都会被当成伴生请求，跑进别人的项目、也不记录会话。
func isCodexAuxRequest(r *http.Request, raw map[string]json.RawMessage, bridge, hasTools bool) bool {
	if bridge || hasTools || len(raw["input"]) >= 3000 {
		return false
	}
	ua := strings.ToLower(r.UserAgent())
	if strings.Contains(ua, "codex") {
		return true
	}
	_, hasCM := raw["client_metadata"]
	return hasCM
}

// writeLocalTitle 本地生成 Codex 任务标题并按请求形态（流式/同步）回包。
//
// 严禁为标题向上游发起真实推理与创建独立项目：会与主请求在同一账号上并发，
// 触发 "Error while processing conversation (403 Forbidden)"，且延迟长达数分钟。
func (h *Handler) writeLocalTitle(w http.ResponseWriter, req *ResponsesRequest, raw map[string]json.RawMessage, turn *responsesTurn) {
	titleJSON := generateLocalTitle(raw["input"])
	h.log.Info("客户端任务标题已本地生成", "title", titleJSON)
	if req.Stream {
		sw, err := sse.New(w)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		defer sw.Close()
		buf := make([]byte, 0, 1024)
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{Type: "response.created", ResponseID: turn.id, Model: req.Model, CreatedAt: turn.created})
		_ = sw.WriteRaw(buf)
		_ = emitTextResponseEvents(sw, &buf, turn.id, req.Model, turn.created, newID("msg_"), titleJSON, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": turn.id, "object": "response", "created_at": turn.created, "status": "completed", "model": req.Model,
		"output": []any{
			map[string]any{
				"type": "message", "id": newID("msg_"), "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": titleJSON}},
			},
		},
	})
}

// isContinuationError 判断失败是否由"续接句柄失效"引起（值得丢弃句柄、用全量上下文重试）。
//
// 只认这一类：超时、限流、鉴权、风控、客户端断开都与句柄无关，
// 拿它们重试只会让一个已经很慢的请求再慢一倍、白扣一次额度。
func isContinuationError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrPollTimeout) || creds.IsAuthError(err) || creds.IsRateLimited(err) || isSentinelThrottle(err) {
		return false
	}
	var ae *creds.APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusGone, http.StatusUnprocessableEntity:
			return true
		}
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, k := range []string{"previous", "conversation", "not found", "expired", "no longer", "snapshot", "transcript"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

// hasContinuation 报告请求是否携带了续接句柄。
func hasContinuation(req *RunRequest) bool {
	if req.PreviousResponseID != "" || req.ConversationID != "" {
		return true
	}
	_, ok := req.Metadata["codex_listen_snapshot"]
	return ok
}

// runResponses 执行一轮，并在"续接句柄失效"时丢弃句柄、用完整上下文原地重试一次。
//
// canRetry 由调用方决定（流式路径只有在客户端还没收到任何正文时才能重试）。
func (h *Handler) runResponses(r *http.Request, runReq *RunRequest, turn *responsesTurn, emit func(Delta) error, canRetry func() bool) (*RunResult, error) {
	res, err := h.runner.Run(r.Context(), runReq, emit)
	bindLogResult(r, res)
	if err == nil || turn.isAux || !hasContinuation(runReq) || !isContinuationError(err) {
		return res, err
	}
	projectGone := strings.Contains(strings.ToLower(err.Error()), "project")
	sessionChainResetSession(turn.chainKey, projectGone)
	if canRetry != nil && !canRetry() {
		return res, err
	}
	h.log.Warn("续接句柄失效，丢弃句柄并以完整上下文重试",
		"err", err, "chainKey", turn.chainKey, "prevResp", runReq.PreviousResponseID, "conv", runReq.ConversationID)
	runReq.dropContinuation()
	if projectGone {
		if runReq.ProjectID != "" && res != nil {
			h.runner.ForgetProject(res.AccountID, runReq.StickyKey)
		}
		runReq.ProjectID = ""
	}
	res, err = h.runner.Run(r.Context(), runReq, emit)
	bindLogResult(r, res)
	return res, err
}

// recordResponsesTurn 把成功的一轮写回会话链。
func recordResponsesTurn(runReq *RunRequest, turn *responsesTurn, res *RunResult) {
	if turn.isAux || res == nil {
		return
	}
	sessionChainRecord(turn.chainKey, res, runReq.Model)
	sessionChainRecordLocalID(turn.chainKey, turn.id)
	if res.Text != "" {
		sessionChainAppend(turn.chainKey, lastUserText(runReq.Input), res.Text)
	}
}

var responsesKnownFields = map[string]struct{}{
	"model": {}, "input": {}, "instructions": {}, "stream": {},
	"max_output_tokens": {}, "temperature": {}, "top_p": {},
	"tools": {}, "tool_choice": {}, "reasoning": {}, "metadata": {},
	"previous_response_id": {}, "previousResponseId": {}, "store": {}, "user": {},
	// conversation_id 已由 conversationIDFrom 消费，不再透传（见 chatKnownFields）。
	"conversation_id": {}, "conversationId": {},
	// Codex CLI（0.15x）专有字段。这些若被当"未知字段"直通上游请求体顶层，
	// 会触发上游 400（实测：client_metadata / include / prompt_cache_key /
	// service_tier / text / parallel_tool_calls 都是 CLI 新增字段，上游不认识）。
	"client_metadata": {}, "include": {}, "prompt_cache_key": {},
	"service_tier": {}, "text": {}, "parallel_tool_calls": {},
}

func responsesConversationKey(r *http.Request, body map[string]json.RawMessage, items []prism.InputItem) string {
	return scopeKey(r, responsesConversationKeyBase(r, body, items))
}

func responsesConversationKeyBase(r *http.Request, body map[string]json.RawMessage, items []prism.InputItem) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderSession)); v != "" {
		return "h:" + v
	}
	// 优先直接解析 Codex CLI 专有的 client_metadata 与 prompt_cache_key
	if raw, ok := body["client_metadata"]; ok && len(raw) > 0 {
		var cm map[string]any
		if err := json.Unmarshal(raw, &cm); err == nil {
			for _, k := range []string{"session_id", "sessionId", "thread_id", "threadId", "conversation_id", "conversationId"} {
				if v, ok := cm[k].(string); ok && strings.TrimSpace(v) != "" {
					return "cm:" + strings.TrimSpace(v)
				}
			}
		}
	}
	if raw, ok := body["prompt_cache_key"]; ok && len(raw) > 0 {
		var pck string
		if err := json.Unmarshal(raw, &pck); err == nil && strings.TrimSpace(pck) != "" {
			return "pck:" + strings.TrimSpace(pck)
		}
	}
	conv := make([]ChatMessage, 0, len(items))
	for _, it := range items {
		var sb strings.Builder
		for _, c := range it.Content {
			sb.WriteString(c.Text)
		}
		conv = append(conv, ChatMessage{Role: it.Role, Content: stringContent(sb.String())})
	}
	return conversationKeyBase(r, body, conv)
}

func (h *Handler) streamResponses(w http.ResponseWriter, r *http.Request, runReq *RunRequest, turn *responsesTurn) {
	id, created, publicModel := turn.id, turn.created, turn.publicModel
	// 流式头必须早于首帧，只能回显客户端带回来的会话 ID（见 streamChat 注释）。
	setConversationHeader(w, runReq.ConversationID)

	sw, err := sse.New(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()

	itemID := newID("msg_")
	buf := make([]byte, 0, 2048)

	// 序言事件：created -> output_item.added -> content_part.added。
	// 顺序是协议强制的，客户端状态机依赖它。
	for _, ev := range []string{"response.created", "response.in_progress"} {
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
			Type: ev, ResponseID: id, Model: publicModel, CreatedAt: created,
		})
		if err := sw.WriteRaw(buf); err != nil {
			return
		}
	}
	if !turn.isAux {
		sessionChainRecordLocalID(turn.chainKey, id)
	}

	// 心跳：等待上游期间必须持续发事件保活。
	//
	// 为什么必需：start+poll 一轮可能要 1-5 分钟（上游沙箱重试、
	// xhigh 长推理），期间若一个字节都不发，中间链路（node sidecar、
	// 反代、Nginx 的 proxy_read_timeout）会按空闲把连接掐掉，
	// 客户端表现为 "stream closed before response.completed"。
	// 实测：Codex CLI 0.154/0.159 对长时间静默同样会判流断。
	heartbeatStop := make(chan struct{})
	var heartbeatWG sync.WaitGroup
	heartbeatWG.Add(1)
	go func() {
		defer heartbeatWG.Done()
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-t.C:
				// 发送标准 SSE 注释保活行（防止任何客户端或代理 idle timeout）
				if err := sw.WriteRaw([]byte(": keepalive\n\n")); err != nil {
					return
				}
				hb := AppendResponsesEvent(nil, ResponsesEvent{
					Type: "response.in_progress", ResponseID: id,
					Model: publicModel, CreatedAt: created,
				})
				if err := sw.WriteRaw(hb); err != nil {
					return // 客户端已断开，主流程会经 ctx 感知
				}
			}
		}
	}()
	defer func() {
		close(heartbeatStop)
		heartbeatWG.Wait()
	}()

	fail := func(runErr error) {
		h.log.Error("responses 流式失败", "err", runErr, "chainKey", turn.chainKey)
		middleware.RecordLogError(r, "responses 流式失败: %v", runErr)
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{Type: "response.failed", ResponseID: id, Model: publicModel, CreatedAt: created, Text: runErr.Error()})
		_ = sw.WriteRaw(buf)
	}

	if turn.bridge {
		// 桥模式不能边收边发：必须先拿到完整回复才能判断它是
		// 工具调用（```codex-exec 块）还是纯文本。缓冲后统一输出 ——
		// 也因此续接失效时总能安全地原地重试（客户端还没收到任何正文）。
		var sb strings.Builder
		emit := func(d Delta) error {
			sb.WriteString(d.Text)
			return nil
		}
		canRetry := func() bool { sb.Reset(); return true }
		res, runErr := h.runResponses(r, runReq, turn, emit, canRetry)
		if runErr != nil {
			if !errors.Is(runErr, context.Canceled) {
				fail(runErr)
			}
			return
		}
		recordResponsesTurn(runReq, turn, res)

		var usage *prism.Usage
		if res != nil {
			usage = res.Usage
		}
		text := sb.String()
		js := h.bridgeExecJS(r, turn, text, res)

		finalRespID := id
		if res != nil && res.ResponseID != "" {
			finalRespID = res.ResponseID
		}
		if js != "" {
			callID := newID("ctc_")
			var item string
			if turn.execKind == "function" {
				item = functionCallItemJSON(callID, turn.execToolName, toFunctionArguments(js))
			} else {
				item = customToolCallItemJSON(callID, js, turn.execToolName)
			}
			done := AppendResponsesEvent(buf[:0], ResponsesEvent{
				Type: "response.output_item.added", ItemJSON: item,
			})
			if err := sw.WriteRaw(done); err != nil {
				return
			}
			if turn.execKind != "function" {
				// custom_tool_call 专用事件；function_call 没有这一段。
				done = AppendResponsesEvent(buf[:0], ResponsesEvent{
					Type: "response.custom_tool_call_input.done", ItemID: callID, Text: js,
				})
				if err := sw.WriteRaw(done); err != nil {
					return
				}
			}
			done = AppendResponsesEvent(buf[:0], ResponsesEvent{
				Type: "response.output_item.done", ItemJSON: item,
			})
			if err := sw.WriteRaw(done); err != nil {
				return
			}
			done = AppendResponsesEvent(buf[:0], ResponsesEvent{
				Type:       "response.completed",
				ResponseID: finalRespID, Model: publicModel, CreatedAt: created,
				OutputJSON: "[" + item + "]",
				Usage:      usage,
			})
			_ = sw.WriteRaw(done)
			return
		}

		// 纯文本：桥模式下一次性给出（模型已完整生成，无需伪增量）。
		_ = emitTextResponseEvents(sw, &buf, finalRespID, publicModel, created, itemID, stripExecFence(text), usage)
		return
	}

	buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
		Type: "response.output_item.added", ItemID: itemID,
	})
	if err := sw.WriteRaw(buf); err != nil {
		return
	}
	buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
		Type: "response.content_part.added", ItemID: itemID,
	})
	if err := sw.WriteRaw(buf); err != nil {
		return
	}

	sentText := false
	emit := func(d Delta) error {
		if d.Text == "" {
			return nil
		}
		sentText = true
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
			Type: "response.output_text.delta", ItemID: itemID, Text: d.Text,
		})
		return sw.WriteRaw(buf)
	}
	// 已经吐过正文就不能重试：客户端会收到两段拼接的回答。
	res, runErr := h.runResponses(r, runReq, turn, emit, func() bool { return !sentText })
	if runErr != nil {
		if !errors.Is(runErr, context.Canceled) {
			fail(runErr)
		}
		return
	}
	recordResponsesTurn(runReq, turn, res)

	text := ""
	var usage *prism.Usage
	if res != nil {
		text = res.Text
		usage = res.Usage
	}
	// 收尾事件必须逐个发全，否则 SDK 会一直等 response.completed。
	finalRespID := id
	if res != nil && res.ResponseID != "" {
		finalRespID = res.ResponseID
	}
	_ = emitTextResponseEvents(sw, &buf, finalRespID, publicModel, created, itemID, text, usage)
}

// bridgeExecJS 从桥模式回复里取出要交给客户端执行的 JS。
//
// 优先用模型输出的 ```codex-exec 块；模型没输出、但上游沙箱里产生了文件变更时，
// 把变更合成为本地落盘命令（经 sessionChainFilterNewDeltaFiles 去重，防止跨轮重复合成死循环）。
func (h *Handler) bridgeExecJS(r *http.Request, turn *responsesTurn, text string, res *RunResult) string {
	if js0, ok := extractExecBlock(text); ok {
		return ensureExecJS(js0)
	}
	if res == nil || len(res.DeltaFiles) == 0 {
		return ""
	}
	dedupKey := turn.chainKey
	if dedupKey == "" {
		dedupKey = scopeKey(r, "r:"+turn.id)
	}
	newDeltaFiles := sessionChainFilterNewDeltaFiles(dedupKey, res.DeltaFiles)
	if len(newDeltaFiles) == 0 {
		return ""
	}
	isWin := strings.Contains(strings.ToLower(r.UserAgent()), "windows")
	js := SynthesizeDeltaFilesExecJS(newDeltaFiles, isWin)
	if js != "" {
		h.log.Info("已将上游沙箱内的文件变更合成为本地执行命令", "files", len(newDeltaFiles), "isWin", isWin)
	}
	return js
}

// emitTextResponseEvents 发文本型回复的收尾事件序列：
// output_text.done -> content_part.done -> output_item.done -> completed。
// 返回第一个写错误（如有）。
func emitTextResponseEvents(sw *sse.Writer, buf *[]byte, id, publicModel string, created int64, itemID, text string, usage *prism.Usage) error {
	events := []ResponsesEvent{
		{Type: "response.output_text.done", ItemID: itemID, Text: text},
		{Type: "response.content_part.done", ItemID: itemID, Text: text},
		{Type: "response.output_item.done", ItemID: itemID, Text: text},
		{Type: "response.completed", ResponseID: id, Model: publicModel,
			CreatedAt: created, ItemID: itemID, Text: text, Usage: usage},
	}
	for _, ev := range events {
		*buf = AppendResponsesEvent((*buf)[:0], ev)
		if err := sw.WriteRaw(*buf); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) syncResponses(w http.ResponseWriter, r *http.Request, runReq *RunRequest, turn *responsesTurn) {
	res, err := h.runResponses(r, runReq, turn, nil, nil)
	if err != nil {
		h.log.Error("responses 同步失败", "err", err, "chainKey", turn.chainKey)
		status, typ, msg := mapError(err)
		middleware.RecordLogError(r, "responses 同步失败: %s", msg)
		writeError(w, status, typ, msg)
		return
	}
	recordResponsesTurn(runReq, turn, res)

	text := ""
	var usage *ResponsesUsage
	var conversationID string
	if res != nil {
		text = res.Text
		conversationID = res.ConversationID
		if res.Usage != nil {
			usage = newResponsesUsage(res.Usage)
		}
	}

	finalRespID := turn.id
	if res != nil && res.ResponseID != "" {
		finalRespID = res.ResponseID
	}

	if turn.bridge {
		if js := h.bridgeExecJS(r, turn, text, res); js != "" {
			callID := newID("ctc_")
			setConversationHeader(w, conversationID)
			var out any
			if turn.execKind == "function" {
				out = map[string]any{
					"id": callID, "type": "function_call",
					"status": "completed", "call_id": callID,
					"name": turn.execToolName, "arguments": toFunctionArguments(js),
				}
			} else {
				out = map[string]any{
					"id": callID, "type": "custom_tool_call",
					"status": "completed", "call_id": callID,
					"name": turn.execToolName, "input": js,
				}
			}
			respMap := map[string]any{
				"id": finalRespID, "object": "response", "created_at": turn.created,
				"status": "completed", "model": turn.publicModel,
				"output": []any{out},
			}
			if usage != nil {
				respMap["usage"] = usage
			}
			writeJSON(w, http.StatusOK, respMap)
			return
		}
		text = stripExecFence(text)
	}
	resp := ResponsesResponse{
		ID:        finalRespID,
		Object:    "response",
		CreatedAt: turn.created,
		Status:    "completed",
		Model:     turn.publicModel,
		Output: []ResponsesItem{{
			Type:   "message",
			ID:     newID("msg_"),
			Role:   "assistant",
			Status: "completed",
			Content: []ResponsesContent{{
				Type: "output_text",
				Text: text,
			}},
		}},
	}
	if usage != nil {
		resp.Usage = usage
	}
	if conversationID != "" {
		resp.ConversationID = conversationID
		setConversationHeader(w, conversationID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// stripExecFence 去掉 codex-exec 围栏（桥模式纯文本路径不再展示它）。
func stripExecFence(text string) string {
	const fence = "```codex-exec"
	idx := strings.Index(text, fence)
	if idx < 0 {
		return text
	}
	end := strings.Index(text[idx:], "```")
	if end < 0 {
		return strings.TrimSpace(text[:idx])
	}
	return strings.TrimSpace(text[:idx] + text[idx+end+3:])
}

// extractSentinelToken 从下游请求头中提取 openai-sentinel-token 或 x-openai-sentinel-token。
func extractSentinelToken(r *http.Request) map[string]string {
	extra := make(map[string]string)
	if tok := strings.TrimSpace(r.Header.Get("openai-sentinel-token")); tok != "" {
		extra["openai-sentinel-token"] = tok
	} else if tok := strings.TrimSpace(r.Header.Get("x-openai-sentinel-token")); tok != "" {
		extra["openai-sentinel-token"] = tok
	}
	return extra
}

// heartbeatInterval 是流式等待期间的心跳间隔（1.5秒一次，确保持续激活下游客户端 SSE 事件流并刷新 idle timeout）。
const heartbeatInterval = 1500 * time.Millisecond

// extractTitleJSON 将上游模型生成的任意格式标题清洗并封装为客户端要求的合法 JSON {"title": "..."}
func extractTitleJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return `{"title": "任务对话"}`
	}
	// 1. 若本身已经是合法含有 title 字段的 JSON
	var parsed struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed.Title != "" {
		cleaned, _ := json.Marshal(map[string]string{"title": parsed.Title})
		return string(cleaned)
	}

	// 2. 处理 markdown code block ```json ... ```
	if idx := strings.Index(raw, "```"); idx != -1 {
		rest := raw[idx+3:]
		if nl := strings.IndexByte(rest, '\n'); nl != -1 {
			rest = rest[nl+1:]
		}
		if endIdx := strings.Index(rest, "```"); endIdx != -1 {
			block := strings.TrimSpace(rest[:endIdx])
			if err := json.Unmarshal([]byte(block), &parsed); err == nil && parsed.Title != "" {
				cleaned, _ := json.Marshal(map[string]string{"title": parsed.Title})
				return string(cleaned)
			}
		}
	}

	// 3. 处理内嵌 {"title": "..."}
	if start := strings.Index(raw, `{"title"`); start != -1 {
		if end := strings.IndexByte(raw[start:], '}'); end != -1 {
			candidate := raw[start : start+end+1]
			if err := json.Unmarshal([]byte(candidate), &parsed); err == nil && parsed.Title != "" {
				cleaned, _ := json.Marshal(map[string]string{"title": parsed.Title})
				return string(cleaned)
			}
		}
	}

	// 4. 普通纯文本：去除首尾引号、多余空白及换行，截取单行
	lines := strings.Split(raw, "\n")
	title := ""
	for _, l := range lines {
		l = strings.TrimSpace(l)
		l = strings.Trim(l, "\"`'#*- ")
		if l != "" {
			title = l
			break
		}
	}
	if title == "" {
		title = "任务对话"
	}
	runes := []rune(title)
	if len(runes) > 30 {
		title = string(runes[:30])
	}
	cleaned, _ := json.Marshal(map[string]string{"title": title})
	return string(cleaned)
}

// generateLocalTitle 从客户端发送的 input 中提取核心任务文本，本地快速生成简短单行标题，杜绝向上游发请求引发 403 并发冲突。
func generateLocalTitle(raw json.RawMessage) string {
	var blocks []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(raw, &blocks)

	extractText := func(r json.RawMessage) string {
		if len(r) == 0 {
			return ""
		}
		var s string
		if json.Unmarshal(r, &s) == nil {
			return s
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(r, &parts) == nil {
			var sb strings.Builder
			for _, p := range parts {
				sb.WriteString(p.Text)
			}
			return sb.String()
		}
		return string(r)
	}

	candidate := ""
	for _, b := range blocks {
		txt := strings.TrimSpace(extractText(b.Content))
		if txt == "" {
			continue
		}
		if strings.HasPrefix(txt, "# AGENTS.md") ||
			strings.HasPrefix(txt, "<permissions") ||
			strings.HasPrefix(txt, "You are a coding agent") {
			continue
		}
		if strings.Contains(txt, "Generate a concise, single-line task title") {
			lines := strings.Split(txt, "\n")
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if l != "" && !strings.Contains(l, "Generate a concise") &&
					!strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "<") &&
					!strings.EqualFold(l, "user prompt:") && !strings.EqualFold(l, "user prompt") {
					candidate = l
					break
				}
			}
			continue
		}
		candidate = txt
	}

	if candidate == "" {
		return `{"title": "任务对话"}`
	}

	lines := strings.Split(candidate, "\n")
	firstLine := strings.TrimSpace(lines[0])
	firstLine = strings.TrimPrefix(firstLine, "User prompt:")
	firstLine = strings.TrimPrefix(firstLine, "User prompt")
	firstLine = strings.TrimPrefix(firstLine, ":")
	firstLine = strings.TrimPrefix(firstLine, "- ")
	firstLine = strings.TrimPrefix(firstLine, "* ")
	firstLine = strings.TrimSpace(firstLine)

	if idx := strings.Index(firstLine, "[LOCAL_EXECUTION"); idx != -1 {
		firstLine = strings.TrimSpace(firstLine[:idx])
	}
	runes := []rune(firstLine)
	if len(runes) > 36 {
		firstLine = string(runes[:33]) + "..."
	}
	if firstLine == "" {
		firstLine = "任务对话"
	}
	cleaned, _ := json.Marshal(map[string]string{"title": firstLine})
	return string(cleaned)
}
