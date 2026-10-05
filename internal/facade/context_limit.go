package facade

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 上游单条消息上限与 context_length_exceeded。
//
// 上游把 [system, user] 压成一条消息交给模型，并按 UTF-8 字节限长：2026-10-04 实测
// 合计 102,299 字节可过、104,560 字节报 "This request is too large to send"，
// 中文与英文是同一条线（中文 32k tokens 照样能过），所以不是 token 限制。
//
// 超限的请求照发会怎样（同日 Codex 实测）：
//   - 上游先回 too large；同一请求重发常变成 "Error while processing conversation
//     (403)"，又被当成沙箱未就绪重试 10 次，一次就是 5 分钟；
//   - 错误码是 server_error，Codex 当断线重连（Reconnecting... 1/5、2/5…），既不压缩
//     也不裁历史 —— 它以为窗口还很空（模型目录写的 272k），会话就此卡死。
//
// 所以：折叠历史时按字节预算裁掉最旧的部分（compress.go），保证放得下；连本轮内容
// 本身（system + 最后一条 user）都放不下时，发之前就以 context_length_exceeded 失败
// —— 客户端得到明确、不可重试的错误，而不是被当断线反复重连。Anthropic 客户端
// （Claude Code）认 "prompt is too long"。
//
// 注意 Codex 0.160 收到 context_length_exceeded 只会结束本轮（"Codex ran out of room
// in the model's context window"），并不会因此压缩，下一轮照发同样的历史（同日实测，
// 窗口配成 272k 时也一样）—— 所以历史超限必须由网关裁，不能指望它。

// ErrContextTooLarge 表示本轮提示词超过了上游单条消息上限（网关预检或上游拒绝）。
var ErrContextTooLarge = errors.New("context_length_exceeded")

// contextTooLargeError 携带超限的量化信息，errors.Is(err, ErrContextTooLarge) 成立。
type contextTooLargeError struct {
	Bytes    int    // 规整后 system + user 的字节数
	Limit    int    // 生效上限（未配置时为 0）
	Tokens   int    // 同一份内容的 token 数
	Upstream string // 上游拒绝时的原文；网关预检拒绝时为空
}

func (e *contextTooLargeError) Error() string {
	if e.Upstream != "" {
		return fmt.Sprintf("上下文超过上游单条消息上限（%d 字节 / %d tokens，上游拒绝：%s）。请压缩历史（Codex 执行 /compact）或开新会话",
			e.Bytes, e.Tokens, e.Upstream)
	}
	return fmt.Sprintf("上下文超过上游单条消息上限：本轮 %d 字节 / %d tokens，上限 %d 字节（facade.max_prompt_bytes）。请压缩历史（Codex 执行 /compact）或开新会话",
		e.Bytes, e.Tokens, e.Limit)
}

func (e *contextTooLargeError) Is(target error) bool { return target == ErrContextTooLarge }

// promptBytes 是条目里文本内容的 UTF-8 字节数（图片走附件，不占这条消息的长度）。
func promptBytes(items []prism.InputItem) int {
	n := 0
	for _, it := range items {
		for _, c := range it.Content {
			switch c.Type {
			case "input_image", "input_file":
			default:
				n += len(c.Text)
			}
		}
	}
	return n
}

// isUpstreamTooLarge 判断上游的失败文案是否为单条消息超限。
func isUpstreamTooLarge(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "too large to send")
}

// upstreamPromptItems 返回本轮真正发往上游的条目：规整成 [system, user]，
// 非桥请求再前置平台声明。预检与 runOnce 共用，保证量的是同一份内容。
func (r *Runner) upstreamPromptItems(req *RunRequest) []prism.InputItem {
	items := canonicalUpstreamInput(req.Input)
	if notice := r.noticeFor(req); notice != "" {
		items = prependSystemText(items, notice)
	}
	return items
}

// checkPromptSize 在占用账号、建项目、申请沙箱之前检查提示词大小。
//
// 原生续接的请求只要求本轮消息加精简 system 放得下：完整 system 放不下时单独发一轮
// （见 native.go 的 deltaItems）。真正发出前 runOnce 还会按实际条目再量一次。
func (r *Runner) checkPromptSize(req *RunRequest) error {
	limit := r.cfg.Facade.PromptByteLimit()
	if limit <= 0 {
		return nil
	}
	items := r.upstreamPromptItems(req)
	n := promptBytes(items)
	if n <= limit {
		return nil
	}
	if nt := req.Native; nt != nil && nt.conv != nil && nativeMinBytes(nt.conv)+promptOverheadReserve <= limit {
		return nil
	}
	return &contextTooLargeError{Bytes: n, Limit: limit, Tokens: countInputTokens(items)}
}

// responsesErrorCode 是 response.failed 里的 error.code。Codex 只有收到
// context_length_exceeded 才会压缩或裁历史，其余代码一律当可重试的断线。
func responsesErrorCode(err error) string {
	if errors.Is(err, ErrContextTooLarge) {
		return "context_length_exceeded"
	}
	return ""
}

// anthropicTooLongMessage 用 Anthropic 官方文案描述超限：Claude Code 等客户端
// 按 "prompt is too long: N tokens > M maximum" 触发压缩。M 是按本轮的字节/token
// 比折算出的上限。
func anthropicTooLongMessage(err error) string {
	var e *contextTooLargeError
	if !errors.As(err, &e) {
		return "prompt is too long"
	}
	limit := e.Limit
	if limit <= 0 {
		limit = 100 << 10
	}
	max := e.Tokens
	if e.Bytes > 0 {
		max = int(int64(e.Tokens) * int64(limit) / int64(e.Bytes))
	}
	if max >= e.Tokens {
		max = e.Tokens - 1
	}
	return fmt.Sprintf("prompt is too long: %d tokens > %d maximum", e.Tokens, max)
}

// writeContextTooLarge 在流开始之前以 HTTP 400 回超限错误（OpenAI 形态，code 为
// context_length_exceeded）。err 不是超限错误时返回 false、不写任何东西。
func writeContextTooLarge(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, ErrContextTooLarge) {
		return false
	}
	writeErrorCode(w, http.StatusBadRequest, "invalid_request_error", "context_length_exceeded", err.Error())
	return true
}

// writeAnthropicTooLarge 同 writeContextTooLarge，但用 Anthropic 的错误形态与文案。
func writeAnthropicTooLarge(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, ErrContextTooLarge) {
		return false
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "invalid_request_error", "message": anthropicTooLongMessage(err)},
	})
	_, _ = w.Write(body)
	return true
}
