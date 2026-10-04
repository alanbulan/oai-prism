package sentinel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// 真实 Chrome 的 sec-ch-ua（品牌、版本与顺序都由大版本号决定）。
func TestBrandListMatchesChrome(t *testing.T) {
	cases := map[int]string{
		120: `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`,
		124: `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		130: `"Chromium";v="130", "Google Chrome";v="130", "Not?A_Brand";v="99"`,
		131: `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		154: `"Chromium";v="154", "Google Chrome";v="154", "Not A(Brand";v="99"`,
	}
	for major, want := range cases {
		p := &Profile{Navigator: Navigator{UAData: UAData{Brands: BrandList(major, "Google Chrome", itoa(major), false)}}}
		if got := p.SecCHUA(); got != want {
			t.Errorf("Chrome %d:\n  得到 %s\n  期望 %s", major, got, want)
		}
	}
	full := BrandList(154, "Google Chrome", "154.0.8037.93", true)
	if full[2].Version != "99.0.0.0" || full[0].Version != "154.0.8037.93" {
		t.Fatalf("fullVersionList 形式不对: %+v", full)
	}
}

func TestSetChromeVersion(t *testing.T) {
	p := DefaultProfile()
	if err := p.SetChromeVersion("157"); err != nil {
		t.Fatal(err)
	}
	if p.ChromeMajor() != 157 || !strings.Contains(p.UserAgent(), "Chrome/157.0.0.0 ") {
		t.Fatalf("userAgent 没换: %s", p.UserAgent())
	}
	for _, it := range p.Navigator.Proto {
		if it[0] == "appVersion" && !strings.Contains(it[2].(string), "Chrome/157.0.0.0") {
			t.Fatalf("appVersion 没换: %v", it[2])
		}
	}
	if got := p.SecCHUA(); got != `"Not-A.Brand";v="99", "Google Chrome";v="157", "Chromium";v="157"` {
		t.Fatalf("品牌列表: %s", got)
	}

	p.Navigator.UAData.HighEntropy = map[string]any{"uaFullVersion": "157.0.1.2", "fullVersionList": []any{}}
	if err := p.SetChromeVersion("158"); err == nil {
		t.Fatal("有完整版本号时只给大版本应报错")
	}
	if err := p.SetChromeVersion("158.0.7600.10"); err != nil {
		t.Fatal(err)
	}
	if p.Navigator.UAData.HighEntropy["uaFullVersion"] != "158.0.7600.10" {
		t.Fatalf("uaFullVersion 没换: %v", p.Navigator.UAData.HighEntropy)
	}
	if fl := p.Navigator.UAData.HighEntropy["fullVersionList"].([]Brand); fl[2].Brand != "Google Chrome" || fl[2].Version != "158.0.7600.10" || fl[1].Version != "24.0.0.0" {
		t.Fatalf("fullVersionList 没换: %+v", fl)
	}
	if err := p.SetChromeVersion("abc"); err == nil {
		t.Fatal("非法版本应报错")
	}
}

func TestStaleness(t *testing.T) {
	p := DefaultProfile() // Chrome 154
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	if msg := staleness(p, now, "156.0.1.1"); msg != "" {
		t.Fatalf("落后 2 个版本不该提醒: %s", msg)
	}
	if msg := staleness(p, now, "157.0.1.1"); !strings.Contains(msg, "157") {
		t.Fatalf("落后 3 个版本应提醒: %q", msg)
	}
	p.CapturedAt = "2026-01-01"
	if msg := staleness(p, now, ""); !strings.Contains(msg, "2026-01-01") {
		t.Fatalf("没装 Chrome 时按采集日期提醒: %q", msg)
	}
	p.CapturedAt = "2026-09-01"
	if msg := staleness(p, now, ""); msg != "" {
		t.Fatalf("一个月前采集的不该提醒: %s", msg)
	}
}

func fakeCapture(base *Profile) *Capture {
	c := &Capture{
		Languages: []string{"en-US", "en"},
		Plugins:   []string{"PDF Viewer"},
		MimeTypes: []string{"application/pdf"},
		UAData: &UAData{
			Brands:      BrandList(156, "Google Chrome", "156", false),
			Platform:    "Windows",
			HighEntropy: map[string]any{"uaFullVersion": "156.0.1.2"},
		},
		Screen:      map[string]any{"width": 2560.0, "height": 1440.0},
		Memory:      map[string]any{"jsHeapSizeLimit": 1.0, "usedJSHeapSize": 2.0},
		Timezone:    Timezone{Offset: 0, Name: "Coordinated Universal Time", IANA: "UTC"},
		WindowTypes: map[string]string{"newApi": "function"},
		Geometry:    map[string]any{"innerWidth": 2560.0, "innerHeight": 1300.0, "outerWidth": 2560.0, "outerHeight": 1400.0},
		Widths:      map[string]float64{},
	}
	for _, it := range base.Navigator.Proto {
		v := it[2]
		switch it[0] {
		case "userAgent", "appVersion":
			v = strings.Replace(v.(string), "Chrome/154.", "Chrome/156.", 1)
		case "webdriver":
			v = true
		}
		c.NavProto = append(c.NavProto, [3]any{it[0], it[1], v})
	}
	for _, k := range base.Window.Keys {
		if k == "__next_f" {
			break // Prism 应用的全局变量从这里开始，本机页面上没有
		}
		c.WindowKeys = append(c.WindowKeys, k)
		if t, ok := base.Window.Types[k]; ok {
			c.WindowTypes[k] = t
		}
	}
	c.WindowKeys = append(c.WindowKeys, "newApi")
	for _, r := range "!\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz" {
		c.Widths[string(r)] = 55.6152
	}
	return c
}

func TestCaptureApply(t *testing.T) {
	base := DefaultProfile()
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	p, err := fakeCapture(base).Apply(base, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.ChromeMajor() != 156 || p.CapturedAt != "2026-10-04" || p.Timezone.IANA != "UTC" {
		t.Fatalf("基本字段没更新: %s", Describe(p))
	}
	for _, it := range p.Navigator.Proto {
		if it[0] == "webdriver" && it[2] != false {
			t.Fatal("webdriver 必须是 false")
		}
		if it[0] == "languages" && it[2] != nil {
			t.Fatal("languages 单独存，proto 里应为 null")
		}
	}
	keys := p.Window.Keys
	if keys[0] != "0" {
		t.Fatalf("iframe 的 0 应排最前: %v", keys[:3])
	}
	ni, nf := indexOf(keys, "newApi"), indexOf(keys, "__next_f")
	if ni < 0 || nf < 0 || ni > nf || keys[len(keys)-1] != base.Window.Keys[len(base.Window.Keys)-1] {
		t.Fatalf("Chrome 新增的键应在 Prism 应用的键前面，应用的键原样保留: newApi=%d __next_f=%d", ni, nf)
	}
	if p.Window.Types["newApi"] != "function" || p.Window.Values["__webpack_hash__"] != "" || p.Window.Values["innerHeight"] != 1300.0 || p.Page.RectY != 1300 {
		t.Fatalf("window 合并不对: %v %v", p.Window.Types["newApi"], p.Window.Values)
	}
	if p.Memory["jsHeapSizeLimit"] != 1.0 || p.Memory["usedJSHeapSize"] == 2.0 {
		t.Fatalf("堆上限取实测、已用量沿用旧值: %v", p.Memory)
	}
	var font struct {
		Widths     map[string]float64 `json:"widths"`
		LineHeight float64            `json:"lineHeight"`
	}
	_ = json.Unmarshal(p.FontMetrics, &font)
	if font.Widths["a"] != 1139 || font.Widths[" "] != 569 || font.LineHeight != 1.5 {
		t.Fatalf("字宽换算不对: a=%v 空格=%v 行高=%v", font.Widths["a"], font.Widths[" "], font.LineHeight)
	}

	// 后台标签页：窗口尺寸沿用旧指纹
	c := fakeCapture(base)
	c.Hidden = true
	c.Geometry["outerWidth"] = 0.0
	p, err = c.Apply(base, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Window.Values["innerHeight"] != base.Window.Values["innerHeight"] || p.Page.RectY != base.Page.RectY {
		t.Fatalf("后台采集不该用读到的窗口尺寸: %v", p.Window.Values["innerHeight"])
	}

	c = fakeCapture(base)
	c.UAData.Brands = BrandList(156, "Microsoft Edge", "156", false)
	if _, err := c.Apply(base, now); err == nil {
		t.Fatal("非 Chrome 应拒绝")
	}
}

func TestProfileSaveAndTLS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.json")
	p := DefaultProfile()
	p.TLS = &TLSInfo{TrustAnchors: "0006" + "0582df130201"}
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatal("覆盖前应留 .bak")
	}
	q, err := LoadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if q.TLS == nil || q.TLS.TrustAnchors != p.TLS.TrustAnchors || q.UserAgent() != p.UserAgent() {
		t.Fatal("保存再读回不一致")
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `\u003c`) {
		t.Fatal("不该转义 HTML 字符")
	}
	bad := strings.Replace(string(raw), p.TLS.TrustAnchors, "00ff01", 1)
	if _, err := parseProfile([]byte(bad)); err == nil {
		t.Fatal("trustAnchors 长度不对应报错")
	}
}

// getHighEntropyValues 和 Chrome 一样只给请求了的字段。
func TestHighEntropyValues(t *testing.T) {
	p := DefaultProfile()
	p.Navigator.UAData.HighEntropy = map[string]any{"uaFullVersion": "154.0.8037.93", "bitness": "64"}
	pg, err := newPage(p, `var SentinelSDK = { token: function () { return "x"; } };`, "https://sentinel.openai.com/sentinel/v1/sdk.js", newFrame(&fakeDoer{}, "https://sentinel.openai.com", "v1", quietLog()), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer pg.close()
	out := make(chan string, 1)
	pg.post(func() {
		v, err := pg.rt.RunString(`navigator.userAgentData.getHighEntropyValues(["uaFullVersion"]).then((v) => JSON.stringify(v))`)
		if err != nil {
			out <- "ERR " + err.Error()
			return
		}
		pr, _ := v.Export().(*goja.Promise)
		if pr == nil || pr.State() != goja.PromiseStateFulfilled {
			out <- "ERR 没有立即兑现"
			return
		}
		out <- pr.Result().String()
	})
	got := <-out
	if !strings.Contains(got, `"uaFullVersion":"154.0.8037.93"`) || strings.Contains(got, "bitness") || !strings.Contains(got, `"platform":"Windows"`) {
		t.Fatalf("getHighEntropyValues: %s", got)
	}
}

func indexOf(s []string, k string) int {
	for i, v := range s {
		if v == k {
			return i
		}
	}
	return -1
}
