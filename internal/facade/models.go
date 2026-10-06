package facade

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// ModelInfo 是 /v1/models 的条目。
//
// Name 是展示名（label），与 ID 分离：ID 是调用时要传的名字，
// Name 给 UI 列表用。没有展示名的模型不带 name 字段。
//
// 后面几项是网关的扩展字段（OpenAI 客户端会忽略）：控制台据此组模型与推理档位下拉框，
// 不再从名字里猜。
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Name    string `json:"name,omitempty"`

	// UpstreamModel 是这个名字对应的上游模型；ReasoningEffort 是它自带的推理档位。
	UpstreamModel   string `json:"upstream_model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ReasoningEfforts / DefaultReasoningEffort 只出现在主条目（名字就是上游模型 id）上：
	// 该模型支持的档位（由低到高）与默认档位。
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	// Default 标出请求不带模型时用的那个名字。
	Default bool `json:"default,omitempty"`
}

// ModelList 是 /v1/models 的返回。
type ModelList struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// handleModels 列出对外可用的模型名。
//
// 上游清单已知时按上游的顺序列：每个模型一个主条目，外加每个非默认档位一个 "<id>-<档位>" 变体；
// 再列 facade.models 里手写的映射（映射到已下架模型的不列）。清单未知时只列手写映射与默认模型。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ModelList{Object: "list", Data: h.modelInfos()})
}

// handleModelByID 返回单个模型信息。
func (h *Handler) handleModelByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "缺少模型 ID")
		return
	}
	for _, m := range h.modelInfos() {
		if m.ID == id {
			writeJSON(w, http.StatusOK, m)
			return
		}
	}
	writeError(w, http.StatusNotFound, "invalid_request_error",
		"未知模型 "+id+"（可用模型见 GET /v1/models）")
}

func (h *Handler) modelInfos() []ModelInfo {
	f := &h.cfg.Facade
	cat := h.catalog()
	snap := cat.current()
	created := time.Now().Unix()
	def := cat.defaultName(f)

	var out []ModelInfo
	seen := map[string]bool{}
	add := func(m ModelInfo) {
		if m.ID == "" || seen[m.ID] {
			return
		}
		seen[m.ID] = true
		m.Object, m.Created, m.OwnedBy = "model", created, "oaiprism"
		m.Default = m.ID == def
		out = append(out, m)
	}

	if snap != nil {
		for _, um := range snap.Models {
			values, defEffort := cat.efforts(f, um.ID)
			add(ModelInfo{ID: um.ID, Name: um.Label, UpstreamModel: um.ID, ReasoningEffort: defEffort,
				ReasoningEfforts: values, DefaultReasoningEffort: defEffort})
			for _, e := range values {
				if e == defEffort {
					continue
				}
				add(ModelInfo{ID: um.ID + "-" + e, Name: um.Label + " (" + cat.effortLabel(e) + ")",
					UpstreamModel: um.ID, ReasoningEffort: e})
			}
		}
	}

	names := make([]string, 0, len(f.Models))
	for name := range f.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := f.Models[name]
		target := m.Model
		if target == "" {
			target = name
		}
		if snap != nil && !snap.has(target) {
			continue // 映射到的模型已下架：请求会被换成默认模型，列出来只会误导
		}
		add(mappingInfo(name, m, target))
	}
	if def != "" && !seen[def] {
		add(ModelInfo{ID: def})
	}
	return out
}

func mappingInfo(name string, m config.ModelMapping, target string) ModelInfo {
	return ModelInfo{ID: name, Name: m.Label, UpstreamModel: target, ReasoningEffort: m.ReasoningEffort}
}
