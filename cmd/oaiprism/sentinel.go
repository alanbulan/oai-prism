package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
	"github.com/oai-prism/oaiprism/internal/logx"
	"github.com/oai-prism/oaiprism/internal/upstream"
)

// cmdSentinel 自检纯 Go 的 Sentinel 签发：连 sentinel.openai.com 签几个 token 并解读内容。
// 只访问 sentinel.openai.com 与 Prism 首页，不带任何账号凭据。
func cmdSentinel(args []string) error {
	fs := flag.NewFlagSet("sentinel", flag.ContinueOnError)
	cfgPath := fs.String("config", "configs/config.yaml", "配置文件路径（读取出站代理与指纹设置）")
	n := fs.Int("n", 2, "签几个 token")
	debug := fs.Bool("debug", false, "调试日志")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		cfg = config.Default()
	}
	level := "info"
	if *debug {
		level = "debug"
	}
	log := logx.Setup(level, "text")
	t, err := upstream.Shared(upstream.Options{Proxy: cfg.Upstream.HTTPProxy, ProfilePath: cfg.Upstream.SentinelProfile, Logger: log})
	if err != nil {
		return err
	}
	defer t.Close()
	s := t.Signer()
	fmt.Printf("指纹 UA: %s\n", s.Profile().UserAgent())
	for i := 1; i <= *n; i++ {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		tok, err := s.Token(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("第 %d 个 token 签发失败: %w", i, err)
		}
		fmt.Printf("#%d 用时 %v，%s\n", i, time.Since(start).Round(time.Millisecond), describeToken(tok))
		if i < *n {
			time.Sleep(time.Second)
		}
	}
	st := s.Stats()
	fmt.Printf("签发 %d 个，失败 %d 个\n", st.Signed, st.Failed)
	return nil
}

func describeToken(tok string) string {
	var m map[string]string
	if err := json.Unmarshal([]byte(tok), &m); err != nil {
		return "不是 JSON: " + tok
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	t := m["t"]
	head, _ := base64.StdEncoding.DecodeString(t)
	state := "dx 完整执行"
	if len(t) < 200 {
		state = fmt.Sprintf("dx 未正常结束（%q）", string(head))
	}
	return fmt.Sprintf("字段 %s；p %d 字符，t %d 字符（%s），c %d 字符，flow=%s",
		strings.Join(sortedKeys(keys), ","), len(m["p"]), len(t), state, len(m["c"]), m["flow"])
}

func sortedKeys(k []string) []string {
	order := map[string]int{"p": 0, "t": 1, "c": 2, "id": 3, "flow": 4}
	for i := 1; i < len(k); i++ {
		for j := i; j > 0 && order[k[j]] < order[k[j-1]]; j-- {
			k[j], k[j-1] = k[j-1], k[j]
		}
	}
	return k
}
