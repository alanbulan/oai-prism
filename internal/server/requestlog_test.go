package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 请求流水：客户端中途断开、实际使用的模型（2026-10-05：Claude Code 等满 5 分钟没收到数据后断开，
// 流式响应头已是 200，流水里成了一条没有错误的成功记录；模型列显示的是它发来的 claude-*）。

type logRow struct {
	Path         string `json:"path"`
	Model        string `json:"model"`
	StatusCode   int    `json:"status_code"`
	ErrorMessage string `json:"error_message"`
}

// waitLogs 等异步落库，返回最新的 n 条流水。
func waitLogs(t *testing.T, base string, n int) []logRow {
	t.Helper()
	for i := 0; i < 100; i++ {
		code, out := doLocal(t, http.MethodGet, base+"/admin/requests?page=1&page_size=20", "", nil)
		if code != http.StatusOK {
			t.Fatalf("GET /admin/requests %d", code)
		}
		var body struct {
			Items []logRow `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &body); err != nil {
			t.Fatal(err)
		}
		var rows []logRow
		for _, it := range body.Items {
			if strings.HasPrefix(it.Path, "/v1/") {
				rows = append(rows, it)
			}
		}
		if len(rows) >= n {
			return rows
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到 %d 条流水", n)
	return nil
}

func TestRequestLog_ActualModel(t *testing.T) {
	ts, _, srv := newTestServerWithSrv(t, &fakeUpstream{t: t}, goodAccount(), nil)
	// Claude Code 发 claude-* 模型名：流水记网关实际用的默认模型；OpenAI 客户端请求的名字照记
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/messages",
		`{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}`, jsonHdr); code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, out)
	}
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/v1/chat/completions",
		`{"model":"gpt-5","messages":[{"role":"user","content":"你好"}]}`, jsonHdr); code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", code, out)
	}
	rows := waitLogs(t, ts.URL, 2)
	if rows[0].Model != "gpt-5" || rows[1].Model != srv.cfg.Facade.DefaultModel {
		t.Fatalf("流水模型不对: chat=%q messages=%q（默认模型 %q）", rows[0].Model, rows[1].Model, srv.cfg.Facade.DefaultModel)
	}
}

func TestRequestLog_StreamClientAbortIs499(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t, pollDelay: 5 * time.Second}, goodAccount(), nil)
	bodies := map[string]string{
		"/v1/chat/completions": `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"你好"}]}`,
		"/v1/responses":        `{"model":"gpt-5","stream":true,"input":"你好"}`,
		"/v1/messages":         `{"model":"claude-x","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"你好"}]}`,
	}
	for path, body := range bodies {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body) // 读到超时断开
			resp.Body.Close()
		}
		cancel()
	}
	rows := waitLogs(t, ts.URL, 3)
	for _, r := range rows[:3] {
		if r.StatusCode != 499 || !strings.Contains(r.ErrorMessage, "断开") {
			t.Fatalf("%s 客户端中途断开应记 499: %+v", r.Path, r)
		}
	}
}
