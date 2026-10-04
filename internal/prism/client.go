package prism

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/httpc"
	"github.com/oai-prism/oaiprism/internal/ydoc"
)

// Principal 是一次上游调用所需的"身份 + 传输通道"。
//
// 之所以不直接吃 *account.Account，是为了切断 prism -> account 的反向依赖，
// 让 prism 包可以被单测用假 Principal 完全隔离地驱动。
type Principal struct {
	Client *httpc.Client
	Cred   *creds.Credential
	// ExtraHeaders 是账号级附加头（设备指纹、灰度标记等）。
	ExtraHeaders map[string]string
	// AccountID 仅用于日志与抓包标注，不参与认证。
	AccountID string
}

// Client 是 Prism 上游 API 客户端。
type Client struct {
	base    *httpc.Client
	up      UpstreamOptions
	schema  SchemaOptions
	rec     Recorder
	metrics MetricsHook

	// debug 是可选的调试日志回调，避免 prism 包反向依赖日志库。
	debug func(string, ...any)

	// actions 是运行期发现的 Server Action ID（见 server_action.go）。
	actions serverActionIDs
}

// UpstreamOptions 是客户端需要的上游行为参数。
type UpstreamOptions struct {
	UserAgent     string
	Origin        string
	Referer       string
	Headers       map[string]string
	MaxRetries    int
	RetryBackoff  time.Duration
	RetryMaxDelay time.Duration
}

// SchemaOptions 是字段名与状态词表。
//
// 默认值全部来自真实报文校准（见 config.defaultSchema 的注释），
// 这里保留可配置只是为了应对上游改字段，而不是因为当前值是猜的。
type SchemaOptions struct {
	StartPath  string
	StatusPath string
	StopPath   string

	FieldModel           string
	FieldMessages        string
	FieldInstructions    string
	FieldInput           string
	FieldTools           string
	FieldStream          string
	FieldPreviousRespID  string
	FieldMetadata        string
	FieldUserID          string
	FieldSessionID       string
	FieldProjectID       string
	FieldSandboxID       string
	FieldConversationID  string
	FieldRequestID       string
	FieldTurnState       string
	FieldResponseID      string
	FieldReasoning       string
	FieldReasoningEffort string
	FieldExtra           map[string]any

	RespIDKeys      []string
	RespStatusKeys  []string
	RespTextKeys    []string
	RespDeltaKeys   []string
	RespMessagesKey string
	RespErrorKeys   []string

	StatusDone []string
	StatusFail []string
	StatusRun  []string
}

// MetricsHook 让上层在不引入依赖的情况下观测上游调用。
type MetricsHook interface {
	ObserveUpstream(path string, status int, d time.Duration, err error)
}

// New 构造客户端。
func New(base *httpc.Client, up UpstreamOptions, schema SchemaOptions) *Client {
	if up.UserAgent == "" {
		up.UserAgent = "Mozilla/5.0"
	}
	if up.MaxRetries <= 0 {
		up.MaxRetries = 3
	}
	if up.RetryBackoff <= 0 {
		up.RetryBackoff = 200 * time.Millisecond
	}
	if up.RetryMaxDelay <= 0 {
		up.RetryMaxDelay = 5 * time.Second
	}
	return &Client{base: base, up: up, schema: schema}
}

// SetRecorder 挂载抓包器。
func (c *Client) SetRecorder(r Recorder) { c.rec = r }

// SetMetrics 挂载指标钩子。
func (c *Client) SetMetrics(m MetricsHook) { c.metrics = m }

// SetDebugLogger 挂载调试日志回调。
func (c *Client) SetDebugLogger(fn func(string, ...any)) { c.debug = fn }

// Base 返回底层客户端（供原样反代复用同一连接池）。
func (c *Client) Base() *httpc.Client { return c.base }

// Schema 返回当前字段映射（供上层组装 payload）。
func (c *Client) Schema() SchemaOptions { return c.schema }

// ---------------------------- 底层请求 ----------------------------

// buildHeaders 组装一次调用的请求头。
//
// 这里做的事情比看上去重要：Prism 的前端是一个浏览器应用，
// 它会带上 Origin / Referer / Sec-Fetch-* / Accept-Language 等一整套头。
// 缺失这些头会让请求看起来像脚本爬虫，进而触发风控。
// 我们不复刻全部（那太脆），但保留最关键的几项。
func (c *Client) buildHeaders(p Principal, contentType string, accept string) map[string]string {
	h := make(map[string]string, 16)
	h["Accept"] = accept
	if contentType != "" {
		h["Content-Type"] = contentType
	}
	h["User-Agent"] = c.up.UserAgent
	h["Accept-Language"] = "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7"
	if c.up.Origin != "" {
		h["Origin"] = c.up.Origin
	}
	if c.up.Referer != "" {
		h["Referer"] = c.up.Referer
	}
	h["Sec-Fetch-Dest"] = "empty"
	h["Sec-Fetch-Mode"] = "cors"
	h["Sec-Fetch-Site"] = "same-origin"
	h["sec-ch-ua"] = `"Chromium";v="131", "Not_A Brand";v="24"`
	h["sec-ch-ua-mobile"] = "?0"
	h["sec-ch-ua-platform"] = `"Windows"`
	h["Cache-Control"] = "no-cache"
	h["Pragma"] = "no-cache"

	for k, v := range c.up.Headers {
		h[k] = v
	}

	if p.Cred != nil {
		if ck := p.Cred.EffectiveCookie(); ck != "" {
			h["Cookie"] = ck
		}
		if p.Cred.AccessToken != "" {
			h["Authorization"] = "Bearer " + strings.TrimPrefix(p.Cred.AccessToken, "Bearer ")
		}
		if p.Cred.AccountID != "" {
			h["oai-account-id"] = p.Cred.AccountID
		}
	}
	for k, v := range p.ExtraHeaders {
		h[k] = v
	}
	return h
}

// Do 是原样的底层调用，返回未消费的响应。
//
// 调用方负责关闭 Body。这个函数是"原样反代"与"高层语义方法"的共同基座——
// 只有一条路径意味着只有一套重试/指标/抓包逻辑需要维护。
func (c *Client) Do(ctx context.Context, p Principal, method, path string, header http.Header, body []byte, bodyReader io.Reader) (*http.Response, error) {
	client := p.Client
	if client == nil {
		client = c.base
	}

	var (
		resp    *http.Response
		lastErr error
	)
	noReplay := isNoReplay(ctx)

	for attempt := 0; attempt <= c.up.MaxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避 + 抖动：多实例同时重试时要避免齐步走。
			delay := backoff(c.up.RetryBackoff, c.up.RetryMaxDelay, attempt)
			if err := httpc.SleepCtx(ctx, delay); err != nil {
				return nil, err
			}
		}

		var reqBody io.Reader
		switch {
		case bodyReader != nil && attempt == 0:
			// 流式 body（上传）无法重放，第一次之后就不能再试了。
			reqBody = bodyReader
		case body != nil:
			reqBody = bytes.NewReader(body)
		}

		req, err := client.NewRequest(ctx, method, path, nil, nil)
		if err != nil {
			return nil, err
		}
		req.Header = header.Clone()
		if reqBody != nil {
			if br, ok := reqBody.(*bytes.Reader); ok {
				req.Body = io.NopCloser(br)
				req.ContentLength = int64(br.Len())
				req.GetBody = func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(body)), nil
				}
			} else {
				req.Body = io.NopCloser(reqBody)
				req.ContentLength = -1
				req.GetBody = nil
			}
		}

		start := time.Now()
		meta := Meta{Method: method, Path: path, Started: start, Attempt: attempt, AccountID: p.AccountID}
		if c.rec != nil {
			c.rec.RecordRequest(meta, req.Header, body)
		}

		resp, lastErr = client.HTTP.Do(req)
		meta.Duration = time.Since(start)

		if c.metrics != nil {
			st := 0
			if resp != nil {
				st = resp.StatusCode
			}
			c.metrics.ObserveUpstream(stripQuery(path), st, meta.Duration, lastErr)
		}

		if lastErr != nil {
			// 请求体是一次性流（上传）时不能重试——重试会发出一个空 body。
			if bodyReader != nil {
				return nil, fmt.Errorf("%s %s: %w", method, path, lastErr)
			}
			if !httpc.IsNetworkError(lastErr) {
				return nil, fmt.Errorf("%s %s: %w", method, path, lastErr)
			}
			// 非幂等请求：只有"连接都没建起来"才能确定上游没收到。
			if noReplay && !isDialError(lastErr) {
				return nil, fmt.Errorf("%s %s: %w", method, path, lastErr)
			}
			continue
		}

		if c.rec != nil {
			meta.Status = resp.StatusCode
			c.rec.RecordResponse(meta, resp.Header, nil)
		}

		// 服务端要求退避时，尊重它给的时长。
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			ra := parseRetryAfter(resp.Header.Get("Retry-After"))
			drainClose(resp)
			if attempt == c.up.MaxRetries {
				return nil, &creds.APIError{
					Op:         method + " " + stripQuery(path),
					Status:     resp.StatusCode,
					Body:       "重试耗尽（上游要求退避）",
					RetryAfter: ra,
				}
			}
			if ra > 0 {
				if err := httpc.SleepCtx(ctx, ra); err != nil {
					return nil, err
				}
			}
			continue
		}

		// 5xx 值得重试；4xx 是确定性错误，重试只会浪费配额。
		// 非幂等请求的 500/502/504 可能已被上游处理，不能重放（429/503 已在上面处理）。
		if httpc.RetryableStatus(resp.StatusCode) && attempt < c.up.MaxRetries && bodyReader == nil && !noReplay {
			drainClose(resp)
			continue
		}

		return resp, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("%s %s 重试耗尽", method, path)
	}
	return nil, lastErr
}

type noReplayKey struct{}

// withNoReplay 标记本次调用为非幂等：Do 只重试确定未送达的失败
// （建连失败、429/503 这类上游明确拒收的响应）。
func withNoReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, noReplayKey{}, true)
}

func isNoReplay(ctx context.Context) bool {
	v, _ := ctx.Value(noReplayKey{}).(bool)
	return v
}

// isDialError 判断错误是否发生在建连阶段（请求必然未送达）。
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// doJSON 发送 JSON 并解析响应。
func (c *Client) doJSON(ctx context.Context, p Principal, method, path string, payload any, out any, accept string) (int, http.Header, []byte, error) {
	return c.doJSONExtra(ctx, p, method, path, payload, out, accept, nil)
}

// doJSONExtra 与 doJSON 相同，但可以追加请求头。
//
// 需要它是因为沙箱代理端点还要带 X-Crixet-Sandbox-Token，
// 而沙箱的认证是**双重**的：Cookie（会话）+ 沙箱令牌。
// 只带其中之一会直接 401 且响应体为空 —— 极难排查，
// 所以这里把"额外头"做成一等参数而不是临时 hack。
func (c *Client) doJSONExtra(ctx context.Context, p Principal, method, path string, payload any, out any, accept string, extra map[string]string) (int, http.Header, []byte, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("序列化请求: %w", err)
		}
	}
	if accept == "" {
		accept = "application/json"
	}
	hdr := c.buildHeaders(p, "application/json", accept)
	for k, v := range extra {
		hdr[k] = v
	}

	resp, err := c.Do(ctx, p, method, path, headerFromMap(hdr), body, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("读取响应: %w", err)
	}
	if c.rec != nil {
		c.rec.RecordResponse(Meta{Method: method, Path: path, Status: resp.StatusCode}, resp.Header, raw)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, resp.Header, raw, &creds.APIError{
			Op:         method + " " + stripQuery(path),
			Status:     resp.StatusCode,
			Body:       extractPrismErrorBody(raw),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, resp.Header, raw, fmt.Errorf("解析 %s 响应: %w", stripQuery(path), err)
		}
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// extractPrismErrorBody 从上游错误 JSON 响应中提取可读的错误文本（对照 PrismOpenAIProxy extractPrismErrorBody）。
func extractPrismErrorBody(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return truncate(string(raw), 800)
	}
	target := m
	if p, ok := m["payload"].(map[string]any); ok && p != nil {
		target = p
	}
	for _, k := range []string{"message", "error", "reason", "detail", "rootCause"} {
		if v, ok := target[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if v, ok := m["detail"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return truncate(string(raw), 800)
}

// ---------------------------- 高层语义接口 ----------------------------

// Session 调用 /api/auth/session 验证凭据并拿回账号信息。
func (c *Client) Session(ctx context.Context, p Principal) (map[string]any, error) {
	var out map[string]any
	_, _, _, err := c.doJSON(ctx, p, http.MethodGet, PathAuthSession, nil, &out, "application/json")
	return out, err
}

// CreateProject 建项目（文档：POST /api/projects）。
func (c *Client) CreateProject(ctx context.Context, p Principal, payload map[string]any) (*Project, error) {
	if payload == nil {
		// 字段名来自上游的 400 校验错误（实测）：
		//   [{'loc': ('body','project_uuid'), 'msg': 'Field required'},
		//    {'loc': ('body','title'),        'msg': 'Field required'}]
		// 只发 {name, kind, ...} 会被直接拒掉。
		//
		// project_uuid 是客户端生成的 —— 上游把它当幂等键用，
		// 重发同一个 uuid 会拿回同一个项目，正好省掉"建了又建"。
		payload = map[string]any{
			"project_uuid": newUUID(),
			"title":        "oaiprism",
		}
	}
	var raw json.RawMessage
	if _, _, _, err := c.doJSON(ctx, p, http.MethodPost, PathProjects, payload, &raw, "application/json"); err != nil {
		return nil, err
	}
	// 上游可能直接回 uuid 字符串，也可能回 {"project":{...}}。
	proj := &Project{Raw: raw}
	if err := json.Unmarshal(raw, proj); err != nil || proj.Key() == "" {
		if v, err2 := DecodeAny(raw); err2 == nil {
			proj.ID = FindString(v, []string{"id", "uuid", "project_id", "projectId"})
			proj.UUID = FindString(v, []string{"uuid"})
			if proj.ID == "" {
				if s, ok := v.(string); ok {
					proj.ID = strings.Trim(s, `"`)
				}
			}
		}
	}
	if proj.Key() == "" {
		return nil, fmt.Errorf("建项目成功但未解析出项目 ID，原始响应: %s", truncate(string(raw), 400))
	}
	return proj, nil
}

// ProjectAccess 查询项目访问权（文档：GET /api/project-access?d={UUID}）。
func (c *Client) ProjectAccess(ctx context.Context, p Principal, projectID string) (*ProjectAccess, error) {
	path := PathProjectAccess + "?d=" + url.QueryEscape(projectID)
	var raw json.RawMessage
	_, _, _, err := c.doJSON(ctx, p, http.MethodGet, path, nil, &raw, "application/json")
	if err != nil {
		return nil, err
	}
	pa := &ProjectAccess{Raw: raw}
	_ = json.Unmarshal(raw, pa)
	return pa, nil
}

// ConversationHistory 拉取会话历史（文档：POST /api/codex/conversation-history）。
func (c *Client) ConversationHistory(ctx context.Context, p Principal, payload map[string]any) ([]Message, json.RawMessage, error) {
	var raw json.RawMessage
	if _, _, _, err := c.doJSON(ctx, p, http.MethodPost, PathConversationHistory, payload, &raw, "application/json"); err != nil {
		return nil, nil, err
	}
	v, err := DecodeAny(raw)
	if err != nil {
		return nil, raw, nil
	}
	// 历史可能挂在 messages / history / turns / items 下。
	v2, ok := FindKey(v, []string{"messages", "history", "turns", "items", "conversation"}, 3)
	if !ok {
		if arr, ok := AsSlice(v); ok {
			v2 = arr
		} else {
			return nil, raw, nil
		}
	}
	arr, ok := AsSlice(v2)
	if !ok {
		return nil, raw, nil
	}
	out := make([]Message, 0, len(arr))
	for _, e := range arr {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		var m Message
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, raw, nil
}

// StartResponse_ 发起一次推理（POST /api/llm/response_with_tools_start）。
//
// 返回值有两种形态，必须区分：
//
//	{status:"started",  request_id, turn_state}      -> 去轮询
//	{status:"completed", request_id, response:{...}} -> 已经结束（快或失败）
//
// 失败也走 "completed" 分支：HTTP 200 + response.status:"error"。
// 只看 HTTP 状态码会把失败当成功，这是本协议最容易踩的坑。
func (c *Client) StartResponse(ctx context.Context, p Principal, req *StartRequest) (*StartResponse, error) {
	payload := c.buildStartPayload(req)

	var raw json.RawMessage
	// start 不是幂等操作：请求一旦送达，上游就开始生成（扣额度、占用会话）。
	// 只允许重试"确定没送达"的失败，否则会在同一会话上并发出两次生成。
	if _, _, _, err := c.doJSON(withNoReplay(ctx), p, http.MethodPost, c.schema.StartPath, payload, &raw, "application/json"); err != nil {
		return nil, err
	}

	out := &StartResponse{Raw: raw}
	v, _ := DecodeAny(raw)

	// 优先走精确的协议解析——我们已经知道包络长什么样了。
	if st, ok := c.parseEnvelope(v, raw, "", ""); ok {
		out.RequestID = st.RequestID
		out.TurnState = st.TurnState
		out.ConversationID = st.ConversationID
		out.ListenSnapshot = st.ListenSnapshot
		out.Status = st.Status
		out.Initial = st
		if out.Status == "" {
			out.Status = st.Status
		}
		return out, nil
	}

	// 保底：宽容抽取。
	if v != nil {
		out.RequestID = FindString(v, c.schema.RespIDKeys)
		out.Status = FindString(v, c.schema.RespStatusKeys)
	}
	if out.RequestID == "" && !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		// 有些实现直接把 id 当纯文本回。
		out.RequestID = strings.Trim(strings.TrimSpace(string(raw)), `"`)
	}
	return out, nil
}

// buildStartPayload 按 Schema 组装 start 请求体。
//
// 真实报文体（实测）：
//
//	{"input":[...], "previousResponseId":..., "metadata":{...}, "conversationId":...}
func (c *Client) buildStartPayload(req *StartRequest) map[string]any {
	s := c.schema
	payload := make(map[string]any, 6)

	for k, v := range s.FieldExtra {
		payload[k] = v
	}
	for k, v := range req.Extra {
		payload[k] = v
	}

	// input 是唯一的内容载体，必须恒为数组。
	// 上游对它的校验很直接：不是数组就回 "input must be an array"。
	if v := valueOr(s.FieldInput, "input"); v != "" {
		items := req.Input
		if items == nil {
			items = []InputItem{}
		}
		payload[v] = items
	}

	// 多轮延续。留空时干脆不发这个字段，
	// 避免上游把 null 当成"延续一个不存在的会话"。
	if v := valueOr(s.FieldPreviousRespID, "previousResponseId"); v != "" && req.PreviousResponseID != "" {
		payload[v] = req.PreviousResponseID
	}
	if v := valueOr(s.FieldConversationID, "conversationId"); v != "" && req.ConversationID != "" {
		payload[v] = req.ConversationID
	}

	// metadata：模型与推理强度都在这里，不在顶层。
	meta := make(map[string]any, 8)
	for k, v := range req.Metadata {
		meta[k] = v
	}
	if req.Model != "" {
		meta["model"] = req.Model
	}
	if req.ReasoningEffort != "" {
		meta["reasoning_effort"] = req.ReasoningEffort
	}
	// 调用方身份。字段名走配置（推断值，见 SchemaConfig.FieldUserID）。
	if req.UserID != "" {
		if v := valueOr(s.FieldUserID, "userId"); v != "" {
			meta[v] = req.UserID
		}
	}
	if v := valueOr(s.FieldMetadata, "metadata"); v != "" && len(meta) > 0 {
		payload[v] = meta
	}

	// 以下字段真实报文里没有，但保留配置能力以应对上游变化；
	// 默认 schema 把它们设成中性值，不会污染请求。
	setIf(payload, s.FieldModel, "")
	setIf(payload, s.FieldSessionID, "")
	setIf(payload, s.FieldProjectID, "")

	return payload
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	// 显式置为 "-" 表示"不要发这个字段"。
	if v == "-" {
		return ""
	}
	return v
}

func setIf(m map[string]any, key, val string) {
	if key == "" || val == "" {
		return
	}
	m[key] = val
}

// buildStatusPayload 组装 status / stop 请求体。
//
// 抽出来是因为 stop 用的是同一套字段（request_id + turn_state），
// 且这个组合是"实测得出、不容有误"的 —— 值得被单测直接覆盖。
func (c *Client) buildStatusPayload(req *StatusRequest) map[string]any {
	payload := make(map[string]any, 4)
	for k, v := range c.schema.FieldExtra {
		payload[k] = v
	}
	if k := valueOr(c.schema.FieldRequestID, "request_id"); k != "" && req.RequestID != "" {
		payload[k] = req.RequestID
	}
	if k := valueOr(c.schema.FieldTurnState, "turn_state"); k != "" && len(req.TurnState) > 0 {
		payload[k] = req.TurnState
	}
	if req.WaitMs > 0 {
		// 当前上游不认这个字段，发出去也不会有副作用。
		payload["waitMs"] = req.WaitMs
	}
	return payload
}

// PollResponse 轮询一次（POST /api/llm/response_with_tools_status）。
//
// 真实报文体只有 {request_id, turn_state}。turn_state 必须用上一轮返回的
// 原样传回去 —— 它就是个"续令牌"，自己造一个会被上游直接拒绝。
func (c *Client) PollResponse(ctx context.Context, p Principal, req *StatusRequest, prevText string) (*StatusResponse, error) {
	payload := c.buildStatusPayload(req)

	var raw json.RawMessage
	if _, _, _, err := c.doJSON(ctx, p, http.MethodPost, c.schema.StatusPath, payload, &raw, "application/json"); err != nil {
		return nil, err
	}

	// 空响应（204 / 长轮询超时）不是错误，只是"暂时没新内容"。
	if len(bytes.TrimSpace(raw)) == 0 {
		return &StatusResponse{RequestID: req.RequestID, TurnState: req.TurnState, Status: "pending", Text: prevText, Raw: raw}, nil
	}
	return c.parseStatus(raw, req.RequestID, prevText)
}

// StopResponse 取消一次进行中的生成（POST /api/llm/response_with_tools_stop）。
//
// 为什么值得做：客户端断开时如果不通知上游，那条生成会继续跑完并消耗额度。
// 对于一个"额度就是成本"的代理来说，这是最直接的省钱手段。
func (c *Client) StopResponse(ctx context.Context, p Principal, requestID, conversationID string, turnState json.RawMessage) error {
	if requestID == "" {
		return nil
	}
	payload := map[string]any{}
	if k := valueOr(c.schema.FieldRequestID, "request_id"); k != "" {
		payload[k] = requestID
	} else {
		payload["request_id"] = requestID
	}
	if conversationID != "" {
		payload["conversation_id"] = conversationID
	}
	if len(turnState) > 0 {
		key := "turn_state"
		if k := valueOr(c.schema.FieldTurnState, "turn_state"); k != "" {
			key = k
		}
		payload[key] = turnState
	}

	path := c.schema.StopPath
	if path == "" {
		path = PathResponseStop
	}
	_, _, _, err := c.doJSON(ctx, p, http.MethodPost, path, payload, nil, "application/json")
	return err
}

// ParseStatusPayload 把任意上游响应体归一化成 StatusResponse。
//
// 导出它是为了让编排层能对 "start 响应" 复用同一套宽容解析——
// 有些实现会把首批内容直接塞在 start 的返回里，不检查就会丢首字。
func (c *Client) ParseStatusPayload(raw []byte, fallbackID, prevText string) (*StatusResponse, error) {
	return c.parseStatus(raw, fallbackID, prevText)
}

// parseStatus 把原始响应归一化成 StatusResponse。
//
// 策略是"精确优先、宽容兜底"：
//  1. 先尝试已知包络（我们从真实上游报文与前端源码确认过它的形状）；
//  2. 认不出来才退回通用宽容解析，让上游改形态时编解码器还能凑合活着。
//
// 这个顺序很重要：宽容解析虽然能work，但它对"错误"的识别是启发式的，
// 而本协议恰好用 HTTP 200 + 嵌套 error 表达失败 —— 只有精确解析
// 才能可靠地区分"成功但还没输出"和"已经失败了"。
func (c *Client) parseStatus(raw []byte, fallbackID, prevText string) (*StatusResponse, error) {
	// 空响应（204 / 长轮询超时）属正常：延续上一次的累计文本，
	// 而不是把它清空成"答案变成空了"。
	if len(bytes.TrimSpace(raw)) == 0 {
		return &StatusResponse{
			RequestID: fallbackID,
			Status:    "pending",
			Text:      prevText,
			Raw:       raw,
		}, nil
	}

	v, err := DecodeAny(raw)
	if err != nil {
		// 非 JSON：当成纯文本增量。
		text := string(raw)
		delta, _ := Diff(prevText, text)
		return &StatusResponse{
			RequestID: fallbackID,
			Status:    "pending",
			Text:      text,
			Delta:     delta,
			Raw:       raw,
		}, nil
	}

	if st, ok := c.parseEnvelope(v, raw, fallbackID, prevText); ok {
		return st, nil
	}
	return c.parseGeneric(v, raw, fallbackID, prevText)
}

// parseEnvelope 解析 Prism/Codex 的已知包络。
//
// 形状（实测）：
//
//	{
//	  "status": "started" | "pending" | "completed",
//	  "request_id": "<uuid>",
//	  "conversation_id": null | "<uuid>",
//	  "turn_state": { ... },                 // 不透明白，必须原样回传
//	  "response": {                          // 终态才有
//	    "status": "success" | "error",
//	    "payload": {
//	       "output": [ {type, role, content:[{type,text}]} , ... ]
//	       // 或 error 时：{"reason","message","rootCause","messageKey","httpStatus"}
//	    }
//	  }
//	}
//
// 返回 false 表示"这不是我们认识的包络"，交给通用解析处理。
func (c *Client) parseEnvelope(v any, raw []byte, fallbackID, prevText string) (*StatusResponse, bool) {
	// 1. 优先使用强类型 PrismEnvelope 确定性反序列化，避免模糊猜测与脆弱的反射遍历。
	var env PrismEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.RequestID != "" {
		out := &StatusResponse{
			Raw:            raw,
			RequestID:      env.RequestID,
			ConversationID: env.ConversationID,
			TurnState:      env.TurnState,
			ListenSnapshot: env.ListenSnapshot,
			Status:         strings.ToLower(strings.TrimSpace(env.Status)),
			Usage:          env.Usage,
		}
		if env.Message != "" && out.Status == "error" {
			out.Fail = true
			out.Done = true
			out.Error = env.Message
		}
		if env.Response != nil {
			if strings.EqualFold(env.Response.Status, "error") {
				out.Fail = true
				out.Done = true
				if env.Response.Payload != nil {
					out.ErrorReason = env.Response.Payload.Reason
					out.Error = env.Response.Payload.Message
					if out.Error == "" {
						out.Error = env.Response.Payload.RootCause
					}
				}
				if out.Error == "" {
					out.Error = "上游返回 response.status=error"
				}
				if out.Status == "" {
					out.Status = "completed"
				}
				return out, true
			}
			if env.Response.Payload != nil {
				payload := env.Response.Payload
				// payload.id 是上游真正的 response 句柄（resp_*），
				// 多轮延续靠它；start 的 request_id 只是受理号。
				out.ResponseID = payload.ID
				// 终态 payload 里的驼峰 conversationId 兜底：顶层
				// conversation_id 在终态常为 null，丢了会让会话链断组。
				if payload.ConversationID != "" {
					out.ConversationID = payload.ConversationID
				}
				out.DeltaFiles = payload.DeltaFiles
				out.OutputItems = payload.Output

				// 确定性提取 reasoning 与 assistant 文本
				for _, item := range payload.Output {
					if item.Type == "reasoning" {
						if len(item.Summary) > 0 {
							out.Reasoning = FlattenContent(item.Summary)
						} else if item.Text != "" {
							out.Reasoning = item.Text
						}
					}
				}
				for i := len(payload.Output) - 1; i >= 0; i-- {
					item := payload.Output[i]
					if item.Type == "message" && (item.Role == "assistant" || item.Role == "") {
						var sb strings.Builder
						for _, b := range item.Content {
							if b.Type == "output_text" || b.Type == "text" || b.Type == "input_text" {
								sb.WriteString(b.Text)
							} else if b.Type == "refusal" {
								sb.WriteString(b.Text)
							}
						}
						if sb.Len() > 0 {
							out.Text = sb.String()
							break
						}
					}
				}
				if out.Text == "" {
					for i := len(payload.Output) - 1; i >= 0; i-- {
						if payload.Output[i].Text != "" {
							out.Text = payload.Output[i].Text
							break
						}
					}
				}
				out.ReasoningDelta = out.Reasoning
			}
		}

		switch out.Status {
		case "completed":
			out.Done = true
		case "started", "pending":
		default:
			if out.Status == "" && len(out.TurnState) > 0 {
				out.Status = "pending"
			}
		}

		if !out.Fail {
			delta, reset := Diff(prevText, out.Text)
			out.Delta = delta
			out.Reset = reset
		}
		return out, true
	}

	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}

	rid, _ := m["request_id"].(string)
	if rid == "" {
		// 没有 request_id 就不是这个协议的响应。
		return nil, false
	}

	out := &StatusResponse{Raw: raw, RequestID: rid}
	if fallbackID != "" {
		// 以服务端回的为准，但带上兜底便于排障。
		out.RequestID = rid
	}

	// turn_state 原样保留成 RawMessage，下一轮直接回传。
	if ts, ok := m["turn_state"]; ok && ts != nil {
		if b, merr := json.Marshal(ts); merr == nil {
			out.TurnState = b
		}
	}
	if st, ok := m["status"].(string); ok {
		out.Status = strings.ToLower(strings.TrimSpace(st))
	}
	if cid, ok := m["conversation_id"].(string); ok {
		out.ConversationID = cid
	}
	if cid, ok := m["conversationId"].(string); ok && out.ConversationID == "" {
		out.ConversationID = cid
	}

	// 有的实现把 usage 放在包络顶层。
	if out.Usage == nil {
		if u, ok := m["usage"].(map[string]any); ok {
			out.Usage = &Usage{
				InputTokens:  intOf(u, "input_tokens", "prompt_tokens"),
				OutputTokens: intOf(u, "output_tokens", "completion_tokens"),
				TotalTokens:  intOf(u, "total_tokens"),
			}
			if out.Usage.TotalTokens == 0 {
				out.Usage.TotalTokens = out.Usage.InputTokens + out.Usage.OutputTokens
			}
		}
	}

	// 协议级错误（HTTP 通常已经是 4xx，这里兜一下）。
	if msg, ok := m["message"].(string); ok && out.Status == "error" {
		out.Fail = true
		out.Done = true
		out.Error = msg
	}

	// response 子对象才是真正的业务结果。
	// 关键：失败是 HTTP 200 + response.status:"error"。
	// 只认状态码的实现会把失败当成功，返回一个空的"完成"。
	if rv, ok := m["response"]; ok && rv != nil {
		if rm, ok := rv.(map[string]any); ok {
			rstatus, _ := rm["status"].(string)
			payload, _ := rm["payload"].(map[string]any)
			if strings.EqualFold(rstatus, "error") {
				out.Fail = true
				out.Done = true
				if payload != nil {
					out.ErrorReason = FindString(payload, []string{"reason"})
					out.Error = FindString(payload, []string{"message"})
					if out.Error == "" {
						out.Error = FindString(payload, []string{"rootCause"})
					}
				}
				if out.Error == "" {
					out.Error = "上游返回 response.status=error"
				}
				if out.Status == "" {
					out.Status = "completed"
				}
				return out, true
			}
			if payload != nil {
				out.Text, out.Reasoning = extractCodexOutput(payload)
				out.ReasoningDelta = out.Reasoning
				// payload.id 是上游真正的 response 句柄（resp_* 形态），
				// 多轮延续（previousResponseId）全靠它。宽松分支同样必须提取，
				// 否则强类型分支因形态漂移失败时会话链就断了（2026-10-02 实测）。
				out.ResponseID = FindString(payload, []string{"id"})
				if u, ok := payload["usage"].(map[string]any); ok {
					out.Usage = &Usage{
						InputTokens:  intOf(u, "input_tokens", "prompt_tokens"),
						OutputTokens: intOf(u, "output_tokens", "completion_tokens"),
						TotalTokens:  intOf(u, "total_tokens"),
					}
					if out.Usage.TotalTokens == 0 {
						out.Usage.TotalTokens = out.Usage.InputTokens + out.Usage.OutputTokens
					}
				}
			}
		}
	}

	switch out.Status {
	case "completed":
		out.Done = true
	case "started", "pending":
		// 运行中，继续轮询。
	default:
		if out.Status == "" && len(out.TurnState) > 0 {
			out.Status = "pending"
		}
	}

	if !out.Fail {
		delta, reset := Diff(prevText, out.Text)
		out.Delta = delta
		out.Reset = reset
	}
	return out, true
}

// extractCodexOutput 从 response.payload 里取出正文与思维链。
//
// 真实结构：payload.output 是条目数组，每个条目有 type/role/content，
// content 是块数组，块有 type 与 text。
//
// 两个刻意的选择：
//   - 只取**最后一个** output 条目：多轮会话下 output 会累积历史，
//     全拼会把之前的回答也当成这一轮的结果；
//   - 跳过 input_image / input_file：那是用户输入的回显，不是回答。
func extractCodexOutput(payload map[string]any) (text, reasoning string) {
	ov, ok := payload["output"]
	if !ok {
		// 兼容没有 output 包裹的实现。
		return FindLongestString(payload, []string{"text", "output_text"}, 3), ""
	}
	arr, ok := ov.([]any)
	if !ok || len(arr) == 0 {
		return "", ""
	}

	for _, it := range arr {
		im, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := im["type"].(string)
		if typ != "reasoning" {
			continue
		}
		// 思维链通常放在 summary 数组里（前端用 getReasoningSummaryText 读它）。
		if s, found := im["summary"]; found {
			reasoning = FlattenContent(s)
		} else {
			reasoning = FlattenContent(im["text"])
		}
	}

	// 倒序找最后一条**有内容的** assistant 消息。
	//
	// 为什么不直接取 arr[len-1]：output 尾部可能夹着非消息条目
	// （工具调用、状态占位），那些条目的 content 是空的，
	// 直接取最后一项会拿回一个空回答。倒序 + 跳过空文本更稳。
	for i := len(arr) - 1; i >= 0; i-- {
		im, ok := arr[i].(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := im["type"].(string); typ != "message" {
			continue
		}
		if role, _ := im["role"].(string); role != "assistant" {
			continue
		}
		if t := textFromBlocks(im["content"]); t != "" {
			text = t
			break
		}
	}

	// 兜底：没有带 role 的条目（有些实现回裸消息）。
	if text == "" {
		if last, ok := arr[len(arr)-1].(map[string]any); ok {
			if t := textFromBlocks(last["content"]); t != "" {
				text = t
			} else {
				text = FlattenContent(last["text"])
			}
		}
	}
	return text, reasoning
}

// textFromBlocks 把 content 块数组拼成纯文本。
//
// 跳过 input_image / input_file —— 那是用户输入的回显，不是回答。
// refusal 是模型的拒答，属于回答内容，保留。
func textFromBlocks(v any) string {
	blocks, ok := v.([]any)
	if !ok {
		if v == nil {
			return ""
		}
		return FlattenContent(v)
	}
	var sb strings.Builder
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			sb.WriteString(FlattenContent(b))
			continue
		}
		switch bt, _ := bm["type"].(string); bt {
		case "refusal":
			sb.WriteString(FlattenContent(bm["refusal"]))
		case "input_image", "input_file":
			// 用户输入的回显，跳过。
		default:
			sb.WriteString(FlattenContent(bm["text"]))
		}
	}
	return sb.String()
}

// parseGeneric 是上游形态未知时的宽容解析。
//
// 它只在 parseEnvelope 认不出包络时才被调用 —— 也就是说，
// 当上游换了响应形状、而我们的配置还没跟上时，这条路径让服务
// 至少能"凑合活着"，而不是全线 500。
func (c *Client) parseGeneric(v any, raw []byte, fallbackID, prevText string) (*StatusResponse, error) {
	out := &StatusResponse{Raw: raw, RequestID: fallbackID}
	s := c.schema

	if id := FindString(v, s.RespIDKeys); id != "" {
		out.RequestID = id
	}
	out.ConversationID = FindString(v, []string{"conversation_id", "conversationId", "thread_id"})
	out.Status = strings.ToLower(FindString(v, s.RespStatusKeys))
	out.Error = FindString(v, s.RespErrorKeys)

	if s.RespMessagesKey != "" {
		if msgv, ok := FindKey(v, []string{s.RespMessagesKey}, 3); ok {
			if arr, ok := AsSlice(msgv); ok {
				out.Messages = decodeMessages(arr)
				out.Text = joinAssistantText(out.Messages)
			}
		}
	}
	if out.Text == "" {
		out.Text = FindLongestString(v, s.RespTextKeys, maxWalkDepth)
	}

	explicitDelta := FindString(v, s.RespDeltaKeys)
	if explicitDelta != "" {
		out.Delta = explicitDelta
		if out.Text == "" {
			out.Text = prevText + explicitDelta
		}
	} else {
		delta, reset := Diff(prevText, out.Text)
		out.Delta = delta
		out.Reset = reset
	}

	out.Reasoning = FindString(v, []string{"reasoning", "reasoning_text", "thinking", "summary"})

	if u, ok := FindKey(v, []string{"usage"}, 3); ok {
		if um, ok := AsMap(u); ok {
			out.Usage = &Usage{
				InputTokens:  intOf(um, "input_tokens", "prompt_tokens"),
				OutputTokens: intOf(um, "output_tokens", "completion_tokens"),
				TotalTokens:  intOf(um, "total_tokens"),
			}
			if out.Usage.TotalTokens == 0 {
				out.Usage.TotalTokens = out.Usage.InputTokens + out.Usage.OutputTokens
			}
		}
	}

	if out.Status != "" {
		out.Done = matchAny(out.Status, s.StatusDone)
		out.Fail = matchAny(out.Status, s.StatusFail)
	} else if out.Error != "" {
		out.Fail = true
	}
	if !out.Done && !out.Fail {
		if b, ok := FindKey(v, []string{"done", "finished", "completed", "is_done", "complete"}, 2); ok {
			if bv, ok := b.(bool); ok && bv {
				out.Done = true
				out.Status = "completed"
			}
		}
	}
	if !out.Done && !out.Fail && out.Error == "" {
		if out.Status == "" {
			out.Status = "pending"
		}
	}
	if out.Fail {
		out.Done = true
	}
	return out, nil
}

func (c *Client) statusOf(status string) string {
	switch {
	case matchAny(status, c.schema.StatusDone):
		return "done"
	case matchAny(status, c.schema.StatusFail):
		return "fail"
	case matchAny(status, c.schema.StatusRun):
		return "run"
	}
	return "unknown"
}

func decodeMessages(arr []any) []Message {
	out := make([]Message, 0, len(arr))
	for _, e := range arr {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		var m Message
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// joinAssistantText 只取 assistant 侧的内容拼接。
//
// 若响应里没有 role 信息（可能是单条裸消息），则全部视为 assistant。
func joinAssistantText(msgs []Message) string {
	if len(msgs) == 0 {
		return ""
	}
	var sb strings.Builder
	hasRole := false
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "assistant") {
			hasRole = true
			break
		}
	}
	for _, m := range msgs {
		if hasRole && !strings.EqualFold(m.Role, "assistant") {
			continue
		}
		sb.WriteString(m.TextContent())
	}
	return sb.String()
}

func matchAny(s string, list []string) bool {
	for _, e := range list {
		if strings.EqualFold(strings.TrimSpace(e), s) {
			return true
		}
	}
	return false
}

func intOf(m map[string]any, keys ...string) int {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i)
			}
		case float64:
			return int(n)
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return 0
}

// ---------------------------- 沙箱 ----------------------------

// AcquireSandbox 申请一个沙箱（POST /api/backend/1/new）。
//
// 真实响应就是 {url, token} 两个字段。注意它**不绑定项目**：
// 同一个令牌可以用于任意项目的会话，所以按账号缓存就好。
func (c *Client) AcquireSandbox(ctx context.Context, p Principal) (*Sandbox, error) {
	var raw json.RawMessage
	if _, _, _, err := c.doJSON(ctx, p, http.MethodPost, PathSandboxNew, map[string]any{}, &raw, "application/json"); err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := json.Unmarshal(raw, &sb); err != nil {
		return nil, fmt.Errorf("解析沙箱响应: %w", err)
	}
	if !sb.Usable() {
		return nil, fmt.Errorf("沙箱响应缺少 url/token: %s", truncate(string(raw), 300))
	}
	return &sb, nil
}

// sandboxPath 把沙箱 URL 与其子路径拼成**相对路径**。
//
// 为什么不直接用绝对 URL：沙箱代理跑在同一个上游域名下
// （https://prism.openai.com/s/sandboxes/proxy/），
// 我们复用的是同一个 HTTP 客户端 —— 连接池、HTTP/2、Cookie 注入全在里面。
// 而 httpc.NewRequest 是把 path **拼接**到 BaseURL 上的，
// 传绝对 URL 会拼出 /https://host/... 这种畸形路径，
// 服务端只会回 404，而且看起来像"端点不存在"。
func sandboxPath(sb *Sandbox, sub string) string {
	base := sb.URL
	if u, err := url.Parse(sb.URL); err == nil && u.Path != "" {
		// 只取 path，丢掉 scheme/host —— 它们由 HTTP 客户端提供。
		base = u.Path
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(sub, "/")
}

// WaitSandboxReady 等沙箱**工作区同步完成**（不只是容器起来）。
//
// 判定规则照抄真实前端（bundle 里的 lr()）—— 它对"哪种情况算就绪"
// 有明确分支，自己发明一套只会得到假就绪或假超时：
//
//	404 / 501                    -> 视为"无需同步"，直接通过
//	readinessCapabilities 缺失   -> 旧协议：synced + 有 Y-Sweet 令牌
//	含 current_y_sweet_provider  -> 新协议：还要求 provider 已同步完成
//
// 未就绪时这个端点可能**直接断掉 TLS**（表现为 EOF），这不是错误，
// 只是"还没好"，所以网络错误同样算继续等。
//
// 返回最后一次状态，便于上层记录"究竟卡在哪一项"——
// 这个信息比一个光秃秃的 false 有用得多。
func (c *Client) WaitSandboxReady(ctx context.Context, p Principal, sb *Sandbox, maxWait time.Duration) (*SandboxSyncStatus, bool) {
	if !sb.Usable() {
		return nil, false
	}
	deadline := time.Now().Add(maxWait)
	var last *SandboxSyncStatus
	// 403 容忍次数与常量：见下方 case 的说明。
	forbidden := 0
	const maxForbiddenRetries = 5

	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return last, false
		}
		resp, err := c.doRaw(ctx, p, http.MethodGet,
			sandboxPath(sb, PathSandboxWaitForSync+"?wait_ms=10000"),
			map[string]string{"X-Crixet-Sandbox-Token": sb.Token}, nil)
		if err == nil && resp != nil {
			status := resp.StatusCode
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			switch {
			case status == http.StatusNotFound || status == http.StatusNotImplemented:
				// 这个沙箱不需要工作区同步。
				return nil, true
			case status == http.StatusOK && rerr == nil:
				var st SandboxSyncStatus
				if json.Unmarshal(body, &st) == nil {
					last = &st
					if st.Ready() {
						return last, true
					}
					if st.Status == "failed" {
						c.logf("沙箱工作区同步失败: %s", truncate(string(body), 200))
						return last, false
					}
				}
			case status >= 400 && status < 500 &&
				status != http.StatusRequestTimeout && status != http.StatusTooManyRequests &&
				status != http.StatusForbidden:
				// 4xx（除超时/限流/403）说明我们请求本身不对，再等也没用。
				c.logf("沙箱 wait-for-sync 返回 %d，放弃等待", status)
				return last, false
			case status == http.StatusForbidden:
				// 403 单独处理：实测它是**瞬时**的（应用层
				// "Request verification failed"，随沙箱会话状态抖动），
				// 早先把它并入 4xx 立即放弃，导致等待窗口只剩一次请求
				// （日志里 waited=11s 而配置是 90s），接着上层"硬上 start"
				// ——上游必然回 122 秒的 workspace_sync_timeout。
				// 这里改为容忍若干次，给沙箱恢复的机会。
				forbidden++
				c.logf("沙箱 wait-for-sync 返回 403（第 %d 次），继续等待", forbidden)
				if forbidden >= maxForbiddenRetries {
					return last, false
				}
			}
		}
		// 网络错误（含 TLS 被断）视为"还没好"，继续等。
		if !httpc.SleepOK(ctx, time.Second) {
			return last, false
		}
	}
}

// AcquireResourceToken 让后端为"某个项目的沙箱资源访问"签发短期令牌。
//
// 这一步漏掉后的症状极其隐蔽：沙箱不会报任何错，只会一直停在
// wait-for-sync 的 status:"syncing"，最终表现为会话处理固定 122 秒后 504。
// 看起来像"上游挂了"或"冷启动慢"，实际是沙箱在等凭证。
//
// 令牌有效期只有 1 小时且**绑定单个项目**（project_uuid 编码在 JWT 里），
// 所以缓存粒度必须是 (账号, 项目)。
func (c *Client) AcquireResourceToken(ctx context.Context, p Principal, projectID, sessionID, sandboxToken string) (*ResourceToken, error) {
	if projectID == "" {
		return nil, errors.New("签发沙箱资源令牌需要 projectID")
	}
	path := fmt.Sprintf(PathResourceToken, url.PathEscape(projectID))
	// sandbox_session_id 允许为 null（实测 /api/backend/1/new 目前不下发它）。
	payload := map[string]any{
		"sandbox_session_id": nilIfEmpty(sessionID),
		"sandbox_token":      sandboxToken,
	}
	var rt ResourceToken
	if _, _, _, err := c.doJSON(ctx, p, http.MethodPost, path, payload, &rt, "application/json"); err != nil {
		return nil, err
	}
	if rt.AccessToken == "" {
		return nil, fmt.Errorf("后端未返回 access_token（项目 %s）", projectID)
	}
	return &rt, nil
}

// DeliverResourceToken 把资源令牌交给沙箱。
//
// body 是 {token, resourceBaseUrl, projectId} —— 字段名大小写是上游的，
// 不要"顺手统一"：resourceBaseUrl 是 camelCase，而 token/projectId 也是。
func (c *Client) DeliverResourceToken(ctx context.Context, p Principal, sb *Sandbox, rt *ResourceToken, projectID string) error {
	base := rt.ResourcesBaseURL
	if base == "" {
		// 后端没给就按前端同样的规则推导：
		// /s/sandboxes/proxy -> /s/sandbox-resources
		base = sandboxResourcesBaseURL(sb.URL)
	}
	if base == "" {
		return errors.New("无法确定沙箱资源基地址")
	}
	payload := map[string]any{
		"token":           rt.AccessToken,
		"resourceBaseUrl": base,
		"projectId":       projectID,
	}
	var out json.RawMessage
	_, _, _, err := c.doJSONExtra(ctx, p, http.MethodPost,
		sandboxPath(sb, PathSandboxResourceToken), payload, &out,
		"application/json", sandboxHeaders(sb))
	return err
}

// AcquireYSweetToken 取 Y-Sweet 协作文档的访问凭证（POST /api/y）。
//
// 拿到后必须原样转交沙箱（见 DeliverYSweetToken）——
// **沙箱会自己连 Y-Sweet 同步文档**，所以我们不需要在 Go 里
// 实现 Yjs / lib0 二进制编解码。这是实测确认的，不是推断。
func (c *Client) AcquireYSweetToken(ctx context.Context, p Principal, projectID string) (*YSweetToken, error) {
	if projectID == "" {
		return nil, errors.New("取 Y-Sweet 令牌需要 projectID")
	}
	payload := map[string]any{
		"docId": projectID,
		"requestContext": map[string]any{
			// source 标识这次同步的起因，上游用它做统计与限流；
			// initial-bootstrap 是"首次为该文档建立同步"。
			"source":          "initial-bootstrap",
			"requestSeriesId": "oaiprism-" + projectID,
			"maxAttempts":     5,
		},
	}
	var (
		raw json.RawMessage
		out YSweetToken
	)
	if _, _, _, err := c.doJSON(ctx, p, http.MethodPost, PathYSweetToken, payload, &raw, "application/json"); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 Y-Sweet 令牌: %w", err)
	}
	if out.Token == "" || out.URL == "" {
		return nil, fmt.Errorf("Y-Sweet 令牌不完整: %s", truncate(string(raw), 300))
	}
	// 转发时用原始 JSON —— 上游新增字段不会被我们的结构体吃掉。
	out.Raw = raw
	return &out, nil
}

// DeliverYSweetToken 把 Y-Sweet 凭证原样交给沙箱。
//
// 实测响应 {"success":true,"message":"Token received"}；
// 交付成功后 wait-for-sync 会立刻从 syncing 变成 synced。
func (c *Client) DeliverYSweetToken(ctx context.Context, p Principal, sb *Sandbox, tk *YSweetToken) error {
	if tk == nil || len(tk.Raw) == 0 {
		return errors.New("Y-Sweet 令牌为空")
	}
	var out json.RawMessage
	_, _, _, err := c.doJSONExtra(ctx, p, http.MethodPost,
		sandboxPath(sb, PathSandboxToken), tk.Raw, &out,
		"application/json", sandboxHeaders(sb))
	return err
}

// sandboxHeaders 是调沙箱代理时额外要带的头。
//
// 沙箱的认证是**双重**的：Cookie（会话，由 buildHeaders 注入）
// 加 X-Crixet-Sandbox-Token。只带后者会直接 401 且响应体为空 ——
// 这是实测踩过的坑，不是猜测。
func sandboxHeaders(sb *Sandbox) map[string]string {
	return map[string]string{"X-Crixet-Sandbox-Token": sb.Token}
}

// sandboxResourcesBaseURL 从沙箱代理 URL 推导资源基地址。
//
// 对应前端 bundle 里的 getSandboxResourcesBaseUrlFromSandboxUrl。
func sandboxResourcesBaseURL(sandboxURL string) string {
	for _, m := range []struct{ from, to string }{
		{"/s/sandboxes/proxy", "/s/sandbox-resources"},
		{"/sandboxes/proxy", "/sandbox-resources"},
	} {
		if i := strings.Index(sandboxURL, m.from); i >= 0 {
			return sandboxURL[:i] + m.to
		}
	}
	return ""
}

// nilIfEmpty 让空字符串序列化成 null 而不是 ""。
// 上游对 sandbox_session_id 的校验接受 null，空字符串则可能被判非法。
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// doRaw 发一个不走"JSON 解析 + 错误包装"的原始请求。
//
// 沙箱的端点不遵守上游那套 {status,response} 信封，
// 用 doJSON 会把正常响应也判成错误。
func (c *Client) doRaw(ctx context.Context, p Principal, method, path string, extra map[string]string, body []byte) (*http.Response, error) {
	hdr := c.buildHeaders(p, "", "application/json")
	for k, v := range extra {
		hdr[k] = v
	}
	return c.Do(ctx, p, method, path, headerFromMap(hdr), body, nil)
}

func (c *Client) logf(format string, args ...any) {
	if c.debug != nil {
		c.debug(format, args...)
	}
}

// ---------------------------- 沙箱编译 ----------------------------

// Render 触发 LaTeX 编译（文档：POST /s/sandboxes/proxy/render，202 异步）。
func (c *Client) Render(ctx context.Context, p Principal, req *RenderRequest) (*RenderResponse, error) {
	payload := map[string]any{}
	if req.ProjectID != "" {
		payload["project_id"] = req.ProjectID
		payload["projectId"] = req.ProjectID
	}
	if req.FileID != "" {
		payload["file_id"] = req.FileID
		payload["fileId"] = req.FileID
	}
	if req.Target != "" {
		payload["target"] = req.Target
	} else {
		payload["target"] = "pdf"
	}
	if req.Engine != "" {
		payload["engine"] = req.Engine
	}
	for k, v := range req.Extra {
		payload[k] = v
	}

	var raw json.RawMessage
	_, hdr, _, err := c.doJSON(ctx, p, http.MethodPost, PathSandboxRender, payload, &raw, "application/json")
	if err != nil {
		return nil, err
	}

	v, _ := DecodeAny(raw)
	out := &RenderResponse{Raw: raw}
	if v != nil {
		out.JobID = FindString(v, []string{"job_id", "jobId", "render_id", "renderId", "id", "sandbox_id"})
		out.Status = FindString(v, []string{"status", "state"})
	}
	if out.JobID == "" {
		// 有些实现把 job id 放在 Location 头里。
		if loc := hdr.Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil {
				out.JobID = u.Query().Get("id")
				if out.JobID == "" {
					parts := strings.Split(strings.Trim(u.Path, "/"), "/")
					out.JobID = parts[len(parts)-1]
				}
			}
		}
	}
	return out, nil
}

// RenderStatus 轮询编译结果（文档：GET /s/sandboxes/proxy/render-status?waitMs=10000）。
//
// waitMs 是文档里明确出现过的长轮询参数，这里默认就用上——
// 一次 10 秒挂起替代 50 次 200ms 空转，对上游和对我们都更友好。
func (c *Client) RenderStatus(ctx context.Context, p Principal, opt RenderStatusOptions) (*RenderStatus, error) {
	waitMs := opt.WaitMs
	if waitMs <= 0 {
		waitMs = 10000
	}
	q := url.Values{}
	if opt.JobID != "" {
		q.Set("id", opt.JobID)
		// 兼容不同参数名，三个都带上比猜一个更稳。
		q.Set("job_id", opt.JobID)
		q.Set("render_id", opt.JobID)
	}
	q.Set("waitMs", strconv.Itoa(waitMs))

	path := PathSandboxRenderStatus + "?" + q.Encode()
	hdr := c.buildHeaders(p, "", "application/json")
	resp, err := c.Do(ctx, p, http.MethodGet, path, headerFromMap(hdr), nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, &creds.APIError{Op: "render-status", Status: resp.StatusCode, Body: truncate(string(raw), 400)}
	}

	v, _ := DecodeAny(raw)
	out := &RenderStatus{Raw: raw}
	if v != nil {
		out.Status = FindString(v, []string{"status", "state", "phase"})
		out.URL = FindString(v, []string{"url", "output_url", "pdf_url", "download_url", "artifact_url"})
		out.Log = FindLongestString(v, []string{"log", "logs", "output", "stdout"}, 4)
		if ev, ok := FindKey(v, []string{"errors", "error"}, 3); ok {
			if arr, ok := AsSlice(ev); ok {
				for _, e := range arr {
					out.Errors = append(out.Errors, FlattenContent(e))
				}
			} else if s := FlattenContent(ev); s != "" {
				out.Errors = append(out.Errors, s)
			}
		}
	}
	out.Done = matchAny(out.Status, []string{"completed", "success", "succeeded", "done", "ready", "finished"})
	out.Fail = matchAny(out.Status, []string{"failed", "error", "cancelled", "canceled", "timeout"})
	return out, nil
}

// UploadFile 上传文件（文档：POST /api/project-files/upload）。
func (c *Client) UploadFile(ctx context.Context, p Principal, up FileUpload) (json.RawMessage, error) {
	var buf bytes.Buffer
	// 预分配：multipart 的开销主要来自多次 grow。
	buf.Grow(len(up.Data) + 1024)

	fileID := newUUID()

	mw := multipart.NewWriter(&buf)
	if up.ProjectID != "" {
		_ = mw.WriteField("project_id", up.ProjectID)
		_ = mw.WriteField("projectId", up.ProjectID)
	}
	_ = mw.WriteField("file_id", fileID)
	_ = mw.WriteField("fileId", fileID)

	if up.Path != "" {
		_ = mw.WriteField("path", up.Path)
	}
	name := up.Filename
	if name == "" {
		name = "main.tex"
	}
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(up.Data); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	hdr := c.buildHeaders(p, mw.FormDataContentType(), "application/json")
	hdr["x-prism-file-id"] = fileID
	if up.ProjectID != "" {
		hdr["x-prism-project-id"] = up.ProjectID
	}
	hdr["x-prism-file-name"] = url.QueryEscape(name)
	hdr["x-prism-file-size"] = strconv.Itoa(len(up.Data))

	resp, err := c.Do(ctx, p, http.MethodPost, PathProjectFilesUpload, headerFromMap(hdr), buf.Bytes(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, &creds.APIError{Op: "upload", Status: resp.StatusCode, Body: truncate(string(raw), 400)}
	}
	return raw, nil
}

// UploadRawProjectFile 直接以原始二进制流上传项目文件（完全对齐官方 WebUI /api/project-files/upload），
// 返回文件 id（x-prism-file-id）。只上传不会出现在项目文件树里，要再用 RegisterProjectFile 登记。
func (c *Client) UploadRawProjectFile(ctx context.Context, p Principal, projectID, filename, contentType string, data []byte) (string, error) {
	fileID := newUUID()
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	hdr := c.buildHeaders(p, contentType, "*/*")
	hdr["x-prism-file-id"] = fileID
	if projectID != "" {
		hdr["x-prism-project-id"] = projectID
	}
	hdr["x-prism-file-name"] = filename
	hdr["x-prism-file-size"] = strconv.Itoa(len(data))
	hdr["x-prism-require-project-edit-access"] = "true"

	resp, err := c.Do(ctx, p, http.MethodPost, PathProjectFilesUpload, headerFromMap(hdr), data, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", &creds.APIError{Op: "upload_raw_project_file", Status: resp.StatusCode, Body: truncate(string(raw), 400)}
	}
	return fileID, nil
}

// UploadDir 是官方前端放聊天附件的项目目录（前端常量 "prism-uploads"）。
const UploadDir = "prism-uploads"

// RegisterProjectFile 把已上传的文件登记进项目文件树：<UploadDir>/<filename>，返回项目内路径
// （"/prism-uploads/<filename>"，即官方 input_file 的 project_path）。
//
// 文件树是 Y-Sweet 托管的 Yjs 文档，沙箱按它同步工作区 —— 不登记，沙箱里就没有这个文件，
// 模型"打开附件"只会得到"文件不存在"。官方前端上传后也是往文档里加节点（addProjectFiles）。
func (c *Client) RegisterProjectFile(ctx context.Context, p Principal, projectID, fileID, filename string) (string, error) {
	tk, err := c.AcquireYSweetToken(ctx, p, projectID)
	if err != nil {
		return "", err
	}
	state, err := c.yDoc(ctx, p, tk, http.MethodGet, "/as-update", nil)
	if err != nil {
		return "", err
	}
	doc, err := ydoc.Decode(state)
	if err != nil {
		return "", fmt.Errorf("解析项目文档: %w", err)
	}
	update, err := ydoc.AddFile(doc, UploadDir, fileID, filename)
	if err != nil {
		return "", err
	}
	if _, err := c.yDoc(ctx, p, tk, http.MethodPost, "/update", update); err != nil {
		return "", err
	}
	return "/" + UploadDir + "/" + filename, nil
}

// yDoc 调 Y-Sweet 的文档 HTTP 接口（{baseUrl}/as-update、{baseUrl}/update），以文档令牌鉴权。
func (c *Client) yDoc(ctx context.Context, p Principal, tk *YSweetToken, method, suffix string, body []byte) ([]byte, error) {
	u, err := url.Parse(tk.BaseURL)
	if err != nil || u.Path == "" || !strings.HasPrefix(u.Path, "/y/") {
		return nil, fmt.Errorf("Y-Sweet 地址不对: %q", tk.BaseURL)
	}
	hdr := map[string]string{
		"Accept":        "*/*",
		"Authorization": "Bearer " + tk.Token,
	}
	if body != nil {
		hdr["Content-Type"] = "application/octet-stream"
	}
	// 文档接口不认账号凭据：不带 Principal 的 Cookie / access token
	resp, err := c.Do(ctx, Principal{Client: p.Client, AccountID: p.AccountID}, method, strings.TrimRight(u.Path, "/")+suffix, headerFromMap(hdr), body, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &creds.APIError{Op: "ysweet " + method + " " + suffix, Status: resp.StatusCode, Body: truncate(string(raw), 300)}
	}
	return raw, nil
}

// PatchThumbnail 更新项目缩略图（文档：PATCH /api/projects/{uuid}/thumbnail）。
func (c *Client) PatchThumbnail(ctx context.Context, p Principal, projectID string, payload map[string]any) (json.RawMessage, error) {
	path := "/api/projects/" + url.PathEscape(projectID) + "/thumbnail"
	var raw json.RawMessage
	_, _, _, err := c.doJSON(ctx, p, http.MethodPatch, path, payload, &raw, "application/json")
	return raw, err
}

// ---------------------------- 工具 ----------------------------

func headerFromMap(m map[string]string) http.Header {
	h := make(http.Header, len(m))
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

func stripQuery(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i]
	}
	return path
}

func backoff(base, max time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	// ±25% 抖动，打散重试风暴。
	if d > 0 {
		jitter := time.Duration(int64(d) / 4)
		if jitter > 0 {
			d += time.Duration(fakeRand(int64(jitter)*2) - int64(jitter))
		}
	}
	return d
}

// fakeRand 是一个极廉价的伪随机，避免为抖动引入 math/rand 的全局锁。
var randState uint64 = 0x9e3779b97f4a7c15

func fakeRand(n int64) int64 {
	randState ^= randState << 13
	randState ^= randState >> 7
	randState ^= randState << 17
	if n <= 0 {
		return 0
	}
	return int64(randState % uint64(n))
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// ErrNotFound 供上层判断。
var ErrNotFound = errors.New("上游资源不存在")

// newUUID 生成一个 RFC 4122 v4 UUID。
//
// 自己写而不引 github.com/google/uuid：本项目坚持"依赖数为 1"，
// 而这里只需要"随机且格式合法"这一个语义，标准库的 crypto/rand 足够。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况（系统熵源不可用）下退化成时间戳，
		// 至少保证格式合法、不会让建项目整体失败。
		now := uint64(time.Now().UnixNano())
		for i := 0; i < 8; i++ {
			b[i] = byte(now >> (8 * i))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	j := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hex[v>>4]
		out[j+1] = hex[v&0x0f]
		j += 2
	}
	return string(out)
}
