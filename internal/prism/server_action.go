package prism

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/oai-prism/oaiprism/internal/creds"
)

// Prism 前端经 Next.js Server Action 管理 AI 会话。会话 ID 形如 cdx1_<uuid>，只能由服务端
// 创建：自造的 ID 上游不认（1b1a67b：首轮带随机 ID 直接 403），不带 ID 时上游临时分配的
// 会话也不会续接（2026-10-04 实测，见 facade/native.go）。
//
// Action ID 是前端构建产物里 createServerReference("<id>", …, "<name>") 的第一个参数。
// Next.js 会在重新构建时换 ID，所以内置值只是起点：调用回 404（Server action not found）
// 时从首页引用的脚本里重新找一次（discoverServerAction）。
const (
	ActionCreateProjectConversation = "createProjectConversation" // (projectId) → cid
)

// knownServerActions 是 2026-10-03 前端构建里的 ID（.ref/prism_frontend）。
var knownServerActions = map[string]string{
	ActionCreateProjectConversation: "60f6ef46a6584bd2320328016a144c15d9401a47e1",
}

// serverActionIDs 是运行期发现的 ID（覆盖内置值），按 Client 共享。
type serverActionIDs struct {
	mu  sync.Mutex
	ids map[string]string
}

func (c *Client) actionID(name string) string {
	c.actions.mu.Lock()
	defer c.actions.mu.Unlock()
	if id := c.actions.ids[name]; id != "" {
		return id
	}
	return knownServerActions[name]
}

func (c *Client) setActionID(name, id string) {
	c.actions.mu.Lock()
	defer c.actions.mu.Unlock()
	if c.actions.ids == nil {
		c.actions.ids = map[string]string{}
	}
	c.actions.ids[name] = id
}

// errActionNotFound 表示 Action ID 已过期（前端重新构建过）。
var errActionNotFound = errors.New("server action not found")

// ServerAction 按名字调用 Prism 前端的一个 Server Action，返回解析后的返回值（JSON）。
// ID 过期时自动重新发现并重试一次。
func (c *Client) ServerAction(ctx context.Context, p Principal, name string, args ...any) (json.RawMessage, error) {
	id := c.actionID(name)
	if id != "" {
		raw, err := c.callServerAction(ctx, p, id, args)
		if !errors.Is(err, errActionNotFound) {
			return raw, err
		}
		c.logf("Server Action %s 的 ID %s 已失效，重新发现", name, id)
	}
	fresh, err := c.discoverServerAction(ctx, p, name)
	if err != nil {
		return nil, fmt.Errorf("发现 Server Action %s: %w", name, err)
	}
	if fresh == id {
		return nil, fmt.Errorf("server action %s (%s) 不可用", name, id)
	}
	c.setActionID(name, fresh)
	return c.callServerAction(ctx, p, fresh, args)
}

// callServerAction 发起一次 Server Action 调用。
//
// 协议：POST 页面路径，头 Next-Action: <id>，请求体是参数数组的 JSON，
// 响应是 RSC 流（text/x-component），每行 "<十六进制行号>:<JSON>"，
// 第 0 行形如 {"a":"$@1",...}，a 是返回值或指向另一行的引用。
func (c *Client) callServerAction(ctx context.Context, p Principal, id string, args []any) (json.RawMessage, error) {
	if args == nil {
		args = []any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("序列化 Server Action 参数: %w", err)
	}
	hdr := c.buildHeaders(p, "text/plain;charset=UTF-8", "text/x-component")
	hdr["Next-Action"] = id
	resp, err := c.Do(withNoReplay(ctx), p, http.MethodPost, "/", headerFromMap(hdr), body, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 Server Action 响应: %w", err)
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	if c.rec != nil {
		c.rec.RecordResponse(Meta{Method: http.MethodPost, Path: "/ [action " + short + "]", Status: resp.StatusCode}, resp.Header, raw)
	}
	if resp.StatusCode == http.StatusNotFound || bytes.Contains(raw, []byte("Server action not found")) {
		return nil, errActionNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &creds.APIError{Op: "server action " + short, Status: resp.StatusCode, Body: truncate(string(raw), 300)}
	}
	return parseActionResult(raw)
}

// parseActionResult 从 RSC 流里取出 Server Action 的返回值。
func parseActionResult(raw []byte) (json.RawMessage, error) {
	rows := map[string]json.RawMessage{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i <= 0 || !json.Valid([]byte(line[i+1:])) {
			continue
		}
		rows[line[:i]] = json.RawMessage(line[i+1:])
	}
	var head struct {
		A json.RawMessage `json:"a"`
	}
	if err := json.Unmarshal(rows["0"], &head); err != nil || head.A == nil {
		return nil, fmt.Errorf("Server Action 响应无返回值: %s", truncate(string(raw), 300))
	}
	v := head.A
	for range 4 { // 跟随 "$@<行号>" 引用
		var s string
		if json.Unmarshal(v, &s) != nil || !strings.HasPrefix(s, "$@") {
			break
		}
		next, ok := rows[strings.TrimPrefix(s, "$@")]
		if !ok {
			return nil, fmt.Errorf("Server Action 返回值引用 %s 缺失", s)
		}
		v = next
	}
	return v, nil
}

var scriptSrcRe = regexp.MustCompile(`src="(/_next/static/[^"]+\.js)"`)

// actionRefRe 匹配 createServerReference)("<id>",uZ.callServer,void 0,uZ.findSourceMapURL,"<name>")。
func actionRefRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`createServerReference\)\("([0-9a-f]{40,})",[^"]*"` + regexp.QuoteMeta(name) + `"\)`)
}

// discoverServerAction 从首页引用的脚本里找出 Action 当前的 ID。
func (c *Client) discoverServerAction(ctx context.Context, p Principal, name string) (string, error) {
	page, err := c.fetchText(ctx, p, "/", "text/html")
	if err != nil {
		return "", err
	}
	re := actionRefRe(name)
	seen := map[string]bool{}
	for _, m := range scriptSrcRe.FindAllStringSubmatch(page, -1) {
		src := m[1]
		if seen[src] {
			continue
		}
		seen[src] = true
		js, err := c.fetchText(ctx, p, src, "*/*")
		if err != nil {
			continue
		}
		if mm := re.FindStringSubmatch(js); mm != nil {
			return mm[1], nil
		}
	}
	return "", fmt.Errorf("首页的 %d 个脚本里没有 %s", len(seen), name)
}

// fetchText GET 一个前端资源（首页或脚本）。
func (c *Client) fetchText(ctx context.Context, p Principal, path, accept string) (string, error) {
	hdr := c.buildHeaders(p, "", accept)
	resp, err := c.Do(ctx, p, http.MethodGet, path, headerFromMap(hdr), nil, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &creds.APIError{Op: "GET " + path, Status: resp.StatusCode, Body: truncate(string(raw), 200)}
	}
	return string(raw), nil
}
