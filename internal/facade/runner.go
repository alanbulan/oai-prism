package facade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/metrics"
	"github.com/oai-prism/oaiprism/internal/prism"
)

// ErrPollTimeout 表示轮询超出了最大时长。
var ErrPollTimeout = errors.New("等待上游生成超时")

// ErrClientGone 表示下游已断开。
var ErrClientGone = errors.New("客户端已断开")

// RunRequest 是一次推理请求的中间表示（与具体对外 API 形态无关）。
type RunRequest struct {
	// Input 是上游要的 input 数组。
	//
	// 由各 API 适配层把 messages / input 翻译成这种条目形态：
	// {type:"message", role:"user"|"assistant", content:[{type,text}]}
	Input []prism.InputItem

	// Model 与 Effort 会写进 metadata，而不是请求体顶层
	// —— 上游的模型参数就在 metadata 里。
	Model  string
	Effort string

	// UserID 是调用方身份（OpenAI 的 user 字段）。
	// 会被写进上游 metadata，用于滥用追踪与配额归属。
	UserID string

	// Metadata 是附加运行上下文，会与 Model/Effort 合并后发送。
	Metadata map[string]any

	// ConversationID 是发给上游的会话 ID（只用经 Server Action 登记的会话，见 native.go）。
	// 原生续接的请求由 runner 自行登记并填写；留空即单次请求，上下文全靠 Input 自带。
	ConversationID string

	// StickyKey 是会话身份，用于账号粘性与项目复用。
	StickyKey string

	// 强制指定（调试用，来自 X-Oaiprism-* 头）。
	AccountID string
	ProjectID string

	// Extra 直通到上游 start 请求体顶层。
	Extra map[string]any

	// ExtraHeaders 是请求级透传头（例如来自客户端的 openai-sentinel-token）。
	ExtraHeaders map[string]string

	// API 是调用来源标识，仅用于指标标签：chat / responses / messages。
	API string

	// Deadline 是本次运行的最长等待时间（0 表示用 facade.max_poll_timeout）。
	// 同步模式会把它收紧到 facade.sync_timeout —— 让一个 HTTP 请求
	// 挂 15 分钟才返回是不可接受的，客户端早就超时了。
	Deadline time.Duration

	// IsAux 标识是否为伴生轻量请求（Codex 标题/摘要生成等），走专用的伴生项目，
	// 不占用、也不污染会话项目。
	IsAux bool

	// Bridge 标识 Codex 工具桥请求：桥指令自带作废上游平台提示词的条款，
	// 不再叠加通用的 platformNotice。
	Bridge bool

	// FileTools：客户端声明了工具，上游沙箱里写的文件会转成它的写文件工具调用（chat.go）。
	// 为假时客户端只看得到回答正文，提示上游把成品直接写在回答里（见 noticeFor）。
	FileTools bool

	// Native 非空时走原生续接（见 native.go）：上游保管会话历史，续接时只发增量。
	// Input 仍是全量折叠后的条目 —— 新建上游会话的首轮与各种回退都发它。
	Native *nativeTurn

	// BoundAccountID 是 ProjectID / ConversationID 所属的账号（来自会话链或原生续接的绑定）。
	// 项目与会话是账号私有资源：实际租到的账号与它不同（粘性过期、原账号冷却、换号重试）时，
	// 它们在新账号上必然 403/404，必须作废（原生续接随之在新账号上新建会话）。
	BoundAccountID string
	// projectFromChain 标记 ProjectID 来自会话链（而不是调用方显式指定）。
	projectFromChain bool
}

// MarkProjectFromChain 标记 ProjectID 来自会话链：作废续接句柄时一并清除。
func (req *RunRequest) MarkProjectFromChain() { req.projectFromChain = true }

// dropContinuation 作废续接句柄（项目仅在来自会话链时清除）。
func (req *RunRequest) dropContinuation() {
	if req.projectFromChain {
		req.ProjectID = ""
		req.projectFromChain = false
	}
	req.ConversationID = ""
	req.BoundAccountID = ""
}

// Delta 是一次增量。
type Delta struct {
	Text      string
	Reasoning string
	Reset     bool
}

// RunResult 是一次运行的最终结果。
type RunResult struct {
	Text      string
	Reasoning string

	// RequestID 是上游 start 受理句柄（request_id，仅用于轮询/停止）。
	//
	// 注意：它**不能**作为下一次的 PreviousResponseID —— 逆向早期以为可以，
	// 实测（2026-10-02 抓包）上游会静默忽略不存在的 response，表现为多轮无上下文。
	// 真正的续接键是终态 payload.id（ResponseID 字段，形如 resp_muqwesyc_7xf7qrm8）。
	RequestID string
	// ResponseID 是终态 response.payload.id，才是多轮延续的 PreviousResponseID。
	ResponseID     string
	ConversationID string

	// DeltaFiles 是沙箱中生成的文件增删改查变更集
	DeltaFiles []prism.CodexDeltaFile
	// OutputItems 是响应条目集合
	OutputItems []prism.CodexOutputItem

	// ListenSnapshot 是沙箱内 codex 会话状态指针（codex_session_id / transcript_cursor 等），
	// 下一轮 start 必须原样回传 —— 这是多轮上下文续接和复用沙箱会话的关键钥匙。
	ListenSnapshot json.RawMessage

	AccountID  string
	ProjectID  string
	Usage      *prism.Usage
	Polls      int
	PollWait   time.Duration
	FirstDelta time.Duration
	Started    time.Time
}

// Runner 把"start + 轮询"的上游协议编排成一次可流式消费的运行。
//
// 上游协议实况（来自真实报文 + 前端源码，详见 prism.parseEnvelope 的注释）：
//
//	start  -> {status:"started", request_id, turn_state}
//	status -> {status:"pending", turn_state}            反复，turn_state 每次换新
//	        -> {status:"completed", response:{...}}     终态
//
// 两个反直觉的地方，各自是一个坑：
//  1. 失败是 HTTP 200 + response.status:"error"，只看状态码会误判成成功；
//  2. turn_state 是不透明令牌，必须原样回传，不能自己构造。
type Runner struct {
	cfg      *config.Config
	log      *slog.Logger
	pool     *account.Pool
	client   *prism.Client
	projects *projectCache
	// sandboxes 按账号缓存沙箱。沙箱令牌不绑定项目，按账号缓存即可，
	// 省掉每次请求都去 POST /api/backend/1/new 的往返与冷启动。
	sandboxes *sandboxCache
	// uploads 记录已上传到项目的图片，避免每轮把历史里的图片重传一遍。
	uploads *uploadCache
	journal *PendingJournal
	app     *metrics.App
	// nativeStore 让原生续接的绑定落盘（见 native_store.go）；nil 时只在内存里。
	nativeStore NativeStore
	// catalog 是上游在售模型清单（见 catalog.go）。
	catalog *ModelCatalog

	bucketSeq atomic.Uint64

	accountRetries int
}

// NewRunner 构造运行器。
func NewRunner(cfg *config.Config, log *slog.Logger, pool *account.Pool, client *prism.Client, app *metrics.App) *Runner {
	r := &Runner{
		cfg:            cfg,
		log:            log,
		pool:           pool,
		client:         client,
		projects:       newProjectCache(cfg.Facade.ProjectTTL, cfg.Facade.ProjectPoolSize),
		sandboxes:      newSandboxCache(cfg.Facade.SandboxTTL),
		uploads:        newUploadCache(cfg.Facade.ProjectTTL),
		journal:        NewPendingJournal(),
		app:            app,
		accountRetries: 2,
	}
	r.catalog = newModelCatalog(cfg, log, r.fetchCatalog)
	go r.projects.gc(context.Background())
	go r.sandboxes.gc(context.Background())
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			r.journal.Cleanup(1 * time.Hour)
		}
	}()
	return r
}

// ProjectCacheSize 供指标使用。
func (r *Runner) ProjectCacheSize() int { return r.projects.Size() }

// Run 执行一次推理。
//
// emit 为 nil 表示同步模式：不回调，只返回最终结果。
// emit 返回 error 会立即中止流程（用于下游断连）。
// 失败时结果也可能非 nil（带着实际使用的账号），调用方须先判 error。
func (r *Runner) Run(ctx context.Context, req *RunRequest, emit func(Delta) error) (*RunResult, error) {
	started := time.Now()
	api := req.API
	if api == "" {
		api = "unknown"
	}

	// 超过上游单条上限的请求发出去必败，还会被当成沙箱未就绪反复重试：
	// 占用账号、建项目、申请沙箱之前就拦下（见 context_limit.go）。
	if err := r.checkPromptSize(req); err != nil {
		r.app.FacadeRuns.Inc(api, req.Model, "too_large")
		r.log.Warn("提示词超过上游单条上限，未发送", "api", api, "err", err)
		return nil, err
	}

	var (
		lastErr error
		// lastRes 是最后一次尝试的结果：失败时也要交还调用方，
		// 请求流水才知道这次失败落在哪个账号上（否则只能显示"未分配"）。
		lastRes *RunResult
		emitted bool
	)

	for attempt := 0; attempt < r.accountRetries; attempt++ {
		lease, err := r.acquire(ctx, req)
		if err != nil {
			r.app.FacadeRuns.Inc(api, req.Model, "no_account")
			return nil, err
		}

		// 同步模式收紧超时：不能让普通 HTTP 请求挂满 max_poll_timeout。
		if emit == nil && req.Deadline == 0 && r.cfg.Facade.SyncTimeout > 0 {
			req.Deadline = r.cfg.Facade.SyncTimeout
		}

		res, err := r.runOnce(ctx, lease.Account, req, emit)
		lease.Release()

		if nt := req.Native; nt != nil {
			if err == nil {
				nt.commit(res, r)
			} else {
				// 续接的上游会话已不可用（被删、超出上游会话上限等）：作废绑定，
				// 还没吐出内容就用新会话 + 全量上下文原地重来一次。
				gone := nt.plan != nil && nt.plan.continued && isConversationGone(err)
				nt.release(gone, r)
				if gone && (res == nil || res.Text == "") && ctx.Err() == nil && attempt+1 < r.accountRetries {
					r.log.Warn("原生续接：上游会话不可用，改用新会话全量重试", "key", nt.key, "err", err)
					lastErr, lastRes = err, res
					continue
				}
			}
		}

		if err == nil {
			r.app.FacadeRuns.Inc(api, req.Model, "ok")
			r.app.FacadeLatency.Observe(time.Since(started).Seconds(), api)
			if res.Usage != nil {
				r.app.Tokens.With(api, "input").Add(int64(res.Usage.InputTokens))
				r.app.Tokens.With(api, "output").Add(int64(res.Usage.OutputTokens))
			}
			return res, err
		}

		lastErr = err
		if res != nil {
			lastRes = res
		}

		// 客户端主动断开：不重试，取消已经传给上游。
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			r.app.ClientAborts.Inc()
			r.app.FacadeRuns.Inc(api, req.Model, "aborted")
			return res, err
		}

		// 超限是请求本身的问题，不记到账号头上。
		if errors.Is(err, ErrContextTooLarge) {
			r.app.FacadeRuns.Inc(api, req.Model, "too_large")
			return res, err
		}
		r.pool.MarkResult(lease.Account, err, retryAfter(err))

		// 已经吐过内容就不能换号重试，否则客户端会收到两段拼接的回答。
		if res != nil && res.Text != "" {
			emitted = true
		}
		if emitted {
			r.app.FacadeRuns.Inc(api, req.Model, "partial_error")
			return res, err
		}

		if !isAccountLevel(err) {
			break
		}
		r.app.UpstreamRetries.Inc("facade")
		r.log.Warn("换号重试", "attempt", attempt+1, "account", lease.Account.ID, "err", err)
	}

	status := "error"
	if errors.Is(lastErr, ErrPollTimeout) {
		status = "timeout"
	}
	r.app.FacadeRuns.Inc(api, req.Model, status)
	r.app.FacadeLatency.Observe(time.Since(started).Seconds(), api)
	return lastRes, lastErr
}

// acquire 选账号。
func (r *Runner) acquire(ctx context.Context, req *RunRequest) (*account.Lease, error) {
	if req.AccountID != "" {
		if a := r.pool.Get(req.AccountID); a != nil {
			if a.Acquire(time.Now()) {
				r.app.AccountPick.Inc("pinned")
				return &account.Lease{Account: a}, nil
			}
			r.app.AccountPick.Inc("pinned_busy")
		}
	}
	lease, err := r.pool.Acquire(ctx, req.StickyKey)
	if err != nil {
		r.app.AccountPick.Inc("empty")
		// 把凭据文件位置写进错误里：这是首次部署最常见的问题，
		// 让用户在一次响应里就知道该去改哪个文件，而不是翻日志。
		return nil, fmt.Errorf("无可用账号: %w（请检查凭据文件 %s，"+
			"可用 `oaiprism import` 导入 Cookie 或 access_token）",
			err, r.cfg.Creds.File)
	}
	r.app.AccountPick.Inc("ok")
	return lease, nil
}

// runOnce 在指定账号上跑一次完整流程。
func (r *Runner) runOnce(ctx context.Context, acct *account.Account, req *RunRequest, emit func(Delta) error) (*RunResult, error) {
	started := time.Now()

	cred := acct.Credential()
	if cred == nil || !cred.Usable() {
		return nil, &creds.APIError{Op: "acquire", Status: 401, Body: "账号凭据不可用"}
	}
	extraHeaders := make(map[string]string, len(cred.Headers)+len(req.ExtraHeaders))
	for k, v := range cred.Headers {
		extraHeaders[k] = v
	}
	for k, v := range req.ExtraHeaders {
		extraHeaders[k] = v
	}
	p := prism.Principal{Client: acct.Client, Cred: cred, ExtraHeaders: extraHeaders, AccountID: acct.ID}

	result := &RunResult{AccountID: acct.ID, Started: started}

	// 项目 / 会话属于别的账号（粘性过期 / 原账号冷却后改绑 / 换号重试）：
	// 在本账号上它们必然 403/404，整体作废。
	if req.BoundAccountID != "" && req.BoundAccountID != acct.ID {
		r.log.Info("续接句柄属于其他账号，已作废",
			"bound", req.BoundAccountID, "account", acct.ID, "stickyKey", req.StickyKey)
		req.dropContinuation()
	}

	// 1) 项目：会话内复用，建工程比推理本身还慢。
	projectID := req.ProjectID
	if projectID == "" {
		id, err := r.resolveProject(ctx, acct, req)
		if err != nil {
			// 上下文取消是**终态**，不是"可降级继续"的故障。
			// 如果把它混进下面的降级分支，我们就会拿着一个已取消的 ctx
			// 继续发起 start 请求 —— 白跑一趟，还会在日志里留下误导性的
			// "建项目失败但已降级"，真正的断开原因反而被淹没了。
			if cerr := ctx.Err(); cerr != nil {
				return result, cerr
			}
			// 其余故障（例如上游维护窗口返回 503）才降级：此时推理端点
			// 未必同样不可用，至少让 start 有机会走通。
			r.log.Warn("建项目失败，改为无项目上下文继续", "account", acct.ID, "err", err)
			r.app.ProjectOps.Inc("create", "degraded")
		} else {
			projectID = id
		}
	}
	result.ProjectID = projectID

	// 调用方身份：真实 Web 每轮都带 metadata.userId
	//（user-Wx7p... 形态，来自 access_token JWT 的 chatgpt_user_id claim，
	// playwright 抓包实证）。字段名由 buildStartPayload 的 schema 处理。
	// 用局部变量：写回 req 会让换号重试时沿用上一个账号的 userId。
	userID := req.UserID
	if userID == "" {
		userID = cred.UserID
	}

	// 2) 沙箱：Prism 的 AI 跑在沙箱容器里，start 必须告诉它用哪个沙箱。
	//
	// 漏掉这一步的表现极具误导性：start 会立刻返回
	// status:"completed" + response.status:"error"、reason="sandbox_reconnecting"，
	// 看起来像"上游挂了"，实际上是"你没给我沙箱"。
	// 上游的 codexRequestDebug 里会直接写 sandbox_url_resolved: null。
	var sb *prism.Sandbox
	if s := r.sandboxes.Get(acct.ID); s.Usable() {
		sb = s
	} else {
		s, err := r.ensureSandbox(ctx, acct, projectID)
		if err != nil {
			r.log.Warn("申请沙箱失败，尝试不带沙箱继续", "account", acct.ID, "err", err)
			r.app.SandboxOps.Inc("acquire", "error")
		} else {
			sb = s
		}
	}

	// 2.2) 规整成上游真正会读的形状：唯一一条 system + 最后一条 user（见 upstream_input.go）。
	// 先于图片上传 —— 被上游丢弃的中间条目里的图片没必要上传；用量也只按这份计。
	inputItems := r.upstreamPromptItems(req)
	convIDOut := req.ConversationID
	if req.Native != nil {
		// 原生续接：会话 ID 由我们登记，续接时只发增量。
		inputItems = r.planNative(ctx, p, acct.ID, projectID, req, inputItems)
		convIDOut = req.Native.plan.cid
		if len(req.Native.plan.seeds) > 0 {
			if err := r.seedConversation(ctx, acct, req, projectID); err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return result, cerr
				}
				r.log.Warn("原生续接：历史补种失败，本轮改发全量（裁剪后）", "cid", convIDOut, "err", err)
				inputItems = req.Native.plan.fallback
				if req.Native.plan.continued {
					req.Native.plan.sysHash = 0 // 单独发的 system 没送到：下一轮重发
				}
			}
		}
		// 回退路径（会话正忙、建会话失败、补种失败）发的是全量：放不下就别发 ——
		// 上游只会回 too large，而且常被当成沙箱未就绪反复重试。
		if limit := r.cfg.Facade.PromptByteLimit(); limit > 0 {
			if n := promptBytes(inputItems); n > limit {
				return result, &contextTooLargeError{Bytes: n, Limit: limit, Tokens: countInputTokens(inputItems)}
			}
		}
	}

	// 2.3) 处理图片上传：必须在沙箱工作区同步之前上传至项目！
	var hasNewUpload bool
	if projectID != "" {
		inputItems, hasNewUpload = r.attachImages(ctx, p, acct.ID, projectID, inputItems)
	}
	// 用量按真正发往上游的条目计（图片预处理之后），与上游生成并行计数
	inputTokens := countInputAsync(inputItems)
	if req.Native != nil && req.Native.plan.continued {
		// 增量只是上游会话的一小段：模型每轮读的是整段会话，用量按完整上下文计
		// （Codex 也据此判断窗口占用、决定何时压缩）。
		inputTokens = countInputAsync(req.Native.conv.logicalItems())
	}

	// 2.5) 工作区同步。沙箱只把它第一次同步的那个项目、当时已登记的文件落进工作区 ——
	// 之后登记的文件、再同步的别的项目的文件都不会出现（2026-10-04 实测：图片登记后重新
	// 同步，模型 view_image 仍是"文件不存在"；换新沙箱立刻能看到，同一会话也行）。
	// 所以本项目有图片登记在当前沙箱落盘之后（或沙箱落的是别的项目），就换一个新沙箱。
	if hasNewUpload {
		r.sandboxes.InvalidateProject(acct.ID, projectID)
	}
	if at, ok := r.uploads.LastUpload(projectID); ok && sb.Usable() && !r.sandboxes.HoldsFiles(acct.ID, projectID, at) {
		r.sandboxes.InvalidateIf(acct.ID, sb)
		if fresh, err := r.ensureSandbox(ctx, acct, projectID); err == nil && fresh.Usable() {
			r.log.Info("项目有新登记的附件，换新沙箱以便它落进工作区", "account", acct.ID, "project", projectID)
			r.app.SandboxOps.Inc("acquire", "attachments")
			sb = fresh
		} else {
			r.log.Warn("为附件换新沙箱失败，沿用旧沙箱（模型可能看不到附件）", "account", acct.ID, "err", err)
		}
	}
	if sb.Usable() && projectID != "" {
		var outcome syncOutcome
		sb, outcome = r.syncOrRebuild(ctx, acct, sb, projectID)
		if outcome != syncReady {
			// 同步未就绪还硬上 start，上游**必然**回
			// "Project file synchronization timed out"（122 秒后 504）——
			// 用户白等两分钟，看到的还是一个伪装成流断的错误。
			// 实测（2026-10-01）：跳过同步的 start 100% 走这条路。
			//
			// 保留可用沙箱容器，仅失效本项目的工作区同步状态，避免容器被无故销毁后重新申请冷启动。
			r.sandboxes.InvalidateProject(acct.ID, projectID)
			r.log.Warn("沙箱工作区同步未就绪，重置项目同步并快速失败（避免上游 122s 超时）",
				"account", acct.ID, "project", projectID)
			r.app.SandboxOps.Inc("sync", "reset")
			return result, fmt.Errorf("沙箱工作区同步未就绪（上游沙箱异常），请重试")
		}
	}

	// 3) 组装 metadata：模型参数与运行上下文都在这里，不在请求体顶层。
	meta := make(map[string]any, 8)
	for k, v := range req.Metadata {
		meta[k] = v
	}
	if projectID != "" {
		meta["projectId"] = projectID
	}
	// 续接只认登记过的会话 ID：不混用客户端 metadata 里带来的沙箱快照。
	delete(meta, "codex_listen_snapshot")
	// frontend_origin 是上游判断"请求来自哪个前端"的依据。
	// 缺了它请求会看起来像脚本 —— 这是风控最容易抓的点之一。
	if _, ok := meta["frontend_origin"]; !ok && r.cfg.Upstream.Origin != "" {
		meta["frontend_origin"] = r.cfg.Upstream.Origin
	}
	if sb.Usable() {
		// 字段名是 snake_case —— 与前端 bundle 里一致，别"顺手改成驼峰"。
		meta["sandbox_url"] = sb.URL
		meta["sandbox_token"] = sb.Token
	}
	// 注意：此处不做任何轮次标记/扰动。2026-10-02 实验矩阵证明
	// system comment 与零宽空格两类标记本身就会让历史到达失败
	//（带 marker 的 W/Y/N 系列全败，无 marker 的 B/B2/K 全胜）。
	// 多轮上下文 = 每轮新 project（reuse_project: false）+ 全量历史，
	// 不再叠加任何变换。

	var (
		startResp *prism.StartResponse
		err       error
	)
	// 沙箱冷启动时上游会回 504 文案并提示 "Please submit prompt again"，
	// 这是上游自己建议的处理方式 —— 照做即可，不要当成协议错误。
	r.log.Info("发给上游的请求参数", "convID", convIDOut, "itemsCount", len(inputItems))
	for attempt := 1; attempt <= sandboxStartRetries; attempt++ {
		startResp, err = r.client.StartResponse(ctx, p, &prism.StartRequest{
			Input:           inputItems,
			ConversationID:  convIDOut,
			Metadata:        meta,
			Model:           req.Model,
			ReasoningEffort: req.Effort,
			UserID:          userID,
			Extra:           req.Extra,
		})
		if err != nil {
			if isSentinelThrottle(err) && attempt < sandboxStartRetries {
				r.log.Info("start 遭遇 Sentinel 风控抖动，稍后重试", "attempt", attempt, "err", err)
				if serr := sleepCtx(ctx, 1500*time.Millisecond); serr != nil {
					return result, serr
				}
				continue
			}
			r.log.Error("start 请求上游失败", "attempt", attempt, "err", err, "convID", convIDOut)
			r.app.ConversationOps.Inc("start", "error")
			return result, err
		}
		if !isSandboxNotReady(startResp) {
			break
		}
		// 下架的模型上游也只回这句 400：第一次碰到时核对一下在售清单，已下架就换默认模型再发，
		// 不必按沙箱未就绪白等五分钟（2026-10-06 gpt-6.1-sol 下架）。
		if attempt == 1 && isProcessingError(startResp) {
			if m, e, ok := r.catalog.replacement(ctx, &r.cfg.Facade, req.Model, req.Effort); ok {
				r.log.Warn("上游已不再提供该模型，改用在售清单里的默认模型", "model", req.Model, "now", m, "effort", e)
				req.Model, req.Effort = m, e
				continue
			}
		}
		r.app.SandboxOps.Inc("start", "not_ready")
		r.log.Info("沙箱未就绪，稍后重试",
			"attempt", attempt, "of", sandboxStartRetries, "reason", sandboxReason(startResp))
		// 关键保护：仅在主请求且确实持有沙箱容器时才执行失效，绝不让伴生轻量请求误杀主会话的沙箱缓存！
		// 注意：冷启动 504（"submit prompt again" 或 "gateway timeout"）时容器正在预热，
		// 绝不能销毁容器重新申请，否则会导致每轮重试都创建新容器并反复冷启动
		//（94d5511 修过一次：阈值 2 会在冷启动期间反复重建容器，必须保持 5）。
		// 仅在明确断连（sandbox_reconnecting / unable to confirm / disconnected）或在同一容器上连续重试 5 次以上才失效容器。
		reasonText := strings.ToLower(sandboxReason(startResp))
		if !req.IsAux && sb.Usable() && projectID != "" {
			if strings.Contains(reasonText, "reconnecting") ||
				strings.Contains(reasonText, "unable to confirm") ||
				strings.Contains(reasonText, "disconnected") ||
				attempt >= 5 {
				// 只失效"我们正在用的那一个"：沙箱按账号共享，若别的会话已经
				// 换上了新容器，不能把它也一起踢掉。
				r.sandboxes.InvalidateIf(acct.ID, sb)
				sb = nil // 必须置空本地指针，触发下方重新申请与装配崭新沙箱容器
			} else {
				// 普通冷启动或工作区未对齐，保留沙箱容器实例，仅重置项目工作区对齐标记
				r.sandboxes.InvalidateProject(acct.ID, projectID)
			}
		}
		if attempt == sandboxStartRetries {
			break // 最后一次仍未就绪：直接报错，不再白等一轮退避和重新装配沙箱
		}
		// 线性退避：上游限流窗口是分钟级，固定短间隔只会打在限流上。
		if serr := sleepCtx(ctx, time.Duration(attempt)*sandboxRetryDelay); serr != nil {
			return result, serr
		}

		// 若之前未装配沙箱（或已被回收），在下一轮 start 前重新补齐沙箱与工作区同步装配进 meta
		if sb == nil || !sb.Usable() {
			if newSb, serr := r.ensureSandbox(ctx, acct, projectID); serr == nil && newSb.Usable() {
				sb = newSb
				meta["sandbox_url"] = sb.URL
				meta["sandbox_token"] = sb.Token
			}
		}
		if sb.Usable() && projectID != "" && !r.sandboxes.Synced(acct.ID, projectID) {
			sb, _ = r.syncOrRebuild(ctx, acct, sb, projectID)
			if sb.Usable() {
				meta["sandbox_url"] = sb.URL
				meta["sandbox_token"] = sb.Token
			}
		}
	}
	r.app.ConversationOps.Inc("start", "ok")

	var (
		prev      string
		firstAt   time.Time
		requestID = startResp.RequestID
		convID    = startResp.ConversationID
		turnState = startResp.TurnState
	)

	result.RequestID = requestID
	result.ConversationID = convID
	if len(startResp.ListenSnapshot) > 0 {
		result.ListenSnapshot = startResp.ListenSnapshot
	}

	if requestID != "" {
		r.journal.RecordStart(requestID, convID, acct.ID, projectID, turnState)
	}
	if len(turnState) > 0 {
		var tsMap map[string]any
		if err := json.Unmarshal(turnState, &tsMap); err == nil {
			if promptStr, ok := tsMap["prompt"].(string); ok && promptStr != "" {
				r.log.Debug("上游组装 Prompt", "bytes", len(promptStr), "head", truncateRunes(promptStr, 500))
			}
		}
	}

	bumpFirstByte := func() {
		if !firstAt.IsZero() {
			return
		}
		firstAt = time.Now()
		result.FirstDelta = firstAt.Sub(started)
		r.app.FacadeFirstByte.Observe(result.FirstDelta.Seconds(), req.API)
	}

	// 上游每次给的是到目前为止的完整思考摘要：只发新增的部分，且不跟正文增量绑在一起
	// （曾经只随正文增量发、每次发全文：只调工具没有正文的一轮丢了思考，正文分多次到时思考重复）。
	var sentReasoning string
	nextReasoning := func(full string) string {
		if full == "" || full == sentReasoning {
			return ""
		}
		d := full
		if strings.HasPrefix(full, sentReasoning) {
			d = full[len(sentReasoning):]
		}
		sentReasoning = full
		return d
	}
	// 进行中的转录（思考摘要、工具调用）与终态思考合成的全文，见 livetrace.go
	var trace liveTrace

	// 3) start 有可能直接就是终态（回答很短，或者立刻失败了）。
	if st := startResp.Initial; st != nil {
		if st.Fail {
			r.app.ConversationOps.Inc("start", "failed")
			err := r.upstreamFailure(st, inputItems, inputTokens)
			r.log.Error("start 初始状态返回失败", "err", err, "reason", st.ErrorReason, "convID", convID)
			r.journal.MarkTerminal(requestID, "failed", "", err)
			return result, err
		}
		if st.Text != "" {
			prev = st.Text
			result.Text = st.Text
		}
		trace.observe(st.Progress)
		reasoning := trace.full(st.Reasoning, st.ReasoningParts)
		if rd := nextReasoning(reasoning); (st.Delta != "" || rd != "") && emit != nil {
			if eerr := emit(Delta{Text: st.Delta, Reasoning: rd, Reset: st.Reset}); eerr != nil {
				return result, eerr
			}
			bumpFirstByte()
			r.app.SSEDeltas.Inc(req.API)
		}
		if st.Usage != nil {
			result.Usage = st.Usage
		}
		if reasoning != "" {
			result.Reasoning = reasoning
		}
		if st.ResponseID != "" {
			result.ResponseID = st.ResponseID
		}
		if st.ConversationID != "" {
			result.ConversationID = st.ConversationID
		}
		if len(st.DeltaFiles) > 0 {
			result.DeltaFiles = st.DeltaFiles
		}
		if len(st.OutputItems) > 0 {
			result.OutputItems = st.OutputItems
		}
		if st.Done {
			r.journal.MarkTerminal(requestID, "completed", result.Text, nil)
			// 上游轮询响应从不回 usage（顶层与 payload 均无此键，抓包实证），
			// 按本轮实际收发内容精确计数（见 usage.go）。
			if result.Usage == nil {
				result.Usage = measuredUsage(inputTokens(), result)
			}
			if result.ProjectID != "" && result.ConversationID != "" {
				r.projects.Put(acct.ID, "cid:"+result.ConversationID, result.ProjectID, time.Now())
			}
			return result, nil
		}
		if len(st.TurnState) > 0 {
			turnState = st.TurnState
			r.journal.UpdateState(requestID, turnState, "pending")
		}
	}

	if requestID == "" {
		return result, fmt.Errorf(
			"start 响应中未解析出 request_id（请用 capture 模式核对报文；当前候选字段: %v）",
			r.client.Schema().RespIDKeys)
	}

	// 4) 轮询直到终态。
	//
	// 两道时限：max_poll_timeout 是"上游毫无进展"的最长等待，转录游标前进、来了思考或正文
	// 都会把它顺延；max_run_timeout 是一轮生成的总时长上限（同步请求再受 req.Deadline 约束）。
	// 上游模型以智能体方式在沙箱里写文件时，一轮跑二三十分钟是常态（2026-10-05：一张
	// 多约束 SVG 插画在 20 分钟固定时限处被切断，上游其实一直在推进 —— 转录里有思考摘要和
	// 一次次 apply_patch，只是正文要到终态才给）。
	f := &r.cfg.Facade
	idle, hard := f.MaxPollTimeout, f.MaxRunTimeout
	if hard < idle {
		hard = idle
	}
	if req.Deadline > 0 && req.Deadline < hard {
		hard = req.Deadline
	}
	if idle > hard {
		idle = hard
	}
	pollStart := time.Now()
	lastProgress := pollStart
	interval := f.PollInterval
	failures := 0

	for {
		if cerr := ctx.Err(); cerr != nil {
			// 客户端断了：通知上游停止，别让这次生成白跑完还吃掉额度。
			r.stopUpstream(p, requestID, convID, turnState)
			return result, cerr
		}
		if now := time.Now(); now.Sub(lastProgress) > idle || now.Sub(pollStart) > hard {
			r.app.PollRounds.Inc("timeout")
			r.stopUpstream(p, requestID, convID, turnState)
			why := fmt.Sprintf("上游 %s 没有任何进展", idle)
			if now.Sub(pollStart) > hard {
				why = fmt.Sprintf("超过单轮生成上限 %s", hard)
			}
			return result, fmt.Errorf("%w：%s（已轮询 %d 次，已收正文 %d 字节）",
				ErrPollTimeout, why, result.Polls, len(result.Text))
		}

		t0 := time.Now()
		st, err := r.client.PollResponse(ctx, p, &prism.StatusRequest{
			RequestID: requestID,
			TurnState: turnState,
			WaitMs:    pollWaitMs(f),
		}, prev)
		elapsed := time.Since(t0)

		if err != nil {
			r.app.PollRounds.Inc("error")
			if isSentinelThrottle(err) {
				failures++
				r.log.Info("轮询遭遇 Sentinel 风控抖动，原地退避重试", "failures", failures, "err", err)
				if failures >= 10 {
					return result, fmt.Errorf("轮询连续遭遇风控失败 %d 次: %w", failures, err)
				}
				if serr := sleepCtx(ctx, 1500*time.Millisecond); serr != nil {
					return result, serr
				}
				continue
			}
			if isAccountLevel(err) {
				return result, err
			}
			failures++
			if failures >= 6 {
				return result, fmt.Errorf("轮询连续失败 %d 次: %w", failures, err)
			}
			if serr := sleepCtx(ctx, interval); serr != nil {
				return result, serr
			}
			interval = nextInterval(interval, f.PollBackoffMax)
			continue
		}
		failures = 0

		result.Polls++
		result.PollWait += elapsed
		r.app.PollRounds.Inc("ok")
		if result.Polls%10 == 0 {
			r.log.Info("模型深度思考推理中...",
				"polls", result.Polls,
				"elapsed", time.Since(started).Round(time.Second).String(),
				"model", req.Model,
				"transcript", trace.cursor,
				"idle", time.Since(lastProgress).Round(time.Second).String(),
			)
		}

		if st.RequestID != "" {
			requestID = st.RequestID
			result.RequestID = requestID
		}
		if st.ResponseID != "" {
			result.ResponseID = st.ResponseID
		}
		if st.ConversationID != "" {
			convID = st.ConversationID
			result.ConversationID = convID
		}
		// turn_state 是续令牌：每轮都必须换成最新的那个。
		// 忘了更新就会一直拿到同一个 pending，表现为"永远不结束"。
		if len(st.TurnState) > 0 {
			turnState = st.TurnState
			r.journal.UpdateState(requestID, turnState, "pending")
		}
		if st.Usage != nil {
			result.Usage = st.Usage
		}
		advanced := trace.observe(st.Progress)
		reasoning := trace.full(st.Reasoning, st.ReasoningParts)
		if reasoning != "" {
			result.Reasoning = reasoning
		}
		if len(st.DeltaFiles) > 0 {
			result.DeltaFiles = st.DeltaFiles
		}
		if len(st.OutputItems) > 0 {
			result.OutputItems = st.OutputItems
		}
		if len(st.ListenSnapshot) > 0 {
			result.ListenSnapshot = st.ListenSnapshot
		}

		rd := nextReasoning(reasoning)
		if advanced || st.Delta != "" || rd != "" {
			lastProgress = time.Now()
		}
		if (st.Delta != "" || rd != "") && emit != nil {
			if eerr := emit(Delta{Text: st.Delta, Reasoning: rd, Reset: st.Reset}); eerr != nil {
				r.stopUpstream(p, requestID, convID, turnState)
				return result, eerr
			}
			bumpFirstByte()
			r.app.SSEDeltas.Inc(req.API)
		}
		if st.Delta != "" {
			result.Text = st.Text
			if result.Text == "" {
				result.Text = prev + st.Delta
			}
			prev = result.Text
			// 有进展就把退避重置回基线。
			interval = f.PollInterval
		} else {
			if st.Text != "" {
				prev = st.Text
				result.Text = st.Text
			}
			r.app.PollEmpty.Inc("status")
			interval = nextInterval(interval, f.PollBackoffMax)
		}

		// 统一的轮询节流。
		//
		// 这里必须是"无条件节流"，不能只在空轮询时睡：
		// 如果上游每轮都返回累计正文（也就每轮都有增量），
		// "有增量就立刻再问一次"会让循环退化成零间隔的忙轮询 ——
		// 4 次轮询 20 毫秒跑完，等于在对上游做拒绝服务。
		// （这个 bug 是被"客户端断开后应通知上游停止"的测试顺带抓出来的。）
		//
		// 规则：本轮耗时已经 >= 目标间隔，说明服务端本身就在等
		// （长轮询语义），直接进下一轮；否则补足差值。
		if elapsed < interval {
			if serr := sleepCtx(ctx, interval-elapsed); serr != nil {
				r.stopUpstream(p, requestID, convID, turnState)
				return result, serr
			}
		}

		if st.Done {
			if result.Text == "" {
				result.Text = prev
			}
			r.app.PollRounds.Inc("done")
			// 关键：失败也算 Done。上游用 HTTP 200 + response.status=error
			// 表达失败，漏判就会返回一个"成功的空回答"。
			if st.Fail {
				err := r.upstreamFailure(st, inputItems, inputTokens)
				r.journal.MarkTerminal(requestID, "failed", "", err)
				return result, err
			}
			r.journal.MarkTerminal(requestID, "completed", result.Text, nil)
			// 用量计算与上方 start 直达完成路径同款。
			if result.Usage == nil {
				result.Usage = measuredUsage(inputTokens(), result)
			}
			if result.ProjectID != "" && result.ConversationID != "" {
				r.projects.Put(acct.ID, "cid:"+result.ConversationID, result.ProjectID, time.Now())
			}
			return result, nil
		}
	}
}

// stopUpstream 通知上游停止生成。
//
// 幂等地"尽力而为"：失败只记 debug，不影响主流程返回。
// 目的很实际 —— 客户端断开后上游那条生成会继续跑到结束并扣额度，
// 对"额度即成本"的代理来说这是最直接的省钱手段。
func (r *Runner) stopUpstream(p prism.Principal, requestID, conversationID string, turnState []byte) {
	if requestID == "" {
		return
	}
	// 用独立的短超时 context：主 context 已经取消了，不能再拿它去发请求。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.client.StopResponse(ctx, p, requestID, conversationID, turnState); err != nil {
		r.log.Debug("通知上游停止失败（忽略）", "request_id", requestID, "err", err)
		return
	}
	r.log.Debug("已通知上游停止生成", "request_id", requestID)
}

const (
	// sandboxStartRetries 是"沙箱未就绪"时的 start 重试次数。
	//
	// 3 -> 10（2026-09-17）：Codex CLI 在流断后会自动重连约 12 分钟
	// （5 次 × 2m23s），此前服务端只重试 15 秒就放弃 —— 上游限流
	// 窗口是分钟级，两边窗口严重错配：CLI 还在等，服务端已经 502 了。
	// 10 次 × 退避（见 sandboxRetryDelay）约 8-9 分钟，基本覆盖
	// CLI 的重连窗口。
	sandboxStartRetries = 10
	// sandboxRetryDelay 是重试间隔。太短没意义（容器起不来），太长浪费。
	// 线性递增：5s, 10s, ... 50s —— 上游限流窗口是分钟级，固定短间隔
	// 只会白白打在限流上。
	sandboxRetryDelay = 5 * time.Second
)

// ensureSandbox 取（或申请）一个沙箱。
func (r *Runner) ensureSandbox(ctx context.Context, acct *account.Account, projectID string) (*prism.Sandbox, error) {
	if !r.cfg.Facade.UseSandbox {
		return nil, nil
	}
	if sb := r.sandboxes.Get(acct.ID); sb.Usable() {
		r.app.SandboxOps.Inc("acquire", "hit")
		return sb, nil
	}

	// 同一个账号只允许一个在飞的申请，避免并发请求各申请一个沙箱。
	unlock := r.sandboxes.Lock(acct.ID)
	defer unlock()
	if sb := r.sandboxes.Get(acct.ID); sb.Usable() {
		r.app.SandboxOps.Inc("acquire", "hit")
		return sb, nil
	}

	cred := acct.Credential()
	p := prism.Principal{Client: acct.Client, Cred: cred, ExtraHeaders: cred.Headers, AccountID: acct.ID}

	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt-1) * 1500 * time.Millisecond):
			}
			r.log.Info("重试申请沙箱", "account", acct.ID, "attempt", attempt)
		}
		sb, err := r.client.AcquireSandbox(ctx, p)
		if err == nil {
			r.log.Info("已申请沙箱", "account", acct.ID, "url", sb.URL)
			r.sandboxes.Put(acct.ID, sb)
			r.app.SandboxOps.Inc("acquire", "ok")
			return sb, nil
		}
		lastErr = err
		if !isSentinelThrottle(err) {
			break
		}
	}
	return nil, lastErr
}

// isSentinelThrottle 判断是否为上游 Sentinel 风控的偶发拒绝：
// HTTP 403 + "Request verification failed"（"Please try again"）。
// 实测重试即恢复，与凭据失效无关 —— 凭据失效是 401，走另一条处理路径。
func isSentinelThrottle(err error) bool {
	var apiErr *creds.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == 403 && strings.Contains(apiErr.Body, "verification")
}

// syncOutcome 是一次工作区同步的结果。
type syncOutcome int

const (
	syncReady       syncOutcome = iota // 已同步（含缓存命中）
	syncFailed                         // 未就绪：保留沙箱，下次只重做同步
	syncSandboxGone                    // 沙箱容器已不可用：须丢弃、换新容器
)

// isSandboxGone 判断沙箱代理的报错是否意味着容器已不在。
//
// 实测（2026-10-04）：沙箱空闲约 20 分钟后被上游回收，代理对它的令牌一律回
// 502（空响应体），而后端签发资源令牌照常 200 —— 缓存里的沙箱看起来完好，
// 实际上再也连不上。404/410/503/504 同理：都不是"再等等就好"的状态。
// 401/403 不算：那是 Cookie 或风控问题，换容器无济于事。
func isSandboxGone(err error) bool {
	var apiErr *creds.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Status {
	case 404, 410, 502, 503, 504:
		return true
	}
	return false
}

// syncOrRebuild 做工作区同步；沙箱容器已被回收时丢弃它、换一个新容器再同步一次。
//
// 只重置项目同步不够：之后每个请求都会撞上同一个死容器，直到 sandbox_ttl 到期。
// 返回最终使用的沙箱（换新失败时为 nil）。只换一次：新容器也不通，说明是上游
// 沙箱服务整体故障，再换只会白白消耗容器额度。
func (r *Runner) syncOrRebuild(ctx context.Context, acct *account.Account, sb *prism.Sandbox, projectID string) (*prism.Sandbox, syncOutcome) {
	outcome := r.syncSandboxWorkspace(ctx, acct, sb, projectID)
	if outcome != syncSandboxGone {
		return sb, outcome
	}
	r.sandboxes.InvalidateIf(acct.ID, sb)
	r.app.SandboxOps.Inc("sync", "sandbox_gone")
	r.log.Warn("沙箱容器已失效（上游已回收），重新申请", "account", acct.ID, "project", projectID)
	fresh, err := r.ensureSandbox(ctx, acct, projectID)
	if err != nil || !fresh.Usable() {
		r.log.Warn("重新申请沙箱失败", "account", acct.ID, "err", err)
		return nil, outcome
	}
	if outcome = r.syncSandboxWorkspace(ctx, acct, fresh, projectID); outcome == syncSandboxGone {
		r.sandboxes.InvalidateIf(acct.ID, fresh)
	}
	return fresh, outcome
}

// syncSandboxWorkspace 保证沙箱已为该项目完成工作区同步。
//
// 这是整条链路里最容易漏、也最难定位的一步。完整四步（都已实测）：
//
//  1. 后端签发资源令牌     POST /api/projects/{id}/sandbox/resources-token
//  2. 把资源令牌交给沙箱    POST <sandbox>/resources-token
//  3. 取 Y-Sweet 文档凭证   POST /api/y
//  4. 把凭证原样交给沙箱    POST <sandbox>/token
//
// 之后 GET <sandbox>/wait-for-sync 才会从 syncing 变成 synced。
//
// 好消息是**不需要实现 Yjs/lib0**：第 4 步之后是沙箱自己连
// Y-Sweet 同步文档，我们只负责把凭证送到。这是实测结论，
// 不是推断 —— 所以 Go 侧不必引入任何 CRDT 依赖。
//
// 认证是双重的（Cookie + X-Crixet-Sandbox-Token），
// 只带后者会 401 且响应体为空，非常难排查。
func (r *Runner) syncSandboxWorkspace(ctx context.Context, acct *account.Account, sb *prism.Sandbox, projectID string) syncOutcome {
	if !r.cfg.Facade.UseSandbox || !sb.Usable() || projectID == "" {
		return syncFailed
	}
	if r.sandboxes.Synced(acct.ID, projectID) {
		r.app.SandboxOps.Inc("sync", "hit")
		return syncReady
	}

	// 同一项目只允许一个在飞的同步：并发的相同请求应该**等**前一个完成，
	// 而不是各自去签一份资源令牌（那会同时开多个沙箱会话，白耗额度）。
	unlock := r.sandboxes.LockProject(acct.ID, projectID)
	defer unlock()
	if r.sandboxes.Synced(acct.ID, projectID) {
		r.app.SandboxOps.Inc("sync", "hit")
		return syncReady
	}

	cred := acct.Credential()
	p := prism.Principal{Client: acct.Client, Cred: cred, ExtraHeaders: cred.Headers, AccountID: acct.ID}
	started := time.Now()

	// 1)+2) 签发并交付资源令牌（绑定本项目，实测 1 小时有效）。
	//
	// 上游对 Sentinel token 的校验存在**偶发风控抖动**：HTTP 403
	// "Request verification failed. Please try again."（2026-10-03
	// Codex 首连实测命中在交付步；此前大请求也偶发于 status 轮询步）。
	// 该错误重试即恢复 —— 但若直接把"沙箱异常"抛给客户端，Codex CLI
	// 会断流重连，用户看到假警报。因此在这里内部自愈一次：
	// 只对风控类 403 重签重交；凭据失效（401）等终态错误不重试。
	var tokenErr error
	tokenStage := ""
	var resourceToken *prism.ResourceToken
	// 5 次尝试 + 递增退避（2s/4s/6s/8s）：风控风暴期单次重试不够
	// 充分自愈，彻底避免抛给客户端导致断流重连。
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return syncFailed
			case <-time.After(time.Duration(attempt-1) * 2000 * time.Millisecond):
			}
			r.log.Info("重试资源令牌签发/交付", "account", acct.ID, "project", projectID, "attempt", attempt)
		}
		tokenStage = "签发"
		rt, err := r.client.AcquireResourceToken(ctx, p, projectID, sb.SessionID, sb.Token)
		if err != nil {
			tokenErr = err
			if isSentinelThrottle(err) {
				continue
			}
			break
		}
		tokenStage = "交付"
		if err = r.client.DeliverResourceToken(ctx, p, sb, rt, projectID); err != nil {
			tokenErr = err
			if isSentinelThrottle(err) {
				continue
			}
			break
		}
		tokenErr = nil
		resourceToken = rt
		break
	}
	if tokenErr != nil {
		if tokenStage == "交付" {
			r.log.Warn("交付资源令牌失败", "account", acct.ID, "err", tokenErr)
			r.app.SandboxOps.Inc("sync", "deliver_error")
			if isSandboxGone(tokenErr) {
				return syncSandboxGone
			}
		} else {
			r.log.Warn("签发沙箱资源令牌失败", "account", acct.ID, "project", projectID, "err", tokenErr)
			r.app.SandboxOps.Inc("sync", "token_error")
		}
		return syncFailed
	}

	// 3)+4) 取与交付 Y-Sweet 协作文档凭证（带风控 403 自动重试自愈）
	var ytk *prism.YSweetToken
	var ysweetErr error
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return syncFailed
			case <-time.After(time.Duration(attempt-1) * 2000 * time.Millisecond):
			}
			r.log.Info("重试 Y-Sweet 凭证签发/交付", "account", acct.ID, "project", projectID, "attempt", attempt)
		}
		ytk, err = r.client.AcquireYSweetToken(ctx, p, projectID)
		if err != nil {
			ysweetErr = err
			if isSentinelThrottle(err) {
				continue
			}
			break
		}
		if err = r.client.DeliverYSweetToken(ctx, p, sb, ytk); err != nil {
			ysweetErr = err
			if isSentinelThrottle(err) {
				continue
			}
			break
		}
		ysweetErr = nil
		break
	}
	if ysweetErr != nil {
		r.log.Warn("交付 Y-Sweet 凭证失败", "account", acct.ID, "err", ysweetErr)
		r.app.SandboxOps.Inc("sync", "ysweet_deliver_error")
		if isSandboxGone(ysweetErr) {
			return syncSandboxGone
		}
		return syncFailed
	}

	// 5) 等同步完成（判定规则与真实前端一致）
	st, ok := r.client.WaitSandboxReady(ctx, p, sb, r.cfg.Facade.SandboxReadyWait)
	if !ok {
		r.log.Warn("沙箱工作区未在等待窗口内就绪",
			"account", acct.ID, "project", projectID,
			"waited", time.Since(started).Round(time.Second), "最后状态", describeSyncStatus(st))
		r.app.SandboxOps.Inc("sync", "timeout")
		// 只让这个项目失效，保留沙箱：重试时不必再花一次容器分配。
		r.sandboxes.InvalidateProject(acct.ID, projectID)
		return syncFailed
	}

	// 用资源令牌的过期时间做缓存失效点：令牌一过期，沙箱就读不到
	// 项目文件了，与其等失败再重试，不如到点主动重同步。
	r.sandboxes.MarkSynced(acct.ID, projectID, resourceToken.Expiry())
	r.log.Info("沙箱工作区已就绪",
		"account", acct.ID, "project", projectID, "耗时", time.Since(started).Round(time.Millisecond))
	r.app.SandboxOps.Inc("sync", "ok")
	return syncReady
}

// describeSyncStatus 把同步状态压成一行，用于排查"卡在哪一项"。
//
// 这个信息比一个光秃秃的 false 有用得多：上游是把
// "还缺什么"拆成若干个布尔标志告知的，不记下来就没法定位。
func describeSyncStatus(st *prism.SandboxSyncStatus) string {
	if st == nil {
		return "(无状态)"
	}
	t := st.Tokens
	return fmt.Sprintf("status=%s caps=%v ySweetToken=%v syncedProvider=%v projId=%v credSrc=%s",
		st.Status, st.ReadinessCapabilities,
		t.HasCurrentYSweetToken, t.HasSyncedYSweetProvider,
		t.HasResourceProjectID, t.FileCredentialSource)
}

// isSandboxNotReady 判断这次 start 是不是"沙箱还没好"而不是真的失败。
func isSandboxNotReady(resp *prism.StartResponse) bool {
	if resp == nil || resp.Initial == nil || !resp.Initial.Fail {
		return false
	}
	reason := strings.ToLower(resp.Initial.ErrorReason)
	msg := strings.ToLower(resp.Initial.Error)
	switch {
	case reason == "sandbox_reconnecting",
		reason == "sandbox_not_ready",
		strings.Contains(msg, "submit prompt again"),
		strings.Contains(msg, "reconnecting to sandbox"),
		strings.Contains(msg, "synchronization timed out"),
		strings.Contains(msg, "gateway timeout"),
		strings.Contains(msg, "unable to confirm"),
		strings.Contains(msg, "environment disconnected"),
		strings.Contains(reason, "unable to confirm"),
		strings.Contains(reason, "disconnected"):
		return true
	}
	return false
}

// isProcessingError 报告 start 是否失败于 "Error while processing conversation"：沙箱没就绪、
// 模型已下架时上游都回这一句。
func isProcessingError(resp *prism.StartResponse) bool {
	return resp != nil && resp.Initial != nil && resp.Initial.Fail &&
		strings.Contains(strings.ToLower(resp.Initial.Error), "error while processing conversation")
}

func sandboxReason(resp *prism.StartResponse) string {
	if resp == nil || resp.Initial == nil {
		return ""
	}
	if resp.Initial.ErrorReason != "" && resp.Initial.Error != "" {
		return fmt.Sprintf("%s (%s)", resp.Initial.ErrorReason, resp.Initial.Error)
	}
	if resp.Initial.ErrorReason != "" {
		return resp.Initial.ErrorReason
	}
	return resp.Initial.Error
}

// upstreamFailure 把上游的失败终态转成 Go error：单条超限转成 contextTooLargeError
// （预检的上限比上游小，正常到不了这里；上游若收紧了限制，客户端照样拿到
// context_length_exceeded，而不是一个会被无限重试的 server_error）。
func (r *Runner) upstreamFailure(st *prism.StatusResponse, items []prism.InputItem, tokens func() int) error {
	if isUpstreamTooLarge(st.Error) {
		return &contextTooLargeError{
			Bytes: promptBytes(items), Limit: r.cfg.Facade.PromptByteLimit(),
			Tokens: tokens(), Upstream: st.Error,
		}
	}
	return upstreamError(st)
}

// upstreamError 把上游的业务错误转成 Go error。
func upstreamError(st *prism.StatusResponse) error {
	msg := st.Error
	if msg == "" {
		msg = "上游返回失败状态"
	}
	if st.ErrorReason != "" {
		return fmt.Errorf("上游生成失败 [%s]: %s", st.ErrorReason, msg)
	}
	return fmt.Errorf("上游生成失败: %s", msg)
}

func pollWaitMs(f *config.FacadeConfig) int {
	if !f.UseStatusWait {
		return 0
	}
	return f.PollWaitMs
}

// ForgetProject 丢弃"会话 -> 项目"的缓存绑定（项目在上游已失效时）。
func (r *Runner) ForgetProject(accountID, stickyKey string) {
	if r == nil || r.projects == nil || accountID == "" || stickyKey == "" {
		return
	}
	r.projects.Invalidate(accountID, stickyKey)
}

// resolveProject 取（或创建）本轮使用的项目。
func (r *Runner) resolveProject(ctx context.Context, acct *account.Account, req *RunRequest) (string, error) {
	f := &r.cfg.Facade

	// 伴生轻量请求（标题/摘要生成）：走账号级专用伴生项目。
	// 绝不借用"最近活跃项目" —— 那是别人会话的工作区，借用等于跨会话读写文件。
	if req.IsAux {
		unlock := r.projects.Lock(acct.ID, "aux_bucket")
		defer unlock()
		if id, ok := r.projects.Get(acct.ID, "aux_bucket", time.Now()); ok {
			return id, nil
		}
		id, err := r.createProject(ctx, acct)
		if err == nil && id != "" {
			r.projects.Put(acct.ID, "aux_bucket", id, time.Now())
		}
		return id, err
	}

	bucketKey := req.StickyKey
	if !f.ReuseProject {
		return r.createProject(ctx, acct)
	}

	if bucketKey == "" {
		// 没有会话标识时用轮转分桶，而不是"所有人共用一个项目"——
		// 共用一个项目会把并发请求在上游串行化。
		n := f.ProjectPoolSize
		if n < 1 {
			n = 1
		}
		bucketKey = "bucket-" + strconv.FormatUint(r.bucketSeq.Add(1)%uint64(n), 10)
	}

	if id, ok := r.projects.Get(acct.ID, bucketKey, time.Now()); ok {
		r.app.ProjectOps.Inc("reuse", "hit")
		return id, nil
	}

	unlock := r.projects.Lock(acct.ID, bucketKey)
	defer unlock()

	if id, ok := r.projects.Get(acct.ID, bucketKey, time.Now()); ok {
		r.app.ProjectOps.Inc("reuse", "hit")
		return id, nil
	}

	id, err := r.createProject(ctx, acct)
	if err != nil {
		r.app.ProjectOps.Inc("create", "error")
		return "", err
	}
	r.projects.Put(acct.ID, bucketKey, id, time.Now())
	r.app.ProjectOps.Inc("create", "ok")
	return id, nil
}

func (r *Runner) createProject(ctx context.Context, acct *account.Account) (string, error) {
	cred := acct.Credential()
	p := prism.Principal{Client: acct.Client, Cred: cred, ExtraHeaders: cred.Headers, AccountID: acct.ID}

	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt-1) * 1500 * time.Millisecond):
			}
			r.log.Info("重试创建项目", "account", acct.ID, "attempt", attempt)
		}
		proj, err := r.client.CreateProject(ctx, p, nil)
		if err == nil {
			r.log.Debug("已创建上游项目", "account", acct.ID, "project", proj.Key())
			return proj.Key(), nil
		}
		lastErr = err
		if !isSentinelThrottle(err) {
			break
		}
	}
	return "", lastErr
}

func nextInterval(cur, max time.Duration) time.Duration {
	if cur <= 0 {
		cur = 200 * time.Millisecond
	}
	n := cur * 2
	if max > 0 && n > max {
		n = max
	}
	return n
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// isAccountLevel 判断错误是否值得换账号重试。
//
// 401 是账号凭据问题；429 是账号配额问题；5xx 值得换号。
// 4xx 协议错误不重试 —— 换号也一样错。
// 注意：Sentinel 风控 403（"Request verification failed"）是请求级
// 签名挑战偶发抖动，同账号退避重试即恢复，**绝不是**账号失效或配额耗尽。
// 若将其判为账号级故障，会引发毁灭性的换号重试：
// 摧毁现有会话、推翻已就绪的工作区、导致下游断流重连。因此它由各请求端点
// 内部原地自愈，绝不上报为账号级故障。
func isAccountLevel(err error) bool {
	if err == nil {
		return false
	}
	if creds.IsAuthError(err) || creds.IsRateLimited(err) {
		return true
	}
	var ae *creds.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return false
}

// retryAfter 从上游错误里提取 Retry-After。
func retryAfter(err error) time.Duration {
	var ae *creds.APIError
	if errors.As(err, &ae) {
		return ae.RetryAfter
	}
	return 0
}
