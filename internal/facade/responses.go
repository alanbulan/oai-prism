package facade

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/sse"
)

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
	// 桥判定诊断：CLI 有两条工具声明路径（use_responses_lite 决定）——
	// true 走 input 里的 additional_tools 条目（我们认得），
	// false 走顶层 tools 字段（旧判据认不出，桥会静默失效，
	// 表现为模型在上游沙箱里干活、用户本地拿不到文件）。
	// 这行日志用于抓真实请求形状，排查后可按需降级为 Debug。
	toolsStr := string(rawFields["tools"])
	execToolName := ExecToolName(rawFields)
	execKind := ExecToolKind(rawFields)
	// 临时诊断：只要带 tools 就 dump（分析 CLI 实际注册的工具名）。
	if len(toolsStr) > 200 {
		_ = os.WriteFile(filepath.Join(os.TempDir(), "oaiprism_tools_dump.json"), []byte(toolsStr), 0o600)
	}
	h.log.Info("桥判定",
		"bridge", bridge,
		"path", func() string {
			if strings.Contains(string(rawFields["input"]), `"additional_tools"`) {
				return "A/additional_tools"
			}
			if strings.Contains(string(rawFields["input"]), `"custom_tool_call"`) {
				return "A/custom_tool_call"
			}
			if toolsStr != "" && toolsStr != "null" && toolsStr != "[]" {
				return "B/tools_field"
			}
			return "none"
		}(),
		"tools_bytes", len(toolsStr),
		"input_bytes", len(rawFields["input"]),
		"exec_tool_name", execToolName,
	)
	input := messagesFromResponsesInput(req.Input, "")
	if bridge {
		// UA 推断的 OS 事实声明随桥指令一起进首条 system（见 osDirective）。
		input = bridgeInputItems(req.Input, osDirective(r.UserAgent()))
	}
	if !bridge {
		if req.Instructions != "" {
			// instructions 就是 Responses API 的 system，保持 system 角色。
			input = append([]prism.InputItem{prism.NewSystemItem(req.Instructions)}, input...)
		} else if h.cfg.Facade.DefaultSystemPrompt != "" {
			input = append([]prism.InputItem{prism.NewSystemItem(h.cfg.Facade.DefaultSystemPrompt)}, input...)
		}
	}
	// 历史注入判据：必须在 foldInputHistory 之前检查客户端是否已经携带了多轮历史。
	// foldInputHistory 会把中间的 assistant 和工具调用折叠进首条 system，
	// 若在此之后检查，input 里的 assistant 条目已经被移出，会导致 hasAssistant 恒为 false，
	// 进而误把 Codex 等完整多轮客户端当成"单条消息客户端"二次注入 sessionChain。
	hasClientHistory := false
	for _, it := range input {
		r := strings.ToLower(strings.TrimSpace(it.Role))
		if r == "assistant" || r == "tool" || r == "function" {
			hasClientHistory = true
			break
		}
	}

	// 提取稳定会话标识（必须在消息加工之前计算，避免折叠或裁剪历史导致哈希漂移！）
	stickyKey := responsesConversationKey(r, rawFields, input)

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
	}

	// 会话状态持久化继承：从 sessionChain 恢复上轮项目句柄、会话句柄、上轮真实 ResponseID 与沙箱快照
	convIDFromReq := conversationIDFrom(r, rawFields)
	chainProj, chainConv, chainPrevResp, _, chainSnap := sessionChainLookup(stickyKey, req.PreviousResponseID, convIDFromReq)
	h.log.Info("会话粘性判定", "stickyKey", stickyKey, "hasClientHistory", hasClientHistory, "chainProj", chainProj, "chainPrevResp", chainPrevResp, "chainConv", chainConv)
	if runReq.ProjectID == "" && chainProj != "" {
		runReq.ProjectID = chainProj
	}
	runReq.ConversationID = convIDFromReq
	if runReq.ConversationID == "" && chainConv != "" {
		runReq.ConversationID = chainConv
	}
	// 首轮尚未绑定上游会话 ID 时，由网关为当前会话生成一个固定的持久会话 ID（对齐 WebUI cdx1_<uuid> 格式）
	// 确保该会话在整个生命周期内固定不变，彻底解决每次请求新建一个会话记录的根本问题！
	if runReq.ConversationID == "" {
		runReq.ConversationID = "cdx1_" + uuid.NewString()
	}

	// 关键：继承上一轮的真实响应句柄（PreviousResponseID）
	// 客户端显式指定时透传；未指定时从网关 sessionChain 继承上游下发的终态 resp_* 句柄。
	runReq.PreviousResponseID = req.PreviousResponseID
	if runReq.PreviousResponseID == "" && chainPrevResp != "" {
		runReq.PreviousResponseID = chainPrevResp
	}

	if len(chainSnap) > 0 {
		if runReq.Metadata == nil {
			runReq.Metadata = make(map[string]any, 2)
		}
		runReq.Metadata["codex_listen_snapshot"] = string(chainSnap)
	}

	// 提前将当前会话与会话 ID 和项目 ID 锁定关联，防止伴生并发请求（如标题生成）走漂
	sessionChainBind(stickyKey, runReq.ConversationID, runReq.ProjectID)

	// 多轮会话状态树承接与增量续传机制：
	// 上游服务端自身具备完备的状态树与对话记录表（由 conversationId 与 previousResponseId 唯一确定）。
	// 在客户端未主动发起上下文压缩（在压缩之前）的常规多轮中：
	// 只要当前会话已持有上一轮响应句柄（PreviousResponseID != ""），
	// 绝不可将往轮数十 KB 的全量日志拼成超大 System Prompt 发送！
	// 必须仅发送首条纯净 System 指令与本轮自上次回复以来的增量消息（Incremental Input），
	// 请求体永远保持在数 KB，彻底根除 16K/超大请求限制报错，同时实现远程项目/会话无缝承接。
	if runReq.PreviousResponseID != "" {
		runReq.Input = extractIncrementalInput(input)
		h.log.Info("会话承接：启用增量续传模式",
			"stickyKey", stickyKey,
			"previousResponseId", runReq.PreviousResponseID,
			"totalItems", len(input),
			"incrementalItems", len(runReq.Input),
			"projectID", runReq.ProjectID,
			"conversationID", runReq.ConversationID,
		)
	} else if hasClientHistory {
		// 首轮或断链重置时，才折叠历史作为初始上下文
		runReq.Input = foldInputHistory(input)
	} else {
		runReq.Input = input
		if hist := sessionChainHistory(runReq.StickyKey); len(hist) > 0 {
			runReq.Input = injectChainHistory(runReq.Input, hist)
		}
	}

	for idx, it := range runReq.Input {
		var preview string
		if len(it.Content) > 0 {
			preview = truncateRunes(it.Content[0].Text, 60)
		}
		h.log.Info("准备发送给上游的 InputItem", "index", idx, "role", it.Role, "preview", preview)
	}

	if len(runReq.Input) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input 不能为空")
		return
	}
	runReq.Extra = passthroughFields(rawFields, responsesKnownFields)

	isAux := !bridge && len(rawFields["input"]) < 3000 && (toolsStr == "" || toolsStr == "null" || toolsStr == "[]")
	runReq.IsAux = isAux
	if isAux {
		// 客户端标题生成：Codex CLI 会发起带有特定 prompt 的单行标题请求。
		// 严禁向上游发起真实的重量级推理与创建独立项目！
		// 否则会导致上游单账号并发冲突（Error while processing conversation (403 Forbidden)）、
		// 浪费沙箱资源，且延迟长达数分钟。
		// 这里直接由网关从任务上下文中就地提取并极速回包，0ms 响应，彻底杜绝并发踩踏！
		if strings.Contains(string(rawFields["input"]), "Generate a concise, single-line task title") {
			titleJSON := generateLocalTitle(rawFields["input"])
			h.log.Info("客户端任务标题已本地秒级生成", "title", titleJSON)

			id := newID("resp_")
			created := time.Now().Unix()
			if req.Stream {
				setConversationHeader(w, runReq.ConversationID)
				sw, err := sse.New(w)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "server_error", err.Error())
					return
				}
				defer sw.Close()
				buf := make([]byte, 0, 1024)
				buf = AppendResponsesEvent(buf[:0], ResponsesEvent{Type: "response.created", ResponseID: id, Model: req.Model, CreatedAt: created})
				_ = sw.WriteRaw(buf)
				_ = emitTextResponseEvents(sw, &buf, id, req.Model, created, newID("msg_"), titleJSON, nil)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": id, "object": "response", "created_at": created, "status": "completed", "model": req.Model,
				"output": []any{
					map[string]any{
						"type": "message", "id": newID("msg_"), "role": "assistant", "status": "completed",
						"content": []any{map[string]any{"type": "output_text", "text": titleJSON}},
					},
				},
			})
			return
		}

		// 其它伴生轻量请求：优先复用活跃项目，绝不新建独立项目，亦不污染会话链
		if runReq.ProjectID == "" {
			if chainProj, _, _, _, _ := sessionChainGet(stickyKey); chainProj != "" {
				runReq.ProjectID = chainProj
			} else if actProj, ok := h.runner.ActiveProject(accountID); ok {
				runReq.ProjectID = actProj
			}
		}
	}

	id := newID("resp_")
	created := time.Now().Unix()

	if req.Stream {
		h.streamResponses(w, r, runReq, id, created, req.Model, bridge, execToolName, execKind)
		return
	}
	h.syncResponses(w, r, runReq, id, created, req.Model, bridge, execToolName, execKind, isAux)
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
	return conversationKey(r, body, conv)
}

func (h *Handler) streamResponses(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id string, created int64, publicModel string, bridge bool, execToolName, execKind string) {
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
	sessionChainRecordLocalID(runReq.StickyKey, id)

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

	if bridge {
		// 桥模式不能边收边发：必须先拿到完整回复才能判断它是
		// 工具调用（```codex-exec 块）还是纯文本。缓冲后统一输出。
		var sb strings.Builder
		var usage *prism.Usage
		emit := func(d Delta) error {
			sb.WriteString(d.Text)
			return nil
		}
		res, runErr := h.runner.Run(r.Context(), runReq, emit)
		bindLogAccount(r, res)
		sessionChainRecord(runReq.StickyKey, res, runReq.Model)
		if res != nil && res.Text != "" {
			sessionChainAppend(runReq.StickyKey, lastUserText(runReq.Input), res.Text)
		}

		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			if strings.Contains(runErr.Error(), "previousResponseId") || strings.Contains(runErr.Error(), "not found") {
				sessionChainResetPrevious(runReq.StickyKey)
			}
			middleware.RecordLogError(r, "responses 流式失败: %v", runErr)
			buf = AppendResponsesEvent(buf[:0], ResponsesEvent{Type: "response.failed", ResponseID: id, Model: publicModel, CreatedAt: created, Text: runErr.Error()})
			_ = sw.WriteRaw(buf)
			return
		}
		if res != nil {
			usage = res.Usage
		}
		text := sb.String()

		var js string
		if js0, ok := extractExecBlock(text); ok {
			js = ensureExecJS(js0)
		}
		deltaCount := 0
		if res != nil {
			deltaCount = len(res.DeltaFiles)
		}
		h.log.Info("模型首轮生成文本完成", "text", truncateRunes(text, 100), "hasJS", js != "", "deltaFiles", deltaCount)

		// 单轮沙箱产物无缝落地：
		// 若模型未直接输出 codex-exec 围栏，但上游沙盒内产生了文件变更（DeltaFiles），
		// 网关在当前轮次直接将上游 DeltaFiles 提取并合成为客户端本地 exec_command 执行命令，
		// 确保输出产物 100% 写入用户本地电脑空间，绝不留在上游沙盒。
		// 必须通过 sessionChainFilterNewDeltaFiles 过滤系统文件并记录哈希，防止重复合成导致无限死循环！
		if js == "" && res != nil && len(res.DeltaFiles) > 0 {
			newDeltaFiles := sessionChainFilterNewDeltaFiles(runReq.StickyKey, res.DeltaFiles)
			if len(newDeltaFiles) > 0 {
				isWin := strings.Contains(strings.ToLower(r.UserAgent()), "windows")
				synth := SynthesizeDeltaFilesExecJS(newDeltaFiles, isWin)
				if synth != "" {
					js = synth
					h.log.Info("已在当前轮次自动将上游沙盒内生成的 DeltaFiles 合成为本地执行命令", "files", len(newDeltaFiles), "isWin", isWin)
				}
			}
		}


		if js != "" {
			callID := newID("ctc_")
			var item string
			if execKind == "function" {
				item = functionCallItemJSON(callID, execToolName, toFunctionArguments(js))
			} else {
				item = customToolCallItemJSON(callID, js, execToolName)
			}
			done := AppendResponsesEvent(buf[:0], ResponsesEvent{
				Type: "response.output_item.added", ItemJSON: item,
			})
			if err := sw.WriteRaw(done); err != nil {
				return
			}
			if execKind != "function" {
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
			finalRespID := id
			if res != nil && res.ResponseID != "" {
				finalRespID = res.ResponseID
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
		finalRespID := id
		if res != nil && res.ResponseID != "" {
			finalRespID = res.ResponseID
		}
		_ = emitTextResponseEvents(sw, &buf, finalRespID, publicModel, created, itemID, text, usage)
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

	emit := func(d Delta) error {
		if d.Text == "" {
			return nil
		}
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{
			Type: "response.output_text.delta", ItemID: itemID, Text: d.Text,
		})
		return sw.WriteRaw(buf)
	}

	res, runErr := h.runner.Run(r.Context(), runReq, emit)
	bindLogAccount(r, res)
	// 非桥流式路径同样要回写会话链，否则下一轮 fill 拿不到句柄。
	sessionChainRecord(runReq.StickyKey, res, runReq.Model)
	if res != nil && res.Text != "" {
		sessionChainAppend(runReq.StickyKey, lastUserText(runReq.Input), res.Text)
	}
	text := ""
	var usage *prism.Usage
	if res != nil {
		text = res.Text
		usage = res.Usage
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		middleware.RecordLogError(r, "responses 流式失败: %v", runErr)
		buf = AppendResponsesEvent(buf[:0], ResponsesEvent{Type: "response.failed", ResponseID: id, Model: publicModel, CreatedAt: created, Text: runErr.Error()})
		_ = sw.WriteRaw(buf)
		return
	}

	// 收尾事件必须逐个发全，否则 SDK 会一直等 response.completed。
	finalRespID := id
	if res != nil && res.ResponseID != "" {
		finalRespID = res.ResponseID
	}
	_ = emitTextResponseEvents(sw, &buf, finalRespID, publicModel, created, itemID, text, usage)
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

func (h *Handler) syncResponses(w http.ResponseWriter, r *http.Request, runReq *RunRequest, id string, created int64, publicModel string, bridge bool, execToolName, execKind string, isAux bool) {
	res, err := h.runner.Run(r.Context(), runReq, nil)
	bindLogAccount(r, res)
	if !isAux {
		sessionChainRecord(runReq.StickyKey, res, runReq.Model)
		sessionChainRecordLocalID(runReq.StickyKey, id)
		if err == nil && res != nil && res.Text != "" {
			sessionChainAppend(runReq.StickyKey, lastUserText(runReq.Input), res.Text)
		}
	}
	if err != nil {
		status, typ, msg := mapError(err)
		middleware.RecordLogError(r, "responses 同步失败: %s", msg)
		writeError(w, status, typ, msg)
		return
	}

	text := ""
	var usage *ResponsesUsage
	var conversationID string
	if res != nil {
		text = res.Text
		conversationID = res.ConversationID
		if res.Usage != nil {
			usage = &ResponsesUsage{
				InputTokens:  res.Usage.InputTokens,
				OutputTokens: res.Usage.OutputTokens,
				TotalTokens:  res.Usage.TotalTokens,
			}
		}
	}

	finalRespID := id
	if res != nil && res.ResponseID != "" {
		finalRespID = res.ResponseID
	}

	if bridge {
		var js string
		if js0, ok := extractExecBlock(text); ok {
			js = ensureExecJS(js0)
		}

		// 单轮沙箱产物无缝落地：
		// 若模型未直接输出 codex-exec 围栏，但上游沙盒内产生了文件变更（DeltaFiles），
		// 网关在当前轮次直接将上游 DeltaFiles 提取并合成为客户端本地 exec_command 执行命令，
		// 确保输出产物 100% 写入用户本地电脑空间，绝不留在上游沙盒。
		// 必须通过 sessionChainFilterNewDeltaFiles 过滤系统文件并记录哈希，防止重复合成导致无限死循环！
		if js == "" && res != nil && len(res.DeltaFiles) > 0 {
			newDeltaFiles := sessionChainFilterNewDeltaFiles(runReq.StickyKey, res.DeltaFiles)
			if len(newDeltaFiles) > 0 {
				isWin := strings.Contains(strings.ToLower(r.UserAgent()), "windows")
				synth := SynthesizeDeltaFilesExecJS(newDeltaFiles, isWin)
				if synth != "" {
					js = synth
					h.log.Info("已在当前轮次自动将上游沙盒内生成的 DeltaFiles 合成为本地执行命令 (sync)", "files", len(newDeltaFiles), "isWin", isWin)
				}
			}
		}

		if js != "" {
			callID := newID("ctc_")
			setConversationHeader(w, conversationID)
			var out any
			if execKind == "function" {
				out = map[string]any{
					"id": callID, "type": "function_call",
					"status": "completed", "call_id": callID,
					"name": execToolName, "arguments": toFunctionArguments(js),
				}
			} else {
				out = map[string]any{
					"id": callID, "type": "custom_tool_call",
					"status": "completed", "call_id": callID,
					"name": execToolName, "input": js,
				}
			}
			respMap := map[string]any{
				"id": finalRespID, "object": "response", "created_at": created,
				"status": "completed", "model": publicModel,
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
		CreatedAt: created,
		Status:    "completed",
		Model:     publicModel,
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

