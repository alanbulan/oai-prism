package facade

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// liveTrace 把上游的思考过程合成为一份只增不减的全文，交给 nextReasoning 求增量。
//
// 来源有两个：
//   - 进行中的转录（codex_live_progress）：每次轮询只带新增的思考摘要与工具调用；
//   - 终态 payload 里的 reasoning 条目：只在最后给一次，内容与转录里的摘要大量重复。
//
// 上游模型以智能体方式在沙箱里写文件时，一轮可以跑二三十分钟，而正文要到终态才有。
// 转录是这段时间里唯一能给客户端看的东西 —— 不转发的话，调用方只能对着空白干等。
type liveTrace struct {
	b      strings.Builder
	seen   map[string]bool
	cursor int
}

// observe 并入一次轮询的进度，返回上游是否有进展（转录游标前进）。
func (l *liveTrace) observe(p *prism.LiveProgress) bool {
	if p == nil {
		return false
	}
	advanced := p.TranscriptCursor > l.cursor
	if advanced {
		l.cursor = p.TranscriptCursor
	}
	type entry struct {
		line      int
		key, text string
	}
	var es []entry
	for _, s := range p.ReasoningSummaries {
		if t := strings.TrimSpace(s.Text); t != "" {
			es = append(es, entry{s.LineIndex, "r:" + squash(t), t})
		}
	}
	for _, c := range p.ToolCalls {
		if c.Name == "" {
			continue
		}
		key := "t:" + c.CallID
		if c.CallID == "" {
			key = "t:" + c.Name + "@" + strconv.Itoa(c.LineIndex)
		}
		es = append(es, entry{c.LineIndex, key, "→ 调用工具 `" + c.Name + "`"})
	}
	// 按转录行号排：同一次轮询里的摘要与工具调用保持上游的先后顺序
	sort.SliceStable(es, func(i, j int) bool { return es[i].line < es[j].line })
	for _, e := range es {
		l.add(e.key, e.text)
	}
	return advanced
}

// full 返回思考全文。final 是终态（或 pending 帧里）的思考摘要全文，parts 是它的逐条拆分。
//
// 没收到过转录时原样用 final（它在 pending 帧里可能逐步变长，nextReasoning 照常求增量）；
// 收到过转录时只把 final 里没展示过的条目补在后面，免得同一段摘要出现两次。
func (l *liveTrace) full(final string, parts []string) string {
	if l.b.Len() == 0 {
		return final
	}
	if len(parts) == 0 && strings.TrimSpace(final) != "" {
		parts = []string{final}
	}
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			l.add("r:"+squash(t), t)
		}
	}
	return l.b.String()
}

func (l *liveTrace) add(key, text string) {
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	if l.seen[key] {
		return
	}
	l.seen[key] = true
	if l.b.Len() > 0 {
		l.b.WriteString("\n\n")
	}
	l.b.WriteString(text)
}

// squash 去掉全部空白：同一段摘要在转录与终态 payload 里的换行、空格写法不一定一致。
func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
