package upstream

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/oai-prism/oaiprism/internal/sentinel"
)

func TestTLSTemplateFollowsProfileVersion(t *testing.T) {
	name, _ := chromeTemplate(154)
	if name != "chrome_152" && name != "chrome_154" {
		t.Fatalf("Chrome 154 应选不新于它的最新模板，得到 %s", name)
	}
	if old, _ := chromeTemplate(50); old == "" || old == "default" {
		t.Fatalf("比所有模板都旧时应退回最旧的模板，得到 %s", old)
	}
	p := sentinel.DefaultProfile()
	if got := TLSTemplate(p); got != name {
		t.Fatalf("内置指纹的模板 %s ≠ %s", got, name)
	}
	p.TLS = &sentinel.TLSInfo{TrustAnchors: "0006" + "0582df130201"}
	if got := TLSTemplate(p); got != name+"，信任锚取自本机 Chrome" {
		t.Fatalf("带信任锚时: %s", got)
	}
}

// 网关真正发出的 ClientHello：Chrome 的组成、扩展顺序每次连接都变、信任锚用指纹里的。
func TestGatewayHello(t *testing.T) {
	p := sentinel.DefaultProfile()
	anchors := "0006" + "0582df130201"
	p.TLS = &sentinel.TLSInfo{TrustAnchors: anchors}
	trap, err := NewHelloTrap()
	if err != nil {
		t.Fatal(err)
	}
	defer trap.Close()
	var orders []string
	var first *Hello
	for i := 0; i < 3; i++ {
		h, err := GatewayHello(context.Background(), p, trap)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = h
		} else if d := first.Diff(h); len(d) > 0 {
			t.Fatalf("同一指纹两次握手的组成不该变: %v", d)
		}
		orders = append(orders, fmt.Sprint(h.Order))
	}
	h := first
	if !slices.Contains(h.Ciphers, 0x1301) || !slices.Contains(h.Groups, 0x11ec) || !slices.Contains(h.KeyShares, 0x11ec) {
		t.Fatalf("不像 Chrome: ciphers=%x groups=%x keyshares=%x", h.Ciphers, h.Groups, h.KeyShares)
	}
	if fmt.Sprint(h.ALPN) != "[h2 http/1.1]" || fmt.Sprint(h.ALPS) != "[h2]" {
		t.Fatalf("ALPN/ALPS: %v %v", h.ALPN, h.ALPS)
	}
	if h.TrustAnchors != anchors {
		t.Fatalf("信任锚应取自指纹: %s", h.TrustAnchors)
	}
	if orders[0] == orders[1] && orders[1] == orders[2] {
		t.Fatalf("扩展顺序应每次连接打乱: %v", orders)
	}

	// 不带信任锚的指纹用模板自带的
	trap2, err := NewHelloTrap()
	if err != nil {
		t.Fatal(err)
	}
	defer trap2.Close()
	h2, err := GatewayHello(context.Background(), sentinel.DefaultProfile(), trap2)
	if err != nil {
		t.Fatal(err)
	}
	if h2.TrustAnchors == "" || h2.TrustAnchors == anchors {
		t.Fatalf("应是模板自带的信任锚: %q", h2.TrustAnchors)
	}
	if d := h.Diff(h2); len(d) != 1 {
		t.Fatalf("两者应只差信任锚: %v", d)
	}
}
