package facade

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/metrics"
	"github.com/oai-prism/oaiprism/internal/middleware"
)

// Handler 是兼容门面的 HTTP 层。
type Handler struct {
	cfg    *config.Config
	log    *slog.Logger
	runner *Runner
	app    *metrics.App
}

// NewHandler 构造门面处理器。
func NewHandler(cfg *config.Config, log *slog.Logger, runner *Runner, app *metrics.App) *Handler {
	return &Handler{cfg: cfg, log: log, runner: runner, app: app}
}

// Register 注册路由。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("POST /v1/completions", h.handleCompletions)
	mux.HandleFunc("POST /v1/responses", h.handleResponses)
	mux.HandleFunc("POST /v1/messages", h.handleAnthropicMessages)

	mux.HandleFunc("GET /v1/models", h.handleModels)
	mux.HandleFunc("GET /v1/models/{id}", h.handleModelByID)

	// 无 /v1 前缀的别名：部分客户端（含 PrismOpenAIProxy 生态）直接打
	// /chat/completions。与其让它们拿到 404 后困惑，不如顺手接住。
	mux.HandleFunc("POST /chat/completions", h.handleChatCompletions)
	mux.HandleFunc("POST /responses", h.handleResponses)
	mux.HandleFunc("GET /models", h.handleModels)

	// 一些客户端会先探测这些端点。
	mux.HandleFunc("GET /v1", h.handleIndex)
	mux.HandleFunc("POST /v1/embeddings", h.handleUnsupported)
	mux.HandleFunc("POST /v1/images/generations", h.handleUnsupported)
}

// 请求级控制头。
const (
	HeaderSession = "X-Oaiprism-Session"
	HeaderAccount = "X-Oaiprism-Account"
	HeaderProject = "X-Oaiprism-Project"
	HeaderEffort  = "X-Oaiprism-Effort"
	HeaderModel   = "X-Oaiprism-Model"
)

// readBody 读取并限制请求体。
func (h *Handler) readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("请求体为空")
	}
	limit := h.cfg.Server.MaxBodyBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取请求体: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("请求体超过上限 %d 字节", limit)
	}
	return b, nil
}

// writeError 输出 OpenAI 风格错误（code 为状态码）。
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeErrorCode(w, status, typ, strconv.Itoa(status), msg)
}

// writeErrorCode 输出带指定 code 的 OpenAI 风格错误（如 context_length_exceeded）。
func writeErrorCode(w http.ResponseWriter, status int, typ, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body, _ := json.Marshal(ErrorResponse{Error: ErrorPayload{
		Message: msg,
		Type:    typ,
		Code:    code,
	}})
	_, _ = w.Write(body)
}

// writeJSON 输出 JSON。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// resolveModel 把对外模型名映射到上游模型与推理强度。
//
// 解析优先级（后者覆盖前者）：
//
//	默认模型  <  models 映射表  <  客户端显式 reasoning_effort  <  X-Oaiprism-* 头
func (h *Handler) resolveModel(requested, effort string) (model string, outEffort string) {
	f := &h.cfg.Facade

	model = strings.TrimSpace(requested)
	if model == "" {
		model = f.DefaultModel
	}
	outEffort = strings.TrimSpace(effort)

	if m, ok := f.Models[model]; ok {
		if m.Model != "" {
			model = m.Model
		}
		if outEffort == "" && m.ReasoningEffort != "" {
			outEffort = m.ReasoningEffort
		}
	}
	return model, outEffort
}

// applyHeaderOverrides 处理 X-Oaiprism-* 头的覆盖。
func applyHeaderOverrides(r *http.Request, model, effort *string) (accountID, projectID string) {
	if v := strings.TrimSpace(r.Header.Get(HeaderModel)); v != "" {
		*model = v
	}
	if v := strings.TrimSpace(r.Header.Get(HeaderEffort)); v != "" {
		*effort = v
	}
	accountID = strings.TrimSpace(r.Header.Get(HeaderAccount))
	projectID = strings.TrimSpace(r.Header.Get(HeaderProject))
	return
}

// bindLogResult 把本次运行实际使用的账号与 token 用量记进审计上下文。
//
// 账号是 Runner 在租约阶段才确定的，外层事先不知道。不能写回请求头：
// X-Oaiprism-Account 是客户端可控输入，失败时流水会记下调用方伪造的值。
// res 可能为 nil（启动即失败，连账号都没租到），此时不写。
func bindLogResult(r *http.Request, res *RunResult) {
	if res == nil {
		return
	}
	if res.AccountID != "" {
		middleware.RecordLogAccount(r, res.AccountID)
	}
	if res.Usage != nil {
		middleware.RecordLogUsage(r, res.Usage.InputTokens, res.Usage.OutputTokens)
	}
}

// conversationKey 推导会话身份。
//
// 推导顺序：
//  1. 显式头 X-Oaiprism-Session（客户端最清楚自己在聊哪个会话）
//  2. body.user（OpenAI 的 user 字段通常被用来放终端用户 ID）
//  3. 从 client_metadata / prompt_cache_key / conversation_id / user 提取会话键
//  4. 对首条 user 消息做稳定指纹（严禁包含会被折叠历史污染的 system 消息）
//
// 绝大多数客户端不带专有 Header，但 Codex CLI 会带 client_metadata 或 prompt_cache_key，
// 即使都不带，首条 user 消息（首问）在多轮中也是绝对不变的，取指纹就能让后续轮次
// 100% 命中同一个账号与项目，彻底杜绝每轮新建 Project 与沙箱重新申请。
func conversationKey(r *http.Request, body map[string]json.RawMessage, msgs []ChatMessage) string {
	return scopeKey(r, conversationKeyBase(r, body, msgs))
}

// scopeKey 给会话键加上调用方（API Key 指纹）前缀，实现租户隔离：
// 不同 Key 的调用方即使会话标识或首条消息相同，也落在不同的会话链上。
// 未经鉴权（本机免 Key）时不加前缀。
func scopeKey(r *http.Request, key string) string {
	if key == "" {
		return ""
	}
	if t := middleware.Tenant(r.Context()); t != "" {
		return t + "|" + key
	}
	return key
}

// conversationKeyBase 是未加租户前缀的会话键推导（规则见 conversationKey）。
func conversationKeyBase(r *http.Request, body map[string]json.RawMessage, msgs []ChatMessage) string {
	if v := strings.TrimSpace(r.Header.Get(HeaderSession)); v != "" {
		return "h:" + v
	}

	// 1. client_metadata（Codex CLI 0.15x / 0.16x 关键会话键）
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

	// 2. prompt_cache_key
	if raw, ok := body["prompt_cache_key"]; ok && len(raw) > 0 {
		var pck string
		if err := json.Unmarshal(raw, &pck); err == nil && strings.TrimSpace(pck) != "" {
			return "pck:" + strings.TrimSpace(pck)
		}
	}

	// 3. 会话 ID（客户端带回我们回传的上游会话 ID，见 conversationIDFrom）/ session_id / metadata
	if cid := conversationIDFrom(r, body); cid != "" {
		return "cid:" + cid
	}
	if raw, ok := body["session_id"]; ok && len(raw) > 0 {
		var sid string
		if err := json.Unmarshal(raw, &sid); err == nil && strings.TrimSpace(sid) != "" {
			return "sid:" + strings.TrimSpace(sid)
		}
	}
	if raw, ok := body["metadata"]; ok && len(raw) > 0 {
		var md map[string]any
		if err := json.Unmarshal(raw, &md); err == nil {
			for _, k := range []string{"session_id", "sessionId", "thread_id", "threadId", "conversation_id", "conversationId"} {
				if v, ok := md[k].(string); ok && strings.TrimSpace(v) != "" {
					return "md:" + strings.TrimSpace(v)
				}
			}
		}
	}

	// 4. user 字段
	if raw, ok := body["user"]; ok && len(raw) > 0 {
		var u string
		if err := json.Unmarshal(raw, &u); err == nil && strings.TrimSpace(u) != "" {
			return "u:" + strings.TrimSpace(u)
		}
	}

	// 5. 首条真实 user 消息采样指纹（排除静态环境指令如 AGENTS.md，严禁包含 system 消息，保持指纹长期稳定）
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "user") {
			txt := strings.TrimSpace(m.Content.Text())
			if txt == "" || isStaticInstruction(txt) {
				continue
			}
			sum := sha256.Sum256([]byte(txt))
			return "f:u:" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10])
		}
	}

	return ""
}

// metadataWith 构造上游要的 metadata 容器。
//
// 值为 nil 时直接返回 nil，避免往请求里塞 {"tools": null}
// 这种"看起来有值其实没值"的字段 —— 部分严格校验的服务端会因此报错。
func metadataWith(key string, val any) map[string]any {
	if val == nil {
		return nil
	}
	if sl, ok := val.([]any); ok && len(sl) == 0 {
		return nil
	}
	return map[string]any{key: val}
}

// metadataReservedKeys 是客户端 metadata 不允许覆盖的保留键。
//
// 这些键由网关自己注入（projectId / sandbox_* / model / reasoning_effort /
// userId / frontend_origin）或由我们翻译（tools）。放行它们等于让调用方
// 冒充网关：换模型、挪沙箱、顶替身份。对照 PrismOpenAIProxy 的
// metadataFor() 保留键清单，按我们的注入点扩充。
var metadataReservedKeys = map[string]struct{}{
	"projectId": {}, "project_id": {},
	"userId": {}, "user_id": {},
	"model": {}, "reasoning_effort": {},
	"frontend_origin": {},
	"sandbox_url":     {}, "sandbox_token": {},
	"sandboxUrl": {}, "sandboxToken": {},
	"tools":                      {},
	"prism_conversation_id":      {},
	"prism_previous_response_id": {},
}

// clientMetadata 解析请求体里的 metadata 字段，剔除保留键后返回。
//
// 为什么透传而不是丢弃：metadata 是调用方给上游塞运行上下文的官方通道
// （如端到端追踪 ID）。此前整个丢弃属于静默失效 —— 客户端以为传了，
// 上游一无所知。返回 nil 表示没有可透传内容。
func clientMetadata(raw map[string]json.RawMessage) map[string]any {
	rawMD, ok := raw["metadata"]
	if !ok || len(rawMD) == 0 {
		return nil
	}
	var md map[string]any
	if err := json.Unmarshal(rawMD, &md); err != nil || len(md) == 0 {
		return nil
	}
	out := make(map[string]any, len(md))
	for k, v := range md {
		if _, reserved := metadataReservedKeys[k]; reserved {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// metadataEffort 从请求体 metadata 里取 reasoning_effort（在保留键过滤之前）。
//
// 这是三级回落的中间一级：顶层 reasoning_effort 字段 > metadata.reasoning_effort >
// 模型映射表配置。对照 PrismOpenAIProxy server.mjs:124 的行为。
func metadataEffort(raw map[string]json.RawMessage) string {
	rawMD, ok := raw["metadata"]
	if !ok || len(rawMD) == 0 {
		return ""
	}
	var md struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(rawMD, &md); err != nil {
		return ""
	}
	return strings.TrimSpace(md.ReasoningEffort)
}

// mergeMetadata 把客户端 metadata（已过滤）与网关注入的键合并。
//
// 网关注入优先：它们是我们对上游的承诺，不能被调用方影响。
func mergeMetadata(client, injected map[string]any) map[string]any {
	if len(client) == 0 {
		return injected
	}
	if len(injected) == 0 {
		return client
	}
	out := make(map[string]any, len(client)+len(injected))
	for k, v := range client {
		out[k] = v
	}
	for k, v := range injected {
		out[k] = v
	}
	return out
}

// conversationIDFrom 取客户端带回的会话 ID。
//
// 网关在响应头 x-prism-conversation-id 与响应体里回传上游会话 ID；客户端带回来时它就是
// 会话键（"cid:<ID>"），原生续接凭它找回绑定（见 native.go nativeConvKey）。它不会被
// 透传给上游：上游只续接经 Server Action 登记的会话，由 runner 自行登记。
//
// 读取顺序（对照 PrismOpenAIProxy server.mjs:177，兼容其客户端生态）：
//  1. 请求头 x-prism-conversation-id
//  2. 请求体 conversation_id（snake_case / camelCase 都认）
//  3. 请求体 metadata.prism_conversation_id（有些客户端塞在 metadata 里）
func conversationIDFrom(r *http.Request, body map[string]json.RawMessage) string {
	if v := strings.TrimSpace(r.Header.Get("x-prism-conversation-id")); v != "" {
		return v
	}
	for _, k := range []string{"conversation_id", "conversationId"} {
		raw, ok := body[k]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(raw, &v); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if rawMD, ok := body["metadata"]; ok && len(rawMD) > 0 {
		var md struct {
			PrismConversationID string `json:"prism_conversation_id"`
		}
		if err := json.Unmarshal(rawMD, &md); err == nil {
			return strings.TrimSpace(md.PrismConversationID)
		}
	}
	return ""
}

// anthropicToolsMetadata 把 Anthropic 工具定义塞进 metadata。
func anthropicToolsMetadata(tools []AnthropicTool) []any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{"name": t.Name, "description": t.Description}
		if len(t.InputSchema) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(t.InputSchema, &schema); err == nil {
				fn["parameters"] = schema
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// ---------------------------- ID 生成 ----------------------------

var idCounter atomic.Uint64

// idSeqState 是 xorshift64 状态。用它而不是 crypto/rand：
// 我们要的只是"看起来随机、不重复"，而不是密码学安全，
// 而 crypto/rand 在每次调用都要走 syscall 或全局锁。
var idSeqState atomic.Uint64

func init() {
	idSeqState.Store(uint64(time.Now().UnixNano()) | 1)
}

func xorshift() uint64 {
	for {
		cur := idSeqState.Load()
		next := cur
		next ^= next << 13
		next ^= next >> 7
		next ^= next << 17
		if idSeqState.CompareAndSwap(cur, next) {
			return next
		}
	}
}

// idAlphabet 必须恰好是 64 个字符。
//
// 这是刻意的约束：64 是 2 的幂，取字符时可以用 v&63 掩码而不是取模，
// 省掉一次除法。但代价是长度必须严格等于 64——
// 少一个字符就会越界 panic（这个 bug 就是被并发测试抓出来的）。
const idAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"

// 编译期断言：字符表长度必须是 64。
var _ [64 - len(idAlphabet)]byte
var _ [len(idAlphabet) - 64]byte

// newID 生成形如 prefix_xxxxxxxx 的标识。
func newID(prefix string) string {
	n := idCounter.Add(1)
	var buf [16]byte
	v := xorshift() ^ (n * 0x9e3779b97f4a7c15) ^ uint64(time.Now().UnixNano())
	for i := range buf {
		buf[i] = idAlphabet[v&63]
		v >>= 6
		if i%10 == 9 {
			v ^= xorshift()
		}
	}
	return prefix + string(buf[:])
}

// handleIndex 返回一个简短的端点说明，方便用户确认服务活着。
func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object":  "list",
		"service": "oaiprism",
		"endpoints": []string{
			"POST /v1/chat/completions",
			"POST /v1/responses",
			"POST /v1/messages",
			"GET  /v1/models",
		},
	})
}

// handleUnsupported 对明确不支持的端点给出清晰错误，
// 而不是让客户端拿到一个 404 后困惑于"是不是路径写错了"。
func (h *Handler) handleUnsupported(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "invalid_request_error",
		fmt.Sprintf("本代理不支持 %s：Prism 上游只提供对话式补全能力", r.URL.Path))
}
