// Package server 负责组装所有组件并管理 HTTP 生命周期。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/capture"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/facade"
	"github.com/oai-prism/oaiprism/internal/metrics"
	"github.com/oai-prism/oaiprism/internal/middleware"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/rawproxy"
	"github.com/oai-prism/oaiprism/internal/tokens"
)

// Server 是完整运行实例。
type Server struct {
	cfg    *config.Config
	log    *slog.Logger
	app    *metrics.App
	pool   *account.Pool
	store  *account.Store
	sqlite *account.SQLiteStore
	rec    *capture.Recorder
	srv    *http.Server
	gwPort string // 网关自身端口（OAuth 回调降级路由用）

	keys   *dynamicKeys   // Dashboard 签发的 API Key（SQLite）
	admins *adminSessions // /admin/login 签发的管理会话
}

// New 组装并返回服务器。
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	app := metrics.NewApp()

	// 1) 账号池。初始账号来自配置文件 + 凭据文件 + SQLite。
	pool, err := account.NewPool(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("初始化账号池: %w", err)
	}

	store := account.NewStore(cfg.Creds.File, log)
	fileAccounts, err := store.Load()
	if err != nil {
		log.Error("凭据文件加载失败，先以配置内账号启动", "path", cfg.Creds.File, "err", err)
	}

	// 初始化 SQLite 持久化存储
	dbPath := "secrets/accounts.db"
	if cfg.Creds.File != "" {
		dbPath = filepath.Join(filepath.Dir(cfg.Creds.File), "accounts.db")
	}
	sqliteStore, sqliteErr := account.NewSQLiteStore(dbPath, log)
	if sqliteErr != nil {
		// Error 级：SQLite 是 Dashboard 的持久化后端（账号/会话/请求日志），
		// 失败意味着这些功能全部退化为内存态。降级可用，但必须显眼 ——
		// 静默 Warn 曾让 CI 上的初始化失败被忽略（2026-10-03）。
		log.Error("初始化 SQLite 账号存储失败，退化为仅文件模式",
			"path", dbPath, "err", sqliteErr)
	} else {
		// 账号以 SQLite 为准：凭据文件与配置内账号只在数据库为空时导入一次。
		_ = sqliteStore.MigrateIfEmpty(mergeAccounts(cfg.Creds.Accounts, fileAccounts))
		// 刷新后的凭据写回 SQLite：refresh_token 每次刷新都会轮换，不落盘的话
		// 重启（或 Dashboard 编辑触发的整池重建）后读回的是已作废的旧值。
		// 只 UPDATE token 列，不碰 Dashboard 编辑的 name / tags / max_concurrency。
		pool.SetOnRefreshed(account.NewRefreshPersister(sqliteStore, cfg.Creds.PersistRefreshMin, log).Persist)
	}

	initial := mergeAccounts(cfg.Creds.Accounts, fileAccounts)
	if sqliteErr == nil {
		if dbAccounts, err := sqliteStore.Load(); err == nil && len(dbAccounts) > 0 {
			initial = dbAccounts
		}
	}
	if len(initial) > 0 {
		if err := pool.Build(initial); err != nil {
			return nil, fmt.Errorf("构建账号池: %w", err)
		}
	}
	if pool.Size() == 0 {
		log.Warn("账号池为空：服务会正常启动，但所有推理请求都会失败。"+
			"可在前端控制面板直接导入或录入账号，将自动持久化至 SQLite",
			"file", cfg.Creds.File, "mode", cfg.Creds.Mode)
	}

	// 2) 抓包器。
	rec := capture.New(capture.Options{
		Enabled:      cfg.Capture.Enabled,
		Dir:          cfg.Capture.Dir,
		MaxBody:      cfg.Capture.MaxBody,
		Redact:       cfg.Capture.Redact,
		Sample:       cfg.Capture.Sample,
		IncludePaths: cfg.Capture.IncludePaths,
		ExcludePaths: cfg.Capture.ExcludePaths,
	}, app)
	if rec.Enabled() {
		log.Info("抓包已开启", "dir", cfg.Capture.Dir, "redact", cfg.Capture.Redact)
	}

	// 3) 上游客户端。
	client := prism.New(pool.Client(), prism.UpstreamOptions{
		UserAgent:     cfg.Upstream.UserAgent,
		Origin:        cfg.Upstream.Origin,
		Referer:       cfg.Upstream.Referer,
		Headers:       cfg.Upstream.Headers,
		MaxRetries:    cfg.Upstream.MaxRetries,
		RetryBackoff:  cfg.Upstream.RetryBackoff,
		RetryMaxDelay: cfg.Upstream.RetryMaxDelay,
	}, schemaOptions(cfg))
	client.SetRecorder(rec)
	client.SetMetrics(app)

	// 4) 门面与通道。
	runner := facade.NewRunner(cfg, log, pool, client, app)
	if sqliteErr == nil {
		// 原生续接的会话绑定落盘：网关重启后客户端会话接回原来的上游会话（见 facade/native_store.go）。
		runner.UseNativeStore(sqliteStore)
	}
	facadeHandler := facade.NewHandler(cfg, log, runner, app)
	rawHandler := rawproxy.New(cfg, log, pool, client, app, rec)

	s := &Server{
		cfg:    cfg,
		log:    log,
		app:    app,
		pool:   pool,
		store:  store,
		sqlite: sqliteStore,
		rec:    rec,
		admins: newAdminSessions(),
	}
	if sqliteStore != nil {
		s.keys = newDynamicKeys(sqliteStore)
	}
	s.warnInsecureDefaults()

	// 5) 路由。
	mux := http.NewServeMux()
	if cfg.Facade.Enabled {
		facadeHandler.Register(mux)
	}
	rawHandler.Register(mux)
	s.registerOps(mux, runner)

	handler := middleware.Chain(s.requestAuditMiddleware(mux),
		middleware.Recover(log, app),
		middleware.RequestIDMiddleware,
		// CORS 在最外层：预检请求不该经过鉴权与限流。
		middleware.CORS(cfg.Server.CORSOrigin),
		middleware.MetricsMiddleware(app),
		// 限流在鉴权之前：拒绝无效流量越早越省资源。
		middleware.NewRateLimiter(cfg.Facade.RateLimitPerSecond(), cfg.Facade.RateLimitBurst(), app).Middleware(),
		middleware.Auth(middleware.AuthOptions{
			StaticKeys:  cfg.Facade.APIKeys,
			DynamicKeys: s.keys.Get,
			AdminToken:  s.admins.Valid,
			ExemptPaths: []string{
				cfg.Metrics.Health, cfg.Metrics.Ready, cfg.Metrics.Path,
				// 登录本身与 OAuth 浏览器回调（由 state 保护）不能要求已登录。
				"/admin/login", "/admin/oauth/callback",
			},
			// Dashboard 只是静态资源，浏览器导航带不了 Bearer；数据接口另行鉴权。
			ExemptPrefixes: []string{"/dashboard"},
			CORSOrigin:     cfg.Server.CORSOrigin,
			App:            app,
		}),
	)

	// 网关自身端口：OAuth 导入的回调降级路由（/admin/oauth/callback）需要它
	if _, port, err := net.SplitHostPort(cfg.Server.Addr()); err == nil {
		s.gwPort = port
	}

	s.srv = &http.Server{
		Addr:              cfg.Server.Addr(),
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		// WriteTimeout 默认为 0：流式响应下任何非零值都会把长回答切断。
		WriteTimeout:   cfg.Server.WriteTimeout,
		IdleTimeout:    cfg.Server.IdleTimeout,
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	return s, nil
}

// peekModel 从（可能被截断的）请求体前缀里取 model 字段。
//
// 完整 JSON 解析不了（超过窥视窗口）时退化为扫描 "model":"..."。
func peekModel(head []byte) string {
	var parsed struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(head, &parsed) == nil {
		return parsed.Model
	}
	s := string(head)
	i := strings.Index(s, `"model"`)
	if i < 0 {
		return ""
	}
	rest := strings.TrimLeft(s[i+len(`"model"`):], " \t\r\n:")
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	if j := strings.IndexByte(rest[1:], '"'); j >= 0 {
		return rest[1 : 1+j]
	}
	return ""
}

// warnInsecureDefaults 在启动时把危险配置喊出来。
func (s *Server) warnInsecureDefaults() {
	dyn := s.keys.Get()
	if len(s.cfg.Facade.APIKeys) == 0 && len(dyn) == 0 {
		s.log.Warn("未配置任何 API Key：仅允许本机访问（Dashboard 生成 Key 或配置 facade.api_keys 后转为全量校验）",
			"listen", s.cfg.Server.Addr())
	}
	for _, k := range append(append([]string{}, s.cfg.Facade.APIKeys...), dyn...) {
		if k == account.DefaultPublicAPIKey {
			s.log.Warn("仍在使用源码内置的公开默认 Key（" + account.DefaultPublicAPIKey + "），任何人都能用它调用网关。" +
				"请在 Dashboard 生成新 Key、更新客户端配置后注销它")
			break
		}
	}
	if s.cfg.RawProxy.PassthroughAuth {
		s.log.Warn("raw_proxy.passthrough_auth 尚未实现，该配置不生效：原样反代始终使用账号池凭据")
	}
	if s.cfg.Facade.LocalWorkspaceWrite {
		s.log.Warn("facade.local_workspace_write 已开启：本机请求可经 X-Local-Workspace 让网关直接写本机目录")
	}
}

// mergeAccounts 合并配置内账号与文件账号，文件优先（同 ID 覆盖）。
//
// 语义选择说明：凭据文件是"运维会频繁改的东西"，
// 让它的优先级高于静态配置，改完即生效才不会出现"改了没反应"的困惑。
func mergeAccounts(base, override []config.AccountConfig) []config.AccountConfig {
	index := make(map[string]int, len(base))
	out := make([]config.AccountConfig, len(base))
	copy(out, base)
	for i, a := range out {
		index[a.ID] = i
	}
	for _, a := range override {
		if i, ok := index[a.ID]; ok {
			out[i] = a
			continue
		}
		index[a.ID] = len(out)
		out = append(out, a)
	}
	return out
}

func schemaOptions(cfg *config.Config) prism.SchemaOptions {
	s := cfg.Facade.Schema
	return prism.SchemaOptions{
		StartPath:            s.StartPath,
		StatusPath:           s.StatusPath,
		StopPath:             s.StopPath,
		FieldModel:           s.FieldModel,
		FieldMessages:        s.FieldMessages,
		FieldInstructions:    s.FieldInstructions,
		FieldInput:           s.FieldInput,
		FieldTools:           s.FieldTools,
		FieldStream:          s.FieldStream,
		FieldSessionID:       s.FieldSessionID,
		FieldProjectID:       s.FieldProjectID,
		FieldSandboxID:       s.FieldSandboxID,
		FieldConversationID:  s.FieldConversationID,
		FieldPreviousRespID:  s.FieldPreviousRespID,
		FieldMetadata:        s.FieldMetadata,
		FieldUserID:          s.FieldUserID,
		FieldRequestID:       s.FieldRequestID,
		FieldTurnState:       s.FieldTurnState,
		FieldResponseID:      s.FieldResponseID,
		FieldReasoning:       s.FieldReasoning,
		FieldReasoningEffort: s.FieldReasoningEffort,
		FieldExtra:           s.FieldExtra,
		RespIDKeys:           s.RespIDKeys,
		RespStatusKeys:       s.RespStatusKeys,
		RespTextKeys:         s.RespTextKeys,
		RespDeltaKeys:        s.RespDeltaKeys,
		RespMessagesKey:      s.RespMessagesKey,
		RespErrorKeys:        s.RespErrorKeys,
		StatusDone:           s.StatusDone,
		StatusFail:           s.StatusFail,
		StatusRun:            s.StatusRun,
	}
}

// registerOps 注册运维端点。
func (s *Server) registerOps(mux *http.ServeMux, runner *facade.Runner) {
	cfg := s.cfg

	if cfg.Metrics.Enabled {
		mux.HandleFunc("GET "+cfg.Metrics.Path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if err := s.app.Reg.Render(w); err != nil {
				s.log.Debug("输出指标失败", "err", err)
			}
		})
	}

	mux.HandleFunc("GET "+cfg.Metrics.Health, func(w http.ResponseWriter, r *http.Request) {
		// 存活探针只表示"进程还在"，不检查下游——
		// 否则上游抖动会导致 k8s 反复重启一个本身健康的进程。
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET "+cfg.Metrics.Ready, func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		healthy := s.pool.Healthy(now)
		body := map[string]any{
			"status":         "ok",
			"accounts_total": s.pool.Size(),
			"accounts_ready": healthy,
			"uptime_sec":     int(time.Since(startTime).Seconds()),
		}
		code := http.StatusOK
		if s.pool.Size() == 0 {
			// 没有账号时明确不 ready：这能让人一眼看出"漏配凭据了"，
			// 而不是等到线上请求全 502 才发现。
			body["status"] = "no_credentials"
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})

	mux.HandleFunc("GET /admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		accounts := s.pool.Accounts()
		out := make([]account.Stats, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, a.Stats(now))
		}
		credsFile := s.store.Path()
		if s.sqlite != nil {
			credsFile = "sqlite:" + s.sqlite.Path()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":      len(out),
			"ready":      s.pool.Healthy(now),
			"creds_file": credsFile,
			"accounts":   out,
		})
	})

	// POST /admin/accounts: 创建/批量导入账号并持久化至 SQLite
	//
	// 请求体按凭据文件的宽容规则解析（account.ParseAccountList）：账号对象、数组、
	// {"accounts":[…]}，以及其它网关（sub2api 等）的导出文件都认，字段名不分大小写与下划线。
	// 早先直接反序列化进 config.AccountConfig —— 它没有 json tag，Dashboard 发的
	// access_token / refresh_token 全被静默丢掉，导入出来的账号只剩名字。
	mux.HandleFunc("POST /admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeAdminErr(w, http.StatusBadRequest, "读取请求失败: "+err.Error())
			return
		}
		batch, err := account.ParseAccountList(body)
		if err != nil {
			writeAdminErr(w, http.StatusBadRequest, "解析账号失败: "+err.Error())
			return
		}
		if len(batch) == 0 {
			writeAdminErr(w, http.StatusBadRequest, "请求里没有账号")
			return
		}
		for i, a := range batch {
			if !account.HasCredentials(a) {
				writeAdminErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 个账号没有任何凭据（access_token / refresh_token / cookies）", i+1))
				return
			}
		}

		var ids []string
		if s.sqlite != nil {
			existing, _ := s.sqlite.Load()
			for i := range batch {
				a := &batch[i]
				if a.ID == "" {
					a.ID = importedAccountID(existing, *a)
				}
				if a.Name == "" {
					a.Name = a.Email
				}
				if err := s.sqlite.SaveAccount(*a); err != nil {
					writeAdminErr(w, http.StatusInternalServerError, "保存至 SQLite 失败: "+err.Error())
					return
				}
				ids = append(ids, a.ID)
			}
			_ = s.syncPoolFromSQLite()
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"created": len(batch),
			"ids":     ids,
			"total":   s.pool.Size(),
		})
	})

	// 官方 OAuth 授权导入（纯 Go，零浏览器依赖）：
	// 后端生成 PKCE 授权 URL → 用户自己的浏览器打开授权页登录（CF 对真人不拦）
	// → 前端把回调 code/state 传回 → 后端 PKCE 换 token 入库。
	// 同 sub2api 的 GenerateAuthURL + ExchangeCode 模式。
	// 本地回调接 code，PKCE 换 token 后直接入库（详 OAuth 流程见 oauth_admin.go）。
	mux.HandleFunc("POST /admin/oauth/begin", s.handleOAuthBegin)
	mux.HandleFunc("GET /admin/oauth/status", s.handleOAuthStatus)
	mux.HandleFunc("POST /admin/oauth/exchange", s.handleOAuthExchange)
	// redirect_uri 若指向网关自身（http://<host>:<port>/admin/oauth/callback）
	// 则走这个路由；指向独立本地端口时由临时监听器接（见 handleOAuthBegin）。
	mux.HandleFunc("GET /admin/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		// state 必须精确匹配（见 ensureOAuthCallbackListener 的说明）。
		state := r.URL.Query().Get("state")
		sess := oauthSessions.getByState(state)
		if sess == nil {
			writeAdminErr(w, http.StatusBadRequest, "会话不存在或已过期")
			return
		}
		s.handleLocalCallback(w, r, sess)
	})

	// PUT /admin/accounts/{id}: 更新账号属性并持久化至 SQLite
	mux.HandleFunc("PUT /admin/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少账号 ID")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeAdminErr(w, http.StatusBadRequest, "读取请求失败: "+err.Error())
			return
		}

		if s.sqlite != nil {
			list, err := s.sqlite.Load()
			if err != nil {
				writeAdminErr(w, http.StatusInternalServerError, "读取 SQLite 失败: "+err.Error())
				return
			}
			var cur *config.AccountConfig
			for i := range list {
				if list[i].ID == id {
					cur = &list[i]
				}
			}
			if cur == nil {
				writeAdminErr(w, http.StatusNotFound, "账号不存在: "+id)
				return
			}
			// 只改请求里给了的字段：Dashboard 编辑框不发 tags，整体覆盖会把标签清空
			update, err := account.MergeAccountPatch(*cur, body)
			if err != nil {
				writeAdminErr(w, http.StatusBadRequest, "解析修改数据失败: "+err.Error())
				return
			}
			update.ID = id
			if err := s.sqlite.SaveAccount(update); err != nil {
				writeAdminErr(w, http.StatusInternalServerError, "更新 SQLite 失败: "+err.Error())
				return
			}
			_ = s.syncPoolFromSQLite()
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"updated": id,
		})
	})

	// DELETE /admin/accounts/{id}: 从 SQLite 中物理删除账号并即时剔除
	mux.HandleFunc("DELETE /admin/accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少账号 ID")
			return
		}

		if s.sqlite != nil {
			if err := s.sqlite.DeleteAccount(id); err != nil {
				writeAdminErr(w, http.StatusNotFound, err.Error())
				return
			}
			_ = s.syncPoolFromSQLite()
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"deleted": id,
			"total":   s.pool.Size(),
		})
	})

	mux.HandleFunc("POST /admin/reload", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.loadPoolAccounts()
		if err != nil {
			writeAdminErr(w, http.StatusBadRequest, "读取账号失败: "+err.Error())
			return
		}
		if err := s.pool.Build(list); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "重建账号池失败: "+err.Error())
			return
		}
		s.log.Info("账号池已手动重载", "count", s.pool.Size())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": s.pool.Size()})
	})

	mux.HandleFunc("GET /admin/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uptime_sec":         int(time.Since(startTime).Seconds()),
			"accounts":           s.pool.Size(),
			"accounts_ready":     s.pool.Healthy(time.Now()),
			"project_cache_size": runner.ProjectCacheSize(),
			"capture_enabled":    s.rec.Enabled(),
			"capture_written":    s.rec.Written(),
			"capture_dropped":    s.rec.Dropped(),
			"upstream":           s.cfg.Upstream.BaseURL,
			"creds_mode":         s.cfg.Creds.Mode,
			"creds_file":         s.store.Path(),
		})
	})

	mux.HandleFunc("POST /admin/accounts/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a := s.pool.Get(id)
		if a == nil {
			writeAdminErr(w, http.StatusNotFound, "账号不存在: "+id)
			return
		}
		refresher, err := creds.NewRefresher(s.cfg.Creds, s.cfg.Upstream, s.pool.Client())
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		c, err := s.pool.RefreshAccount(ctx, refresher, a)
		if err != nil {
			writeAdminErr(w, http.StatusBadGateway, "刷新失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account":    id,
			"source":     c.Source,
			"plan":       c.Plan,
			"email":      c.Email,
			"expires_at": c.ExpiresAt.UTC().Format(time.RFC3339),
			"user_id":    c.UserID,
		})
	})

	// GET /admin/requests: 真实请求明细流水分页查询
	mux.HandleFunc("GET /admin/requests", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		pageSize, _ := strconv.Atoi(q.Get("page_size"))
		// status 支持结果（ok / failed）、精确状态码（429）与状态码段（2xx / 4xx / 5xx）
		status := strings.ToLower(strings.TrimSpace(q.Get("status")))
		var statusCode, statusClass int
		var outcome string
		if status == "ok" || status == "failed" {
			outcome = status
		} else if len(status) == 3 && strings.HasSuffix(status, "xx") {
			statusClass, _ = strconv.Atoi(status[:1])
		} else {
			statusCode, _ = strconv.Atoi(status)
		}

		filter := account.RequestLogFilter{
			Page:        page,
			PageSize:    pageSize,
			Model:       strings.TrimSpace(q.Get("model")),
			AccountID:   strings.TrimSpace(q.Get("account_id")),
			StatusCode:  statusCode,
			StatusClass: statusClass,
			Outcome:     outcome,
		}

		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}

		items, total, err := s.sqlite.QueryRequestLogs(filter)
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "查询请求明细失败: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total":     total,
			"page":      filter.Page,
			"page_size": filter.PageSize,
			"items":     items,
		})
	})

	// GET /admin/statistics: 真实请求聚合统计（杜绝模拟假数据）
	mux.HandleFunc("GET /admin/statistics", func(w http.ResponseWriter, r *http.Request) {
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}

		stats, err := s.sqlite.GetAggregatedStats()
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "获取聚合统计失败: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	})

	// Chat 调试工作台会话与消息持久化接口 (全面替代 localStorage)
	mux.HandleFunc("GET /admin/chat/sessions", func(w http.ResponseWriter, r *http.Request) {
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		list, err := s.sqlite.ListChatSessions()
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "查询会话失败: "+err.Error())
			return
		}
		if list == nil {
			list = []account.ChatSessionRecord{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})

	mux.HandleFunc("POST /admin/chat/sessions", func(w http.ResponseWriter, r *http.Request) {
		var sess account.ChatSessionRecord
		if err := json.NewDecoder(r.Body).Decode(&sess); err != nil {
			writeAdminErr(w, http.StatusBadRequest, "解析会话数据失败: "+err.Error())
			return
		}
		if sess.ID == "" {
			sess.ID = fmt.Sprintf("sess_%d", time.Now().UnixNano())
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.SaveChatSession(sess); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "保存会话失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sess)
	})

	mux.HandleFunc("DELETE /admin/chat/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少会话 ID")
			return
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.DeleteChatSession(id); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "删除会话失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "deleted": id})
	})

	mux.HandleFunc("GET /admin/chat/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少会话 ID")
			return
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		list, err := s.sqlite.ListChatMessages(id)
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "查询消息失败: "+err.Error())
			return
		}
		if list == nil {
			list = []account.ChatMessageRecord{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})

	mux.HandleFunc("POST /admin/chat/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少会话 ID")
			return
		}
		var msg account.ChatMessageRecord
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			writeAdminErr(w, http.StatusBadRequest, "解析消息数据失败: "+err.Error())
			return
		}
		msg.SessionID = id
		if msg.ID == "" {
			msg.ID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.SaveChatMessage(msg); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "保存消息失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msg)
	})

	// GET /admin/apikeys: 列出所有对外 API Keys
	mux.HandleFunc("GET /admin/apikeys", func(w http.ResponseWriter, r *http.Request) {
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		list, err := s.sqlite.ListAPIKeys()
		if err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "查询 API Keys 失败: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})

	// POST /admin/apikeys: 创建或生成新 API Key
	mux.HandleFunc("POST /admin/apikeys", func(w http.ResponseWriter, r *http.Request) {
		var item account.APIKeyItem
		_ = json.NewDecoder(r.Body).Decode(&item)
		item.Key = strings.TrimSpace(item.Key)
		if item.Key == "" {
			item.Key = account.NewAPIKey()
		} else if len(item.Key) < 20 {
			writeAdminErr(w, http.StatusBadRequest, "自定义 API Key 至少 20 个字符（留空则自动生成随机 Key）")
			return
		}
		if item.Name == "" {
			item.Name = "对外访问密钥"
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.SaveAPIKey(item); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "保存 API Key 失败: "+err.Error())
			return
		}
		s.keys.Invalidate()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	})

	// DELETE /admin/apikeys/{key}: 物理注销 API Key
	mux.HandleFunc("DELETE /admin/apikeys/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key == "" {
			writeAdminErr(w, http.StatusBadRequest, "缺少 Key")
			return
		}
		if s.sqlite == nil {
			writeAdminErr(w, http.StatusInternalServerError, "SQLite 存储未初始化")
			return
		}
		if err := s.sqlite.DeleteAPIKey(key); err != nil {
			writeAdminErr(w, http.StatusInternalServerError, "删除 API Key 失败: "+err.Error())
			return
		}
		s.keys.Invalidate()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "deleted": key})
	})

	// POST /admin/login: 管理员登录（密码来自配置，见 handleAdminLogin）
	mux.HandleFunc("POST /admin/login", s.handleAdminLogin)

	// GET /admin/me: 能走到这里说明已通过管理端鉴权（见 middleware.Auth）
	mux.HandleFunc("GET /admin/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"username": "admin",
			"role":     "superadmin",
			"status":   "authenticated",
		})
	})

	// 前端控制面板静态托管 (web/dist)
	candidates := []string{
		"web/dist",
		"../../web/dist",
		"../web/dist",
	}
	var distDir string
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "index.html")); err == nil {
			distDir = c
			break
		}
	}
	if distDir != "" {
		fs := http.FileServer(http.Dir(distDir))
		mux.HandleFunc("GET /dashboard/", func(w http.ResponseWriter, r *http.Request) {
			p := strings.TrimPrefix(r.URL.Path, "/dashboard")
			if p == "" || p == "/" {
				http.ServeFile(w, r, filepath.Join(distDir, "index.html"))
				return
			}
			r2 := new(http.Request)
			*r2 = *r
			r2.URL.Path = p
			fs.ServeHTTP(w, r2)
		})
		mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/", http.StatusFound)
		})
	}
}

func writeAdminErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type statusResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *statusResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.statusCode = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) requestAuditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 明细流水只记录「模型调用」：四个推理入口。
		// 其余流量（模型清单、管理端、Dashboard、原始反代等）不产生推理，
		// 不进明细 —— 否则表会被 /v1/models 这类探测请求刷屏，
		// 且这些请求本就没有模型/账号可言，绑定列永远是 "-"。
		isInference := false
		if r.Method == http.MethodPost {
			switch r.URL.Path {
			case "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/messages":
				isInference = true
			}
		}
		if !isInference || s.sqlite == nil {
			next.ServeHTTP(w, r)
			return
		}

		path := r.URL.Path

		start := time.Now()
		var model string

		// SSE 内部失败的错误通道：流式响应的 HTTP 状态永远是 200，
		// 真正的失败只存在于事件流里。facade 在失败点调
		// middleware.RecordLogError 写进来，这里落库 —— 否则流水里
		// 会出现"200 + 2ms + 无错误"的迷惑记录（2026-10-03 实证）。
		logErrBox := &middleware.LogErrorBox{}
		r = r.WithContext(context.WithValue(r.Context(), middleware.CtxKeyLogError{}, logErrBox))

		if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
			// 只窥视前 1MB 取 model 字段，然后把窥视过的部分与剩余部分拼回去。
			// 早期直接用这 1MB 替换了整个请求体：超过 1MB 的推理请求（Codex 长会话、
			// 带图片的请求）被截断，handler 一律报"请求体不是合法 JSON"。
			head, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err == nil {
				r.Body = struct {
					io.Reader
					io.Closer
				}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
				model = peekModel(head)
			}
		}

		sw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(sw, r)

		duration := time.Since(start).Milliseconds()
		// 实际路由到的账号由 facade 记进审计上下文；不读请求头 —— 那是客户端可控输入。
		accountID := middleware.LogAccount(r.Context())
		if hModel := r.Header.Get("X-Oaiprism-Model"); hModel != "" {
			model = hModel
		}

		item := account.RequestLogItem{
			ID:         fmt.Sprintf("req_%d", time.Now().UnixNano()),
			Timestamp:  start,
			Method:     r.Method,
			Path:       path,
			Model:      model,
			AccountID:  accountID,
			StatusCode: sw.statusCode,
			DurationMs: duration,
			ClientIP:   clientIP(r),
			UserAgent:  r.UserAgent(),
		}
		item.PromptTokens, item.CompletionTokens = middleware.LogUsage(r.Context())
		// SSE 内部失败（response.failed / SSE error 事件）在这里补记 ——
		// HTTP 状态码帮不上忙，错误只存在事件流里。
		if errs := middleware.LogErrors(r.Context()); len(errs) > 0 {
			item.ErrorMessage = strings.Join(errs, " | ")
		}

		go func() {
			// s.sqlite 可能是 nil（初始化失败时仅告警不阻断）。
			// 这里必须判空：nil 接收者会让方法内解引用崩掉整个进程，
			// 而且是在后台 goroutine 里 —— 一个请求日志就能让服务下线。
			// （2026-10-03 CI 在 Linux 上实测踩中；SQLiteStore 内部
			// 也有 ready() 防御，这里是第一道闸。）
			if s.sqlite == nil {
				return
			}
			_ = s.sqlite.RecordRequestLog(item)
		}()
	})
}

var startTime = time.Now()

// Run 启动服务并阻塞直到 ctx 取消。
func (s *Server) Run(ctx context.Context) error {
	// 分词表后台预热：首次构建约需数百毫秒，不让第一笔请求承担。
	tokens.Warmup()

	// 后台任务：账号刷新、健康巡检、凭据热重载。
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	s.pool.StartBackground(bgCtx)
	// 凭据文件热重载只在没有 SQLite 时启用：有 SQLite 时文件里的令牌早已过时
	// （续期轮换只写回数据库），按文件重建会丢掉控制台导入的账号、换回作废的令牌。
	if (s.cfg.Creds.Mode == "file" || s.cfg.Creds.Mode == "hybrid") && s.sqlite == nil {
		go s.store.Watch(bgCtx, s.cfg.Creds.ReloadInterval, func(list []config.AccountConfig) {
			if err := s.pool.Build(mergeAccounts(s.cfg.Creds.Accounts, list)); err != nil {
				s.log.Error("热重载账号池失败，保留原池", "err", err)
			}
		})
	}

	ln, err := s.listen()
	if err != nil {
		return err
	}

	// 连接预热：把 DNS / TCP / TLS 的首轮成本挪到启动阶段。
	go func() {
		warmCtx, cancel := context.WithTimeout(bgCtx, 15*time.Second)
		defer cancel()
		n := s.pool.Client().Warmup(warmCtx, warmConnections)
		s.log.Info("上游连接预热完成", "connections", n)
	}()

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("OAIprism 已启动",
			"addr", s.cfg.Server.Addr(),
			"upstream", s.cfg.Upstream.BaseURL,
			"facade", s.cfg.Facade.Enabled,
			"raw_prefix", s.cfg.RawProxy.Prefix,
			"accounts", s.pool.Size(),
			"metrics", s.cfg.Metrics.Path,
		)
		if s.cfg.Server.TLS.Enabled {
			errCh <- s.srv.ServeTLS(ln, s.cfg.Server.TLS.CertFile, s.cfg.Server.TLS.KeyFile)
			return
		}
		errCh <- s.srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		s.log.Info("收到退出信号，开始优雅关闭")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		s.log.Warn("优雅关闭超时，强制关闭", "err", err)
		_ = s.srv.Close()
	}
	s.rec.Close()
	s.pool.Client().CloseIdle()
	s.log.Info("已退出")
	return nil
}

const warmConnections = 4

// listen 创建监听，并在 socket 层做一次缓冲调优。
func (s *Server) listen() (net.Listener, error) {
	lc := net.ListenConfig{}
	if s.cfg.Server.ReadBufferSize > 0 {
		size := s.cfg.Server.ReadBufferSize
		lc.Control = func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				// 失败不阻断启动：内核 rmem_max 不够大时 setsockopt 会被
				// 静默裁剪到上限，这不是错误，只是调优没完全生效。
				_ = setRecvBuf(fd, size)
			})
		}
	}

	ln, err := lc.Listen(context.Background(), "tcp", s.cfg.Server.Addr())
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", s.cfg.Server.Addr(), err)
	}
	return ln, nil
}

// Addr 返回实际监听地址（便于测试取随机端口）。
func (s *Server) Addr() string { return s.srv.Addr }

// Handler 暴露处理器，供测试直接驱动。
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Pool 暴露账号池。
func (s *Server) Pool() *account.Pool { return s.pool }

// Metrics 暴露指标注册表。
func (s *Server) Metrics() *metrics.App { return s.app }

// Close 释放服务占用的所有资源（包含 SQLite 句柄）。
func (s *Server) Close() error {
	if s.sqlite != nil {
		_ = s.sqlite.Close()
	}
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

// syncPoolFromSQLite 从 SQLite 读取全量账号并重新热加载至账号池。
// importedAccountID 给导入时没写 id 的账号定 id：凭据与已有账号相同（同一个号再导一次）
// 就沿用它的 id 覆盖更新，否则新起一个。
func importedAccountID(existing []config.AccountConfig, a config.AccountConfig) string {
	for _, e := range existing {
		if (a.RefreshToken != "" && e.RefreshToken == a.RefreshToken) ||
			(a.AccessToken != "" && e.AccessToken == a.AccessToken) ||
			(a.Cookies != "" && e.Cookies == a.Cookies) {
			return e.ID
		}
	}
	return "acc-" + oauthRandomHex(4)
}

// loadPoolAccounts 是账号池的唯一来源，启动、手动重载、控制台增删改之后的重建都从这里取。
//
// 有 SQLite 时以数据库为准：控制台导入与编辑、续期轮换后的令牌都只写在数据库里，
// 凭据文件与配置内账号只在数据库为空时导入一次（见 New）。曾经手动重载读的是凭据文件，
// 一点「重载并刷新」控制台导入的账号就从池里消失，留下的还是文件里早已作废的令牌。
func (s *Server) loadPoolAccounts() ([]config.AccountConfig, error) {
	if s.sqlite != nil {
		return s.sqlite.Load()
	}
	list, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	return mergeAccounts(s.cfg.Creds.Accounts, list), nil
}

func (s *Server) syncPoolFromSQLite() error {
	if s.sqlite == nil {
		return nil
	}
	dbAccounts, err := s.loadPoolAccounts()
	if err != nil {
		s.log.Error("从 SQLite 读取账号失败", "err", err)
		return err
	}
	if err := s.pool.Build(dbAccounts); err != nil {
		s.log.Error("重构账号池失败", "err", err)
		return err
	}
	s.log.Info("SQLite 账号池已实时重载同步", "count", s.pool.Size())
	return nil
}
