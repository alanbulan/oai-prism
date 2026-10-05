// Package prism 封装 prism.openai.com 的内部 API。
//
// 这些端点都不是公开 API，是从网络面板里逆出来的，因此本包的设计原则是：
//
//  1. 字段名一律从配置读取，协议变了改 YAML 不改代码；
//  2. 响应解析做"宽容抽取"而不是强类型绑定——上游加字段不该让我们 500；
//  3. 所有原始报文可被 capture 录制，便于离线校准。
package prism

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Endpoint 常量。集中在一处，方便对照真实报文。
//
// 推理端点路径为 /api/llm/response_with_tools_*（实测校准）。
const (
	PathProjects            = "/api/projects"
	PathProjectAccess       = "/api/project-access"
	PathConversationHistory = "/api/codex/conversation-history"
	PathResponseStart       = "/api/llm/response_with_tools_start"
	PathResponseStatus      = "/api/llm/response_with_tools_status"
	PathResponseStop        = "/api/llm/response_with_tools_stop"
	// PathSandboxNew 是"申请一个沙箱"的入口。
	//
	// 这一步是必需的，而且极容易被漏掉：Prism 的 AI 助手跑在一个
	// 沙箱容器里，start 请求不带 sandbox_url / sandbox_token 的话，
	// 上游会立刻回 status:"completed" + response.status:"error"，
	// reason 是 sandbox_reconnecting，看起来像"上游挂了"，
	// 实际上是我们没告诉它用哪个沙箱。
	PathSandboxNew = "/api/backend/1/new"

	// PathResourceToken 让**后端**为"某个项目的沙箱资源访问"签发短期令牌。
	//
	// 这是最容易被漏掉、漏掉后最难定位的一步：只申请沙箱而不签发资源令牌，
	// 沙箱会一直停在 wait-for-sync 的 status:"syncing"，
	// 最终表现为会话处理固定 122 秒后 504 —— 看起来像"上游挂了"或"冷启动慢"，
	// 实际上是沙箱在等我们把资源凭证交给它。
	//
	// 需要 fmt.Sprintf 填项目 uuid。
	PathResourceToken = "/api/projects/%s/sandbox/resources-token"

	// PathYSweetToken 取 Y-Sweet 协作文档的访问凭证。
	//
	// 拿到后**必须原样转交沙箱**（见 PathSandboxToken），
	// 沙箱会用它在容器内自行连接 Y-Sweet 同步文档 ——
	// 这也是我们不需要在 Go 里实现 Yjs/lib0 的原因。
	PathYSweetToken = "/api/y"

	// PathSandboxToken 是沙箱侧的"接收凭证"端点（相对沙箱 URL 拼接）。
	PathSandboxToken = "token"
	// PathSandboxResourceToken 是沙箱侧的"接收资源令牌"端点。
	PathSandboxResourceToken = "resources-token"
	// PathSandboxWaitForSync 是沙箱侧的"同步状态"端点。
	PathSandboxWaitForSync = "wait-for-sync"

	PathSandboxRender       = "/s/sandboxes/proxy/render"
	PathSandboxRenderStatus = "/s/sandboxes/proxy/render-status"
	PathProjectFilesUpload  = "/api/project-files/upload"
	PathAuthSession         = "/api/auth/session"
)

// Project 表示一个 Prism 项目（LaTeX 工程）。
//
// 字段故意放宽：上游返回 id 还是 uuid 无从确认，两个都收。
type Project struct {
	ID        string `json:"id"`
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	// Raw 保留原始响应，便于排障时看清楚上游到底回了什么。
	Raw json.RawMessage `json:"-"`
}

// Key 返回项目的稳定标识，优先 uuid。
func (p *Project) Key() string {
	if p == nil {
		return ""
	}
	if p.UUID != "" {
		return p.UUID
	}
	return p.ID
}

// ProjectAccess 是 GET /api/project-access?d={UUID} 的返回。
type ProjectAccess struct {
	HasAccess bool   `json:"has_access"`
	Access    string `json:"access"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`

	Raw json.RawMessage `json:"-"`
}

// Message 是一条会话消息。
//
// Content 用 any 是为了同时容纳字符串与多模态数组——
// 强类型会让我们在遇到 content 数组时直接解析失败。
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	Name    string `json:"name,omitempty"`
	ID      string `json:"id,omitempty"`

	// Attachments / Files 等扩展字段原样带上。
	Extra map[string]any `json:"-"`
}

// MarshalJSON 把 Extra 里的字段合并回顶层。
func (m Message) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(m.Extra)+3)
	for k, v := range m.Extra {
		out[k] = v
	}
	out["role"] = m.Role
	out["content"] = m.Content
	if m.Name != "" {
		out["name"] = m.Name
	}
	if m.ID != "" {
		out["id"] = m.ID
	}
	return json.Marshal(out)
}

// UnmarshalJSON 把未知字段收进 Extra。
func (m *Message) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.Extra = make(map[string]any, len(raw))
	for k, v := range raw {
		switch k {
		case "role":
			_ = json.Unmarshal(v, &m.Role)
		case "content":
			_ = json.Unmarshal(v, &m.Content)
		case "name":
			_ = json.Unmarshal(v, &m.Name)
		case "id":
			_ = json.Unmarshal(v, &m.ID)
		default:
			var any1 any
			if err := json.Unmarshal(v, &any1); err == nil {
				m.Extra[k] = any1
			}
		}
	}
	return nil
}

// TextContent 尽力把 Content 拉平成纯文本。
func (m *Message) TextContent() string {
	return FlattenContent(m.Content)
}

// InputItem 是 response_with_tools_start 的 input 数组元素。
//
// 结构来自真实前端源码：
//
//	{type:"message", role:"user"|"assistant", content:[{type:"input_text", text}]}
//
// 注意 content 里的块类型：用户输入是 input_text，助手历史是 output_text。
// 传错类型上游不会报错，但模型会"看不见"这段内容 —— 属于静默失效。
type InputItem struct {
	Type    string         `json:"type"`
	Role    string         `json:"role,omitempty"`
	Content []InputContent `json:"content,omitempty"`
	ID      string         `json:"id,omitempty"`
}

// InputContent 是内容块。
//
// ImageURL / Detail 只对 input_image 有意义，但必须在这里有字段 ——
// 少了它们，图像块的 URL 会无处安放，最终发一个空的 input_image 给上游：
// 上游不报错，模型只是"看不见图"。这类静默失效极难定位。
type InputContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// ImageURL 是 input_image 的图片地址。
	// 上游字段名就是 image_url（输入块也用它，不是 output 那套）。
	ImageURL string `json:"image_url,omitempty"`
	// Detail 是图像精细度（auto / low / high），透传给上游。
	Detail string `json:"detail,omitempty"`
	// Filename 与 ProjectPath 用于 input_file（Prism 附件与多模态转存文件）。
	Filename    string `json:"filename,omitempty"`
	ProjectPath string `json:"project_path,omitempty"`
}

// 内容块类型常量。
const (
	BlockInputText  = "input_text"
	BlockOutputText = "output_text"
)

// NewUserItem 构造一条用户消息块。
func NewUserItem(text string) InputItem {
	return InputItem{
		Type:    "message",
		Role:    "user",
		Content: []InputContent{{Type: BlockInputText, Text: text}},
	}
}

// NewSystemItem 构造一条系统消息块。
//
// 上游的 input 数组**接受 system 角色**：服务端把最后一条 system 当作 Context、
// 最后一条 user 当作 User request，拼成一段文本交给沙箱里的模型，其余条目丢弃
// （2026-10-04 实测）。所以系统提示要保持 system 角色、且全部合并成一条 ——
// 折成 user 会顶替掉真正的提问。
func NewSystemItem(text string) InputItem {
	return InputItem{
		Type:    "message",
		Role:    "system",
		Content: []InputContent{{Type: BlockInputText, Text: text}},
	}
}

// NewAssistantItem 构造一条助手消息块。
//
// 上游输入端 input 数组中的所有文本块均为 input_text（与 PrismOpenAIProxy 对齐）。
// output_text 仅存在于上游返回的 output 载荷中。
func NewAssistantItem(text string) InputItem {
	return InputItem{
		Type:    "message",
		Role:    "assistant",
		Content: []InputContent{{Type: BlockInputText, Text: text}},
	}
}

// StartRequest 是 /api/llm/response_with_tools_start 的语义化请求。
//
// 字段与真实报文的对应关系（来自前端 bundle 实测）：
//
//	Input              -> input                （数组）
//	PreviousResponseID -> previousResponseId   （多轮延续，可为空）
//	Metadata           -> metadata             （模型参数与运行上下文都塞这里）
//	ConversationID     -> conversationId       （camelCase！）
type StartRequest struct {
	Input []InputItem

	// PreviousResponseID 是上一轮的 request_id。
	// 留空表示"这是一次独立请求，上下文全靠 Input 自带"。
	PreviousResponseID string
	ConversationID     string

	// Metadata 是运行上下文。模型参数也在里面，不在顶层。
	Metadata map[string]any

	// 下面三个是便捷字段，最终会被合进 Metadata。
	Model           string
	ReasoningEffort string
	// UserID 是调用方身份，用于上游侧的滥用追踪与配额归属。
	// 留空表示调用方没给，此时不要发这个字段。
	UserID string

	// Extra 直通字段，会合并进顶层请求体。
	Extra map[string]any
}

// Tool 是工具定义（原样透传，不做语义解释）。
type Tool struct {
	Name        string         `json:"name,omitempty"`
	Type        string         `json:"type,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Raw         map[string]any `json:"-"`
}

// StartResponse 是 start 接口的归一化返回。
type StartResponse struct {
	// RequestID 是本次生成的句柄（轮询与取消都用它）。
	RequestID      string
	ConversationID string
	Status         string

	// TurnState 是服务端下发的不透明状态对象，轮询时必须原样回传。
	TurnState json.RawMessage

	// ListenSnapshot 是沙箱 codex 会话状态指针，下一轮 start 回传。
	ListenSnapshot json.RawMessage

	// Initial 在 start 直接返回 completed 时携带完整结果
	// （内容已生成完，或者立刻失败了）。
	Initial *StatusResponse

	Raw json.RawMessage
}

// StatusRequest 是轮询请求。
//
// 真实报文体只有两个字段：{request_id, turn_state}。
// turn_state 是服务端下发的不透明状态，**必须原样回传**——
// 任何自己构造的值都会被判为无效（实测会被拒成 "turn_state is required"）。
type StatusRequest struct {
	RequestID string
	TurnState json.RawMessage

	// WaitMs 是机会性优化：若上游某天支持长轮询，这个字段能省掉大量空转。
	// 当前上游不认这个字段，所以发送它没有副作用，也不会有收益。
	WaitMs int
}

// StatusResponse 是轮询结果的归一化返回。
type StatusResponse struct {
	RequestID string
	// ResponseID 是终态 response.payload.id（形如 resp_muqwesyc_7xf7qrm8），
	// 多轮延续时作为下一轮 start 的 previousResponseId。
	// 与 RequestID（start 受理句柄）是两个不同的东西，实测勿混。
	ResponseID     string
	ConversationID string

	// Status 是上游状态词：started / pending / completed。
	Status string

	// TurnState 是本轮下发的状态令牌，下一轮原样回传。
	// 这是整个轮询协议的核心——它就是一个"续令牌"。
	TurnState json.RawMessage

	// ListenSnapshot 是沙箱 codex 会话状态指针（同上，续接另一半）。
	ListenSnapshot json.RawMessage

	Text      string
	Delta     string
	Reset     bool
	Reasoning string
	// ReasoningParts 是 Reasoning 的逐条拆分（每条 summary 一段），用于与进行中的转录摘要去重。
	ReasoningParts []string

	// OutputItems 是上游返回的确定性 Response 条目列表（支持 message, function_call, reasoning 等）。
	OutputItems []CodexOutputItem
	// DeltaFiles 是沙箱中执行的文件增删改查变更集（真实 Codex 文件操作输出）。
	DeltaFiles []CodexDeltaFile

	Messages []Message

	// Error / ErrorReason 描述失败原因。
	// 注意上游的失败表达方式很反直觉：HTTP 200 + status:"completed" +
	// response.status:"error"，所以只看 HTTP 状态码会把失败当成成功。
	Error       string
	ErrorReason string

	Done  bool
	Fail  bool
	Usage *Usage

	// Progress 是生成进行中的增量进度（codex_live_progress），没有时为 nil。
	Progress *LiveProgress

	Raw json.RawMessage
}

// CodexDeltaFile 是 Codex 在沙箱中执行文件增删改查的变更记录。
type CodexDeltaFile struct {
	FilePath string `json:"file_path"`
	Status   string `json:"status"` // "added", "modified", "deleted"
	// Diff 是 diff 内容。实测上游形态不统一：早期是纯字符串（unified diff），
	// 2026-10-02 起观察到对象形态（结构化变更）。强类型 string 会让整个
	// PrismEnvelope 反序列化失败、退回宽松解析分支 —— 这里用 RawMessage 兼容。
	Diff json.RawMessage `json:"diff,omitempty"`
}

// DiffString 把 Diff 归一化为字符串：JSON 字符串解出内容，其他形态
// （对象/数组）原样 marshal 为文本。空返回 ""。
func (f CodexDeltaFile) DiffString() string {
	if len(f.Diff) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(f.Diff, &s); err == nil {
		return s
	}
	// 非 JSON 字符串形态：去掉可能的首尾引号后原样返回。
	out := string(f.Diff)
	out = strings.TrimSpace(out)
	return out
}

// LiveProgress 是上游在 pending 轮询里下发的进度（codex_live_progress）。
//
// 上游模型以 Codex 智能体的方式在沙箱里干活：思考、调工具（search / list / apply_patch …）、
// 写文件，最后才给出回答。正文与终态思考摘要只在终态一次性给出，但每次轮询都会带上
// 自上次 turn_state.transcript_cursor 之后新增的转录行 —— 只含增量，不是全量。
//
// 实测形状（2026-10-05）：
//
//	{"transcriptCursor":31,"lineCount":9,
//	 "reasoningSummaries":[{"line_index":23,"text":"**Designing …** …","source":"reasoning"}],
//	 "toolCalls":[{"line_index":26,"call_id":"tool:26:0","name":"apply_patch","call_type":"function_call",…}],
//	 "eventPreviews":[]}
type LiveProgress struct {
	// TranscriptCursor 是转录游标：前进了就说明上游还在干活（哪怕这一轮没有可展示的条目）。
	TranscriptCursor   int             `json:"transcriptCursor"`
	LineCount          int             `json:"lineCount"`
	ReasoningSummaries []LiveReasoning `json:"reasoningSummaries"`
	ToolCalls          []LiveToolCall  `json:"toolCalls"`
}

// LiveReasoning 是一条进行中的思考摘要。
type LiveReasoning struct {
	LineIndex int    `json:"line_index"`
	Text      string `json:"text"`
}

// LiveToolCall 是一次进行中的上游工具调用（只有名字，参数不下发）。
type LiveToolCall struct {
	LineIndex int    `json:"line_index"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
}

// CodexOutputItem 是上游返回的单个 Output 条目（Response 协议一等公民）。
type CodexOutputItem struct {
	ID        string          `json:"id,omitempty"`
	Type      string          `json:"type"` // "message", "function_call", "reasoning"
	Role      string          `json:"role,omitempty"`
	Status    string          `json:"status,omitempty"`
	Content   []InputContent  `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`      // Function call 名称
	CallID    string          `json:"call_id,omitempty"`   // Function call ID
	Arguments string          `json:"arguments,omitempty"` // Function call 参数
	Text      string          `json:"text,omitempty"`      // 文本回显
	Summary   json.RawMessage `json:"summary,omitempty"`   // 思维链 summary
}

// CodexPayload 是上游 response.payload 确定的业务载荷结构。
type CodexPayload struct {
	ID             string            `json:"id,omitempty"`
	ConversationID string            `json:"conversationId,omitempty"`
	Output         []CodexOutputItem `json:"output,omitempty"`
	DeltaFiles     []CodexDeltaFile  `json:"codexDeltaFiles,omitempty"`
	Usage          *Usage            `json:"usage,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	Message        string            `json:"message,omitempty"`
	RootCause      string            `json:"rootCause,omitempty"`
}

// PrismEnvelope 是上游轮询接口 /api/llm/* 的确定性外层包络结构。
type PrismEnvelope struct {
	Status         string          `json:"status"` // "started", "pending", "completed", "error"
	RequestID      string          `json:"request_id"`
	ConversationID string          `json:"conversation_id"`
	TurnState      json.RawMessage `json:"turn_state"`
	Usage          *Usage          `json:"usage,omitempty"`
	Message        string          `json:"message,omitempty"`
	// ListenSnapshot 是沙箱内 codex 会话的状态指针（codex_session_id /
	// transcript_cursor 等）。下一轮 start 必须原样回传 —— 多轮续接的
	// 另一半钥匙（另一半是 previousResponseId）。真实 Web 每轮都带。
	ListenSnapshot json.RawMessage `json:"codex_listen_snapshot,omitempty"`
	// LiveProgress 单独解码（见 parseEnvelope）：进度字段形状一变，不能连累整个包络退回宽松解析。
	LiveProgress json.RawMessage `json:"codex_live_progress,omitempty"`
	Response     *struct {
		Status  string        `json:"status"` // "success", "error"
		Payload *CodexPayload `json:"payload"`
	} `json:"response,omitempty"`
}

// Usage 是 token 用量。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// ReasoningTokens 是 OutputTokens 中推理文本所占的部分（与 OpenAI 口径一致，已含在 OutputTokens 内）。
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// Sandbox 是一次沙箱申请的结果。
type Sandbox struct {
	// URL 是沙箱代理地址，形如 https://prism.openai.com/s/sandboxes/proxy
	URL string `json:"url"`
	// Token 是访问该沙箱的令牌，走 X-Crixet-Sandbox-Token 头。
	Token string `json:"token"`

	// ID / SessionID 是沙箱与沙箱会话标识。
	//
	// 实测 /api/backend/1/new 目前**不返回**这两个字段（可选），
	// 但签发资源令牌时 sandbox_session_id 是合法入参，所以一并收着：
	// 上游一旦开始下发，我们不用改代码。
	ID        string `json:"sandbox_id"`
	SessionID string `json:"sandbox_session_id"`
}

// Usable 判断沙箱信息是否完整。
func (s *Sandbox) Usable() bool {
	return s != nil && s.URL != "" && s.Token != ""
}

// ResourceToken 是后端为"沙箱访问项目资源"签发的短期令牌。
//
// 实测返回：
//
//	{"access_token":"eyJ...",         // HS256 JWT，claims 带 project_uuid
//	 "resources_base_url":"https://prism.openai.com/s/sandbox-resources",
//	 "expires_at":1789609321,          // unix 秒
//	 "max_age_seconds":3600}
//
// 有效期只有 1 小时，而且**绑定单个项目**（project_uuid 编码在 JWT 里），
// 所以缓存粒度必须是 (账号, 项目) 而不是账号。
type ResourceToken struct {
	AccessToken      string `json:"access_token"`
	ResourcesBaseURL string `json:"resources_base_url"`
	ExpiresAt        int64  `json:"expires_at"`
	MaxAgeSeconds    int64  `json:"max_age_seconds"`
}

// Expiry 返回令牌的过期时间。
// 两个字段都给时取更早的那个——保守估计不会让令牌在途中失效。
func (r *ResourceToken) Expiry() time.Time {
	if r == nil {
		return time.Time{}
	}
	var a, b time.Time
	if r.ExpiresAt > 0 {
		a = time.Unix(r.ExpiresAt, 0)
	}
	if r.MaxAgeSeconds > 0 {
		b = time.Now().Add(time.Duration(r.MaxAgeSeconds) * time.Second)
	}
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	case a.Before(b):
		return a
	default:
		return b
	}
}

// YSweetToken 是 Y-Sweet 协作文档的访问凭证。
//
// **必须原样转交沙箱**：字段名就是上游的，少一个沙箱就同步不起来。
// 所以除了结构化的常用字段，还额外保留 Raw ——
// 转发时发 Raw，这样上游**新增字段也不会被我们吃掉**。
type YSweetToken struct {
	DocID         string `json:"docId"`
	URL           string `json:"url"`     // wss://.../y/d/<uuid>/ws
	BaseURL       string `json:"baseUrl"` // https://.../y/d/<uuid>
	Authorization string `json:"authorization"`
	Token         string `json:"token"`

	// Raw 是接口返回的原始 JSON，转发时用它。
	Raw json.RawMessage `json:"-"`
}

// SandboxSyncStatus 是沙箱工作区同步状态（wait-for-sync 的响应）。
type SandboxSyncStatus struct {
	Status string `json:"status"`

	// ReadinessCapabilities 指示这个沙箱**需要**哪些能力才算就绪。
	//
	// 字段缺失（nil）与空数组是两种不同语义：
	//   nil      -> 旧协议，只要求 synced + 有 Y-Sweet 令牌
	//   ["current_y_sweet_provider"] -> 新协议，还要求 provider 已同步完成
	// 前端就是这么分支的，我们照抄。
	ReadinessCapabilities []string `json:"readinessCapabilities"`

	Tokens struct {
		HasResourceToken        bool   `json:"hasResourceToken"`
		HasResourceBaseURL      bool   `json:"hasResourceBaseUrl"`
		HasResourceProjectID    bool   `json:"hasResourceProjectId"`
		HasCurrentYSweetToken   bool   `json:"hasCurrentYSweetToken"`
		HasSyncedYSweetProvider bool   `json:"hasSyncedYSweetProvider"`
		FileCredentialSource    string `json:"fileCredentialSource"`
	} `json:"tokens"`
}

// NeedsYSweetProvider 报告该沙箱是否要求"Y-Sweet provider 已完成同步"。
func (s *SandboxSyncStatus) NeedsYSweetProvider() bool {
	for _, c := range s.ReadinessCapabilities {
		if c == "current_y_sweet_provider" {
			return true
		}
	}
	return false
}

// Ready 判断沙箱工作区是否已就绪。
//
// 判定规则照抄真实前端（bundle 里的 lr()），因为它对"哪种情况算就绪"
// 有明确分支——自己发明一套只会得到假就绪或假超时。
func (s *SandboxSyncStatus) Ready() bool {
	if s.Status == "failed" {
		return false
	}
	if !s.Tokens.HasCurrentYSweetToken {
		return false
	}
	if s.ReadinessCapabilities == nil {
		// 旧协议：不要求 provider 同步标记。
		return s.Status == "synced"
	}
	if !s.NeedsYSweetProvider() {
		return s.Status == "synced"
	}
	return s.Status == "synced" && s.Tokens.HasSyncedYSweetProvider
}

// RenderRequest 是 LaTeX 编译请求。
type RenderRequest struct {
	ProjectID string
	FileID    string
	Target    string // 例如 "pdf"
	Engine    string
	Extra     map[string]any
}

// RenderResponse 对应 202 Accepted 的返回。
//
// 文档明确写了 render 是 202 异步：返回一个 job id，然后靠 render-status 轮询。
type RenderResponse struct {
	JobID  string
	Status string
	Raw    json.RawMessage
}

// RenderStatus 对应 render-status 的返回。
type RenderStatus struct {
	Status string
	URL    string
	Log    string
	Errors []string
	Done   bool
	Fail   bool
	Raw    json.RawMessage
}

// RenderStatusOptions 控制长轮询行为。
type RenderStatusOptions struct {
	JobID  string
	WaitMs int
}

// FileUpload 是上传文件的描述。
type FileUpload struct {
	Filename    string
	ContentType string
	Data        []byte
	ProjectID   string
	Path        string
}

// HTTP 层共享的元信息。
type Meta struct {
	AccountID string
	Method    string
	Path      string
	Started   time.Time
	Status    int
	Duration  time.Duration
	Attempt   int
}

// Recorder 是可选的报文录制器（capture 模式）。
// 热路径上只在开启时才会有非 nil 值，避免关闭状态下的接口调用开销。
type Recorder interface {
	RecordRequest(meta Meta, header http.Header, body []byte)
	RecordResponse(meta Meta, header http.Header, body []byte)
}
