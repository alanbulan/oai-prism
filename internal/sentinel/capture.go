package sentinel

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 重新采集指纹：oaiprism 在本机起一个页面，用户平时用的 Chrome 打开它，页面把 SDK 能读到的
// 浏览器环境发回来（见 capture.html），再与旧指纹合并。不需要 Playwright / Node，也不是自动化
// 浏览器（navigator.webdriver 天然为 false）。
//
// 本机页面不是 Prism，所以只取"浏览器本身"的部分；Prism 页面特有的部分（应用自己的全局变量、
// React 挂在 document 上的键、localStorage 键名、页面标题与脚本）沿用旧指纹。

//go:embed capture.html
var capturePage string

// CapturePage 返回采集页 HTML。token 防止别的本机页面冒充提交；tlsPort 非 0 时页面会向
// https://localhost:tlsPort 发起一次握手，供比对 TLS 指纹。
func CapturePage(token string, tlsPort int) []byte {
	port := ""
	if tlsPort > 0 {
		port = strconv.Itoa(tlsPort)
	}
	r := strings.NewReplacer("{{TOKEN}}", token, "{{TLS_PORT}}", port)
	return []byte(r.Replace(capturePage))
}

// Capture 是采集页发回的数据。
type Capture struct {
	NavProto    [][3]any           `json:"navProto"`
	Languages   []string           `json:"languages"`
	Plugins     []string           `json:"plugins"`
	MimeTypes   []string           `json:"mimeTypes"`
	UAData      *UAData            `json:"uaData"`
	Connection  map[string]any     `json:"connection"`
	Screen      map[string]any     `json:"screen"`
	Memory      map[string]any     `json:"memory"`
	Timezone    Timezone           `json:"timezone"`
	WindowKeys  []string           `json:"windowKeys"`
	WindowTypes map[string]string  `json:"windowTypes"`
	Geometry    map[string]any     `json:"geometry"`
	Widths      map[string]float64 `json:"widths"` // 100px Helvetica 下各字符的宽度
	Hidden      bool               `json:"hidden"` // 采集时标签页在后台：窗口尺寸不可信
}

var (
	indexKeyRe   = regexp.MustCompile(`^\d+$`)
	geometryKeys = []string{"innerWidth", "innerHeight", "outerWidth", "outerHeight", "screenX", "screenY", "screenLeft",
		"screenTop", "devicePixelRatio", "scrollX", "scrollY", "pageXOffset", "pageYOffset", "originAgentCluster"}
)

// Apply 用采集结果更新 base（不改 base），返回新指纹。
func (c *Capture) Apply(base *Profile, now time.Time) (*Profile, error) {
	if c.UAData == nil || !hasBrand(c.UAData.Brands, "Google Chrome") {
		return nil, errors.New("这不是 Google Chrome（请用 Chrome 打开采集页；Edge 等其他浏览器的指纹与 TLS 模板对不上）")
	}
	if len(c.NavProto) < 20 || len(c.WindowKeys) < 100 || len(c.Languages) == 0 || len(c.Widths) < 50 {
		return nil, errors.New("采集结果不完整")
	}
	p := base.clone()

	// navigator：键序与取值都用 Chrome 实测的；languages 单独存，webdriver 一个正常的 Chrome 必为 false
	proto := make([][3]any, 0, len(c.NavProto))
	for _, it := range c.NavProto {
		k, _ := it[0].(string)
		switch kind, _ := it[1].(string); kind {
		case "fn":
			proto = append(proto, [3]any{k, "fn", nil})
		case "obj":
			proto = append(proto, [3]any{k, "obj", it[2]})
		default:
			v := it[2]
			switch k {
			case "languages":
				v = nil
			case "webdriver":
				v = false
			}
			proto = append(proto, [3]any{k, "val", v})
		}
	}
	p.Navigator.Proto = proto
	p.Navigator.Languages = c.Languages
	p.Navigator.Plugins = c.Plugins
	p.Navigator.MimeTypes = c.MimeTypes
	p.Navigator.UAData = *c.UAData
	if len(p.Navigator.UAData.HighEntropy) == 0 {
		p.Navigator.UAData.HighEntropy = nil
	}
	if c.Connection != nil {
		p.Navigator.Connection = c.Connection
	}
	if p.ChromeMajor() == 0 {
		return nil, errors.New("采集到的 userAgent 里没有 Chrome 版本")
	}

	p.Screen = c.Screen
	p.Timezone = c.Timezone
	// 堆内存：上限是机器属性，取实测；已用量是 Prism 页面的（本机空白页小得多），沿用旧值
	if c.Memory != nil {
		mem := map[string]any{}
		for k, v := range p.Memory {
			mem[k] = v
		}
		mem["jsHeapSizeLimit"] = c.Memory["jsHeapSizeLimit"]
		p.Memory = mem
	}

	// window：Chrome 自带的键用实测的（顺序与类型），末尾 Prism 应用自己的那段沿用旧指纹；
	// Prism 页面里有 Sentinel 的 iframe，所以 "0" 排最前
	builtin := make([]string, 0, len(c.WindowKeys))
	seen := map[string]bool{"0": true}
	for _, k := range c.WindowKeys {
		if indexKeyRe.MatchString(k) || seen[k] {
			continue
		}
		seen[k] = true
		builtin = append(builtin, k)
	}
	app := appKeys(base.Window.Keys, seen)
	keys := append([]string{"0"}, builtin...)
	types := map[string]string{}
	values := map[string]any{}
	for _, k := range builtin {
		if t, ok := c.WindowTypes[k]; ok && !strings.HasPrefix(k, "on") {
			types[k] = t
		}
	}
	for _, k := range app {
		keys = append(keys, k)
		if t, ok := base.Window.Types[k]; ok {
			types[k] = t
		}
		if v, ok := base.Window.Values[k]; ok {
			values[k] = v
		}
	}
	types["0"] = "object"        // Sentinel 的 iframe
	types["event"] = "undefined" // SDK 运行时不在事件处理中
	if _, ok := types["fence"]; ok {
		types["fence"] = "null" // 顶层页面没有 fence
	}
	geometry := c.Geometry
	if ow, _ := geometry["outerWidth"].(float64); c.Hidden || ow == 0 {
		// 后台标签页读到的窗口尺寸不可信：沿用旧指纹的
		geometry = map[string]any{}
		for _, k := range geometryKeys {
			if v, ok := base.Window.Values[k]; ok {
				geometry[k] = v
			}
		}
	} else if h, ok := geometry["innerHeight"].(float64); ok {
		p.Page.RectY = h // Prism 页面撑满视口，SDK 量字用的 div 落在视口底部
	}
	for k, v := range geometry {
		if v != nil {
			values[k] = v
		}
	}
	p.Window = Window{Keys: keys, Types: types, Values: values}

	// 字体：100px 下的实测宽度换算成 Arial 的 2048 单位，其余参数（行高等）沿用
	var font map[string]any
	if err := json.Unmarshal(p.FontMetrics, &font); err != nil || font == nil {
		font = map[string]any{"unitsPerEm": 2048, "ascii": 1139, "latin": 1366, "wide": 2048, "lineHeight": 1.5}
	}
	upem := 2048.0
	if u, ok := font["unitsPerEm"].(float64); ok && u > 0 {
		upem = u
	}
	widths := map[string]any{" ": 569} // 空格：首尾会被折叠，单独量不出来，用 Arial 的 569
	if old, ok := font["widths"].(map[string]any); ok {
		if v, ok := old[" "]; ok {
			widths[" "] = v
		}
	}
	for ch, w := range c.Widths {
		widths[ch] = math.Round(w / 100 * upem)
	}
	font["widths"] = widths
	raw, err := json.Marshal(font)
	if err != nil {
		return nil, err
	}
	p.FontMetrics = raw

	p.CapturedAt = now.Format("2006-01-02")
	return p.clone(), nil
}

// appKeys 是旧指纹 window 键末尾那段 Chrome 没有的（Prism 应用的全局变量），保持原顺序。
func appKeys(old []string, builtin map[string]bool) []string {
	i := len(old)
	for i > 0 && !builtin[old[i-1]] {
		i--
	}
	var out []string
	for _, k := range old[i:] {
		if k != "0" {
			out = append(out, k)
		}
	}
	return out
}

func hasBrand(bs []Brand, name string) bool {
	for _, b := range bs {
		if b.Brand == name {
			return true
		}
	}
	return false
}

// Describe 用一行概括指纹（给人看）。
func Describe(p *Profile) string {
	num := func(m map[string]any, k string) string {
		if v, ok := m[k].(float64); ok {
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return "?"
	}
	var cores, mem any = "?", "?"
	for _, it := range p.Navigator.Proto {
		switch it[0] {
		case "hardwareConcurrency":
			cores = it[2]
		case "deviceMemory":
			mem = it[2]
		}
	}
	s := fmt.Sprintf("Chrome %d · %s · 屏幕 %s×%s · %v 核 · %v GB · %s · %s",
		p.ChromeMajor(), p.Navigator.UAData.Platform, num(p.Screen, "width"), num(p.Screen, "height"),
		cores, mem, strings.Join(p.Navigator.Languages, ","), p.Timezone.IANA)
	if p.CapturedAt != "" {
		s += " · 采集于 " + p.CapturedAt
	}
	return s
}
