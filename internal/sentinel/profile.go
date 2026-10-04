package sentinel

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Profile 是签 token 时扮演的那台浏览器的指纹快照（Chrome 打开 Prism 首页时 SDK 能看到的一切）。
//
// 内置的是一台常见配置的 Windows Chrome；想沿用自己浏览器的指纹，把采集结果存成 JSON
// 用 sentinel.profile 指过去即可（字段形状同 profile_default.json）。
type Profile struct {
	Timezone       Timezone        `json:"timezone"`
	Screen         map[string]any  `json:"screen"`
	Memory         map[string]any  `json:"memory"`
	Navigator      Navigator       `json:"navigator"`
	Page           Page            `json:"page"`
	Window         Window          `json:"window"`
	FontMetrics    json.RawMessage `json:"fontMetrics"`
	SentinelOrigin string          `json:"sentinelOrigin"`
}

type Timezone struct {
	Offset int    `json:"offset"` // Date#getTimezoneOffset()，东八区为 -480
	Name   string `json:"name"`   // Date#toString() 括号里的名字，如"中国标准时间"
	IANA   string `json:"iana"`
}

type Navigator struct {
	// Proto 按 Navigator.prototype 的键序列出 [键, 类型, 值]：
	// 类型 "fn" 方法、"obj" 对象（值是接口名，如 Geolocation），其余直接是值。
	Proto      [][3]any       `json:"proto"`
	Languages  []string       `json:"languages"`
	Plugins    []string       `json:"plugins"`
	MimeTypes  []string       `json:"mimeTypes"`
	UAData     UAData         `json:"uaData"`
	Connection map[string]any `json:"connection"`
}

type UAData struct {
	Brands      []Brand        `json:"brands"`
	Platform    string         `json:"platform"`
	HighEntropy map[string]any `json:"highEntropy,omitempty"`
}

type Brand struct {
	Brand   string `json:"brand"`
	Version string `json:"version"`
}

type Page struct {
	Href           string      `json:"href"`
	Title          string      `json:"title"`
	Lang           string      `json:"lang"`
	Cookie         string      `json:"cookie"`
	HistoryLength  int         `json:"historyLength"`
	HistoryOwnKeys []string    `json:"historyOwnKeys"`
	LocalStorage   [][2]string `json:"localStorage"`
	SessionStorage [][2]string `json:"sessionStorage"`
	// Scripts 是页面上的 <script src>（不含 sentinel 自己的两个，运行时补上并尽量用线上最新的）。
	Scripts      []string `json:"scripts"`
	DocumentKeys []string `json:"documentKeys"` // {s1}/{s2} 换成每次开页面随机的 React 后缀
	RectX        float64  `json:"rectX"`
	RectY        float64  `json:"rectY"`
}

type Window struct {
	Keys   []string          `json:"keys"`   // Object.keys(window) 的顺序
	Types  map[string]string `json:"types"`  // 没单独实现的键的 typeof
	Values map[string]any    `json:"values"` // 数值类属性（innerWidth、devicePixelRatio……）
}

//go:embed profile_default.json
var defaultProfileJSON []byte

// DefaultProfile 返回内置指纹的一份拷贝。
func DefaultProfile() *Profile {
	p, err := parseProfile(defaultProfileJSON)
	if err != nil {
		panic("sentinel: 内置指纹损坏: " + err.Error())
	}
	return p
}

// LoadProfile 读取自定义指纹。
func LoadProfile(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := parseProfile(raw)
	if err != nil {
		return nil, fmt.Errorf("指纹文件 %s: %w", path, err)
	}
	return p, nil
}

func parseProfile(raw []byte) (*Profile, error) {
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.UserAgent() == "" {
		return nil, fmt.Errorf("navigator.proto 里缺 userAgent")
	}
	if len(p.Window.Keys) == 0 || len(p.Navigator.Languages) == 0 || p.Page.Href == "" {
		return nil, fmt.Errorf("缺少 window.keys / navigator.languages / page.href")
	}
	if p.SentinelOrigin == "" {
		p.SentinelOrigin = "https://sentinel.openai.com"
	}
	return &p, nil
}

func (p *Profile) clone() *Profile {
	raw, _ := json.Marshal(p)
	c, _ := parseProfile(raw)
	return c
}

// UserAgent 是指纹里的 navigator.userAgent —— 所有出站请求的 User-Agent 都要和它一致。
func (p *Profile) UserAgent() string {
	for _, it := range p.Navigator.Proto {
		if k, _ := it[0].(string); k == "userAgent" {
			s, _ := it[2].(string)
			return s
		}
	}
	return ""
}

// SecCHUA 拼出与指纹一致的 sec-ch-ua 请求头。
func (p *Profile) SecCHUA() string {
	parts := make([]string, 0, len(p.Navigator.UAData.Brands))
	for _, b := range p.Navigator.UAData.Brands {
		parts = append(parts, fmt.Sprintf("%q;v=%q", b.Brand, b.Version))
	}
	return strings.Join(parts, ", ")
}

// Platform 是 sec-ch-ua-platform 的值（带引号）。
func (p *Profile) Platform() string {
	return fmt.Sprintf("%q", p.Navigator.UAData.Platform)
}

// AcceptLanguage 按 navigator.languages 拼出 Chrome 会发的 Accept-Language。
func (p *Profile) AcceptLanguage() string {
	var b strings.Builder
	for i, l := range p.Navigator.Languages {
		if i > 0 {
			q := 1.0 - 0.1*float64(i)
			if q < 0.1 {
				q = 0.1
			}
			fmt.Fprintf(&b, ",%s;q=%.1f", l, q)
			continue
		}
		b.WriteString(l)
	}
	return b.String()
}
