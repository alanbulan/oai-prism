package prism

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// 上游在售模型清单与推理档位。
//
// Prism 网页的模型下拉框先取 GET /api/inference/models（按账号下发），取不到才退回 Statsig 动态
// 配置 prism_codex_models（2026-10-06 前端代码）。清单每项只有 id 与 label；推理档位不在清单里，
// 写在前端代码的下拉框选项里（[{value:"low",labelKey:"reasoningEffortLowLabel"},…]），默认档位是
// 前端存 localStorage 时的回落值，档位的展示名在页面内嵌的 i18n 文案里。网关照这几处解析，
// 不在代码或配置里写死模型名与档位 —— 上游下架模型（2026-10-06 的 gpt-6.1-sol）、调整档位时跟着变。

// PathInferenceModels 是 Prism 前端拉取本账号可用模型的接口。
const PathInferenceModels = "/api/inference/models"

// UpstreamModel 是上游模型清单里的一项。
//
// 上游目前只给 id 与 label；推理档位字段是预留的：哪天上游按模型下发档位，网关直接用。
type UpstreamModel struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Efforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultEffort string   `json:"default_reasoning_effort,omitempty"`
}

// InferenceModels 取本账号的在售模型清单（按上游给的顺序；前端把第一个当默认模型）。
func (c *Client) InferenceModels(ctx context.Context, p Principal) ([]UpstreamModel, error) {
	_, _, raw, err := c.doJSON(ctx, p, http.MethodGet, PathInferenceModels, nil, nil, "application/json")
	if err != nil {
		return nil, err
	}
	return ParseModelCatalog(raw)
}

// ParseModelCatalog 解析模型清单：数组本身，或 {"models":[…]}（Statsig prism_codex_models 的形态）。
//
// 与前端同一套规则：id、label 都必须是非空字符串，重复的 id 只留第一个。
func ParseModelCatalog(raw []byte) ([]UpstreamModel, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrapped struct {
			Models []json.RawMessage `json:"models"`
			Data   []json.RawMessage `json:"data"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return nil, fmt.Errorf("模型清单不是 JSON 数组: %w", err)
		}
		list = wrapped.Models
		if len(list) == 0 {
			list = wrapped.Data
		}
	}
	out := make([]UpstreamModel, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		var m map[string]any
		if json.Unmarshal(item, &m) != nil {
			continue
		}
		id, _ := m["id"].(string)
		label, _ := m["label"].(string)
		id, label = strings.TrimSpace(id), strings.TrimSpace(label)
		if id == "" || label == "" || seen[id] {
			continue
		}
		seen[id] = true
		um := UpstreamModel{ID: id, Label: label}
		for _, k := range []string{"reasoning_efforts", "supported_reasoning_efforts", "reasoning_effort_options"} {
			if um.Efforts = effortValues(m[k]); len(um.Efforts) > 0 {
				break
			}
		}
		if d, _ := m["default_reasoning_effort"].(string); d != "" {
			um.DefaultEffort = strings.TrimSpace(d)
		}
		out = append(out, um)
	}
	if len(out) == 0 {
		return nil, errors.New("模型清单为空")
	}
	return out, nil
}

// effortValues 读档位列表：字符串数组，或 [{value|effort|id: …}] 形态。
func effortValues(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, e := range arr {
		var s string
		switch t := e.(type) {
		case string:
			s = t
		case map[string]any:
			for _, k := range []string{"value", "effort", "id"} {
				if s, _ = t[k].(string); s != "" {
					break
				}
			}
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// EffortOptions 是 Prism 网页"推理强度"下拉框的选项（由低到高）与默认档位。
type EffortOptions struct {
	Values  []string          `json:"values"`
	Labels  map[string]string `json:"labels,omitempty"` // 档位 → 英文展示名
	Default string            `json:"default,omitempty"`
	Script  string            `json:"script,omitempty"` // 选项所在的前端脚本（文件名带内容哈希）
}

var (
	reScriptSrc   = regexp.MustCompile(`/_next/static/[^"'\s<>\\]+?\.js`)
	reEffortList  = regexp.MustCompile(`\[\{value:"[a-z_]+",labelKey:"[A-Za-z]+"\}(?:,\{value:"[a-z_]+",labelKey:"[A-Za-z]+"\})*\]`)
	reEffortItem  = regexp.MustCompile(`\{value:"([a-z_]+)",labelKey:"([A-Za-z]+)"\}`)
	reEffortStore = regexp.MustCompile(`"prism:ai-assistant:reasoning-effort:v\d+",([A-Za-z_$][\w$]*)`)
)

// FrontendEffortOptions 从 Prism 网页解析推理强度选项。
//
// 取首页，依次下载页面引用的脚本，直到找到下拉框选项。prev 是上次的结果：页面仍引用
// 同一个脚本时直接沿用（脚本名带内容哈希，内容不会变），不重复下载几 MB 的前端代码。
func (c *Client) FrontendEffortOptions(ctx context.Context, p Principal, prev *EffortOptions) (*EffortOptions, error) {
	html, err := c.getPage(ctx, p, "/", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if err != nil {
		return nil, err
	}
	var scripts []string
	seen := map[string]bool{}
	for _, m := range reScriptSrc.FindAll(html, -1) {
		if s := string(m); !seen[s] {
			seen[s] = true
			scripts = append(scripts, s)
		}
	}
	if prev != nil && prev.Script != "" && seen[prev.Script] {
		out := *prev
		return &out, nil
	}
	lastErr := errors.New("页面没有引用任何脚本")
	for _, s := range scripts {
		js, err := c.getPage(ctx, p, s, "*/*")
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if opts := ParseEffortOptions(js, html); opts != nil {
			opts.Script = s
			return opts, nil
		}
		lastErr = fmt.Errorf("%d 个脚本里都没有推理强度选项", len(scripts))
	}
	return nil, lastErr
}

// ParseEffortOptions 从一段前端脚本里取推理强度选项；没有时返回 nil。
// page 是首页 HTML，档位的英文展示名从里面内嵌的 i18n 文案取（取不到就不填）。
func ParseEffortOptions(js, page []byte) *EffortOptions {
	var list []byte
	for _, m := range reEffortList.FindAll(js, -1) {
		if bytes.Contains(m, []byte(`labelKey:"reasoningEffort`)) {
			list = m
			break
		}
	}
	if list == nil {
		return nil
	}
	opts := &EffortOptions{Labels: map[string]string{}}
	for _, it := range reEffortItem.FindAllSubmatch(list, -1) {
		v := string(it[1])
		opts.Values = append(opts.Values, v)
		if l := i18nText(page, string(it[2])); l != "" {
			opts.Labels[v] = l
		}
	}
	if len(opts.Values) == 0 {
		return nil
	}
	if len(opts.Labels) == 0 {
		opts.Labels = nil
	}
	opts.Default = frontendDefaultEffort(js, opts.Values)
	return opts
}

// frontendDefaultEffort 找前端存档位时的回落值：localStorage 键后面跟的变量，再找它的字面量赋值。
func frontendDefaultEffort(js []byte, values []string) string {
	loc := reEffortStore.FindSubmatchIndex(js)
	if loc == nil {
		return ""
	}
	name := string(js[loc[2]:loc[3]])
	re := regexp.MustCompile(`(?:^|[^\w$])` + regexp.QuoteMeta(name) + `="([a-z_]+)"`)
	best := ""
	for _, m := range re.FindAllSubmatchIndex(js, -1) {
		v := string(js[m[2]:m[3]])
		if !containsString(values, v) {
			continue
		}
		// 压缩后的变量名在别的模块里可能重名：取键之前最近的一处赋值。
		if m[0] < loc[0] || best == "" {
			best = v
		}
	}
	return best
}

// i18nText 在页面内嵌的 i18n 文案里找 key 的英文值（页面里中英文都有，取纯 ASCII 的那个）。
func i18nText(page []byte, key string) string {
	if len(page) == 0 {
		return ""
	}
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `\\?"\s*:\s*\\?"([^"\\]{1,40})`)
	first := ""
	for _, m := range re.FindAllSubmatch(page, 8) {
		v := strings.TrimSpace(string(m[1]))
		if v == "" {
			continue
		}
		if first == "" {
			first = v
		}
		if isASCII(v) {
			return v
		}
	}
	return first
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// getPage 像浏览器那样取一个页面或静态脚本，返回响应体（上限 32 MiB）。
func (c *Client) getPage(ctx context.Context, p Principal, path, accept string) ([]byte, error) {
	hdr := c.buildHeaders(p, "", accept)
	if strings.HasPrefix(accept, "text/html") {
		hdr["Sec-Fetch-Dest"], hdr["Sec-Fetch-Mode"], hdr["Sec-Fetch-Site"] = "document", "navigate", "none"
	} else {
		hdr["Sec-Fetch-Dest"], hdr["Sec-Fetch-Mode"] = "script", "no-cors"
	}
	resp, err := c.Do(ctx, p, http.MethodGet, path, headerFromMap(hdr), nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}
