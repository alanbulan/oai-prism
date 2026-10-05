// Package config 定义并加载 OAIprism 的全部运行期配置。
//
// 配置来源优先级（后者覆盖前者）：
//
//	内置默认值  <  YAML 文件  <  环境变量（OAI_PRISM_*）
//
// 之所以要支持环境变量，是为了容器化部署时把凭据从镜像里剥离出去。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是顶层配置。
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Creds    CredsConfig    `yaml:"creds"`
	Pool     PoolConfig     `yaml:"pool"`
	Facade   FacadeConfig   `yaml:"facade"`
	RawProxy RawProxyConfig `yaml:"raw_proxy"`
	Metrics  MetricsConfig  `yaml:"metrics"`
	Capture  CaptureConfig  `yaml:"capture"`
	Log      LogConfig      `yaml:"log"`
}

// ServerConfig 描述对外监听。
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`

	// 流式响应下 WriteTimeout 必须为 0，否则长回答会被硬切断。
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`

	MaxHeaderBytes int `yaml:"max_header_bytes"`

	// MaxBodyBytes 限制入站请求体大小，防止内存被单请求打爆。
	MaxBodyBytes int64 `yaml:"max_body_bytes"`

	// TCP 层调优：大流量反代建议开启 NoDelay 并放大读写缓冲。
	TCPNoDelay     bool `yaml:"tcp_no_delay"`
	ReadBufferSize int  `yaml:"read_buffer_size"`

	TLS      TLSConfig `yaml:"tls"`
	BasePath string    `yaml:"base_path"`

	// CORSOrigin 是允许跨域访问的来源，留空表示不启用 CORS。
	//
	// 浏览器直连本代理时才需要它（纯服务端调用无需开启）。
	// 留空即完全不发 Access-Control-* 头，避免在不需要的场景
	// 给响应平白增加头部开销。
	CORSOrigin string `yaml:"cors_origin"`

	// AdminUser / AdminPassword 是 Dashboard 管理员登录凭据。
	//
	// 留空表示不开放密码登录：管理端只接受本机请求或配置文件里的 API Key
	// （见 middleware.Auth）。绝不内置默认密码 —— 内置口令等于没有口令。
	AdminUser     string `yaml:"admin_user"`
	AdminPassword string `yaml:"admin_password"`
}

// TLSConfig 可选服务端 TLS。
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// UpstreamConfig 描述到 prism.openai.com 的连接行为。
type UpstreamConfig struct {
	BaseURL string `yaml:"base_url"`

	// 单请求整体超时；流式场景由 Context 控制，这里是兜底。
	Timeout time.Duration `yaml:"timeout"`

	// 连接层。
	DialTimeout           time.Duration `yaml:"dial_timeout"`
	KeepAlive             time.Duration `yaml:"keep_alive"`
	TLSHandshakeTimeout   time.Duration `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	ExpectContinueTimeout time.Duration `yaml:"expect_continue_timeout"`

	// 连接池。反代场景 MaxIdleConnsPerHost 是吞吐的第一瓶颈，
	// 默认值 2 会导致高频建连，这里给 256。
	MaxIdleConns        int           `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost int           `yaml:"max_idle_conns_per_host"`
	MaxConnsPerHost     int           `yaml:"max_conns_per_host"`
	IdleConnTimeout     time.Duration `yaml:"idle_conn_timeout"`

	// ForceHTTP2 强制尝试 HTTP/2（prism 是 HTTPS，默认即可协商成功）。
	ForceHTTP2 bool `yaml:"force_http2"`

	// DisableCompression 关闭 Go 自动 gzip 解压：我们要原样透传字节，
	// 让客户端自己协商压缩。开启自动解压会强制 Accept-Encoding 并丢掉原始编码。
	DisableCompression bool `yaml:"disable_compression"`

	// 读写缓冲：32KB 比默认 4KB 显著减少 syscall 次数。
	ReadBufferSize  int `yaml:"read_buffer_size"`
	WriteBufferSize int `yaml:"write_buffer_size"`

	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`

	// HTTPProxy 支持 per-upstream 出站代理（http/https/socks5）。
	HTTPProxy string `yaml:"http_proxy"`

	// SentinelProfile 指向自定义的浏览器指纹（JSON，形状同 internal/sentinel/profile_default.json）。
	// 留空用内置指纹。base_url 指向 prism.openai.com 时，出站自动走内置的 Chrome 指纹传输
	// 并用纯 Go 签发 Sentinel token（internal/upstream），User-Agent 等浏览器头也以指纹为准。
	SentinelProfile string `yaml:"sentinel_profile"`

	UserAgent string            `yaml:"user_agent"`
	Origin    string            `yaml:"origin"`
	Referer   string            `yaml:"referer"`
	Headers   map[string]string `yaml:"headers"`

	// 重试策略。
	MaxRetries    int           `yaml:"max_retries"`
	RetryBackoff  time.Duration `yaml:"retry_backoff"`
	RetryMaxDelay time.Duration `yaml:"retry_max_delay"`

	// 熔断：连续失败多少次后打开断路器。
	BreakerThreshold int           `yaml:"breaker_threshold"`
	BreakerCooldown  time.Duration `yaml:"breaker_cooldown"`
}

// CredsConfig 描述"凭据从哪来"。
type CredsConfig struct {
	// Mode: static | file | passthrough | hybrid
	//   static      - 凭据写在配置文件/env 里
	//   file        - 凭据放在独立 JSON 文件，按 mtime 热重载（推荐）
	//   passthrough - 由调用方在每个请求上携带，不落盘
	//   hybrid      - 池里优先用静态账号，缺号时透传
	Mode string `yaml:"mode"`

	File           string        `yaml:"file"`
	ReloadInterval time.Duration `yaml:"reload_interval"`

	// 凭据自愈。
	AutoRefresh       bool          `yaml:"auto_refresh"`     // 会话过期自动刷新
	RefreshSkew       time.Duration `yaml:"refresh_skew"`     // 提前多久刷新
	RefreshInterval   time.Duration `yaml:"refresh_interval"` // 后台巡检周期
	SessionPath       string        `yaml:"session_path"`     // 默认 /api/auth/session
	OAuthTokenURL     string        `yaml:"oauth_token_url"`  // refresh_token 换发地址
	OAuthClientID     string        `yaml:"oauth_client_id"`  // 默认 codex 客户端
	OAuthScope        string        `yaml:"oauth_scope"`
	PersistRefresh    bool          `yaml:"persist_refresh"`     // 刷新结果写回 JSON 凭据文件（未实现）；SQLite 回写不受此开关控制，始终开启
	PersistRefreshMin time.Duration `yaml:"persist_refresh_min"` // 同一账号两次回写 SQLite 的最小间隔；refresh_token 轮换时不受限

	Accounts []AccountConfig `yaml:"accounts"`
}

// AccountConfig 是单个账号的静态定义。
//
// 认证字段按"能拿到什么填什么"设计，四种形态任意组合：
//
//	Cookies       - 浏览器里整串 Cookie（最省事，包含 session-token）
//	SessionToken  - 只给 __Secure-next-auth.session-token 的值
//	AccessToken   - 直接给 JWT（chatgpt accessToken）
//	RefreshToken  - 给 OAuth refresh_token，由代理自动换 accessToken
type AccountConfig struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	Enabled *bool  `yaml:"enabled"` // 指针以便区分"未设置"与"显式 false"

	Cookies      string            `yaml:"cookies"`
	CookieMap    map[string]string `yaml:"cookie_map"`
	SessionToken string            `yaml:"session_token"`
	AccessToken  string            `yaml:"access_token"`
	RefreshToken string            `yaml:"refresh_token"`
	ExpiresAt    *time.Time        `yaml:"expires_at"`

	// OAuthClientID 是签发该账号 refresh_token 的 OAuth client（为空则用 creds.oauth_client_id）。
	// refresh_token 与 client 绑定，混用会 invalid_grant。读取请走 EffectiveOAuthClientID。
	OAuthClientID string `yaml:"oauth_client_id"`

	AccountID string `yaml:"account_id"`
	Email     string `yaml:"email"`
	Plan      string `yaml:"plan"`

	// 网络出口。留空走 Upstream.HTTPProxy。
	Proxy string `yaml:"proxy"`

	// 单账号并发上限。Prism 对同一账号并发比较敏感，默认保守。
	MaxConcurrency int `yaml:"max_concurrency"`

	// 速率限制：每秒补充 tokens 个令牌，桶容量 burst。
	RatePerSecond float64 `yaml:"rate_per_second"`
	RateBurst     int     `yaml:"rate_burst"`

	// Weight 用于加权轮询。
	Weight int `yaml:"weight"`

	// Priority 是调度优先级，数值越大越先用（默认 0）。调度先在最高的一档里按策略挑，
	// 这一档全部不可用（冷却、满并发、凭据失效）才轮到下一档。
	Priority int `yaml:"priority"`

	Headers map[string]string `yaml:"headers"`
	Tags    []string          `yaml:"tags"`
}

// IsEnabled 处理 *bool 的三态。
func (a AccountConfig) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

// LegacyOAuthClientIDHeader 是早期版本存放 oauth_client_id 的 Headers 键名。
//
// 那时它混在账号级附加头里，而 Headers 会原样转发给上游 —— 等于每个请求都多带
// 一个浏览器不会发的 Oauth_client_id 头。现在只作旧 accounts.json 的读取兜底，
// 并在构造凭据时从 Headers 中剔除（见 creds.FromAccountConfig）。
const LegacyOAuthClientIDHeader = "oauth_client_id"

// EffectiveOAuthClientID 返回账号的 OAuth client：专用字段优先，其次旧版 Headers 兜底。
func (a AccountConfig) EffectiveOAuthClientID() string {
	if v := strings.TrimSpace(a.OAuthClientID); v != "" {
		return v
	}
	for k, v := range a.Headers {
		if strings.EqualFold(k, LegacyOAuthClientIDHeader) {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}

// PoolConfig 描述账号池调度。
type PoolConfig struct {
	// Strategy: round_robin | least_inflight | random | sticky_hash | weighted
	Strategy string `yaml:"strategy"`

	// StickyTTL 粘性会话存活时间。Prism 的会话/项目是按账号绑定的，
	// 同一对话必须固定命中同一账号，否则会 404。
	StickyTTL time.Duration `yaml:"sticky_ttl"`

	// Cooldown 账号命中 401/429/风控后进入冷却的时长。
	Cooldown time.Duration `yaml:"cooldown"`

	// MaxWait 是"全池冷却时最多等多久"。
	// 超过就直接报错，而不是让调用方挂在一个注定超时的请求上。
	MaxWait         time.Duration `yaml:"max_wait"`
	CooldownBackoff float64       `yaml:"cooldown_backoff"` // 每次追加冷却的倍率
	MaxCooldown     time.Duration `yaml:"max_cooldown"`

	// 健康检查。
	HealthCheck         bool          `yaml:"health_check"`
	HealthCheckPath     string        `yaml:"health_check_path"`
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
	HealthCheckTimeout  time.Duration `yaml:"health_check_timeout"`
}

// FacadeConfig 描述 OpenAI/Anthropic 兼容门面。
type FacadeConfig struct {
	Enabled bool     `yaml:"enabled"`
	APIKeys []string `yaml:"api_keys"` // 空 = 不校验（仅建议本地）

	DefaultModel string `yaml:"default_model"`

	// LocalWorkspaceWrite 允许 chat 接口按 X-Local-Workspace 请求头把上游
	// 沙箱产物直接写到**网关所在机器**的目录里。
	//
	// 默认关闭：这等于让调用方指定网关主机上的任意写入位置。只在网关与
	// 客户端同机、且调用方可信时开启；开启后也仅接受本机请求。
	LocalWorkspaceWrite bool `yaml:"local_workspace_write"`

	// PlatformNotice 在非 Codex 桥请求的 system 最前面加一段声明，点名作废上游
	// 沙箱注入的 Prism AGENTS.md（LaTeX 编辑器规则）。上游没有去掉它的请求字段，
	// 它与我们的内容同属 user 层，只能靠后到 + 点名压住。Codex 桥请求自带同类
	// 条款，不受此开关影响。默认开启。
	PlatformNotice bool `yaml:"platform_notice"`

	// MaxPromptBytes 是发往上游的单条提示词（合并后的 system + 最后一条 user）的
	// UTF-8 字节上限。上游按字节而不是 token 限长：2026-10-04 实测合计 102,299 字节
	// 可过、104,560 字节报 "This request is too large to send"（约 100 KiB），与内容
	// 是中文还是英文无关。新建上游会话时按它决定历史随首轮一条发完还是分段补种（见
	// internal/facade/native.go）；本轮内容本身就超限的请求不再发出，直接以
	// context_length_exceeded 失败。
	// 默认 96 KiB，给上游自己的包装文本留余量；设为 -1 关闭检查。
	MaxPromptBytes int `yaml:"max_prompt_bytes"`

	// Models 把对外模型名映射到 Prism 内部的 model / reasoning effort。
	// 例：gpt-5-codex-fast -> {model: gpt-5, effort: high}
	Models map[string]ModelMapping `yaml:"models"`

	// 上游协议字段名——留成可配置是因为这套内部 API 会变，
	// 改字段不该逼着重新编译。
	Schema SchemaConfig `yaml:"schema"`

	// 会话与项目复用：避免每个请求都 POST /api/projects。
	ReuseProject    bool          `yaml:"reuse_project"`
	ProjectTTL      time.Duration `yaml:"project_ttl"`
	ProjectPoolSize int           `yaml:"project_pool_size"`

	// 轮询参数。render-status 支持 waitMs 长轮询，
	// response_with_tools_status 是否支持由 enable_wait_ms 控制。
	PollInterval  time.Duration `yaml:"poll_interval"`
	PollWaitMs    int           `yaml:"poll_wait_ms"`
	UseStatusWait bool          `yaml:"use_status_wait"`
	// MaxPollTimeout 是上游毫无进展时最多等多久：转录游标前进、来了思考摘要或正文都会顺延。
	MaxPollTimeout time.Duration `yaml:"max_poll_timeout"`
	// MaxRunTimeout 是一轮生成的总时长上限（不论有没有进展）。
	MaxRunTimeout  time.Duration `yaml:"max_run_timeout"`
	PollBackoffMax time.Duration `yaml:"poll_backoff_max"`

	// 同步模式下最多等多久；超时后返回已完成部分。
	SyncTimeout time.Duration `yaml:"sync_timeout"`

	// 把 OpenAI 的 system 消息改写成 Prism 的 instructions 字段。
	SystemToInstructions bool `yaml:"system_to_instructions"`

	// 是否在响应中回传 Prism 原始元数据（debug 用）。
	ExposeRawMetadata bool `yaml:"expose_raw_metadata"`

	// UseSandbox 是否自动申请并使用沙箱。
	//
	// Prism 的 AI 助手跑在沙箱容器里，start 请求必须带上 sandbox_url /
	// sandbox_token，否则上游会回 sandbox_reconnecting（看起来像"上游挂了"）。
	// 默认开启；若某天上游不再强制沙箱，可以关掉省一次往返。
	UseSandbox bool `yaml:"use_sandbox"`

	// SandboxTTL 是沙箱信息的缓存时长。
	// 申请沙箱要真的分配容器，很慢，必须缓存复用。
	SandboxTTL time.Duration `yaml:"sandbox_ttl"`

	// SandboxReadyWait 是"等沙箱容器就绪"的最长等待。
	// 就绪前 wait-for-sync 会直接断 TLS，所以这里靠重试熬过去。
	SandboxReadyWait time.Duration `yaml:"sandbox_ready_wait"`

	// DefaultSystemPrompt 在调用方没有提供 system 消息时注入。留空表示不注入。
	DefaultSystemPrompt string `yaml:"default_system_prompt"`

	// 入站全局限流：每秒补充 RatePerSecond 个令牌，桶容量 RateBurst。
	// 0 表示不限流。这是保护上游账号不被自己的重试打爆的最后一道闸。
	RatePerSecond float64 `yaml:"rate_per_second"`
	RateBurst     int     `yaml:"rate_burst"`
}

// PromptByteLimit 返回生效的单条提示词字节上限；0 表示不限（max_prompt_bytes 设为负数）。
func (f FacadeConfig) PromptByteLimit() int {
	if f.MaxPromptBytes < 0 {
		return 0
	}
	return f.MaxPromptBytes
}

// RateLimitPerSecond 返回全局限流速率。
func (f FacadeConfig) RateLimitPerSecond() float64 { return f.RatePerSecond }

// RateLimitBurst 返回令牌桶容量。
func (f FacadeConfig) RateLimitBurst() int { return f.RateBurst }

// ModelMapping 对外模型名 -> 上游参数。
type ModelMapping struct {
	Model           string `yaml:"model"`
	ReasoningEffort string `yaml:"reasoning_effort"`
	Instructions    string `yaml:"instructions"`
	// Label 是 /v1/models 里展示用的名字（可选）。
	// 不填时条目只带 id，不带 name 字段。
	Label string `yaml:"label"`
}

// SchemaConfig 是上游报文的字段名映射。
// 每一项都可以在 YAML 里被覆盖，从而无需改代码即可适配协议变更。
type SchemaConfig struct {
	StartPath  string `yaml:"start_path"`
	StatusPath string `yaml:"status_path"`
	// StopPath 用于主动取消进行中的生成（前端"停止"按钮走的就是它）。
	StopPath string `yaml:"stop_path"`

	// 请求字段名。
	FieldModel          string `yaml:"field_model"`
	FieldMessages       string `yaml:"field_messages"`
	FieldInstructions   string `yaml:"field_instructions"`
	FieldInput          string `yaml:"field_input"`
	FieldTools          string `yaml:"field_tools"`
	FieldStream         string `yaml:"field_stream"`
	FieldSessionID      string `yaml:"field_session_id"`
	FieldProjectID      string `yaml:"field_project_id"`
	FieldSandboxID      string `yaml:"field_sandbox_id"`
	FieldConversationID string `yaml:"field_conversation_id"`
	// FieldPreviousRespID / FieldMetadata 对应真实请求体里的
	// previousResponseId 与 metadata —— 多轮延续与模型参数都走 metadata。
	FieldPreviousRespID string `yaml:"field_previous_response_id"`
	FieldMetadata       string `yaml:"field_metadata"`
	// FieldUserID 是调用方身份（OpenAI 的 user / Anthropic 的 metadata.user_id）。
	// 它只进 metadata，不进请求体顶层。
	//
	// 注意：这个名字来自对端实现，尚未经真实报文验证（与 schema 里其它
	// 字段一样属于推断值）。设成 "-" 即可整体关闭这个字段。
	FieldUserID string `yaml:"field_user_id"`
	// FieldRequestID / FieldTurnState 只出现在 status / stop 请求里。
	// turn_state 是服务端下发的不透明状态对象，必须原样回传，
	// 自己构造会被上游拒绝（实测：任何自造值都回 "turn_state is required"）。
	FieldRequestID       string         `yaml:"field_request_id"`
	FieldTurnState       string         `yaml:"field_turn_state"`
	FieldResponseID      string         `yaml:"field_response_id"`
	FieldReasoning       string         `yaml:"field_reasoning"`
	FieldReasoningEffort string         `yaml:"field_reasoning_effort"`
	FieldExtra           map[string]any `yaml:"field_extra"`

	// 响应字段名（用于抽取增量文本）。
	RespIDKeys      []string `yaml:"resp_id_keys"`
	RespStatusKeys  []string `yaml:"resp_status_keys"`
	RespTextKeys    []string `yaml:"resp_text_keys"`
	RespDeltaKeys   []string `yaml:"resp_delta_keys"`
	RespMessagesKey string   `yaml:"resp_messages_key"`
	RespErrorKeys   []string `yaml:"resp_error_keys"`

	// 终态状态值。
	StatusDone []string `yaml:"status_done"`
	StatusFail []string `yaml:"status_fail"`
	StatusRun  []string `yaml:"status_running"`
}

// RawProxyConfig 描述透明反代通道。
type RawProxyConfig struct {
	Enabled bool `yaml:"enabled"`

	// Prefix 对外挂载路径，默认 /prism。
	Prefix string `yaml:"prefix"`

	// AllowPaths 白名单前缀。默认只放开文档里列出的那些端点。
	AllowPaths []string `yaml:"allow_paths"`

	// PassthroughAuth 允许客户端用自己的 Cookie/JWT 覆盖账号池（本地调试用）。
	PassthroughAuth bool `yaml:"passthrough_auth"`

	// StripHeaders 出站前剥掉的入站头（默认剥掉 Host 与 hop-by-hop）。
	StripHeaders []string `yaml:"strip_headers"`

	// BufferSize 流式透传的拷贝缓冲。
	BufferSize int `yaml:"buffer_size"`
}

// MetricsConfig 描述指标与探针。
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
	Health  string `yaml:"health_path"`
	Ready   string `yaml:"ready_path"`
}

// CaptureConfig 描述抓包。
type CaptureConfig struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`

	// MaxBody 单个 body 落盘上限，防止把 Bearer 之外的大文件也录进去。
	MaxBody int64 `yaml:"max_body"`

	// Redact 是否把认证类字段替换为 ***（默认 true）。
	Redact bool `yaml:"redact"`

	// Sample 采样率 0~1。
	Sample float64 `yaml:"sample"`

	// IncludePaths / ExcludePaths 按路径过滤。
	IncludePaths []string `yaml:"include_paths"`
	ExcludePaths []string `yaml:"exclude_paths"`

	// ReplayFile 以回放模式加载抓包文件，用于离线校准编解码器。
	ReplayFile string `yaml:"replay_file"`
}

// LogConfig 日志。
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host:              "0.0.0.0",
			Port:              8787,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       0,
			IdleTimeout:       120 * time.Second,
			WriteTimeout:      0,
			MaxHeaderBytes:    1 << 20,
			MaxBodyBytes:      64 << 20,
			TCPNoDelay:        true,
			ReadBufferSize:    64 << 10,
		},
		Upstream: UpstreamConfig{
			BaseURL:               "https://prism.openai.com",
			Timeout:               0,
			DialTimeout:           10 * time.Second,
			KeepAlive:             30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   256,
			MaxConnsPerHost:       0,
			IdleConnTimeout:       90 * time.Second,
			ForceHTTP2:            true,
			DisableCompression:    true,
			ReadBufferSize:        32 << 10,
			WriteBufferSize:       32 << 10,
			UserAgent:             DefaultUserAgent,
			Origin:                "https://prism.openai.com",
			Referer:               "https://prism.openai.com/",
			MaxRetries:            3,
			RetryBackoff:          200 * time.Millisecond,
			RetryMaxDelay:         5 * time.Second,
			BreakerThreshold:      12,
			BreakerCooldown:       30 * time.Second,
		},
		Creds: CredsConfig{
			Mode:            "file",
			File:            "secrets/accounts.json",
			ReloadInterval:  5 * time.Second,
			AutoRefresh:     true,
			RefreshSkew:     5 * time.Minute,
			RefreshInterval: 10 * time.Minute,
			SessionPath:     "/api/auth/session",
			OAuthTokenURL:   "https://auth.openai.com/oauth/token",
			OAuthClientID:   DefaultOAuthClientID,
			OAuthScope:      "openid profile email offline_access",
		},
		Pool: PoolConfig{
			Strategy:            "least_inflight",
			StickyTTL:           30 * time.Minute,
			Cooldown:            60 * time.Second,
			CooldownBackoff:     2,
			MaxCooldown:         30 * time.Minute,
			MaxWait:             10 * time.Second,
			HealthCheck:         true,
			HealthCheckPath:     "/api/auth/session",
			HealthCheckInterval: 5 * time.Minute,
			HealthCheckTimeout:  15 * time.Second,
		},
		Facade: FacadeConfig{
			Enabled:      true,
			DefaultModel: DefaultPrismModel,
			// 内置清单 = 4 个现役模型 x 各自的推理档位，与 configs/config.yaml 保持一致。
			// 已下线模型（astra 系）与历史别名（gpt-5、短别名等）不再内置 —— 以
			// Statsig prism_codex_models 实测清单为准，维护时同步改这里和 yaml。
			Models: map[string]ModelMapping{
				// 6.1 Sol（当前旗舰）
				"gpt-6.1-sol":       {Model: "gpt-6.1-sol", ReasoningEffort: "medium", Label: "6.1 Sol"},
				"gpt-6.1-sol-low":   {Model: "gpt-6.1-sol", ReasoningEffort: "low", Label: "6.1 Sol (Low)"},
				"gpt-6.1-sol-high":  {Model: "gpt-6.1-sol", ReasoningEffort: "high", Label: "6.1 Sol (High)"},
				"gpt-6.1-sol-xhigh": {Model: "gpt-6.1-sol", ReasoningEffort: "xhigh", Label: "6.1 Sol (Extra High)"},
				// 6 Luna
				"gpt-6-luna":       {Model: "gpt-6-luna", ReasoningEffort: "medium", Label: "6 Luna"},
				"gpt-6-luna-high":  {Model: "gpt-6-luna", ReasoningEffort: "high", Label: "6 Luna (High)"},
				"gpt-6-luna-xhigh": {Model: "gpt-6-luna", ReasoningEffort: "xhigh", Label: "6 Luna (Extra High)"},
				// 5.6 Sol
				"gpt-5.6-sol":       {Model: "gpt-5.6-sol", ReasoningEffort: "medium", Label: "5.6 Sol"},
				"gpt-5.6-sol-low":   {Model: "gpt-5.6-sol", ReasoningEffort: "low", Label: "5.6 Sol (Low)"},
				"gpt-5.6-sol-high":  {Model: "gpt-5.6-sol", ReasoningEffort: "high", Label: "5.6 Sol (High)"},
				"gpt-5.6-sol-xhigh": {Model: "gpt-5.6-sol", ReasoningEffort: "xhigh", Label: "5.6 Sol (Extra High)"},
				// 5.6 Terra
				"gpt-5.6-terra":       {Model: "gpt-5.6-terra", ReasoningEffort: "medium", Label: "5.6 Terra"},
				"gpt-5.6-terra-high":  {Model: "gpt-5.6-terra", ReasoningEffort: "high", Label: "5.6 Terra (High)"},
				"gpt-5.6-terra-xhigh": {Model: "gpt-5.6-terra", ReasoningEffort: "xhigh", Label: "5.6 Terra (Extra High)"},
			},
			Schema:              defaultSchema(),
			ReuseProject:        true,
			ProjectTTL:          30 * time.Minute,
			ProjectPoolSize:     4,
			PollInterval:        DefaultPollInterval,
			PollWaitMs:          10000,
			UseStatusWait:       true,
			MaxPollTimeout:      15 * time.Minute,
			MaxRunTimeout:       DefaultMaxRunTimeout,
			PollBackoffMax:      DefaultPollBackoffMax,
			SyncTimeout:         10 * time.Minute,
			UseSandbox:          true,
			SandboxTTL:          30 * time.Minute,
			SandboxReadyWait:    60 * time.Second,
			DefaultSystemPrompt: DefaultFacadeSystemPrompt,
			PlatformNotice:      true,
			MaxPromptBytes:      DefaultFacadeMaxPromptBytes,
		},
		RawProxy: RawProxyConfig{
			Enabled:         true,
			Prefix:          "/prism",
			PassthroughAuth: false,
			BufferSize:      64 << 10,
			AllowPaths: []string{
				"/api/projects",
				"/api/project-access",
				"/api/codex/conversation-history",
				"/api/llm/",
				"/api/backend/",
				"/api/y",
				"/s/sandboxes/",
				"/api/project-files/",
				"/api/auth/session",
			},
		},
		Metrics: MetricsConfig{Enabled: true, Path: "/metrics", Health: "/healthz", Ready: "/readyz"},
		Capture: CaptureConfig{
			Enabled:      false,
			Dir:          "captures",
			MaxBody:      4 << 20,
			Redact:       true,
			Sample:       1,
			ExcludePaths: []string{"/s/sandboxes/proxy/render", "/api/project-files/upload"},
		},
		Log: LogConfig{Level: "info", Format: "json"},
	}
}

// defaultSchema 是**已用真实流量校准过**的字段映射。
//
// 校准依据（三处独立证据交叉验证，不是猜的）：
//
//  1. 对 /api/llm/response_with_tools_start 发畸形请求，上游直接回
//     {"status":"error","message":"input must be an array"}
//     —— 既证明路由存在，又证明了请求字段名是 input；
//
//  2. 对 /api/llm/response_with_tools_status 发空体，回
//     {"status":"error","message":"request_id is required"}；
//     再带上 request_id，回 {"status":"error","message":"turn_state is required"}
//     —— 证明 status 接口要 request_id + turn_state 两个字段；
//
//  3. 前端 bundle（_next/static/chunks/11850yk199q3a.js）里的真实调用代码：
//
//     fetch("/api/llm/response_with_tools_start", {body: JSON.stringify({
//     input, previousResponseId, metadata, conversationId })})
//     fetch("/api/llm/response_with_tools_status", {body: JSON.stringify({
//     request_id, turn_state })})
//     fetch("/api/llm/response_with_tools_stop", {body: JSON.stringify({
//     request_id, conversation_id, turn_state })})
//
// 推理端点路径是 /api/llm/response_with_tools_*（实测校准，
// 见 docs/协议校准报告.md）。
func defaultSchema() SchemaConfig {
	return SchemaConfig{
		StartPath:  "/api/llm/response_with_tools_start",
		StatusPath: "/api/llm/response_with_tools_status",
		StopPath:   "/api/llm/response_with_tools_stop",

		FieldModel:        "model",
		FieldMessages:     "messages",
		FieldInstructions: "instructions",
		// 真实请求体里的对话内容字段叫 input（数组），不是 messages。
		FieldInput:  "input",
		FieldTools:  "tools",
		FieldStream: "stream",
		// 多轮延续与模型参数。
		FieldPreviousRespID: "previousResponseId",
		FieldMetadata:       "metadata",
		FieldUserID:         "userId",

		FieldSessionID: "session_id",
		FieldProjectID: "project_id",
		FieldSandboxID: "sandbox_id",
		// 注意大小写：start 请求体里是 camelCase 的 conversationId，
		// 而 status 请求里又是 snake_case 的 request_id ——
		// 上游自己不一致，我们照抄，不要"顺手统一"。
		FieldConversationID:  "conversationId",
		FieldRequestID:       "request_id",
		FieldTurnState:       "turn_state",
		FieldResponseID:      "response_id",
		FieldReasoning:       "reasoning",
		FieldReasoningEffort: "effort",

		RespIDKeys:     []string{"request_id", "id", "response_id", "stream_id"},
		RespStatusKeys: []string{"status", "state", "phase"},
		// 正文路径：response.payload.output[-1].content[-1].text
		RespTextKeys:    []string{"text", "output_text", "content", "message"},
		RespDeltaKeys:   []string{"delta", "delta_text", "chunk", "output_text_delta"},
		RespMessagesKey: "messages",
		RespErrorKeys:   []string{"error", "errors"},

		// 实测状态机：start 回 started，status 回 pending，终态是 completed。
		StatusDone: []string{"completed", "complete", "done", "succeeded", "success", "finished", "final"},
		StatusFail: []string{"failed", "error", "cancelled", "canceled", "expired"},
		StatusRun:  []string{"started", "pending", "running", "in_progress", "queued", "streaming", "generating"},
	}
}

// 常量：默认 UA / OAuth client。
// 轮询节奏的默认值。
//
// 取舍说明：真实前端（prism.openai.com 自己的界面）是**固定 5 秒**轮询一次，
// 而且它在 pending 分支只读 turn_state、完全不看 response —— 这暗示
// pending 帧里可能根本没有正文，那样的话"流式"实质上是一次性给出。
//
// 我们取 1 秒：
//   - 若上游确实会在 pending 帧里给累计正文，1 秒能带来可接受的准流式体验；
//   - 若不给，1 秒也没有额外收益，但仍远低于会触发限流的频率；
//   - 无论如何循环都会做无条件节流（见 Runner.pollOnce 的注释），
//     绝不会退化成零间隔忙轮询。
//
// 调优建议：上游若出现 429，优先调大 poll_interval；想再快就调小，
// 但请同时观察 oaiprism_poll_rounds_total 的增速。
const (
	DefaultPollInterval   = 1 * time.Second
	DefaultPollBackoffMax = 3 * time.Second
	DefaultMaxRunTimeout  = 2 * time.Hour
)

// DefaultFacadeSystemPrompt 是兜底系统提示：中性角色，不带任何平台人设。
//
// 曾经写成 "the AI assistant inside Prism, an online LaTeX editor"，以为在对齐官方
// 前端的 makeSystemPrompt —— 实测（2026-10-04）上游只读最后一条 system，前端那条
// 人设根本到不了模型；我们自己反倒给每个请求加上了 LaTeX 编辑器人设。
const DefaultFacadeSystemPrompt = "You are a helpful assistant. Answer the user's request directly."

// DefaultFacadeMaxPromptBytes 是 facade.max_prompt_bytes 的默认值（见 FacadeConfig.MaxPromptBytes）。
const DefaultFacadeMaxPromptBytes = 96 << 10

// DefaultPrismModel 是上游对话模型的默认值。
//
// 来自前端 bundle 里的 `let n="gpt-5.6-sol"`（UI 显示为 "5.6 Sol"）。
// 真实可用列表由 Statsig 开关 prism_codex_models 动态下发，
// 因此这个值可能随上游灰度变化 —— 变更时改配置即可，不用改代码。
// 清单来源：Statsig 动态配置 prism_codex_models（2026-10 实测）。
// 上游会下线/新增模型：gpt-6-astra 已下线（上游路由到
// codex_v2_restore_start 并返回 400，无创建入口），
// gpt-6.1-sol / gpt-6-luna 为当前在售。维护时以该配置为准。
const DefaultPrismModel = "gpt-6.1-sol"

const (
	// DefaultUserAgent 与 Codex CLI 保持一致的形态，避免被上游按"未知客户端"降级。
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

	// DefaultOAuthClientID 是换发 refresh_token 时用的 client_id。
	//
	// 取自 Prism 自己的 access token 里的 client_id 声明（实测）。
	// 这个值必须与签发 refresh_token 的那个 client 一致，否则
	// OAuth 会直接回 invalid_client。
	//
	// 参考：Codex CLI 用的是另一个 client_id（app_EMoamEEZ73f0CkXaXp7hrann），
	// 两者不通用 —— 拿错会得到"凭据明明有效却刷新失败"的假象。
	DefaultOAuthClientID = "app_jqKb52JverFFcl5GP4axT8QY"

	// EnvPrefix 环境变量前缀。
	EnvPrefix = "OAI_PRISM_"
)

// Load 读取配置：默认值 -> 文件 -> 环境变量。
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(false) // 允许未知字段，便于向前兼容
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
		}
	}

	applyEnv(cfg)

	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// normalize 补齐零值、做基本合法性校验。
func (c *Config) normalize() error {
	// 账户文件相对路径基于配置文件所在目录解析更符合直觉，
	// 但为了脚本可预测，这里统一按进程工作目录解析，仅在缺失时提示。
	if c.Creds.File == "" && c.Creds.Mode == "file" {
		c.Creds.File = "secrets/accounts.json"
	}
	if c.Creds.ReloadInterval <= 0 {
		c.Creds.ReloadInterval = 5 * time.Second
	}
	if c.Creds.RefreshSkew <= 0 {
		c.Creds.RefreshSkew = 5 * time.Minute
	}
	if c.Creds.SessionPath == "" {
		c.Creds.SessionPath = "/api/auth/session"
	}
	if c.Creds.OAuthTokenURL == "" {
		c.Creds.OAuthTokenURL = "https://auth.openai.com/oauth/token"
	}
	if c.Creds.OAuthClientID == "" {
		c.Creds.OAuthClientID = DefaultOAuthClientID
	}

	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = "https://prism.openai.com"
	}
	c.Upstream.BaseURL = strings.TrimRight(c.Upstream.BaseURL, "/")

	if c.Upstream.MaxIdleConnsPerHost <= 0 {
		c.Upstream.MaxIdleConnsPerHost = 256
	}
	if c.Upstream.MaxIdleConns <= 0 {
		c.Upstream.MaxIdleConns = 512
	}
	if c.Upstream.ReadBufferSize <= 0 {
		c.Upstream.ReadBufferSize = 32 << 10
	}
	if c.Upstream.WriteBufferSize <= 0 {
		c.Upstream.WriteBufferSize = 32 << 10
	}
	if c.Upstream.UserAgent == "" {
		c.Upstream.UserAgent = DefaultUserAgent
	}

	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port 非法: %d", c.Server.Port)
	}
	if c.Server.MaxBodyBytes <= 0 {
		c.Server.MaxBodyBytes = 64 << 20
	}
	if c.Server.ReadHeaderTimeout <= 0 {
		c.Server.ReadHeaderTimeout = 10 * time.Second
	}

	if c.Pool.Strategy == "" {
		c.Pool.Strategy = "least_inflight"
	}
	if c.Pool.StickyTTL <= 0 {
		c.Pool.StickyTTL = 30 * time.Minute
	}
	if c.Pool.Cooldown <= 0 {
		c.Pool.Cooldown = 60 * time.Second
	}
	if c.Pool.MaxWait <= 0 {
		c.Pool.MaxWait = 10 * time.Second
	}

	f := &c.Facade
	if f.DefaultModel == "" {
		f.DefaultModel = DefaultPrismModel
	}
	if f.Schema.StartPath == "" || f.Schema.StatusPath == "" {
		def := defaultSchema()
		if f.Schema.StartPath == "" {
			f.Schema.StartPath = def.StartPath
		}
		if f.Schema.StatusPath == "" {
			f.Schema.StatusPath = def.StatusPath
		}
	}
	if len(f.Schema.RespIDKeys) == 0 {
		f.Schema.RespIDKeys = defaultSchema().RespIDKeys
	}
	if len(f.Schema.RespStatusKeys) == 0 {
		f.Schema.RespStatusKeys = defaultSchema().RespStatusKeys
	}
	if len(f.Schema.RespTextKeys) == 0 {
		f.Schema.RespTextKeys = defaultSchema().RespTextKeys
	}
	if len(f.Schema.RespDeltaKeys) == 0 {
		f.Schema.RespDeltaKeys = defaultSchema().RespDeltaKeys
	}
	if len(f.Schema.StatusDone) == 0 {
		f.Schema.StatusDone = defaultSchema().StatusDone
	}
	if len(f.Schema.StatusFail) == 0 {
		f.Schema.StatusFail = defaultSchema().StatusFail
	}
	if f.SandboxTTL <= 0 {
		f.SandboxTTL = 30 * time.Minute
	}
	if f.SandboxReadyWait <= 0 {
		f.SandboxReadyWait = 60 * time.Second
	}
	if f.PollInterval <= 0 {
		f.PollInterval = DefaultPollInterval
	}
	if f.PollBackoffMax <= 0 {
		f.PollBackoffMax = DefaultPollBackoffMax
	}
	if f.MaxPollTimeout <= 0 {
		f.MaxPollTimeout = 15 * time.Minute
	}
	if f.MaxRunTimeout <= 0 {
		f.MaxRunTimeout = DefaultMaxRunTimeout
	}
	if f.SyncTimeout <= 0 {
		f.SyncTimeout = 10 * time.Minute
	}
	if f.ProjectPoolSize <= 0 {
		f.ProjectPoolSize = 4
	}
	if f.MaxPromptBytes == 0 {
		f.MaxPromptBytes = DefaultFacadeMaxPromptBytes
	}

	if c.RawProxy.Prefix == "" {
		c.RawProxy.Prefix = "/prism"
	}
	if c.RawProxy.BufferSize <= 0 {
		c.RawProxy.BufferSize = 64 << 10
	}

	if c.Metrics.Path == "" {
		c.Metrics.Path = "/metrics"
	}
	if c.Metrics.Health == "" {
		c.Metrics.Health = "/healthz"
	}
	if c.Metrics.Ready == "" {
		c.Metrics.Ready = "/readyz"
	}

	if c.Capture.Enabled && c.Capture.Dir == "" {
		c.Capture.Dir = "captures"
	}

	switch c.Creds.Mode {
	case "static", "file", "passthrough", "hybrid":
	default:
		return fmt.Errorf("creds.mode 非法 %q（可选 static|file|passthrough|hybrid）", c.Creds.Mode)
	}

	switch c.Pool.Strategy {
	case "round_robin", "least_inflight", "random", "sticky_hash", "weighted":
	default:
		return fmt.Errorf("pool.strategy 非法 %q（可选 round_robin|least_inflight|random|sticky_hash|weighted）", c.Pool.Strategy)
	}

	if len(c.Facade.APIKeys) == 0 {
		// 不阻断启动：此时鉴权中间件只放行本机请求（Dashboard 签发 Key 后
		// 自动转为全量校验）。启动告警由 server.New 输出（这里没有 logger）。
		c.Facade.APIKeys = nil
	}
	return nil
}

// Addr 返回监听地址。
func (s ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// applyEnv 支持最常用的几项用环境变量覆盖。
//
// 优先使用 OAI_PRISM_* 前缀；若未设置，兼容 PrismOpenAIProxy 的生态变量名
// （PRISM_COOKIE, PROXY_API_KEY, PORT, HOST, CORS_ORIGIN, PRISM_MODEL, PRISM_BASE_URL）。
func applyEnv(c *Config) {
	if v := os.Getenv(EnvPrefix + "ADDR"); v != "" {
		host, port, ok := strings.Cut(v, ":")
		if ok {
			c.Server.Host = host
			if p, err := strconv.Atoi(port); err == nil {
				c.Server.Port = p
			}
		}
	} else if v := os.Getenv("HOST"); v != "" {
		c.Server.Host = strings.TrimSpace(v)
	}

	if v := os.Getenv(EnvPrefix + "PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Server.Port = p
		}
	} else if v := os.Getenv("PORT"); v != "" {
		if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			c.Server.Port = p
		}
	}

	if v := os.Getenv(EnvPrefix + "UPSTREAM"); v != "" {
		c.Upstream.BaseURL = v
	} else if v := os.Getenv("PRISM_BASE_URL"); v != "" {
		c.Upstream.BaseURL = strings.TrimSpace(v)
	}

	if v := os.Getenv(EnvPrefix + "CREDS_FILE"); v != "" {
		c.Creds.File = v
	}
	if v := os.Getenv(EnvPrefix + "CREDS_MODE"); v != "" {
		c.Creds.Mode = v
	}

	if v := os.Getenv(EnvPrefix + "API_KEYS"); v != "" {
		c.Facade.APIKeys = splitCSV(v)
	} else if v := os.Getenv("PROXY_API_KEY"); v != "" {
		c.Facade.APIKeys = splitCSV(v)
	}

	if v := os.Getenv(EnvPrefix + "ADMIN_USER"); v != "" {
		c.Server.AdminUser = strings.TrimSpace(v)
	}
	if v := os.Getenv(EnvPrefix + "ADMIN_PASSWORD"); v != "" {
		c.Server.AdminPassword = v
	}

	if v := os.Getenv(EnvPrefix + "CORS_ORIGIN"); v != "" {
		c.Server.CORSOrigin = strings.TrimSpace(v)
	} else if v := os.Getenv("CORS_ORIGIN"); v != "" {
		c.Server.CORSOrigin = strings.TrimSpace(v)
	}

	if v := os.Getenv(EnvPrefix + "MODEL"); v != "" {
		c.Facade.DefaultModel = strings.TrimSpace(v)
	} else if v := os.Getenv("PRISM_MODEL"); v != "" {
		c.Facade.DefaultModel = strings.TrimSpace(v)
	}

	modelsJSON := os.Getenv(EnvPrefix + "MODELS_JSON")
	if modelsJSON == "" {
		modelsJSON = os.Getenv("PRISM_MODELS_JSON")
	}
	if strings.TrimSpace(modelsJSON) != "" {
		var rawList []json.RawMessage
		if err := json.Unmarshal([]byte(modelsJSON), &rawList); err == nil && len(rawList) > 0 {
			if c.Facade.Models == nil {
				c.Facade.Models = make(map[string]ModelMapping)
			}
			for _, item := range rawList {
				var strItem string
				if err := json.Unmarshal(item, &strItem); err == nil && strings.TrimSpace(strItem) != "" {
					id := strings.TrimSpace(strItem)
					c.Facade.Models[id] = ModelMapping{Model: id, Label: id}
					continue
				}
				var objItem struct {
					ID    string `json:"id"`
					Label string `json:"label"`
				}
				if err := json.Unmarshal(item, &objItem); err == nil && strings.TrimSpace(objItem.ID) != "" {
					id := strings.TrimSpace(objItem.ID)
					label := strings.TrimSpace(objItem.Label)
					if label == "" {
						label = id
					}
					c.Facade.Models[id] = ModelMapping{Model: id, Label: label}
				}
			}
		}
	}

	modelsCSV := os.Getenv(EnvPrefix + "MODELS")
	if modelsCSV == "" {
		modelsCSV = os.Getenv("PRISM_MODELS")
	}
	if strings.TrimSpace(modelsCSV) != "" {
		if c.Facade.Models == nil {
			c.Facade.Models = make(map[string]ModelMapping)
		}
		for _, id := range splitCSV(modelsCSV) {
			id = strings.TrimSpace(id)
			if id != "" {
				c.Facade.Models[id] = ModelMapping{Model: id, Label: id}
			}
		}
	}

	if v := os.Getenv(EnvPrefix + "LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv(EnvPrefix + "CAPTURE"); v != "" {
		c.Capture.Enabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv(EnvPrefix + "HTTP_PROXY"); v != "" {
		c.Upstream.HTTPProxy = v
	}

	// 便捷的单账号注入：整串 Cookie 直接塞这儿就能跑。
	cookieVal := os.Getenv(EnvPrefix + "COOKIE")
	if cookieVal == "" {
		cookieVal = os.Getenv("PRISM_COOKIE")
	}
	if cookieVal != "" {
		c.Creds.Accounts = append([]AccountConfig{{
			ID:      "env-default",
			Name:    "env-default",
			Cookies: cookieVal,
		}}, c.Creds.Accounts...)
	}

	if v := os.Getenv(EnvPrefix + "ACCESS_TOKEN"); v != "" {
		c.Creds.Accounts = append([]AccountConfig{{
			ID:          "env-default",
			Name:        "env-default",
			AccessToken: v,
		}}, c.Creds.Accounts...)
	}
	if v := os.Getenv(EnvPrefix + "REFRESH_TOKEN"); v != "" {
		c.Creds.Accounts = append([]AccountConfig{{
			ID:           "env-default",
			Name:         "env-default",
			RefreshToken: v,
		}}, c.Creds.Accounts...)
	}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ErrNoConfig 在既没有账号也没有透传能力时返回，提示用户补齐凭据。
var ErrNoConfig = errors.New("没有可用凭据：请在 creds.accounts 或 secrets/accounts.json 中提供 cookies/access_token")

// Abs 把相对路径转成基于 baseDir 的绝对路径。
func Abs(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

// ResolveDataPath 解析"数据文件"路径（凭据文件、抓包目录）。
//
// 这里的优先级是刻意设计的，为的是消掉一个极常见的踩坑：
// 用户 cp config.example.yaml configs/config.yaml，里面写的是
// secrets/accounts.json，如果单纯按配置文件目录解析，就会去找
// configs/secrets/accounts.json —— 用户把凭据放在项目根的 secrets/ 下，
// 结果是"文件明明存在却没生效"，非常难排查。
//
// 因此规则是：
//  1. 绝对路径 -> 原样使用；
//  2. 相对配置文件目录存在 -> 用它（配置与数据同目录的部署方式）；
//  3. 相对当前工作目录存在 -> 用它（从项目根启动的常见方式）；
//  4. 都不存在 -> 落在配置文件目录下（首次导入时的写入位置，可预测）。
//
// 返回的第二个值说明命中了哪条规则，便于启动日志里打印清楚。
func ResolveDataPath(baseDir, p string) (string, string) {
	if p == "" {
		return p, "empty"
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), "absolute"
	}

	fromConfig := filepath.Join(baseDir, p)
	if _, err := os.Stat(fromConfig); err == nil {
		return fromConfig, "config-dir"
	}

	if wd, err := os.Getwd(); err == nil {
		fromWD := filepath.Join(wd, p)
		if _, err := os.Stat(fromWD); err == nil {
			return fromWD, "cwd"
		}
	}

	return fromConfig, "config-dir(default)"
}

// ResolveOutputDir 解析"产出目录"路径（抓包目录等）。
//
// 与 ResolveDataPath 的规则不同：输入文件要"哪里存在就用哪里"，
// 而产出目录不存在时，落在当前工作目录下才是符合直觉的
// （用户敲 `oaiprism serve` 的地方），而不是悄悄写进 configs/ 里面。
func ResolveOutputDir(baseDir, p string) string {
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if fi, err := os.Stat(filepath.Join(baseDir, p)); err == nil && fi.IsDir() {
		return filepath.Join(baseDir, p)
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, p)
	}
	return filepath.Join(baseDir, p)
}
