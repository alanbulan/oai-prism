package upstream

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/tls-client/profiles"
	tls "github.com/bogdanfinn/utls"

	"github.com/oai-prism/oaiprism/internal/sentinel"
)

var chromeTemplateRe = regexp.MustCompile(`^chrome_(\d+)$`)

// tlsTemplate 选 tls-client 里不新于指纹 Chrome 版本的最新 Chrome 模板。Chrome 的握手隔好几个版本
// 才变一次，tls-client 也只在变化时出新模板；升级依赖后有了更新的模板会自动用上，不用改代码。
// 指纹里有本机 Chrome 的信任锚列表时，替换模板自带的那份。
func tlsTemplate(p *sentinel.Profile) (string, profiles.ClientProfile) {
	name, tmpl := chromeTemplate(p.ChromeMajor())
	if p.TLS == nil || p.TLS.TrustAnchors == "" {
		return name, tmpl
	}
	anchors, err := hex.DecodeString(p.TLS.TrustAnchors)
	if err != nil {
		return name, tmpl
	}
	id := tmpl.GetClientHelloId()
	base := id.SpecFactory
	if base == nil {
		return name, tmpl
	}
	id.Version += "-anchors"
	id.SpecFactory = func() (tls.ClientHelloSpec, error) {
		spec, err := base()
		if err != nil {
			return spec, err
		}
		for i, e := range spec.Extensions {
			if g, ok := e.(*tls.GenericExtension); ok && g.Id == extTrustAnchors {
				spec.Extensions[i] = &tls.GenericExtension{Id: extTrustAnchors, Data: anchors}
			}
		}
		return spec, nil
	}
	return name, profiles.NewClientProfile(id, tmpl.GetSettings(), tmpl.GetSettingsOrder(), tmpl.GetPseudoHeaderOrder(),
		tmpl.GetConnectionFlow(), tmpl.GetPriorities(), tmpl.GetHeaderPriority(), tmpl.GetStreamID(), tmpl.GetAllowHTTP(),
		tmpl.GetHttp3Settings(), tmpl.GetHttp3SettingsOrder(), tmpl.GetHttp3PriorityParam(), tmpl.GetHttp3PseudoHeaderOrder(),
		tmpl.GetHttp3SendGreaseFrames())
}

const extTrustAnchors = 0xca34

func chromeTemplate(major int) (string, profiles.ClientProfile) {
	best, oldest := -1, -1
	for name := range profiles.MappedTLSClients {
		m := chromeTemplateRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n <= major && n > best {
			best = n
		}
		if oldest < 0 || n < oldest {
			oldest = n
		}
	}
	if best < 0 {
		best = oldest // 指纹比所有模板都旧：用最旧的那个
	}
	if best < 0 {
		return "default", profiles.DefaultClientProfile
	}
	name := "chrome_" + strconv.Itoa(best)
	return name, profiles.MappedTLSClients[name]
}

// TLSTemplate 描述这份指纹用的 TLS 模板（如 "chrome_152，信任锚取自本机 Chrome"）。
func TLSTemplate(p *sentinel.Profile) string {
	name, _ := tlsTemplate(p)
	if p.TLS != nil && p.TLS.TrustAnchors != "" {
		name += "，信任锚取自本机 Chrome"
	}
	return name
}

// Hello 是 ClientHello 里构成 TLS 指纹的部分：GREASE 去掉；扩展只看集合（Chrome 每次连接都打乱顺序）。
type Hello struct {
	Ciphers      []uint16
	Extensions   []uint16 // 已排序
	Groups       []uint16
	KeyShares    []uint16
	SigAlgs      []uint16
	Versions     []uint16
	ALPN         []string
	ALPS         []string
	CertCompress []uint16
	PointFormats []byte
	PSKModes     []byte
	TrustAnchors string   // 0xca34 扩展内容（十六进制）
	Order        []uint16 // 扩展的实际发送顺序（不参与比对）
}

func isGREASE(v uint16) bool { return v&0x0f0f == 0x0a0a && v>>8 == v&0xff }

// ParseClientHello 解析一个 TLS 握手的开头（一或多个记录层包里的 ClientHello）。
func ParseClientHello(r io.Reader) (*Hello, error) {
	var body []byte
	need := -1
	for need < 0 || len(body) < need {
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, err
		}
		if hdr[0] != 22 {
			return nil, fmt.Errorf("不是 TLS 握手（记录类型 %d）", hdr[0])
		}
		frag := make([]byte, binary.BigEndian.Uint16(hdr[3:]))
		if _, err := io.ReadFull(r, frag); err != nil {
			return nil, err
		}
		body = append(body, frag...)
		if need < 0 && len(body) >= 4 {
			if body[0] != 1 {
				return nil, errors.New("不是 ClientHello")
			}
			need = 4 + (int(body[1])<<16 | int(body[2])<<8 | int(body[3]))
		}
	}
	return parseHelloBody(body[4:need])
}

type reader struct {
	b   []byte
	err bool
}

func (r *reader) bytes(n int) []byte {
	if r.err || n > len(r.b) {
		r.err = true
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}
func (r *reader) u8() int {
	b := r.bytes(1)
	if b == nil {
		return 0
	}
	return int(b[0])
}
func (r *reader) u16() int {
	b := r.bytes(2)
	if b == nil {
		return 0
	}
	return int(binary.BigEndian.Uint16(b))
}
func (r *reader) vec8() *reader  { return &reader{b: r.bytes(r.u8()), err: r.err} }
func (r *reader) vec16() *reader { return &reader{b: r.bytes(r.u16()), err: r.err} }
func (r *reader) u16s() []uint16 {
	var out []uint16
	for len(r.b) >= 2 && !r.err {
		if v := uint16(r.u16()); !isGREASE(v) {
			out = append(out, v)
		}
	}
	return out
}
func (r *reader) strs() []string {
	var out []string
	for len(r.b) > 0 && !r.err {
		out = append(out, string(r.vec8().b))
	}
	return out
}

func parseHelloBody(b []byte) (*Hello, error) {
	r := &reader{b: b}
	r.bytes(2 + 32) // legacy_version + random
	r.vec8()        // session id
	h := &Hello{Ciphers: r.vec16().u16s()}
	r.vec8() // compression
	exts := r.vec16()
	for len(exts.b) > 0 && !exts.err {
		typ := uint16(exts.u16())
		data := exts.vec16()
		if isGREASE(typ) {
			continue
		}
		h.Order = append(h.Order, typ)
		h.Extensions = append(h.Extensions, typ)
		switch typ {
		case 10:
			h.Groups = data.vec16().u16s()
		case 51:
			ks := data.vec16()
			for len(ks.b) > 0 && !ks.err {
				g := uint16(ks.u16())
				ks.vec16()
				if !isGREASE(g) {
					h.KeyShares = append(h.KeyShares, g)
				}
			}
		case 13:
			h.SigAlgs = data.vec16().u16s()
		case 43:
			h.Versions = data.vec8().u16s()
		case 16:
			h.ALPN = data.vec16().strs()
		case 17513, 17613:
			h.ALPS = data.vec16().strs()
		case 27:
			v := data.vec8()
			h.CertCompress = v.u16s()
		case 11:
			h.PointFormats = data.vec8().b
		case 45:
			h.PSKModes = data.vec8().b
		case extTrustAnchors:
			h.TrustAnchors = hex.EncodeToString(data.b)
		}
	}
	if r.err || exts.err {
		return nil, errors.New("ClientHello 截断或格式不对")
	}
	slices.Sort(h.Extensions)
	return h, nil
}

// Diff 列出两份 ClientHello 在指纹意义上的不同（a 是 Chrome，b 是网关），相同返回 nil。
func (a *Hello) Diff(b *Hello) []string {
	var out []string
	cmp := func(name string, x, y any) {
		if fmt.Sprint(x) != fmt.Sprint(y) {
			out = append(out, fmt.Sprintf("%s：Chrome %v，网关 %v", name, x, y))
		}
	}
	cmp("密码套件", hexList(a.Ciphers), hexList(b.Ciphers))
	if missing, extra := setDiff(a.Extensions, b.Extensions); len(missing)+len(extra) > 0 {
		out = append(out, fmt.Sprintf("扩展：网关缺 %v，多出 %v", hexList(missing), hexList(extra)))
	}
	cmp("支持的组", hexList(a.Groups), hexList(b.Groups))
	cmp("密钥交换", hexList(a.KeyShares), hexList(b.KeyShares))
	cmp("签名算法", hexList(a.SigAlgs), hexList(b.SigAlgs))
	cmp("TLS 版本", hexList(a.Versions), hexList(b.Versions))
	cmp("ALPN", a.ALPN, b.ALPN)
	cmp("ALPS", a.ALPS, b.ALPS)
	cmp("证书压缩", hexList(a.CertCompress), hexList(b.CertCompress))
	cmp("点格式", a.PointFormats, b.PointFormats)
	cmp("PSK 模式", a.PSKModes, b.PSKModes)
	if a.TrustAnchors != b.TrustAnchors {
		out = append(out, "信任锚 ID（0xca34）内容不同")
	}
	return out
}

func hexList(v []uint16) string {
	s := make([]string, len(v))
	for i, x := range v {
		s[i] = fmt.Sprintf("%04x", x)
	}
	return "[" + strings.Join(s, " ") + "]"
}

func setDiff(a, b []uint16) (missing, extra []uint16) {
	for _, x := range a {
		if !slices.Contains(b, x) {
			missing = append(missing, x)
		}
	}
	for _, x := range b {
		if !slices.Contains(a, x) {
			extra = append(extra, x)
		}
	}
	return
}

// HelloTrap 在本机回环地址上收 ClientHello：只读握手的第一个包就断开（不需要证书）。
// 同一端口同时监听 127.0.0.1 与 ::1，因为 Chrome 解析 localhost 可能先走 IPv6。
type HelloTrap struct {
	Port  int
	lns   []net.Listener
	hello chan *Hello
}

// NewHelloTrap 开始监听。
func NewHelloTrap() (*HelloTrap, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t := &HelloTrap{Port: ln.Addr().(*net.TCPAddr).Port, lns: []net.Listener{ln}, hello: make(chan *Hello, 8)}
	if ln6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", t.Port)); err == nil {
		t.lns = append(t.lns, ln6)
	}
	for _, l := range t.lns {
		go t.serve(l)
	}
	return t, nil
}

func (t *HelloTrap) serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if h, err := ParseClientHello(c); err == nil {
				select {
				case t.hello <- h:
				default:
				}
			}
		}()
	}
}

// Next 等下一份 ClientHello。
func (t *HelloTrap) Next(ctx context.Context) (*Hello, error) {
	select {
	case h := <-t.hello:
		return h, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close 停止监听。
func (t *HelloTrap) Close() {
	for _, l := range t.lns {
		l.Close()
	}
}

// GatewayHello 让网关出站用的 TLS 客户端（同一指纹、同一模板）向 trap 握手一次，返回它的 ClientHello。
func GatewayHello(ctx context.Context, p *sentinel.Profile, trap *HelloTrap) (*Hello, error) {
	c, err := newClient("", nil, p)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	go func() {
		req, _ := fhttp.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://localhost:%d/", trap.Port), nil)
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	return trap.Next(ctx)
}
