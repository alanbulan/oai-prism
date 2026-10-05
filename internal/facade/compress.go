package facade

import (
	"fmt"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 历史折叠与按字节预算裁剪。
//
// 上游只读「最后一条 system + 最后一条 user」，往轮对话只能折成文本挂进 system
// （见 upstream_input.go），而这一整条又受上游单条字节上限约束（facade.max_prompt_bytes，
// 实测约 100 KiB，见 context_limit.go）。历史怎么折、超了怎么裁都收口在这里：
//
//   - 放得下就原样全留。旧实现超过 6 轮就把早期消息截成前 197 个字，2026-10-04 实测
//     一段只有 481 tokens 的对话因此答不出首轮给的暗号；
//   - 放不下先丢大块的工具输出与助手回复（从最旧的丢起），再丢其余消息（也从最旧的
//     丢起）。用户自己说的话最后才动 —— 交代的要求、给的暗号都在里面，Codex 自己压缩
//     时保留的也正是用户消息。丢掉的位置原地注明省略了几条、多少字节，让模型知道上文
//     不完整，而不是悄悄缺一块。
//
// Codex 桥请求同样在这里裁：实测 Codex 0.160 收到 context_length_exceeded 只会结束
// 本轮、下一轮照发同样的历史，并不会因此压缩，所以网关自己保证请求放得下。
// 只有本轮内容本身（system + 最后一条 user）就超限时才失败（见 context_limit.go）。

const historyHeader = "\n\n[Previous Conversation History]\n"

// historyOmittedMark 是省略说明里的固定片段（historyTrimmed 据此判断是否裁过）。
const historyOmittedMark = " omitted: exceeded the upstream message size limit]"

const (
	// omitMarkerCost 是一行省略说明的字节上限（数字位数按最大估）。
	omitMarkerCost = len("[999999 messages (9999999999 bytes)") + len(historyOmittedMark) + 1
	// bulkyEntryBytes 以上的工具输出 / 助手回复算"大块"，超限时先丢。
	// 太小的不先丢：一行省略说明本身就要近百字节，丢了反而更长。
	bulkyEntryBytes = 512
	// promptOverheadReserve 是算历史预算时为平台声明与上游包装预留的字节。
	promptOverheadReserve = len(inlineNotice) + 64
)

// historyEntry 是折叠进 system 的一条往轮消息。
type historyEntry struct {
	speaker string // User / Assistant / Tool
	text    string
	fp      uint64 // 非 0 时是比对用的指纹（见 ChatMessage.fp），否则按 speaker + text 算
}

// speakerOf 把消息角色映射成历史里的说话人标签。
func speakerOf(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return "Assistant"
	case "tool", "function":
		return "Tool"
	}
	return "User"
}

// historyBudget 算出历史段可用的字节数：limit 减去同一条提示词里其余部分（fixed）与预留。
// limit <= 0 表示不限，返回 0；其余部分已经超限时返回 -1（历史一条也放不下）。
func historyBudget(limit, fixed int) int {
	if limit <= 0 {
		return 0
	}
	if b := limit - fixed - promptOverheadReserve; b > 0 {
		return b
	}
	return -1
}

// renderHistory 把往轮消息渲染成以 "\n\n" 开头的 [Previous Conversation History] 段，
// 总长不超过 budget 字节。budget == 0 表示不限；budget < 0 或没有可渲染的内容时返回空串。
func renderHistory(entries []historyEntry, budget int) string {
	var lines []string
	var bulky []bool // 超限时先丢：工具输出（含桥的 [CLIENT RESULT]）与助手回复中的大块
	total := len(historyHeader)
	for _, e := range entries {
		t := strings.TrimSpace(e.text)
		if t == "" {
			continue
		}
		l := e.speaker + ": " + t
		lines = append(lines, l)
		bulky = append(bulky, len(l) >= bulkyEntryBytes &&
			(e.speaker != "User" || strings.HasPrefix(t, "[CLIENT RESULT")))
		total += len(l) + 1
	}
	if len(lines) == 0 || budget < 0 {
		return ""
	}
	if budget == 0 || total <= budget {
		return historyHeader + strings.Join(lines, "\n")
	}

	// 两轮丢弃：先丢大块（从最旧的起），仍超再从最旧的起丢其余。每段连续丢弃
	// 都要换成一行省略说明，丢的时候把这一行的开销也算进去。
	dropped := make([]bool, len(lines))
	over := total - budget
	drop := func(i int) {
		dropped[i] = true
		over -= len(lines[i]) + 1
		prev := i > 0 && dropped[i-1]
		next := i+1 < len(lines) && dropped[i+1]
		switch {
		case !prev && !next:
			over += omitMarkerCost // 新开一段
		case prev && next:
			over -= omitMarkerCost // 两段并成一段
		}
	}
	for pass := 0; pass < 2 && over > 0; pass++ {
		for i := 0; i < len(lines) && over > 0; i++ {
			if !dropped[i] && (pass == 1 || bulky[i]) {
				drop(i)
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(historyHeader)
	for i := 0; i < len(lines); {
		if sb.Len() > len(historyHeader) {
			sb.WriteByte('\n')
		}
		if !dropped[i] {
			sb.WriteString(lines[i])
			i++
			continue
		}
		n, bytes := 0, 0
		for ; i < len(lines) && dropped[i]; i++ {
			n++
			bytes += len(lines[i]) + 1
		}
		fmt.Fprintf(&sb, "[%d messages (%d bytes)%s", n, bytes, historyOmittedMark)
	}
	return sb.String()
}

// historyTrimmed 报告首条 system 里折叠的历史是否因超出单条上限被裁过。
func historyTrimmed(items []prism.InputItem) bool {
	if len(items) == 0 || !isSystemRole(items[0].Role) {
		return false
	}
	for _, c := range items[0].Content {
		if i := strings.LastIndex(c.Text, historyHeader); i >= 0 && strings.Contains(c.Text[i:], historyOmittedMark) {
			return true
		}
	}
	return false
}
