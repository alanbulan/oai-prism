package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/sentinel"
	"github.com/oai-prism/oaiprism/internal/upstream"
)

const defaultProfilePath = "secrets/sentinel_profile.json"

// cmdProfile 查看 / 更新签 Sentinel token 用的浏览器指纹。
//
//	oaiprism profile            看指纹状态（版本、是否过时、TLS 模板）
//	oaiprism profile -capture   用本机 Chrome 重新采集完整指纹，并比对 TLS 握手
//	oaiprism profile -bump      只把版本号改成本机 Chrome 的（不开浏览器）
func cmdProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	cfgPath := fs.String("config", "configs/config.yaml", "配置文件路径（读取 upstream.sentinel_profile）")
	file := fs.String("file", "", "指纹文件（默认取配置里的 upstream.sentinel_profile，没配则 "+defaultProfilePath+"）")
	capture := fs.Bool("capture", false, "用本机 Chrome 重新采集完整指纹，并比对 TLS 握手")
	bump := fs.Bool("bump", false, "只把版本号改成本机 Chrome 的，不开浏览器")
	chrome := fs.String("chrome", "", "配合 -bump：指定版本，如 157.0.7512.80（默认读本机安装的 Chrome）")
	noOpen := fs.Bool("no-open", false, "配合 -capture：不自动打开 Chrome，只打印采集页地址")
	timeout := fs.Duration("timeout", 3*time.Minute, "配合 -capture：最多等多久")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *capture && *bump {
		return errors.New("-capture 和 -bump 只能选一个")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		cfg = config.Default()
	}
	configured := cfg.Upstream.SentinelProfile
	path := *file
	if path == "" {
		path = configured
	}
	if path == "" {
		path = defaultProfilePath
	}
	base, exists := sentinel.DefaultProfile(), false
	if p, err := sentinel.LoadProfile(path); err == nil {
		base, exists = p, true
	} else if !os.IsNotExist(err) {
		return err
	}

	var next *sentinel.Profile
	switch {
	case *capture:
		next, err = captureProfile(base, *noOpen, *timeout)
	case *bump:
		next, err = bumpProfile(base, *chrome)
	default:
		printProfileStatus(path, exists, configured != "" || *file != "", base)
		return nil
	}
	if err != nil || next == nil {
		return err
	}
	fmt.Printf("原指纹：%s\n新指纹：%s\n", sentinel.Describe(base), sentinel.Describe(next))
	if err := next.Save(path); err != nil {
		return fmt.Errorf("保存指纹失败: %w", err)
	}
	fmt.Printf("已写入 %s", path)
	if exists {
		fmt.Printf("（旧文件留作 %s.bak）", path)
	}
	fmt.Println()
	if configured == "" && *file == "" {
		fmt.Printf("配置里还没用上它：在 %s 的 upstream 下加一行  sentinel_profile: %s\n", *cfgPath, path)
	}
	fmt.Println("重启网关后生效（tools\\stop.ps1 再 tools\\start.ps1）。")
	return nil
}

func printProfileStatus(path string, exists, configured bool, p *sentinel.Profile) {
	switch {
	case exists && configured:
		fmt.Printf("指纹文件：%s\n", path)
	case exists:
		fmt.Printf("指纹文件：%s（配置里没指向它，网关实际用的是内置通用指纹）\n", path)
	default:
		fmt.Printf("指纹文件：%s 不存在，网关用的是内置通用指纹\n", path)
	}
	fmt.Printf("指纹：%s\n", sentinel.Describe(p))
	if v, _ := sentinel.LocalChrome(); v != "" {
		fmt.Printf("本机 Chrome：%s\n", v)
	} else {
		fmt.Println("本机 Chrome：没找到")
	}
	fmt.Printf("TLS 模板：%s（扩展顺序每个连接随机，和 Chrome 一样）\n", upstream.TLSTemplate(p))
	if msg := sentinel.Staleness(p, time.Now()); msg != "" {
		fmt.Println("提醒：" + msg)
	} else {
		fmt.Println("状态：没有过时")
	}
}

// bumpProfile 只换版本号：userAgent、品牌列表（按 Chrome 的算法重排）与完整版本号。
func bumpProfile(base *sentinel.Profile, version string) (*sentinel.Profile, error) {
	if version == "" {
		version, _ = sentinel.LocalChrome()
		if version == "" {
			return nil, errors.New("没找到本机 Chrome，请用 -chrome 指定版本（chrome://version 第一行，如 157.0.7512.80）")
		}
	}
	next := base.Clone()
	if err := next.SetChromeVersion(version); err != nil {
		return nil, err
	}
	if sentinel.Describe(next) == sentinel.Describe(base) && sameJSON(next, base) {
		fmt.Printf("指纹已经是 Chrome %s，不用更新\n", version)
		return nil, nil
	}
	return next, nil
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// captureProfile 在本机起一个采集页，用 Chrome 打开它，收集浏览器环境并合并进指纹；
// 同时让 Chrome 向本机握手一次，与网关的 TLS 握手比对。
func captureProfile(base *sentinel.Profile, noOpen bool, timeout time.Duration) (*sentinel.Profile, error) {
	chromeTrap, err := upstream.NewHelloTrap()
	if err != nil {
		return nil, err
	}
	defer chromeTrap.Close()

	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	token := hex.EncodeToString(tok)
	type result struct {
		p   *sentinel.Profile
		err error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(sentinel.CapturePage(token, chromeTrap.Port))
	})
	mux.HandleFunc("POST /result", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != token {
			http.Error(w, "token 不对", http.StatusForbidden)
			return
		}
		var c sentinel.Capture
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&c); err != nil {
			http.Error(w, "数据格式不对: "+err.Error(), http.StatusBadRequest)
			return
		}
		p, err := c.Apply(base, time.Now())
		if err != nil {
			http.Error(w, "采集失败："+err.Error(), http.StatusBadRequest)
			select {
			case done <- result{err: err}:
			default:
			}
			return
		}
		fmt.Fprintf(w, "采集完成：%s。可以关闭这个标签页了，结果请看 oaiprism 的输出。", sentinel.Describe(p))
		select {
		case done <- result{p: p}:
		default:
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	url := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	fmt.Printf("采集页：%s\n", url)
	switch {
	case noOpen:
		fmt.Println("请用 Google Chrome 打开上面的地址。")
	case openInChrome(url) != nil:
		fmt.Println("没能自动打开 Chrome，请手动用 Google Chrome 打开上面的地址。")
	default:
		fmt.Println("已在 Chrome 里打开采集页，等它发回结果……")
	}

	var res result
	select {
	case res = <-done:
	case <-time.After(timeout):
		return nil, fmt.Errorf("等了 %v 没收到采集结果", timeout)
	}
	if res.err != nil {
		return nil, res.err
	}
	compareTLS(res.p, chromeTrap)
	return res.p, nil
}

// compareTLS 记下 Chrome 的信任锚列表，再比对 Chrome 与网关（用新指纹）的 ClientHello。
func compareTLS(p *sentinel.Profile, chromeTrap *upstream.HelloTrap) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := chromeTrap.Next(ctx)
	if err != nil {
		fmt.Println("TLS 握手：没收到 Chrome 的握手，跳过比对")
		return
	}
	if ch.TrustAnchors != "" {
		p.TLS = &sentinel.TLSInfo{TrustAnchors: ch.TrustAnchors}
	}
	gwTrap, err := upstream.NewHelloTrap()
	if err != nil {
		fmt.Println("TLS 握手：比对失败：", err)
		return
	}
	defer gwTrap.Close()
	gw, err := upstream.GatewayHello(context.Background(), p, gwTrap)
	if err != nil {
		fmt.Println("TLS 握手：没取到网关的握手：", err)
		return
	}
	tmpl := upstream.TLSTemplate(p)
	diff := ch.Diff(gw)
	if len(diff) == 0 {
		fmt.Printf("TLS 握手：与本机 Chrome 一致（模板 %s，扩展顺序每个连接随机）\n", tmpl)
		return
	}
	fmt.Printf("TLS 握手：与本机 Chrome 不一致（模板 %s）：\n  %s\n", tmpl, strings.Join(diff, "\n  "))
	fmt.Println("  先升级 TLS 库再重新构建：go get -u github.com/bogdanfinn/tls-client && go mod tidy；")
	fmt.Println("  升级后仍不一致，说明 tls-client 还没跟上这一版 Chrome，可以先继续用（目前 OpenAI 没有因此拒绝过请求）。")
}

func openInChrome(url string) error {
	_, exe := sentinel.LocalChrome()
	switch {
	case exe == "":
		return errors.New("没找到 Chrome")
	case runtime.GOOS == "darwin":
		return exec.Command("open", "-a", "Google Chrome", url).Start()
	default:
		return exec.Command(exe, url).Start()
	}
}
