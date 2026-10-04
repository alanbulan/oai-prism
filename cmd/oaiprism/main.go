// Command oaiprism 是 OpenAI Prism 的高性能反向代理。
//
// 子命令设计说明：把"凭据导入 / 校验"做成 CLI 而不是只靠配置文件，
// 是因为凭据是会过期的运行期资产。用户需要一条命令就能
// "把浏览器里的 Cookie 变成可用账号并验证它能用"，
// 而不是手工编辑 JSON 再重启服务去猜哪里错了。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/capture"
	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/httpc"
	"github.com/oai-prism/oaiprism/internal/logx"
	"github.com/oai-prism/oaiprism/internal/prism"
	"github.com/oai-prism/oaiprism/internal/server"
)

// version 由 -ldflags 注入。
var version = "dev"

const usage = `oaiprism - OpenAI Prism 高性能反向代理

用法:
  oaiprism <子命令> [参数]

子命令:
  serve            启动代理服务
  probe            校验凭据文件里的账号是否可用
  import           把 Cookie / Token 写入凭据文件并验证
  capture-summary  汇总抓包文件，输出各端点的协议字段清单
  sentinel         自检纯 Go 的 Sentinel 签发（只连 sentinel.openai.com，不用账号）
  version          打印版本

示例:
  oaiprism serve -config configs/config.yaml
  oaiprism import -cookie "__Secure-next-auth.session-token=eyJ..." -id main
  oaiprism import -access-token "eyJhbGci..." -id main2
  oaiprism probe -config configs/config.yaml
  oaiprism capture-summary -file captures/capture-2026-09-16.jsonl
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "serve", "s":
		err = cmdServe(args)
	case "probe", "p":
		err = cmdProbe(args)
	case "import", "i":
		err = cmdImport(args)
	case "capture-summary", "summary":
		err = cmdSummary(args)
	case "sentinel":
		err = cmdSentinel(args)
	case "version", "-v", "--version":
		fmt.Printf("oaiprism %s\n", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------- serve ----------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/config.yaml", "配置文件路径")
	port := fs.Int("port", 0, "覆盖监听端口")
	credsFile := fs.String("creds", "", "覆盖凭据文件路径")
	debug := fs.Bool("debug", false, "调试日志")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *port > 0 {
		cfg.Server.Port = *port
	}
	if *credsFile != "" {
		cfg.Creds.File = *credsFile
	}
	if *debug {
		cfg.Log.Level = "debug"
	}

	log := logx.Setup(cfg.Log.Level, cfg.Log.Format)

	// 数据文件路径解析：优先配置文件目录，其次当前工作目录。
	// 详见 config.ResolveDataPath 的注释——这是为了消掉
	// "凭据文件明明存在却没被加载"这类难查的坑。
	base := dirOf(*cfgPath)
	var how string
	cfg.Creds.File, how = config.ResolveDataPath(base, cfg.Creds.File)
	log.Info("凭据文件已定位", "path", cfg.Creds.File, "resolved_by", how)
	if cfg.Capture.Dir != "" {
		// 产出目录用另一套规则：不存在时落在当前工作目录，
		// 而不是悄悄写进 configs/ 里。
		cfg.Capture.Dir = config.ResolveOutputDir(base, cfg.Capture.Dir)
	}

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
}

func dirOf(p string) string {
	if p == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "."
		}
		return wd
	}
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		wd, err := os.Getwd()
		if err != nil {
			return "."
		}
		return wd
	}
	if i == 0 {
		return p[:1]
	}
	return p[:i]
}

// ---------------------------- probe ----------------------------

func cmdProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/config.yaml", "配置文件路径")
	timeout := fs.Duration("timeout", 20*time.Second, "单个账号的探测超时")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	cfg.Creds.File, _ = config.ResolveDataPath(dirOf(*cfgPath), cfg.Creds.File)

	log := logx.Setup("warn", "text")

	store := account.NewStore(cfg.Creds.File, log)
	list, err := store.Load()
	if err != nil {
		return err
	}
	list = append(append([]config.AccountConfig{}, cfg.Creds.Accounts...), list...)
	if len(list) == 0 {
		return fmt.Errorf("没有账号可探测（配置文件与 %s 都是空的）", cfg.Creds.File)
	}

	client, err := httpc.New(cfg.Upstream, httpc.Options{})
	if err != nil {
		return err
	}
	pclient := prism.New(client, prism.UpstreamOptions{
		UserAgent: cfg.Upstream.UserAgent,
		Origin:    cfg.Upstream.Origin,
		Referer:   cfg.Upstream.Referer,
	}, prism.SchemaOptions{StartPath: cfg.Facade.Schema.StartPath, StatusPath: cfg.Facade.Schema.StatusPath})

	fmt.Printf("上游: %s\n账号: %d 个\n\n", cfg.Upstream.BaseURL, len(list))

	failures := 0
	for _, ac := range list {
		if !ac.IsEnabled() {
			continue
		}
		c := creds.FromAccountConfig(ac)

		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		p := prism.Principal{Client: client, Cred: c, AccountID: ac.ID}
		info, err := pclient.Session(ctx, p)
		cancel()

		if err != nil {
			failures++
			fmt.Printf("  [失败] %-16s %s\n", ac.ID, err)
			if creds.IsAuthError(err) {
				fmt.Printf("         → 凭据已失效。请重新登录 prism.openai.com，" +
					"从开发者工具复制 Cookie 或 accessToken 后用 import 导入。\n")
			}
			continue
		}

		email, _ := info["user"].(map[string]any)
		mailStr := ""
		if email != nil {
			mailStr, _ = email["email"].(string)
		}
		plan := ""
		if acc, ok := info["account"].(map[string]any); ok {
			plan, _ = acc["planType"].(string)
		}
		tok, _ := info["accessToken"].(string)
		exp := "未知"
		if t, ok := creds.TokenExpiry(tok); ok {
			exp = fmt.Sprintf("%s (剩余 %s)", t.Local().Format(time.RFC3339), time.Until(t).Round(time.Minute))
		}
		fmt.Printf("  [正常] %-16s email=%s plan=%s token到期=%s\n", ac.ID, mailStr, plan, exp)
	}

	if failures > 0 {
		return fmt.Errorf("%d 个账号不可用", failures)
	}
	fmt.Println("\n全部账号可用。")
	return nil
}

// ---------------------------- import ----------------------------

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	cfgPath := fs.String("config", "configs/config.yaml", "配置文件路径")
	out := fs.String("out", "", "凭据文件输出路径（默认取配置里的 creds.file）")
	id := fs.String("id", "main", "账号 ID")
	name := fs.String("name", "", "账号显示名")
	cookie := fs.String("cookie", "", "浏览器完整 Cookie 串")
	sessionToken := fs.String("session-token", "", "只给 session-token 的值")
	accessToken := fs.String("access-token", "", "直接给 JWT accessToken")
	refreshToken := fs.String("refresh-token", "", "OAuth refresh_token")
	proxy := fs.String("proxy", "", "该账号专属出口代理，如 http://127.0.0.1:7890")
	maxConc := fs.Int("max-concurrency", 2, "该账号并发上限")
	skipVerify := fs.Bool("skip-verify", false, "跳过在线校验（离线写入）")
	stdin := fs.Bool("stdin", false, "从标准输入读取 Cookie 串（避免 shell 历史泄漏）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *stdin {
		b, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return fmt.Errorf("读取标准输入: %w", err)
		}
		*cookie = strings.TrimSpace(string(b))
	}

	if *cookie == "" && *sessionToken == "" && *accessToken == "" && *refreshToken == "" {
		return fmt.Errorf("至少需要提供 -cookie / -session-token / -access-token / -refresh-token 之一" +
			"（推荐 -cookie，它信息最全）")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// 没有配置文件也要能用：这是"第一次导入凭据"的常见场景。
		cfg = config.Default()
	}
	base := dirOf(*cfgPath)
	credsPath := *out
	if credsPath == "" {
		// import 是"写"操作，因此：
		//   已存在的凭据文件 -> 就地更新（保持原位置，别搬走用户的文件）
		//   还不存在         -> 落在当前工作目录下（用户敲命令的地方）
		// 否则会出现"导入成功但文件跑到 configs/ 里面去了"的困惑。
		credsPath, _ = config.ResolveDataPath(base, cfg.Creds.File)
		if _, statErr := os.Stat(credsPath); os.IsNotExist(statErr) && !filepath.IsAbs(cfg.Creds.File) {
			credsPath = config.ResolveOutputDir(base, cfg.Creds.File)
		}
	}

	log := logx.Setup("warn", "text")
	store := account.NewStore(credsPath, log)

	existing, err := store.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: 现有凭据文件解析失败，将被覆盖: %v\n", err)
		existing = nil
	}

	ac := config.AccountConfig{
		ID:             *id,
		Name:           *name,
		Cookies:        *cookie,
		SessionToken:   *sessionToken,
		AccessToken:    *accessToken,
		RefreshToken:   *refreshToken,
		Proxy:          *proxy,
		MaxConcurrency: *maxConc,
	}
	if ac.Name == "" {
		ac.Name = ac.ID
	}

	c := creds.FromAccountConfig(ac)

	// 在线校验：把 session cookie 换成 accessToken，顺便拿到账号信息。
	// 这一步同时完成三件事：验证凭据、补全元信息、拿到 refresh_token（如果有）。
	if !*skipVerify {
		fmt.Println("正在向上游校验凭据…")
		client, err := httpc.New(cfg.Upstream, httpc.Options{})
		if err != nil {
			return err
		}
		up := cfg.Upstream
		if up.UserAgent == "" {
			up.UserAgent = config.DefaultUserAgent
		}
		r, err := creds.NewRefresher(cfg.Creds, up, client)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		next, rerr := r.Refresh(ctx, c)
		cancel()

		if rerr != nil {
			fmt.Fprintf(os.Stderr,
				"校验失败: %v\n"+
					"提示：请确认 Cookie 完整（至少包含 %s），且是从 prism.openai.com 登录后复制的最新值。\n"+
					"若确定抄写无误，可加 -skip-verify 先离线写入，稍后再用 probe 复查。\n",
				rerr, creds.CookieSessionToken)
			return rerr
		}
		c = next
		ac.AccessToken = c.AccessToken
		if c.RefreshToken != "" {
			ac.RefreshToken = c.RefreshToken
		}
		if c.CookieHeader != "" {
			ac.Cookies = c.CookieHeader
		}
		ac.AccountID = c.AccountID
		ac.Email = c.Email
		ac.Plan = c.Plan
		if !c.ExpiresAt.IsZero() {
			t := c.ExpiresAt
			ac.ExpiresAt = &t
		}
		fmt.Printf("校验通过: email=%s plan=%s account_id=%s token到期=%s\n",
			c.Email, c.Plan, c.AccountID, c.ExpiresAt.Local().Format(time.RFC3339))
	} else if c.AccessToken != "" {
		// 离线模式也能从 JWT 里挖出过期时间。
		if t, ok := creds.TokenExpiry(c.AccessToken); ok {
			ac.ExpiresAt = &t
		}
	}

	// upsert 到凭据文件。
	merged := make([]config.AccountConfig, 0, len(existing)+1)
	replaced := false
	for _, e := range existing {
		if e.ID == ac.ID {
			merged = append(merged, ac)
			replaced = true
			continue
		}
		merged = append(merged, e)
	}
	if !replaced {
		merged = append(merged, ac)
	}

	if err := store.Persist(merged); err != nil {
		return fmt.Errorf("写入凭据文件: %w", err)
	}

	fmt.Printf("\n已写入 %s（共 %d 个账号，本次为%s）\n", credsPath, len(merged),
		map[bool]string{true: "更新", false: "新增"}[replaced])
	fmt.Printf("自愈能力: %s\n", canRefreshDesc(c))
	fmt.Println("\n服务运行中时会自动热加载；未运行时下次启动即生效。")
	return nil
}

func canRefreshDesc(c *creds.Credential) string {
	if c == nil {
		return "无"
	}
	switch {
	case c.RefreshToken != "":
		return "有 refresh_token —— 可无限自动续期（最稳）"
	case c.SessionToken != "" || creds.HasSessionCookie(c.CookieHeader):
		return "有 session cookie —— 会话过期前可自动刷新，过期后需重新导入"
	case c.AccessToken != "":
		return "仅 access_token —— 到期后需重新导入"
	}
	return "无"
}

// ---------------------------- summary ----------------------------

func cmdSummary(args []string) error {
	fs := flag.NewFlagSet("capture-summary", flag.ExitOnError)
	file := fs.String("file", "", "抓包文件路径（capture-YYYY-MM-DD.jsonl）")
	jsonOut := fs.Bool("json", false, "以 JSON 输出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("必须指定 -file")
	}

	records, err := capture.ReplayFile(*file)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(records)
	}
	fmt.Print(capture.Summarize(records))
	return nil
}
