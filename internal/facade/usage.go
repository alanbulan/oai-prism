package facade

import (
	"sync"

	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/tokens"
)

// 用量计算。
//
// 上游轮询响应从不回 usage（顶层与 payload 均无此键，抓包实证），
// 网关按本轮实际收发的内容用 o200k_base 分词表精确计数：
//   - 输入：真正发往上游的 input 条目（system + 折叠历史 + 本轮消息 + 图片），
//     含 OpenAI 官方口径的消息帧开销 —— 全量折叠模式下它就是"上下文窗口占用"；
//   - 输出：模型生成的正文与推理文本（推理部分另记 reasoning_tokens，已含在输出内）。

// countInputTokens 计算 input 条目的 prompt token 数。
func countInputTokens(items []prism.InputItem) int {
	if len(items) == 0 {
		return 0
	}
	n := tokens.ReplyPriming
	for _, it := range items {
		n += tokens.PerMessage + tokens.Count(it.Role)
		for _, c := range it.Content {
			switch c.Type {
			case "input_image":
				n += tokens.ImageURL(c.ImageURL, c.Detail)
			case "input_file":
				// 文件以项目路径引用的形式出现在上下文里
				n += tokens.Count(c.Filename)
			default:
				n += tokens.Count(c.Text)
			}
		}
	}
	return n
}

// countInputAsync 在后台计数：上游生成通常要数秒，计数与之并行，
// 终态时直接取结果，长上下文也不会给最后一个事件增加延迟。
func countInputAsync(items []prism.InputItem) func() int {
	ch := make(chan int, 1)
	go func() { ch <- countInputTokens(items) }()
	return sync.OnceValue(func() int { return <-ch })
}

// measuredUsage 由输入计数与本轮生成结果组装用量。
func measuredUsage(in int, res *RunResult) *prism.Usage {
	reasoning := tokens.Count(res.Reasoning)
	out := tokens.Count(res.Text) + reasoning
	return &prism.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out, ReasoningTokens: reasoning}
}

// outputOnlyUsage 是拿不到输入条目时的兜底（runner 成功路径总会给出完整用量）。
func outputOnlyUsage(res *RunResult) *prism.Usage {
	if res == nil {
		return &prism.Usage{}
	}
	return measuredUsage(0, res)
}

func newChatUsage(u *prism.Usage) *ChatUsage {
	return &ChatUsage{
		PromptTokens:            u.InputTokens,
		CompletionTokens:        u.OutputTokens,
		TotalTokens:             u.TotalTokens,
		PromptTokensDetails:     &PromptTokensDetails{},
		CompletionTokensDetails: &CompletionTokensDetails{ReasoningTokens: u.ReasoningTokens},
	}
}

func newResponsesUsage(u *prism.Usage) *ResponsesUsage {
	return &ResponsesUsage{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		TotalTokens:         u.TotalTokens,
		OutputTokensDetails: ResponsesOutputTokenDetails{ReasoningTokens: u.ReasoningTokens},
	}
}
