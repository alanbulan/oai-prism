package account

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	_ "modernc.org/sqlite"
)

// SQLiteStore 负责通过 SQLite 持久化账号配置，支持动态增删改查 (CRUD)。
type SQLiteStore struct {
	db   *sql.DB
	path string
	log  *slog.Logger
	mu   sync.Mutex
}

// errSQLiteUnavailable 表示 SQLite 存储未就绪（初始化失败或零值实例）。
// 所有公开方法在这种情况下返回该错误而不是 panic —— 记录请求日志发生在
// 后台 goroutine 里，nil 解引用会直接崩掉整个进程（2026-10-03 CI 实证）。
var errSQLiteUnavailable = errors.New("sqlite 存储不可用（初始化失败）")

// ready 判断实例是否可用（同时防御 nil 接收者与 db 未打开的零值实例）。
func (s *SQLiteStore) ready() bool { return s != nil && s.db != nil }

// NewSQLiteStore 打开或创建 SQLite 数据库。
func NewSQLiteStore(dbPath string, log *slog.Logger) (*SQLiteStore, error) {
	if dbPath == "" {
		dbPath = "secrets/accounts.db"
	}
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite 数据库失败: %w", err)
	}

	// 优化 SQLite 并发与可靠性（使用 DELETE 模式确保 Windows 下句柄关闭后彻底释放文件）
	db.SetMaxOpenConns(1)
	_, _ = db.Exec("PRAGMA journal_mode=DELETE;")
	_, _ = db.Exec("PRAGMA synchronous=NORMAL;")

	s := &SQLiteStore{
		db:   db,
		path: dbPath,
		log:  log,
	}

	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// initSchema 创建 accounts、request_logs、chat_sessions、chat_messages、api_keys 与 native_bindings 表。
func (s *SQLiteStore) initSchema() error {
	if !s.ready() {
		return errSQLiteUnavailable
	}
	schema := `
	CREATE TABLE IF NOT EXISTS accounts (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		plan TEXT DEFAULT 'pro',
		email TEXT DEFAULT '',
		cookies TEXT DEFAULT '',
		access_token TEXT DEFAULT '',
		refresh_token TEXT DEFAULT '',
		max_concurrency INTEGER DEFAULT 2,
		tags TEXT DEFAULT '',
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS request_logs (
		id TEXT PRIMARY KEY,
		timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		method TEXT NOT NULL,
		path TEXT NOT NULL,
		model TEXT DEFAULT '',
		account_id TEXT DEFAULT '',
		status_code INTEGER DEFAULT 200,
		duration_ms INTEGER DEFAULT 0,
		prompt_tokens INTEGER DEFAULT 0,
		completion_tokens INTEGER DEFAULT 0,
		error_message TEXT DEFAULT '',
		client_ip TEXT DEFAULT '',
		user_agent TEXT DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_req_time ON request_logs(timestamp);
	CREATE INDEX IF NOT EXISTS idx_req_model ON request_logs(model);
	CREATE INDEX IF NOT EXISTS idx_req_account ON request_logs(account_id);

	CREATE TABLE IF NOT EXISTS chat_sessions (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		model TEXT NOT NULL,
		reasoning_effort TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS chat_messages (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT DEFAULT '',
		reasoning TEXT DEFAULT '',
		status TEXT DEFAULT 'success',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_msg_session ON chat_messages(session_id);

	CREATE TABLE IF NOT EXISTS api_keys (
		key TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err := s.db.Exec(schema + nativeBindingsSchema)
	if err != nil {
		return fmt.Errorf("初始化 sqlite 表结构失败: %w", err)
	}

	// 后加的列走 ALTER TABLE：CREATE TABLE IF NOT EXISTS 对已有库是空操作。
	//
	// oauth_client_id：refresh_token 与签发它的 OAuth client 绑定（见 creds.RefreshOAuth），
	// 对应 AccountConfig / Credential 的 OAuthClientID。早期表结构没有这一列，
	// 重启后该值丢失，刷新只能退回全局 creds.oauth_client_id —— 两者不一致就是 invalid_grant。
	if err := s.addColumnIfMissing("accounts", "oauth_client_id", "TEXT"); err != nil {
		return err
	}

	// 不再内置默认密钥：写死在源码里的 Key 对所有人公开，等于没有鉴权。
	// 没有任何 Key 时鉴权中间件只放行本机请求，用户在 Dashboard 生成
	// 第一把 Key 后自动转为全量校验。
	return nil
}

// addColumnIfMissing 在列不存在时追加一列（可空，旧行取 NULL）。
// 仅在 initSchema 内调用，表名/列名均为常量，不存在注入面。
func (s *SQLiteStore) addColumnIfMissing(table, column, decl string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("读取 %s 表结构失败: %w", table, err)
	}
	found := false
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("读取 %s 表结构失败: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			found = true
		}
	}
	// 必须先关游标再 ALTER：连接池只有一条连接（SetMaxOpenConns(1)）。
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取 %s 表结构失败: %w", table, err)
	}
	if found {
		return nil
	}
	if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl); err != nil {
		return fmt.Errorf("为 %s 表追加 %s 列失败: %w", table, column, err)
	}
	return nil
}

// Close 关闭数据库。
func (s *SQLiteStore) Close() error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}

// Path 返回数据库路径。
func (s *SQLiteStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Load 读取 SQLite 中所有已保存的账号。
func (s *SQLiteStore) Load() ([]config.AccountConfig, error) {
	if s == nil {
		return nil, errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return nil, errSQLiteUnavailable
	}

	rows, err := s.db.Query(`
		SELECT id, name, plan, email, cookies, access_token, refresh_token, max_concurrency, tags,
		       COALESCE(oauth_client_id, '')
		FROM accounts
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("查询 accounts 失败: %w", err)
	}
	defer rows.Close()

	var list []config.AccountConfig
	for rows.Next() {
		var a config.AccountConfig
		var tagsStr, clientID string
		err := rows.Scan(
			&a.ID,
			&a.Name,
			&a.Plan,
			&a.Email,
			&a.Cookies,
			&a.AccessToken,
			&a.RefreshToken,
			&a.MaxConcurrency,
			&tagsStr,
			&clientID,
		)
		if err != nil {
			return nil, fmt.Errorf("读取账号行失败: %w", err)
		}
		if tagsStr != "" {
			_ = json.Unmarshal([]byte(tagsStr), &a.Tags)
		}
		a.OAuthClientID = clientID
		list = append(list, a)
	}
	return list, rows.Err()
}

// SaveAccount 插入或更新单账号 (Create or Update)。
func (s *SQLiteStore) SaveAccount(a config.AccountConfig) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}

	if strings.TrimSpace(a.ID) == "" {
		a.ID = fmt.Sprintf("acc_%d", time.Now().UnixNano())
	}
	if strings.TrimSpace(a.Name) == "" {
		a.Name = a.ID
	}
	if strings.TrimSpace(a.Plan) == "" {
		a.Plan = "pro"
	}
	if a.MaxConcurrency <= 0 {
		a.MaxConcurrency = 2
	}

	tagsJSON := "[]"
	if len(a.Tags) > 0 {
		if b, err := json.Marshal(a.Tags); err == nil {
			tagsJSON = string(b)
		}
	}

	// oauth_client_id 与 token 同理：Dashboard 编辑不会带它，空值时保留库内原值。
	query := `
	INSERT INTO accounts (id, name, plan, email, cookies, access_token, refresh_token, max_concurrency, tags, oauth_client_id, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	ON CONFLICT(id) DO UPDATE SET
		name = excluded.name,
		plan = excluded.plan,
		email = excluded.email,
		cookies = CASE WHEN excluded.cookies != '' THEN excluded.cookies ELSE accounts.cookies END,
		access_token = CASE WHEN excluded.access_token != '' THEN excluded.access_token ELSE accounts.access_token END,
		refresh_token = CASE WHEN excluded.refresh_token != '' THEN excluded.refresh_token ELSE accounts.refresh_token END,
		max_concurrency = excluded.max_concurrency,
		tags = excluded.tags,
		oauth_client_id = COALESCE(excluded.oauth_client_id, accounts.oauth_client_id),
		updated_at = CURRENT_TIMESTAMP;
	`
	_, err := s.db.Exec(query,
		a.ID,
		a.Name,
		a.Plan,
		a.Email,
		a.Cookies,
		a.AccessToken,
		a.RefreshToken,
		a.MaxConcurrency,
		tagsJSON,
		nullIfEmpty(a.EffectiveOAuthClientID()), // 兼容旧 accounts.json 迁移进来的 Headers 写法
	)
	if err != nil {
		return fmt.Errorf("保存账号 %s 至 SQLite 失败: %w", a.ID, err)
	}
	return nil
}

// TokenUpdate 是凭据刷新后要回写的字段。空串表示"不改"，库内原值保持不动。
type TokenUpdate struct {
	AccessToken   string
	RefreshToken  string
	Plan          string
	Email         string
	OAuthClientID string
}

// ErrAccountNotInStore 表示 UpdateTokens 的目标账号不在 SQLite 里
// （来自 config.yaml 的静态账号，或刷新途中已被删除）。
var ErrAccountNotInStore = errors.New("账号不在 SQLite 中")

// UpdateTokens 只回写凭据相关列，供后台刷新使用。
//
// 刻意不走 SaveAccount：那是"整行 upsert"，会用调用方手里的 name / tags /
// max_concurrency 覆盖 Dashboard 上的编辑，而刷新流程手里只有凭据。
// 也不改 updated_at：Load 按它排序（进而决定池内平票顺序与 Dashboard 列表顺序），
// 后台续期不该让账号顺序跳动。目标行不存在时返回 ErrAccountNotInStore，不会凭空插入。
func (s *SQLiteStore) UpdateTokens(id string, t TokenUpdate) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db == nil {
		return errSQLiteUnavailable
	}

	res, err := s.db.Exec(`
		UPDATE accounts SET
			access_token    = COALESCE(?, access_token),
			refresh_token   = COALESCE(?, refresh_token),
			plan            = COALESCE(?, plan),
			email           = COALESCE(?, email),
			oauth_client_id = COALESCE(?, oauth_client_id)
		WHERE id = ?`,
		nullIfEmpty(t.AccessToken),
		nullIfEmpty(t.RefreshToken),
		nullIfEmpty(t.Plan),
		nullIfEmpty(t.Email),
		nullIfEmpty(t.OAuthClientID),
		id,
	)
	if err != nil {
		return fmt.Errorf("回写账号 %s 凭据失败: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrAccountNotInStore, id)
	}
	return nil
}

// nullIfEmpty 把空串（含纯空白）映射为 SQL NULL，配合 COALESCE 表达"不改"。
func nullIfEmpty(v string) sql.NullString {
	v = strings.TrimSpace(v)
	return sql.NullString{String: v, Valid: v != ""}
}

// DeleteAccount 从 SQLite 中物理删除账号 (Delete)。
func (s *SQLiteStore) DeleteAccount(id string) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}

	res, err := s.db.Exec("DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("从 SQLite 删除账号 %s 失败: %w", id, err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("账号不存在: %s", id)
	}
	return nil
}

// MigrateIfEmpty 如果 SQLite 为空，自动把现有列表迁移进来。
func (s *SQLiteStore) MigrateIfEmpty(existing []config.AccountConfig) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	if s.db == nil {
		s.mu.Unlock()
		return errSQLiteUnavailable
	}
	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&count)
	s.mu.Unlock()

	if count > 0 || len(existing) == 0 {
		return nil
	}

	s.log.Info("SQLite 数据库为空，自动初始化导入已有账号", "count", len(existing))
	for _, a := range existing {
		if a.ID == "" {
			continue
		}
		if err := s.SaveAccount(a); err != nil {
			s.log.Warn("初始化迁移账号失败", "id", a.ID, "err", err)
		}
	}
	return nil
}

// -------------------------------------------------------------
// 请求流水明细 (Request Logs) 持久化与分析
// -------------------------------------------------------------

// RequestLogItem 对应一次真实请求明细流水记录。
type RequestLogItem struct {
	ID               string    `json:"id"`
	Timestamp        time.Time `json:"timestamp"`
	Method           string    `json:"method"`
	Path             string    `json:"path"`
	Model            string    `json:"model"`
	AccountID        string    `json:"account_id"`
	StatusCode       int       `json:"status_code"`
	DurationMs       int64     `json:"duration_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	ErrorMessage     string    `json:"error_message"`
	ClientIP         string    `json:"client_ip"`
	UserAgent        string    `json:"user_agent"`
}

// RequestLogFilter 过滤条件
type RequestLogFilter struct {
	Page       int
	PageSize   int
	Model      string
	AccountID  string
	StatusCode int
	// StatusClass 按状态码段过滤：2 → 2xx，4 → 4xx，5 → 5xx（与 StatusCode 二选一）。
	StatusClass int
	// Outcome 按结果过滤："ok" 成功 / "failed" 失败（含流式中途失败，见 failedExpr）。
	Outcome string
}

// failedExpr 判定一条流水是否失败：4xx/5xx，或响应头（200）已发出后流式中途失败 ——
// 后者状态码已改不了，失败原因记在 error_message（只有失败路径会写它）。
const failedExpr = `(status_code >= 400 OR COALESCE(error_message, '') <> '')`

// ModelUsageStat 真实模型使用占比
type ModelUsageStat struct {
	Model            string  `json:"model"`
	Requests         int     `json:"requests"`
	Percentage       float64 `json:"percentage"`
	AvgLatencyMs     int64   `json:"avg_latency_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
}

// TimeSeriesPoint 真实时间序列统计点
type TimeSeriesPoint struct {
	Timestamp string  `json:"timestamp"`
	Requests  int     `json:"requests"`
	Failures  int     `json:"failures"`
	Tokens    int64   `json:"tokens"`
	QPS       float64 `json:"qps"`
	Latency   int64   `json:"latency"`
	ErrorRate float64 `json:"error_rate"`
}

// AggregatedStats 真实聚合统计数据（拒绝假数据）
type AggregatedStats struct {
	TotalRequests         int               `json:"total_requests"`
	Failures              int               `json:"failures"`
	SuccessRate           float64           `json:"success_rate"`
	AvgLatencyMs          int64             `json:"avg_latency_ms"`
	TotalPromptTokens     int64             `json:"total_prompt_tokens"`
	TotalCompletionTokens int64             `json:"total_completion_tokens"`
	ModelUsages           []ModelUsageStat  `json:"model_usages"`
	TimeSeries            []TimeSeriesPoint `json:"time_series"`
}

// RecordRequestLog 将一笔真实请求记录持久化至 SQLite。
func (s *SQLiteStore) RecordRequestLog(item RequestLogItem) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}

	if item.ID == "" {
		item.ID = fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	if item.Timestamp.IsZero() {
		item.Timestamp = time.Now()
	}

	query := `
	INSERT INTO request_logs (
		id, timestamp, method, path, model, account_id,
		status_code, duration_ms, prompt_tokens, completion_tokens,
		error_message, client_ip, user_agent
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`
	_, err := s.db.Exec(query,
		item.ID,
		item.Timestamp.UTC().Format("2006-01-02 15:04:05"),
		item.Method,
		item.Path,
		item.Model,
		item.AccountID,
		item.StatusCode,
		item.DurationMs,
		item.PromptTokens,
		item.CompletionTokens,
		item.ErrorMessage,
		item.ClientIP,
		item.UserAgent,
	)
	return err
}

// QueryRequestLogs 分页查询请求流水明细。
func (s *SQLiteStore) QueryRequestLogs(filter RequestLogFilter) ([]RequestLogItem, int, error) {
	if s == nil {
		return nil, 0, errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return nil, 0, errSQLiteUnavailable
	}

	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 10
	}
	offset := (filter.Page - 1) * filter.PageSize

	var conditions []string
	var args []any

	if filter.Model != "" {
		conditions = append(conditions, "model = ?")
		args = append(args, filter.Model)
	}
	if filter.AccountID != "" {
		conditions = append(conditions, "account_id = ?")
		args = append(args, filter.AccountID)
	}
	if filter.StatusCode > 0 {
		conditions = append(conditions, "status_code = ?")
		args = append(args, filter.StatusCode)
	} else if filter.StatusClass >= 1 && filter.StatusClass <= 5 {
		conditions = append(conditions, "status_code >= ? AND status_code < ?")
		args = append(args, filter.StatusClass*100, filter.StatusClass*100+100)
	}
	switch filter.Outcome {
	case "ok":
		conditions = append(conditions, "NOT "+failedExpr)
	case "failed":
		conditions = append(conditions, failedExpr)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// 统计总数
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM request_logs %s", whereClause)
	var total int
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// 分页查询列表
	query := fmt.Sprintf(`
		SELECT id, timestamp, method, path, model, account_id,
		       status_code, duration_ms, prompt_tokens, completion_tokens,
		       error_message, client_ip, user_agent
		FROM request_logs
		%s
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?
	`, whereClause)

	queryArgs := append(args, filter.PageSize, offset)
	rows, err := s.db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var list []RequestLogItem
	for rows.Next() {
		var it RequestLogItem
		var tsStr string
		if err := rows.Scan(
			&it.ID,
			&tsStr,
			&it.Method,
			&it.Path,
			&it.Model,
			&it.AccountID,
			&it.StatusCode,
			&it.DurationMs,
			&it.PromptTokens,
			&it.CompletionTokens,
			&it.ErrorMessage,
			&it.ClientIP,
			&it.UserAgent,
		); err != nil {
			continue
		}
		if t, err := time.Parse("2006-01-02 15:04:05", tsStr); err == nil {
			it.Timestamp = t
		} else if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			// 兼容历史行：早期版本曾以 RFC3339（time.Time 默认序列化）写入，
			// 当前写入统一为空格格式；读取端两种都认，避免时间列静默变零值。
			it.Timestamp = t
		}
		list = append(list, it)
	}

	return list, total, nil
}

// GetAggregatedStats 基于 SQLite 真实请求明细计算真实统计指标。
func (s *SQLiteStore) GetAggregatedStats() (*AggregatedStats, error) {
	if s == nil {
		return nil, errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return nil, errSQLiteUnavailable
	}

	stats := &AggregatedStats{
		ModelUsages: []ModelUsageStat{},
		TimeSeries:  []TimeSeriesPoint{},
	}

	// 1. 基础总览
	var total, fails int
	var avgLatency sql.NullFloat64
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN `+failedExpr+` THEN 1 ELSE 0 END), 0),
		       AVG(duration_ms),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0)
		FROM request_logs
	`).Scan(&total, &fails, &avgLatency, &stats.TotalPromptTokens, &stats.TotalCompletionTokens)
	if err != nil {
		return stats, err
	}

	stats.TotalRequests = total
	stats.Failures = fails
	if total > 0 {
		stats.SuccessRate = float64(total-fails) / float64(total) * 100
		if avgLatency.Valid {
			stats.AvgLatencyMs = int64(avgLatency.Float64)
		}
	} else {
		stats.SuccessRate = 100.0
	}

	// 2. 模型使用分布
	mRows, err := s.db.Query(`
		SELECT model, COUNT(*), COALESCE(AVG(duration_ms), 0),
		       COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM request_logs
		WHERE model != ''
		GROUP BY model
		ORDER BY COUNT(*) DESC
	`)
	if err == nil {
		defer mRows.Close()
		for mRows.Next() {
			var m ModelUsageStat
			var mAvg sql.NullFloat64
			if err := mRows.Scan(&m.Model, &m.Requests, &mAvg, &m.PromptTokens, &m.CompletionTokens); err == nil {
				if total > 0 {
					m.Percentage = float64(m.Requests) / float64(total) * 100
				}
				if mAvg.Valid {
					m.AvgLatencyMs = int64(mAvg.Float64)
				}
				stats.ModelUsages = append(stats.ModelUsages, m)
			}
		}
	}

	// 3. 近 24 小时趋势：按小时分桶，空桶补零，按时间先后排列。
	//    早期实现按 '%H:%M' 分桶后按字符串升序取前 24 个 —— 拿到的是
	//    "一天里最早的 24 个分钟"而不是最近 24 小时，走势图横轴与时间对不上。
	stats.TimeSeries = hourlySeries(s.db, time.Now())

	return stats, nil
}

// hourlySeries 统计截至 now 的最近 24 个整点小时桶（含当前小时）。
// 桶时间戳为 RFC3339 UTC，由前端换算本地时区展示。
func hourlySeries(db *sql.DB, now time.Time) []TimeSeriesPoint {
	const n = 24
	end := now.UTC().Truncate(time.Hour)
	start := end.Add(-(n - 1) * time.Hour)

	series := make([]TimeSeriesPoint, n)
	for i := range series {
		series[i].Timestamp = start.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)
	}

	rows, err := db.Query(`
		SELECT strftime('%Y-%m-%d %H:00:00', timestamp) AS bucket,
		       COUNT(*),
		       COALESCE(AVG(duration_ms), 0),
		       COALESCE(SUM(CASE WHEN `+failedExpr+` THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(prompt_tokens + completion_tokens), 0)
		FROM request_logs
		WHERE timestamp >= ?
		GROUP BY bucket
	`, start.Format("2006-01-02 15:04:05"))
	if err != nil {
		return series
	}
	defer rows.Close()

	for rows.Next() {
		var bucket string
		var count, fails int
		var avg float64
		var toks int64
		if err := rows.Scan(&bucket, &count, &avg, &fails, &toks); err != nil {
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", bucket)
		if err != nil {
			continue
		}
		i := int(t.Sub(start) / time.Hour)
		if i < 0 || i >= n {
			continue
		}
		p := &series[i]
		p.Requests = count
		p.Failures = fails
		p.Tokens = toks
		p.Latency = int64(avg)
		if count > 0 {
			p.ErrorRate = float64(fails) / float64(count) * 100
			p.QPS = float64(count) / 3600
		}
	}
	return series
}

// -------------------------------------------------------------
// Chat 调试工作台会话与消息持久化 (彻底替换浏览器 LocalStorage)
// -------------------------------------------------------------

// ChatSessionRecord 会话实体
type ChatSessionRecord struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ChatMessageRecord 消息实体
type ChatMessageRecord struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Reasoning string    `json:"reasoning"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// ListChatSessions 查询所有持久化调试会话。
func (s *SQLiteStore) ListChatSessions() ([]ChatSessionRecord, error) {
	if s == nil {
		return nil, errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return nil, errSQLiteUnavailable
	}

	rows, err := s.db.Query(`
		SELECT id, title, model, reasoning_effort, created_at, updated_at
		FROM chat_sessions
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []ChatSessionRecord
	for rows.Next() {
		var r ChatSessionRecord
		var cStr, uStr string
		if err := rows.Scan(&r.ID, &r.Title, &r.Model, &r.ReasoningEffort, &cStr, &uStr); err != nil {
			continue
		}
		if t, err := time.Parse("2006-01-02 15:04:05", cStr); err == nil {
			r.CreatedAt = t
		}
		if t, err := time.Parse("2006-01-02 15:04:05", uStr); err == nil {
			r.UpdatedAt = t
		}
		list = append(list, r)
	}
	return list, nil
}

// SaveChatSession 保存或更新会话。
func (s *SQLiteStore) SaveChatSession(sess ChatSessionRecord) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}

	query := `
	INSERT INTO chat_sessions (id, title, model, reasoning_effort, created_at, updated_at)
	VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	ON CONFLICT(id) DO UPDATE SET
		title = excluded.title,
		model = excluded.model,
		reasoning_effort = excluded.reasoning_effort,
		updated_at = CURRENT_TIMESTAMP;
	`
	_, err := s.db.Exec(query, sess.ID, sess.Title, sess.Model, sess.ReasoningEffort)
	return err
}

// DeleteChatSession 删除会话及级联消息。
func (s *SQLiteStore) DeleteChatSession(id string) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}

	_, _ = s.db.Exec("DELETE FROM chat_messages WHERE session_id = ?", id)
	_, err := s.db.Exec("DELETE FROM chat_sessions WHERE id = ?", id)
	return err
}

// ListChatMessages 获取会话的消息历史。
func (s *SQLiteStore) ListChatMessages(sessionID string) ([]ChatMessageRecord, error) {
	if s == nil {
		return nil, errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return nil, errSQLiteUnavailable
	}

	rows, err := s.db.Query(`
		SELECT id, session_id, role, content, reasoning, status, created_at
		FROM chat_messages
		WHERE session_id = ?
		ORDER BY created_at ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []ChatMessageRecord
	for rows.Next() {
		var m ChatMessageRecord
		var cStr string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Reasoning, &m.Status, &cStr); err != nil {
			continue
		}
		if t, err := time.Parse("2006-01-02 15:04:05", cStr); err == nil {
			m.CreatedAt = t
		}
		list = append(list, m)
	}
	return list, nil
}

// SaveChatMessage 保存一条消息。
func (s *SQLiteStore) SaveChatMessage(m ChatMessageRecord) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}

	if m.ID == "" {
		m.ID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}

	query := `
	INSERT INTO chat_messages (id, session_id, role, content, reasoning, status, created_at)
	VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	ON CONFLICT(id) DO UPDATE SET
		content = excluded.content,
		reasoning = excluded.reasoning,
		status = excluded.status;
	`
	_, err := s.db.Exec(query, m.ID, m.SessionID, m.Role, m.Content, m.Reasoning, m.Status)
	if err == nil {
		_, _ = s.db.Exec("UPDATE chat_sessions SET updated_at = CURRENT_TIMESTAMP WHERE id = ?", m.SessionID)
	}
	return err
}

// -------------------------------------------------------------
// 对外 API 密钥 (API Keys) 持久化管理
// -------------------------------------------------------------

// APIKeyItem 对外调用 API 密钥。
type APIKeyItem struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// DefaultPublicAPIKey 是早期版本自动种入的默认 Key。它写死在源码里，
// 等于公开；启动时若发现仍在使用会告警，提示轮换。
const DefaultPublicAPIKey = "sk-prism-live-master"

// NewAPIKey 生成密码学随机的 API Key（128 bit）。
//
// 早期用纳秒时间戳拼 Key，能被枚举猜中。
func NewAPIKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return "sk-prism-" + hex.EncodeToString(b[:])
}

// ListAPIKeys 列出所有已授权的 API Keys。
func (s *SQLiteStore) ListAPIKeys() ([]APIKeyItem, error) {
	if s == nil {
		return nil, errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return nil, errSQLiteUnavailable
	}

	rows, err := s.db.Query(`
		SELECT key, name, created_at
		FROM api_keys
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []APIKeyItem
	for rows.Next() {
		var it APIKeyItem
		var cStr string
		if err := rows.Scan(&it.Key, &it.Name, &cStr); err != nil {
			continue
		}
		if t, err := time.Parse("2006-01-02 15:04:05", cStr); err == nil {
			it.CreatedAt = t
		}
		list = append(list, it)
	}
	return list, nil
}

// SaveAPIKey 保存新的 API Key。
func (s *SQLiteStore) SaveAPIKey(item APIKeyItem) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}

	if item.Key == "" {
		item.Key = NewAPIKey()
	}
	if item.Name == "" {
		item.Name = "新建访问令牌"
	}

	query := `
	INSERT INTO api_keys (key, name, created_at)
	VALUES (?, ?, CURRENT_TIMESTAMP)
	ON CONFLICT(key) DO UPDATE SET name = excluded.name;
	`
	_, err := s.db.Exec(query, item.Key, item.Name)
	return err
}

// DeleteAPIKey 删除指定的 API Key。
func (s *SQLiteStore) DeleteAPIKey(key string) error {
	if s == nil {
		return errSQLiteUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁内检查：Close() 会在持锁状态下把 s.db 置 nil，
	// 锁外检查存在 TOCTOU 竞态（ready() 过后 db 被清空 → exec(nil) panic，
	// 2026-10-03 CI 并发测试实证）。
	if s.db == nil {
		return errSQLiteUnavailable
	}

	_, err := s.db.Exec("DELETE FROM api_keys WHERE key = ?", key)
	return err
}
