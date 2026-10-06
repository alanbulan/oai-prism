package prism

import (
	"reflect"
	"testing"
)

func TestParseModelCatalog(t *testing.T) {
	// /api/inference/models 的实测形态（2026-10-06）：数组，每项只有 id 与 label。
	got, err := ParseModelCatalog([]byte(`[{"id":"gpt-5.6-sol","label":"5.6 Sol"},{"id":"gpt-5.6-terra","label":"5.6 Terra"},` +
		`{"id":" gpt-6-luna ","label":"6 Luna"},{"id":"gpt-5.6-sol","label":"重复"},{"id":"no-label"},{"label":"no id"},"junk"]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []UpstreamModel{{ID: "gpt-5.6-sol", Label: "5.6 Sol"}, {ID: "gpt-5.6-terra", Label: "5.6 Terra"}, {ID: "gpt-6-luna", Label: "6 Luna"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}

	// Statsig prism_codex_models 的形态，以及预留的按模型下发档位。
	got, err = ParseModelCatalog([]byte(`{"free_model":"x","models":[{"id":"a","label":"A","reasoning_efforts":["low","high"],` +
		`"default_reasoning_effort":"high"},{"id":"b","label":"B","supported_reasoning_efforts":[{"value":"medium"},{"effort":"xhigh"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want = []UpstreamModel{{ID: "a", Label: "A", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
		{ID: "b", Label: "B", Efforts: []string{"medium", "xhigh"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}

	if _, err := ParseModelCatalog([]byte(`[]`)); err == nil {
		t.Fatal("空清单应报错")
	}
	if _, err := ParseModelCatalog([]byte(`<html>`)); err == nil {
		t.Fatal("非 JSON 应报错")
	}
}

// 片段取自 2026-10-06 的 Prism 前端代码（压缩后的形态）。
const effortJS = `var Ip=e.i(320647);let Ac="high";` + // 别的模块里的同名变量：不是这个
	`let Ac="medium";function Ad(e){return"low"===e||"medium"===e||"high"===e||"xhigh"===e}` +
	`let If=[{value:"low",labelKey:"reasoningEffortLowLabel"},{value:"medium",labelKey:"reasoningEffortMediumLabel"},` +
	`{value:"high",labelKey:"reasoningEffortHighLabel"},{value:"xhigh",labelKey:"reasoningEffortExtraHighLabel"}];` +
	`function Ig({model:e}){}[eh,ep]=function(){let[e,t]=(0,Au.default)("prism:ai-assistant:reasoning-effort:v1",Ac)}`

// 首页内嵌的 i18n 文案（JSON 字符串里的转义引号），中文在前。
const effortHTML = `<script>self.__next_f.push([1,"{\"reasoningEffortHighLabel\":\"高\",\"reasoningEffortExtraHighLabel\":\"极高\"}` +
	`{\"reasoningEffortLowLabel\":\"Low\",\"reasoningEffortMediumLabel\":\"Medium\",\"reasoningEffortHighLabel\":\"High\",` +
	`\"reasoningEffortExtraHighLabel\":\"Extra high\"}"])</script>`

func TestParseEffortOptions(t *testing.T) {
	got := ParseEffortOptions([]byte(effortJS), []byte(effortHTML))
	if got == nil {
		t.Fatal("没解析出档位")
	}
	want := &EffortOptions{
		Values:  []string{"low", "medium", "high", "xhigh"},
		Labels:  map[string]string{"low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra high"},
		Default: "medium",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}

	// 页面里没有文案：只是不带展示名。
	if got := ParseEffortOptions([]byte(effortJS), nil); got == nil || got.Labels != nil || got.Default != "medium" {
		t.Fatalf("got %+v", got)
	}
	// 别的脚本：没有下拉框选项。
	if got := ParseEffortOptions([]byte(`let a=[{value:"x",labelKey:"fooLabel"}];`), nil); got != nil {
		t.Fatalf("不该解析出档位: %+v", got)
	}
}
