package account

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// 回归测试（2026-10-03 CI 事故）：SQLite 初始化失败时 server 会拿到
// nil store 并继续运行；任何方法在 nil 接收者/零值实例上都不能 panic
// —— 记录请求日志发生在后台 goroutine 里，一次解引用就能崩掉进程。
func TestSQLiteStore_NilSafety(t *testing.T) {
	var nilStore *SQLiteStore
	zero := &SQLiteStore{} // db 未打开的零值实例

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	_ = log

	// nil 接收者：全部方法都不能 panic
	_ = nilStore.Close()
	if got := nilStore.Path(); got != "" {
		t.Fatalf("nil Path() 应为空串，得到 %q", got)
	}
	if _, err := nilStore.Load(); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("nil Load() 应返回 errSQLiteUnavailable，得到 %v", err)
	}
	if err := nilStore.RecordRequestLog(RequestLogItem{}); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("nil RecordRequestLog 应返回 errSQLiteUnavailable，得到 %v", err)
	}
	if _, _, err := nilStore.QueryRequestLogs(RequestLogFilter{}); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("nil QueryRequestLogs 应返回 errSQLiteUnavailable，得到 %v", err)
	}
	if _, err := nilStore.GetAggregatedStats(); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("nil GetAggregatedStats 应返回 errSQLiteUnavailable，得到 %v", err)
	}
	if _, err := nilStore.ListChatSessions(); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("nil ListChatSessions 应返回 errSQLiteUnavailable，得到 %v", err)
	}
	if _, err := nilStore.ListAPIKeys(); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("nil ListAPIKeys 应返回 errSQLiteUnavailable，得到 %v", err)
	}

	// 零值实例（db=nil）：同样的契约
	if _, err := zero.Load(); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("零值 Load() 应返回 errSQLiteUnavailable，得到 %v", err)
	}
	if err := zero.RecordRequestLog(RequestLogItem{}); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("零值 RecordRequestLog 应返回 errSQLiteUnavailable，得到 %v", err)
	}
}

// 正常路径不受 guard 影响：打开 -> 写入 -> 读回。
func TestSQLiteStore_RecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "test.db"), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	item := RequestLogItem{ID: "r1", Method: "POST", Path: "/v1/chat/completions"}
	if err := store.RecordRequestLog(item); err != nil {
		t.Fatalf("RecordRequestLog: %v", err)
	}
	items, total, err := store.QueryRequestLogs(RequestLogFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("QueryRequestLogs: %v", err)
	}
	if total < 1 || len(items) < 1 || items[0].Path != "/v1/chat/completions" {
		t.Fatalf("查询结果异常: total=%d items=%+v", total, items)
	}

	// 账号 CRUD 冒烟
	acc := config.AccountConfig{ID: "a1", Name: "test"}
	if err := store.SaveAccount(acc); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	list, err := store.Load()
	if err != nil || len(list) == 0 {
		t.Fatalf("Load: err=%v list=%+v", err, list)
	}
}

// 回归：走势图曾按 '%H:%M' 分桶并按字符串取前 24 个，得到的是"一天里最早的
// 24 个分钟"。现在必须是最近 24 个整点小时、按时间先后排列、空桶补零。
func TestSQLiteStore_HourlySeriesAndStatusClass(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "series.db"), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	// 固定时钟：避免测试恰好跨整点时桶位漂移
	now := time.Date(2026, 10, 3, 14, 30, 0, 0, time.UTC)
	logs := []RequestLogItem{
		{ID: "a", Timestamp: now, StatusCode: 200, DurationMs: 100, PromptTokens: 30, CompletionTokens: 12},
		{ID: "b", Timestamp: now, StatusCode: 502, DurationMs: 300},
		{ID: "c", Timestamp: now.Add(-3 * time.Hour), StatusCode: 401},
		{ID: "d", Timestamp: now.Add(-30 * time.Hour), StatusCode: 200}, // 窗口外
	}
	for _, l := range logs {
		if err := store.RecordRequestLog(l); err != nil {
			t.Fatalf("RecordRequestLog: %v", err)
		}
	}

	ts := hourlySeries(store.db, now)
	if len(ts) != 24 {
		t.Fatalf("应固定 24 个小时桶，得到 %d", len(ts))
	}
	for i := 1; i < len(ts); i++ {
		if ts[i].Timestamp <= ts[i-1].Timestamp {
			t.Fatalf("桶未按时间升序: %s 之后是 %s", ts[i-1].Timestamp, ts[i].Timestamp)
		}
	}
	last, back3 := ts[23], ts[20]
	if last.Requests != 2 || last.Failures != 1 || last.Latency != 200 || last.Tokens != 42 {
		t.Fatalf("当前小时桶异常: %+v", last)
	}
	if back3.Requests != 1 || back3.Failures != 1 {
		t.Fatalf("3 小时前的桶异常: %+v", back3)
	}
	total := 0
	for _, p := range ts {
		total += p.Requests
	}
	if total != 3 {
		t.Fatalf("窗口外的记录不应计入，总数 %d", total)
	}

	agg, err := store.GetAggregatedStats()
	if err != nil {
		t.Fatalf("GetAggregatedStats: %v", err)
	}
	if agg.TotalPromptTokens != 30 || agg.TotalCompletionTokens != 12 {
		t.Fatalf("token 汇总异常: prompt=%d completion=%d", agg.TotalPromptTokens, agg.TotalCompletionTokens)
	}

	for class, want := range map[int]int{2: 2, 4: 1, 5: 1} {
		_, n, err := store.QueryRequestLogs(RequestLogFilter{StatusClass: class})
		if err != nil || n != want {
			t.Fatalf("%dxx 过滤: n=%d err=%v，期望 %d", class, n, err, want)
		}
	}
}

// 流式请求的响应头（200）一旦发出，状态码就改不了；中途失败只能记在 error_message。
// 这类请求必须算失败 —— 否则统计里成功率虚高，筛"失败"也找不到它们。
func TestSQLiteStore_StreamFailureCountsAsFailure(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "outcome.db"), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC()
	for _, l := range []RequestLogItem{
		{ID: "ok", Timestamp: now, StatusCode: 200},
		{ID: "midstream", Timestamp: now, StatusCode: 200, ErrorMessage: "chat 流式失败: 沙箱工作区同步未就绪"},
		{ID: "bad", Timestamp: now, StatusCode: 502, ErrorMessage: "上游 502"},
	} {
		if err := store.RecordRequestLog(l); err != nil {
			t.Fatalf("RecordRequestLog: %v", err)
		}
	}

	agg, err := store.GetAggregatedStats()
	if err != nil {
		t.Fatalf("GetAggregatedStats: %v", err)
	}
	if agg.TotalRequests != 3 || agg.Failures != 2 {
		t.Fatalf("流式中途失败应计入失败: total=%d failures=%d", agg.TotalRequests, agg.Failures)
	}
	if ts := hourlySeries(store.db, now); ts[23].Failures != 2 {
		t.Fatalf("走势图当前小时失败数 = %d，期望 2", ts[23].Failures)
	}
	for outcome, want := range map[string]int{"ok": 1, "failed": 2} {
		_, n, err := store.QueryRequestLogs(RequestLogFilter{Outcome: outcome})
		if err != nil || n != want {
			t.Fatalf("%s 过滤: n=%d err=%v，期望 %d", outcome, n, err, want)
		}
	}
}

// 并发回归（2026-10-03 CI 实证）：Close() 与后台写入竞争时不能 panic。
// 修复前 Close 在锁内把 s.db 置 nil，而写入方法的 ready() 检查在锁外 ——
// ready() 过后 db 被清空，exec(nil) 直接 panic。
func TestSQLiteStore_CloseRace(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "race.db"), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	// 写侧：持续写日志（模拟 requestAuditMiddleware 的后台 goroutine）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				// 允许返回 errSQLiteUnavailable（Close 之后），但不能 panic
				_ = store.RecordRequestLog(RequestLogItem{Method: "POST", Path: "/v1/chat/completions"})
			}
		}
	}()
	// 关闭侧：与其他操作并发
	time.Sleep(20 * time.Millisecond)
	_ = store.Close()
	close(done)
	wg.Wait()

	// Close 之后再调用也必须安全
	if err := store.RecordRequestLog(RequestLogItem{}); !errors.Is(err, errSQLiteUnavailable) {
		t.Fatalf("Close 后 RecordRequestLog 应返回 errSQLiteUnavailable，得到 %v", err)
	}
	_ = store.Close() // 幂等
}
