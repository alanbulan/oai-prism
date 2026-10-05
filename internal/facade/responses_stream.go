package facade

import (
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/sse"
)

// responsesStream 按协议顺序写 /v1/responses 的输出条目：条目一个接一个开、关，
// output_index 依次递增，response.completed 按发出顺序列出全部条目。
//
// 思考摘要放在最前面的 reasoning 条目里（OpenAI 推理模型的标准形状，Codex 据此显示
// 思考过程）。上游的摘要通常随最终结果才到；正文已经开始后才到的摘要，等正文条目
// 关闭后作为下一个条目补发。
type responsesStream struct {
	sw    *sse.Writer
	buf   []byte
	items []string // 已关闭条目的 JSON

	reasoningID   string
	reasoning     strings.Builder
	reasoningOpen bool
	lateReasoning strings.Builder

	msgID   string
	msgOpen bool
}

func newResponsesStream(sw *sse.Writer, msgID string) *responsesStream {
	return &responsesStream{sw: sw, buf: make([]byte, 0, 2048), msgID: msgID}
}

func (s *responsesStream) write(e ResponsesEvent) error {
	e.OutputIndex = len(s.items) // 当前打开的条目排在已关闭条目之后
	s.buf = AppendResponsesEvent(s.buf[:0], e)
	return s.sw.WriteRaw(s.buf)
}

// reasoningDelta 追加一段思考摘要。正文条目已经打开时先攒着，见 finish。
func (s *responsesStream) reasoningDelta(text string) error {
	if text == "" {
		return nil
	}
	if s.msgOpen {
		s.lateReasoning.WriteString(text)
		return nil
	}
	if !s.reasoningOpen {
		s.reasoningID, s.reasoningOpen = newID("rs_"), true
		if err := s.write(ResponsesEvent{Type: "response.output_item.added", ItemJSON: reasoningItemJSON(s.reasoningID, "")}); err != nil {
			return err
		}
		if err := s.write(ResponsesEvent{Type: "response.reasoning_summary_part.added", ItemID: s.reasoningID}); err != nil {
			return err
		}
	}
	s.reasoning.WriteString(text)
	return s.write(ResponsesEvent{Type: "response.reasoning_summary_text.delta", ItemID: s.reasoningID, Text: text})
}

func (s *responsesStream) closeReasoning() error {
	if !s.reasoningOpen {
		return nil
	}
	s.reasoningOpen = false
	text := s.reasoning.String()
	item := reasoningItemJSON(s.reasoningID, text)
	for _, e := range []ResponsesEvent{
		{Type: "response.reasoning_summary_text.done", ItemID: s.reasoningID, Text: text},
		{Type: "response.reasoning_summary_part.done", ItemID: s.reasoningID, Text: text},
		{Type: "response.output_item.done", ItemJSON: item},
	} {
		if err := s.write(e); err != nil {
			return err
		}
	}
	s.items = append(s.items, item)
	return nil
}

func (s *responsesStream) openMessage() error {
	if s.msgOpen {
		return nil
	}
	if err := s.closeReasoning(); err != nil {
		return err
	}
	s.msgOpen = true
	if err := s.write(ResponsesEvent{Type: "response.output_item.added", ItemID: s.msgID}); err != nil {
		return err
	}
	return s.write(ResponsesEvent{Type: "response.content_part.added", ItemID: s.msgID})
}

func (s *responsesStream) textDelta(text string) error {
	if text == "" {
		return nil
	}
	if err := s.openMessage(); err != nil {
		return err
	}
	return s.write(ResponsesEvent{Type: "response.output_text.delta", ItemID: s.msgID, Text: text})
}

// finishMessage 关闭正文条目（没打开过就先打开：空回答也要有一条 message）。
func (s *responsesStream) finishMessage(text string) error {
	if err := s.openMessage(); err != nil {
		return err
	}
	for _, e := range []ResponsesEvent{
		{Type: "response.output_text.done", ItemID: s.msgID, Text: text},
		{Type: "response.content_part.done", ItemID: s.msgID, Text: text},
		{Type: "response.output_item.done", ItemID: s.msgID, Text: text},
	} {
		if err := s.write(e); err != nil {
			return err
		}
	}
	s.msgOpen = false
	s.items = append(s.items, strippedTextItemJSON(s.msgID, text))
	return nil
}

// toolCall 输出一条工具调用条目（工具桥）。
func (s *responsesStream) toolCall(call bridgeToolCall) error {
	if err := s.closeReasoning(); err != nil {
		return err
	}
	ev := []ResponsesEvent{{Type: "response.output_item.added", ItemJSON: call.item}}
	if call.input != "" {
		// custom_tool_call 专用事件；function_call 没有这一段。
		ev = append(ev, ResponsesEvent{Type: "response.custom_tool_call_input.done", ItemID: call.id, Text: call.input})
	}
	ev = append(ev, ResponsesEvent{Type: "response.output_item.done", ItemJSON: call.item})
	for _, e := range ev {
		if err := s.write(e); err != nil {
			return err
		}
	}
	s.items = append(s.items, call.item)
	return nil
}

// completed 关闭仍打开的思考条目、补发迟到的思考，然后发 response.completed。
func (s *responsesStream) completed(respID, model string, created int64, usage *prism.Usage) error {
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if late := s.lateReasoning.String(); late != "" {
		s.lateReasoning.Reset()
		if err := s.reasoningDelta(late); err != nil {
			return err
		}
		if err := s.closeReasoning(); err != nil {
			return err
		}
	}
	s.buf = AppendResponsesEvent(s.buf[:0], ResponsesEvent{
		Type: "response.completed", ResponseID: respID, Model: model, CreatedAt: created,
		OutputJSON: "[" + strings.Join(s.items, ",") + "]", Usage: usage,
	})
	return s.sw.WriteRaw(s.buf)
}

// reasoningItemJSON 构造 Responses 协议的 reasoning 条目；text 为空时 summary 是空数组。
func reasoningItemJSON(id, text string) string {
	var sb strings.Builder
	sb.WriteString(`{"id":`)
	writeJSONString(&sb, id)
	sb.WriteString(`,"type":"reasoning","summary":[`)
	if text != "" {
		sb.WriteString(`{"type":"summary_text","text":`)
		writeJSONString(&sb, text)
		sb.WriteString(`}`)
	}
	sb.WriteString(`]}`)
	return sb.String()
}
