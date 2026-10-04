package facade

import (
	"fmt"
	"strings"
)

// CompressionConfig 控制上下文压缩参数。
type CompressionConfig struct {
	// MaxHistoryTurns 是触发历史压缩的最大轮数阈值（1 轮 = 1 次 user + 1 次 assistant）。
	MaxHistoryTurns int
	// KeepRecentTurns 是保留完整原始对话的最近轮数。
	KeepRecentTurns int
	// MaxCharsThreshold 是触发压缩的最大字符数阈值。
	MaxCharsThreshold int
}

// DefaultCompressionConfig 默认上下文压缩配置（短会话兜底）。
var DefaultCompressionConfig = CompressionConfig{
	MaxHistoryTurns:   6,
	KeepRecentTurns:   2,
	MaxCharsThreshold: 16000,
}

// DefaultContextWindowConfig 适用于现代大上下文窗口（如 Codex 256K / 258,400 tokens）的压缩配置。
// 注意：单次请求输入大小与连续多轮输入的上下文窗口是两个不同维度的概念。
// 压缩只在多轮累积真正达到/逼近上下文窗口容量（如 ~200k tokens，约 600,000 字符，或超过 40 轮）时才触发；
// 未达到窗口阈值时完整保留全部历史，不进行过度截断。
var DefaultContextWindowConfig = CompressionConfig{
	MaxHistoryTurns:   40,
	KeepRecentTurns:   10,
	MaxCharsThreshold: 600000,
}

// CompressChatMessages 对超长的 messages 列表执行确定性滑动窗口与摘要压缩。
// 返回压缩后的消息列表，以及提取出的历史摘要文本（若有）。
func CompressChatMessages(msgs []ChatMessage, cfg CompressionConfig) ([]ChatMessage, string) {
	if len(msgs) <= 4 {
		return msgs, ""
	}

	totalChars := 0
	for _, m := range msgs {
		totalChars += len(m.Content.Text())
	}

	// 找出所有非 system/developer 消息
	type indexedMsg struct {
		index int
		msg   ChatMessage
	}
	var nonSys []indexedMsg
	var sysMsgs []ChatMessage

	for i, m := range msgs {
		r := strings.ToLower(strings.TrimSpace(m.Role))
		if r == "system" || r == "developer" {
			sysMsgs = append(sysMsgs, m)
		} else {
			nonSys = append(nonSys, indexedMsg{index: i, msg: m})
		}
	}

	// 如果非系统消息轮数未超标且总字符未超标，无需压缩
	if len(nonSys) <= cfg.MaxHistoryTurns*2 && totalChars <= cfg.MaxCharsThreshold {
		return msgs, ""
	}

	// 需要保留的最新消息数
	keepCount := cfg.KeepRecentTurns * 2
	if keepCount >= len(nonSys) {
		keepCount = len(nonSys) - 1
	}
	if keepCount < 1 {
		keepCount = 1
	}

	splitIdx := len(nonSys) - keepCount
	toCompress := nonSys[:splitIdx]
	toKeep := nonSys[splitIdx:]

	var summaryBuilder strings.Builder
	summaryBuilder.WriteString("[Compressed Earlier Conversation Context]\n")

	for i, item := range toCompress {
		r := strings.ToLower(strings.TrimSpace(item.msg.Role))
		speaker := "User"
		if r == "assistant" {
			speaker = "Assistant"
		} else if r == "tool" || r == "function" {
			speaker = "Tool"
		}

		// 截断单条过长历史，避免压缩后依然膨胀。按字符截：按字节切会把中文切成半个字符。
		txt := truncateRunes(item.msg.Content.Text(), 197)
		summaryBuilder.WriteString(fmt.Sprintf("%d. %s: %s\n", i+1, speaker, txt))
	}

	summaryText := summaryBuilder.String()

	// 组装最终的消息数组：系统消息 + 最近的完整消息
	out := make([]ChatMessage, 0, len(sysMsgs)+len(toKeep))
	out = append(out, sysMsgs...)
	for _, item := range toKeep {
		out = append(out, item.msg)
	}

	return out, summaryText
}
