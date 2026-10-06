package facade

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/prism"
)

// 2026-10-06 上游的实际清单与 Prism 网页的推理强度选项。
func liveSnapshot() *catalogSnapshot {
	return &catalogSnapshot{
		Models: []prism.UpstreamModel{
			{ID: "gpt-5.6-sol", Label: "5.6 Sol"}, {ID: "gpt-5.6-terra", Label: "5.6 Terra"}, {ID: "gpt-6-luna", Label: "6 Luna"},
		},
		Efforts: &prism.EffortOptions{
			Values:  []string{"low", "medium", "high", "xhigh"},
			Labels:  map[string]string{"low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra high"},
			Default: "medium",
		},
	}
}

func catalogWith(cfg *config.Config, snap *catalogSnapshot, fetch catalogFetcher) *ModelCatalog {
	c := newModelCatalog(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), fetch)
	if snap != nil {
		c.snap.Store(snap.index())
	}
	return c
}

func TestCatalogResolve(t *testing.T) {
	cfg := config.Default()
	f := &cfg.Facade
	f.Models = map[string]config.ModelMapping{
		"fast":  {Model: "gpt-5.6-terra", ReasoningEffort: "low"},
		"stale": {Model: "gpt-6.1-sol", ReasoningEffort: "high"},
	}
	c := catalogWith(cfg, liveSnapshot(), nil)

	cases := []struct {
		name, requested, effort string
		want                    resolvedModel
	}{
		{"空名用上游清单第一个与默认档位", "", "", resolvedModel{Name: "gpt-5.6-sol", Model: "gpt-5.6-sol", Effort: "medium"}},
		{"主条目", "gpt-6-luna", "", resolvedModel{Name: "gpt-6-luna", Model: "gpt-6-luna", Effort: "medium"}},
		{"档位变体", "gpt-6-luna-xhigh", "", resolvedModel{Name: "gpt-6-luna-xhigh", Model: "gpt-6-luna", Effort: "xhigh"}},
		{"显式档位优先于名字后缀", "gpt-6-luna-xhigh", "low", resolvedModel{Name: "gpt-6-luna-xhigh", Model: "gpt-6-luna", Effort: "low"}},
		{"大小写不敏感", "gpt-5.6-sol", "HIGH", resolvedModel{Name: "gpt-5.6-sol", Model: "gpt-5.6-sol", Effort: "high"}},
		{"minimal 落到最低档", "gpt-5.6-sol", "minimal", resolvedModel{Name: "gpt-5.6-sol", Model: "gpt-5.6-sol", Effort: "low"}},
		{"不认识的档位用默认档", "gpt-5.6-sol", "turbo", resolvedModel{Name: "gpt-5.6-sol", Model: "gpt-5.6-sol", Effort: "medium"}},
		{"手写映射", "fast", "", resolvedModel{Name: "fast", Model: "gpt-5.6-terra", Effort: "low"}},
		{"已下架的模型换默认模型并保留档位", "gpt-6.1-sol-xhigh", "",
			resolvedModel{Name: "gpt-5.6-sol", Model: "gpt-5.6-sol", Effort: "xhigh", Substituted: true}},
		{"已下架的模型", "gpt-6.1-sol", "", resolvedModel{Name: "gpt-5.6-sol", Model: "gpt-5.6-sol", Effort: "medium", Substituted: true}},
		{"映射到已下架的模型", "stale", "", resolvedModel{Name: "gpt-5.6-sol", Model: "gpt-5.6-sol", Effort: "high", Substituted: true}},
		{"认不出的名字", "claude-sonnet-4-5", "", resolvedModel{Name: "gpt-5.6-sol", Model: "gpt-5.6-sol", Effort: "medium", Substituted: true}},
	}
	for _, tc := range cases {
		if got := c.resolve(f, tc.requested, tc.effort); got != tc.want {
			t.Errorf("%s: resolve(%q, %q) = %+v, want %+v", tc.name, tc.requested, tc.effort, got, tc.want)
		}
	}

	// 配置的默认模型仍在售就用它；已下架就回落到清单第一个。
	f.DefaultModel = "gpt-6-luna-high"
	if got := c.resolve(f, "", ""); got.Model != "gpt-6-luna" || got.Effort != "high" {
		t.Errorf("配置的默认模型: %+v", got)
	}
	f.DefaultModel = "gpt-6.1-sol"
	if got := c.resolve(f, "", ""); got.Model != "gpt-5.6-sol" || got.Name != "gpt-5.6-sol" {
		t.Errorf("默认模型已下架: %+v", got)
	}
}

func TestCatalogResolve_Unknown(t *testing.T) {
	cfg := config.Default()
	f := &cfg.Facade

	// 清单未知（还没拉到）：原样透传，不加档位。
	var c *ModelCatalog
	if got := c.resolve(f, "gpt-6.1-sol-high", ""); got != (resolvedModel{Name: "gpt-6.1-sol-high", Model: "gpt-6.1-sol-high"}) {
		t.Errorf("清单未知应原样透传: %+v", got)
	}
	// 配置了兜底档位时照样补默认档位、修正不支持的档位。
	f.ReasoningEfforts, f.DefaultReasoningEffort = []string{"low", "medium", "high"}, "medium"
	if got := c.resolve(f, "x", ""); got.Effort != "medium" {
		t.Errorf("兜底默认档位: %+v", got)
	}
	if got := c.resolve(f, "x", "none"); got.Effort != "low" {
		t.Errorf("兜底档位修正: %+v", got)
	}

	// 清单里只有模型、档位未知：名字后缀原样当档位。
	c = catalogWith(config.Default(), &catalogSnapshot{Models: []prism.UpstreamModel{{ID: "m", Label: "M"}}}, nil)
	if got := c.resolve(&config.Default().Facade, "m-ultra", ""); got != (resolvedModel{Name: "m-ultra", Model: "m", Effort: "ultra"}) {
		t.Errorf("档位未知时的后缀: %+v", got)
	}
}

func TestCatalogModelInfos(t *testing.T) {
	cfg := config.Default()
	cfg.Facade.Models = map[string]config.ModelMapping{
		"fast":  {Model: "gpt-5.6-terra", ReasoningEffort: "low", Label: "Fast"},
		"stale": {Model: "gpt-6.1-sol"},
	}
	r := &Runner{catalog: catalogWith(cfg, liveSnapshot(), nil)}
	h := &Handler{cfg: cfg, runner: r}

	rec := httptest.NewRecorder()
	h.handleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var out ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	want := "gpt-5.6-sol,gpt-5.6-sol-low,gpt-5.6-sol-high,gpt-5.6-sol-xhigh," +
		"gpt-5.6-terra,gpt-5.6-terra-low,gpt-5.6-terra-high,gpt-5.6-terra-xhigh," +
		"gpt-6-luna,gpt-6-luna-low,gpt-6-luna-high,gpt-6-luna-xhigh,fast"
	if got := strings.Join(ids, ","); got != want {
		t.Fatalf("ids = %s\nwant  %s", got, want)
	}
	if out.Data[0].Name != "5.6 Sol" || !out.Data[0].Default || out.Data[3].Name != "5.6 Sol (Extra high)" {
		t.Fatalf("展示名 / 默认标记不对: %+v", out.Data[:4])
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models/gpt-6-luna-high", nil)
	req.SetPathValue("id", "gpt-6-luna-high")
	h.handleModelByID(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reasoning_effort":"high"`) {
		t.Fatalf("单个模型: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models/gpt-6.1-sol", nil)
	req.SetPathValue("id", "gpt-6.1-sol")
	h.handleModelByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("已下架的模型应 404，得到 %d", rec.Code)
	}
}

type memCatalogStore struct {
	data []byte
	at   time.Time
}

func (s *memCatalogStore) SaveModelCatalog(data []byte, at time.Time) error {
	s.data, s.at = data, at
	return nil
}
func (s *memCatalogStore) LoadModelCatalog() ([]byte, time.Time, error) { return s.data, s.at, nil }

func TestCatalogRefresh(t *testing.T) {
	cfg := config.Default()
	calls := 0
	var prevSeen *prism.EffortOptions
	next := liveSnapshot()
	fetch := func(ctx context.Context, prev *prism.EffortOptions) (*catalogSnapshot, error) {
		calls++
		prevSeen = prev
		if calls == 2 {
			return nil, errors.New("boom")
		}
		s := *next
		if calls == 3 {
			s.Efforts = nil // 档位这次没解析出来
			s.Models = s.Models[1:]
		}
		return &s, nil
	}
	st := &memCatalogStore{}
	c := catalogWith(cfg, nil, fetch)
	c.UseStore(st)

	if err := c.Refresh(context.Background(), 0); err != nil || !c.current().has("gpt-5.6-sol") {
		t.Fatalf("第一次刷新: %v", err)
	}
	if len(st.data) == 0 {
		t.Fatal("清单应落盘")
	}
	if err := c.Refresh(context.Background(), time.Minute); err != nil || calls != 1 {
		t.Fatalf("一分钟内不应重复拉取: calls=%d err=%v", calls, err)
	}
	if err := c.Refresh(context.Background(), 0); err == nil || !c.current().has("gpt-5.6-sol") {
		t.Fatalf("拉取失败应沿用现有清单: %v", err)
	}
	if st := c.Status(); st.LastError != "boom" || len(st.Models) != 3 {
		t.Fatalf("状态: %+v", st)
	}
	if err := c.Refresh(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if prevSeen == nil || c.current().Efforts == nil || c.current().has("gpt-5.6-sol") {
		t.Fatalf("档位没解析出来时应沿用上次的；下架的模型应移除: %+v", c.current())
	}

	// 重启：从落盘的清单恢复。
	c2 := catalogWith(cfg, nil, nil)
	c2.UseStore(st)
	if got := c2.resolve(&cfg.Facade, "", ""); got.Model != "gpt-5.6-terra" || got.Effort != "medium" {
		t.Fatalf("落盘的清单: %+v", got)
	}
}
