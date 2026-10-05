package prism

import (
	"strings"
	"testing"
)

// pending 帧里的 codex_live_progress（2026-10-05 实测形状，turn_state 等字段从简）。
func TestParseStatus_LiveProgress(t *testing.T) {
	raw := `{"status":"pending","request_id":"r1","codex_async_job_id":"2953","turn_state":{"transcript_cursor":31},
	"codex_live_progress":{"transcriptCursor":31,"lineCount":9,
	  "reasoningSummaries":[{"line_index":23,"text":"**Designing** an airliner","source":"reasoning"}],
	  "toolCalls":[{"line_index":26,"call_id":"tool:26:0","name":"apply_patch","call_type":"function_call","status":null,"arguments_preview":null,"source":"transcript"}],
	  "eventPreviews":[]}}`
	st, err := (&Client{}).ParseStatusPayload([]byte(raw), "fallback", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.RequestID != "r1" || st.Status != "pending" || st.Done {
		t.Fatalf("包络没按已知形状解析: %+v", st)
	}
	p := st.Progress
	if p == nil || p.TranscriptCursor != 31 || len(p.ReasoningSummaries) != 1 || len(p.ToolCalls) != 1 {
		t.Fatalf("进度没解析出来: %+v", p)
	}
	if p.ReasoningSummaries[0].Text != "**Designing** an airliner" || p.ToolCalls[0].Name != "apply_patch" {
		t.Fatalf("进度条目不对: %+v", p)
	}
}

// 进度字段形状变了也不能连累整个包络（turn_state 丢了轮询就续不上）。
func TestParseStatus_LiveProgressShapeDrift(t *testing.T) {
	raw := `{"status":"pending","request_id":"r1","turn_state":{"seq":2},"codex_live_progress":{"transcriptCursor":"31"}}`
	st, err := (&Client{}).ParseStatusPayload([]byte(raw), "fallback", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Progress != nil || string(st.TurnState) != `{"seq":2}` || st.Status != "pending" {
		t.Fatalf("进度形状异常时应忽略进度、照常解析包络: progress=%+v turn_state=%s", st.Progress, st.TurnState)
	}
}

// 智能体式的一轮有多个 reasoning 条目：全部收下，逐条拆分供去重。
func TestParseStatus_MultipleReasoningItems(t *testing.T) {
	raw := `{"status":"completed","request_id":"r1","turn_state":{},"response":{"status":"success","payload":{"output":[
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"A1"},{"type":"summary_text","text":"A2"}]},
	  {"type":"function_call","name":"apply_patch"},
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"B"}]},
	  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"好了"}]}]}}}`
	st, err := (&Client{}).ParseStatusPayload([]byte(raw), "fallback", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Reasoning != "A1A2\n\nB" || strings.Join(st.ReasoningParts, "|") != "A1|A2|B" || st.Text != "好了" {
		t.Fatalf("reasoning=%q parts=%q text=%q", st.Reasoning, st.ReasoningParts, st.Text)
	}
}
