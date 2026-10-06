package facade

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/prism"
)

// 上游在售模型清单。
//
// 对外模型名、展示名、推理档位和默认模型都跟着上游走，不写死在代码或配置里：
//   - 模型：GET /api/inference/models（Prism 网页模型下拉框的来源），按上游给的顺序，第一个是默认；
//   - 档位：Prism 网页推理强度下拉框的选项与默认值（解析前端代码，见 prism/catalog.go）。
//
// 对外名 = 上游模型 id，外加每个非默认档位一个 "<id>-<档位>" 变体（不认推理参数的客户端靠名字选档位）。
// 上游下架的模型（2026-10-06 的 gpt-6.1-sol）交上去只会换来 "Error while processing
// conversation (400)"，还会被当成沙箱未就绪重试十次、拖上五分钟 —— 清单里没有的模型改用默认模型。
// facade.models 里手写的映射优先；清单拉不到（还没拉过、上游出错）时一切照旧原样透传。

// CatalogStore 让清单落盘（*account.SQLiteStore 实现了它）。
type CatalogStore interface {
	SaveModelCatalog(data []byte, at time.Time) error
	LoadModelCatalog() ([]byte, time.Time, error)
}

// catalogSnapshot 是某一时刻的上游清单。
type catalogSnapshot struct {
	Models    []prism.UpstreamModel `json:"models"`
	Efforts   *prism.EffortOptions  `json:"efforts,omitempty"`
	Account   string                `json:"account,omitempty"`
	FetchedAt time.Time             `json:"fetched_at"`

	byID map[string]int
}

func (s *catalogSnapshot) index() *catalogSnapshot {
	s.byID = make(map[string]int, len(s.Models))
	for i, m := range s.Models {
		s.byID[m.ID] = i
	}
	return s
}

func (s *catalogSnapshot) has(id string) bool {
	if s == nil {
		return false
	}
	_, ok := s.byID[id]
	return ok
}

func (s *catalogSnapshot) model(id string) *prism.UpstreamModel {
	if s == nil {
		return nil
	}
	if i, ok := s.byID[id]; ok {
		return &s.Models[i]
	}
	return nil
}

func (s *catalogSnapshot) ids() []string {
	out := make([]string, len(s.Models))
	for i, m := range s.Models {
		out[i] = m.ID
	}
	return out
}

// catalogFetcher 拉一份新清单。prev 是上次解析出的推理档位（前端没换版本时直接沿用）。
type catalogFetcher func(ctx context.Context, prev *prism.EffortOptions) (*catalogSnapshot, error)

// ModelCatalog 维护上游清单并据此解析模型名。nil 接收者可用（等同于"清单未知"）。
type ModelCatalog struct {
	cfg   *config.Config
	log   *slog.Logger
	fetch catalogFetcher
	store CatalogStore

	snap atomic.Pointer[catalogSnapshot]

	mu      sync.Mutex // 串行化刷新
	lastTry time.Time
	lastErr error
	errMsg  atomic.Pointer[string] // 最近一次拉取的错误（给控制台看，不用等刷新锁）

	warned atomic.Pointer[sync.Map] // 已告警过的名字；换一份清单重新告警
}

func newModelCatalog(cfg *config.Config, log *slog.Logger, fetch catalogFetcher) *ModelCatalog {
	c := &ModelCatalog{cfg: cfg, log: log, fetch: fetch}
	c.warned.Store(&sync.Map{})
	return c
}

// UseStore 让清单落盘，并载入上次保存的那份（网关重启后立即可用）。
func (c *ModelCatalog) UseStore(st CatalogStore) {
	if c == nil || st == nil {
		return
	}
	c.store = st
	data, at, err := st.LoadModelCatalog()
	if err != nil || len(data) == 0 {
		return
	}
	var s catalogSnapshot
	if json.Unmarshal(data, &s) != nil || len(s.Models) == 0 {
		return
	}
	if s.FetchedAt.IsZero() {
		s.FetchedAt = at
	}
	if c.snap.CompareAndSwap(nil, s.index()) {
		c.log.Info("已载入落盘的上游模型清单", "models", strings.Join(s.ids(), ","), "fetched_at", s.FetchedAt.Format(time.RFC3339))
	}
}

// Start 启动后台刷新：立即拉一次，之后按 model_catalog.refresh_interval 定时刷新。
func (c *ModelCatalog) Start(ctx context.Context) {
	if c == nil || !c.cfg.Facade.ModelCatalog.Enabled {
		return
	}
	go func() {
		_ = c.Refresh(ctx, 0)
		t := time.NewTicker(c.cfg.Facade.ModelCatalog.RefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = c.Refresh(ctx, 0)
			}
		}
	}()
}

// Refresh 从上游拉一份新清单。距上次尝试不到 minGap 时不拉，返回上次的结果。
func (c *ModelCatalog) Refresh(ctx context.Context, minGap time.Duration) error {
	if c == nil || c.fetch == nil || !c.cfg.Facade.ModelCatalog.Enabled {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lastTry.IsZero() && time.Since(c.lastTry) < minGap {
		return c.lastErr
	}
	c.lastTry = time.Now()

	prev := c.snap.Load()
	var prevEfforts *prism.EffortOptions
	if prev != nil {
		prevEfforts = prev.Efforts
	}
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	next, err := c.fetch(fctx, prevEfforts)
	c.lastErr = err
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	c.errMsg.Store(&msg)
	if err != nil {
		c.log.Warn("拉取上游模型清单失败，沿用现有清单", "err", err, "have", prev != nil)
		return err
	}
	if next.Efforts == nil {
		next.Efforts = prevEfforts // 档位没解析出来：沿用上次的
	}
	next.index()
	c.snap.Store(next)
	c.warned.Store(&sync.Map{})

	switch {
	case prev == nil:
		c.log.Info("上游模型清单", "models", strings.Join(next.ids(), ","), "efforts", effortSummary(next.Efforts), "account", next.Account)
	case strings.Join(prev.ids(), ",") != strings.Join(next.ids(), ","):
		var added, removed []string
		for _, id := range next.ids() {
			if !prev.has(id) {
				added = append(added, id)
			}
		}
		for _, id := range prev.ids() {
			if !next.has(id) {
				removed = append(removed, id)
			}
		}
		c.log.Warn("上游在售模型有变化", "models", strings.Join(next.ids(), ","),
			"added", strings.Join(added, ","), "removed", strings.Join(removed, ","))
	}
	if c.store != nil {
		if data, err := json.Marshal(next); err == nil {
			if err := c.store.SaveModelCatalog(data, next.FetchedAt); err != nil {
				c.log.Warn("上游模型清单落盘失败", "err", err)
			}
		}
	}
	return nil
}

func effortSummary(e *prism.EffortOptions) string {
	if e == nil {
		return ""
	}
	return strings.Join(e.Values, ",") + " (默认 " + e.Default + ")"
}

// CatalogStatus 是清单的当前状态（控制台 /admin/models/catalog）。
type CatalogStatus struct {
	Enabled   bool                  `json:"enabled"`
	Models    []prism.UpstreamModel `json:"models"`
	Efforts   *prism.EffortOptions  `json:"efforts,omitempty"`
	Account   string                `json:"account,omitempty"`
	FetchedAt *time.Time            `json:"fetched_at,omitempty"`
	LastError string                `json:"last_error,omitempty"`
}

// Status 返回清单的当前状态。
func (c *ModelCatalog) Status() CatalogStatus {
	st := CatalogStatus{Models: []prism.UpstreamModel{}}
	if c == nil {
		return st
	}
	st.Enabled = c.cfg.Facade.ModelCatalog.Enabled
	if snap := c.current(); snap != nil {
		st.Models, st.Efforts, st.Account = snap.Models, snap.Efforts, snap.Account
		at := snap.FetchedAt
		st.FetchedAt = &at
	}
	if m := c.errMsg.Load(); m != nil {
		st.LastError = *m
	}
	return st
}

func (c *ModelCatalog) current() *catalogSnapshot {
	if c == nil {
		return nil
	}
	return c.snap.Load()
}

// warnOnce 对同一个名字在同一份清单下只告警一次（客户端配错模型时每个请求都会走到这里）。
func (c *ModelCatalog) warnOnce(key, msg string, args ...any) {
	if c == nil {
		return
	}
	if _, loaded := c.warned.Load().LoadOrStore(key, true); !loaded {
		c.log.Warn(msg, args...)
	}
}

// ---------------------------- 解析 ----------------------------

// resolvedModel 是对外模型名解析的结果。
type resolvedModel struct {
	Name        string // 实际使用的对外模型名（替换后的）
	Model       string // 上游模型
	Effort      string
	Substituted bool // 请求的模型上游不在售，换成了默认模型
}

// resolve 把对外模型名翻译成上游模型与推理强度。effort 是客户端显式给的档位（优先于名字后缀）。
func (c *ModelCatalog) resolve(f *config.FacadeConfig, requested, effort string) resolvedModel {
	name := strings.TrimSpace(requested)
	if name == "" {
		name = c.defaultName(f)
	}
	out := resolvedModel{Name: name, Model: name}
	model, suffix := c.target(f, name)
	if model != "" {
		out.Model = model
	}
	if snap := c.current(); snap != nil && out.Model != "" && !snap.has(out.Model) {
		def := c.defaultName(f)
		defModel, defEffort := c.target(f, def)
		if defModel == "" {
			defModel = def
		}
		if snap.has(defModel) && defModel != out.Model {
			c.warnOnce("model:"+name, "请求的模型不在上游在售清单里，改用默认模型",
				"requested", name, "model", def, "available", strings.Join(snap.ids(), ","))
			if suffix == "" {
				suffix = c.trailingEffort(f, name) // gpt-6.1-sol-xhigh → 默认模型的 xhigh
			}
			if suffix == "" {
				suffix = defEffort
			}
			out.Name, out.Model, out.Substituted = def, defModel, true
		}
	}
	out.Effort = strings.TrimSpace(effort)
	if out.Effort == "" {
		out.Effort = suffix
	}
	out.Effort = c.fitEffort(f, out.Model, out.Effort)
	return out
}

// target 把对外名翻译成上游模型与名字自带的档位：先查手写映射，再按清单拆 "<id>-<档位>"。
// 认不出时返回空模型。
func (c *ModelCatalog) target(f *config.FacadeConfig, name string) (model, effort string) {
	if m, ok := f.Models[name]; ok {
		model = m.Model
		if model == "" {
			model = name
		}
		return model, m.ReasoningEffort
	}
	if base, e, ok := c.split(f, name); ok {
		return base, e
	}
	return "", ""
}

// split 按清单把 "<id>" 或 "<id>-<档位>" 拆成上游模型与档位。
func (c *ModelCatalog) split(f *config.FacadeConfig, name string) (model, effort string, ok bool) {
	snap := c.current()
	if snap == nil {
		return "", "", false
	}
	if snap.has(name) {
		return name, "", true
	}
	i := strings.LastIndexByte(name, '-')
	if i <= 0 {
		return "", "", false
	}
	base, e := name[:i], name[i+1:]
	if !snap.has(base) {
		return "", "", false
	}
	values, _ := c.efforts(f, base)
	if len(values) == 0 || containsFold(values, e) {
		return base, strings.ToLower(e), true // 档位未知时后缀原样当档位
	}
	return "", "", false
}

// trailingEffort 取名字末尾的档位后缀（只认已知档位）。
func (c *ModelCatalog) trailingEffort(f *config.FacadeConfig, name string) string {
	i := strings.LastIndexByte(name, '-')
	if i <= 0 {
		return ""
	}
	e := strings.ToLower(name[i+1:])
	values, _ := c.efforts(f, "")
	if containsFold(values, e) {
		return e
	}
	return ""
}

// efforts 返回模型支持的推理档位（由低到高）与默认档位：上游清单按模型给的 > Prism 网页的选项 >
// 配置兜底。都没有时返回空，档位原样交给上游。
func (c *ModelCatalog) efforts(f *config.FacadeConfig, model string) (values []string, def string) {
	snap := c.current()
	m := snap.model(model)
	var opts *prism.EffortOptions
	if snap != nil {
		opts = snap.Efforts
	}
	switch {
	case m != nil && len(m.Efforts) > 0:
		values = m.Efforts
	case opts != nil && len(opts.Values) > 0:
		values = opts.Values
	default:
		values = f.ReasoningEfforts
	}
	for _, d := range []string{modelDefaultEffort(m), optsDefault(opts), f.DefaultReasoningEffort} {
		if d != "" && (len(values) == 0 || containsFold(values, d)) {
			return values, d
		}
	}
	if len(values) > 0 {
		def = values[(len(values)-1)/2]
	}
	return values, def
}

func modelDefaultEffort(m *prism.UpstreamModel) string {
	if m == nil {
		return ""
	}
	return m.DefaultEffort
}

func optsDefault(o *prism.EffortOptions) string {
	if o == nil {
		return ""
	}
	return o.Default
}

// fitEffort 把档位落到模型支持的范围里：没给就用默认档位；不支持的 none / minimal 落到最低一档，
// 其余不认识的用默认档位。档位未知时原样返回。
func (c *ModelCatalog) fitEffort(f *config.FacadeConfig, model, effort string) string {
	values, def := c.efforts(f, model)
	if effort == "" {
		return def
	}
	if len(values) == 0 {
		return effort
	}
	if containsFold(values, effort) {
		return strings.ToLower(effort)
	}
	switch strings.ToLower(effort) {
	case "none", "minimal":
		return values[0]
	}
	if def != "" {
		return def
	}
	return effort
}

// effortLabel 是档位的展示名（取自 Prism 网页的文案，没有就用档位本身）。
func (c *ModelCatalog) effortLabel(e string) string {
	if snap := c.current(); snap != nil && snap.Efforts != nil {
		if l := snap.Efforts.Labels[e]; l != "" {
			return l
		}
	}
	return e
}

// defaultName 是默认的对外模型名：配置的 default_model 仍在售就用它，否则用上游清单第一个。
func (c *ModelCatalog) defaultName(f *config.FacadeConfig) string {
	def := strings.TrimSpace(f.DefaultModel)
	snap := c.current()
	if snap == nil || len(snap.Models) == 0 {
		return def
	}
	if def != "" {
		if m, _ := c.target(f, def); m != "" && snap.has(m) {
			return def
		}
	}
	first := snap.Models[0].ID
	if def != "" {
		c.warnOnce("default:"+def, "配置的默认模型已不在上游在售清单里，改用上游清单第一个",
			"default_model", def, "model", first)
	}
	return first
}

// known 报告对外名是否认得（手写映射、默认模型、清单里的模型及其档位变体）。
func (c *ModelCatalog) known(f *config.FacadeConfig, name string) bool {
	if _, ok := f.Models[name]; ok || name == f.DefaultModel {
		return true
	}
	if _, _, ok := c.split(f, name); ok {
		return true
	}
	for _, m := range f.Models {
		if m.Model == name {
			return true
		}
	}
	return false
}

// replacement 在上游对 model 回 "Error while processing conversation" 时确认它是否已下架：
// 刷新清单（一分钟内最多一次），仍在售返回 false（照常按沙箱未就绪重试），已下架返回默认模型。
func (c *ModelCatalog) replacement(ctx context.Context, f *config.FacadeConfig, model, effort string) (string, string, bool) {
	if c == nil {
		return "", "", false
	}
	_ = c.Refresh(ctx, time.Minute)
	snap := c.current()
	if snap == nil || snap.has(model) {
		return "", "", false
	}
	def := c.defaultName(f)
	m, e := c.target(f, def)
	if m == "" || m == model || !snap.has(m) {
		return "", "", false
	}
	if effort != "" {
		e = effort
	}
	return m, c.fitEffort(f, m, e), true
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// ---------------------------- 拉取 ----------------------------

// Catalog 返回上游在售模型清单。
func (r *Runner) Catalog() *ModelCatalog { return r.catalog }

// UseCatalogStore 让模型清单落盘，并载入上次保存的那份。
func (r *Runner) UseCatalogStore(st CatalogStore) { r.catalog.UseStore(st) }

// fetchCatalog 用第一个可用账号拉清单与推理档位。清单按账号下发；多账号时以第一个拉成功的为准。
func (r *Runner) fetchCatalog(ctx context.Context, prev *prism.EffortOptions) (*catalogSnapshot, error) {
	lastErr := fmt.Errorf("没有可用账号: %w", account.ErrNoAccount)
	for _, a := range r.pool.Accounts() {
		cred := a.Credential()
		if cred == nil || !cred.Usable() || a.AuthFailed() {
			continue
		}
		p := prism.Principal{Client: a.Client, Cred: cred, ExtraHeaders: cred.Headers, AccountID: a.ID}
		models, err := r.client.InferenceModels(ctx, p)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("账号 %s: %w", a.ID, err)
			continue
		}
		snap := &catalogSnapshot{Models: models, Account: a.ID, FetchedAt: time.Now()}
		if eff, err := r.client.FrontendEffortOptions(ctx, p, prev); err == nil {
			snap.Efforts = eff
		} else {
			r.log.Warn("解析 Prism 网页的推理档位失败，沿用上次的结果", "err", err)
		}
		return snap, nil
	}
	return nil, lastErr
}
