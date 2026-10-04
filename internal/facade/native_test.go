package facade

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/prism"
)

func nativeConv(system string, current string, history ...historyEntry) *nativeConversation {
	return &nativeConversation{system: system, history: history, current: prism.NewUserItem(current)}
}

// committed 模拟一轮成功：按本轮的全部条目写回绑定。
func committed(nt *nativeTurn, reply string) *nativeBinding {
	b := &nativeBinding{cid: "cdx1_x", account: "a", project: "p"}
	b.mu.Lock()
	nt.plan = &nativePlan{b: b, cid: "cdx1_x", delivered: nt.fingerprints(nt.conv.entries()), sysHash: textFingerprint(nt.conv.system)}
	nt.commit(&RunResult{AccountID: "a", ProjectID: "p", Text: reply}, nil)
	return b
}

// 强会话键（Codex）：助手条目由上游生成、回传形态不同，不参与比对；增量只含客户端新增的条目。
func TestNativeDelta_StrongKeySkipsAssistant(t *testing.T) {
	t1 := &nativeTurn{strong: true, conv: nativeConv("SYS", "读 a.txt")}
	b := committed(t1, "```codex-exec\ncat a.txt\n```")

	t2 := &nativeTurn{strong: true, conv: nativeConv("SYS", "[CLIENT RESULT] hello",
		historyEntry{"User", "读 a.txt"}, historyEntry{"Assistant", "[tool_call] exec(cat a.txt)"})}
	rest, ok := t2.delta(b, t2.conv.entries())
	if !ok || len(rest) != 1 || rest[0].text != "[CLIENT RESULT] hello" {
		t.Fatalf("增量应只有本轮工具结果: ok=%v %+v", ok, rest)
	}

	// 并行工具调用：两条结果都是新的，最后一条是本轮消息。
	t3 := &nativeTurn{strong: true, conv: nativeConv("SYS", "[CLIENT RESULT] two",
		historyEntry{"User", "读 a.txt"}, historyEntry{"Assistant", "call"}, historyEntry{"User", "[CLIENT RESULT] one"})}
	rest, ok = t3.delta(b, t3.conv.entries())
	if !ok || len(rest) != 2 {
		t.Fatalf("应带上两条新结果: ok=%v %+v", ok, rest)
	}

	// 历史被改写（首条不同）：对不上，须新建会话。
	t4 := &nativeTurn{strong: true, conv: nativeConv("SYS", "继续", historyEntry{"User", "读 b.txt"}, historyEntry{"Assistant", "x"})}
	if _, ok := t4.delta(b, t4.conv.entries()); ok {
		t.Fatal("改写过的历史不能接到旧会话上")
	}
}

// 弱键（首条消息指纹）会撞键：连助手原文一起比对，同开头的另一段对话不能串进来。
func TestNativeDelta_WeakKeyMatchesAssistantText(t *testing.T) {
	t1 := &nativeTurn{conv: nativeConv("", "你好")}
	b := committed(t1, "你好！我是 A")

	same := &nativeTurn{conv: nativeConv("", "第二问", historyEntry{"User", "你好"}, historyEntry{"Assistant", "你好！我是 A"})}
	if rest, ok := same.delta(b, same.conv.entries()); !ok || len(rest) != 1 || rest[0].text != "第二问" {
		t.Fatalf("同一段对话应只发本轮: ok=%v %+v", ok, rest)
	}
	other := &nativeTurn{conv: nativeConv("", "第二问", historyEntry{"User", "你好"}, historyEntry{"Assistant", "你好！我是 B"})}
	if _, ok := other.delta(b, other.conv.entries()); ok {
		t.Fatal("助手原文不同说明是另一段对话，不能续接")
	}
	// 重发同一轮（重试 / 重新生成）：没有新内容，新建会话。
	retry := &nativeTurn{conv: nativeConv("", "你好")}
	if _, ok := retry.delta(b, retry.conv.entries()); ok {
		t.Fatal("没有新条目时不能续接")
	}
	// 弱键下不按"只发本轮"追加。
	single := &nativeTurn{conv: nativeConv("", "无关的新问题")}
	if _, ok := single.delta(b, single.conv.entries()); ok {
		t.Fatal("弱键不带历史的请求是新对话")
	}
}

// 只发本轮消息的客户端（显式会话键）：直接追加。
func TestNativeDelta_SingleMessageClient(t *testing.T) {
	b := committed(&nativeTurn{strong: true, conv: nativeConv("", "记住 OLIVE-88")}, "好")
	nt := &nativeTurn{strong: true, conv: nativeConv("", "暗号是什么？")}
	if rest, ok := nt.delta(b, nt.conv.entries()); !ok || len(rest) != 1 || rest[0].text != "暗号是什么？" {
		t.Fatalf("应追加本轮: ok=%v %+v", ok, rest)
	}
}

// Codex 压缩后历史被替换成"user 消息 + 摘要"：认出上游写的摘要，从它之后接上同一个会话。
func TestNativeDelta_RebaseOnCompactionSummary(t *testing.T) {
	summary := "Progress summary: created a.txt and b.txt; next step is to run the tests. " + strings.Repeat("detail ", 20)
	comp := &nativeTurn{strong: true, compaction: true, conv: nativeConv("SYS", "You are performing a CONTEXT CHECKPOINT COMPACTION.",
		historyEntry{"User", "建两个文件"}, historyEntry{"Assistant", "call"}, historyEntry{"User", "[CLIENT RESULT] ok"})}
	b := committed(comp, summary)
	if b.summary == "" {
		t.Fatal("压缩轮应记下摘要")
	}

	after := &nativeTurn{strong: true, conv: nativeConv("SYS", "现在跑测试",
		historyEntry{"User", "建两个文件"},
		historyEntry{"User", "Another language model started to solve this problem and produced a summary of its thinking process.\n" + summary})}
	rest, ok := after.delta(b, after.conv.entries())
	if !ok || len(rest) != 1 || rest[0].text != "现在跑测试" {
		t.Fatalf("应从摘要之后接上: ok=%v %+v", ok, rest)
	}
	// 压缩发生在一轮中间：摘要就是最后一条，原样发给上游让它接着干。
	mid := &nativeTurn{strong: true, conv: nativeConv("SYS", "Another language model started… "+summary, historyEntry{"User", "建两个文件"})}
	if rest, ok := mid.delta(b, mid.conv.entries()); !ok || len(rest) != 1 {
		t.Fatalf("摘要是本轮消息时应续接: ok=%v %+v", ok, rest)
	}
}

// system 不每轮重发：未变时用一句话代替；变了、或累计增量超过阈值时重发完整内容。
func TestNativeDeltaItems_SystemRefresh(t *testing.T) {
	sys := strings.Repeat("bridge rules ", 1000)
	nt := &nativeTurn{strong: true, conv: nativeConv(sys, "[CLIENT RESULT] ok")}
	b := &nativeBinding{sysHash: textFingerprint(sys), sinceSys: 100}
	rest := nt.conv.entries()

	items, _, since := nt.deltaItems(b, rest, 96<<10, "")
	if got := itemText(items[0]); got != nativeBriefSystem {
		t.Fatalf("system 未变时不应重发: %.60q", got)
	}
	if since != 100+len("[CLIENT RESULT] ok") || itemText(items[1]) != "[CLIENT RESULT] ok" {
		t.Fatalf("累计字节或本轮消息不对: since=%d %q", since, itemText(items[1]))
	}

	b.sinceSys = nativeSystemRefresh
	items, _, since = nt.deltaItems(b, rest, 96<<10, "NOTICE")
	if got := itemText(items[0]); !strings.HasPrefix(got, "NOTICE\n\n") || !strings.Contains(got, sys) {
		t.Fatal("超过阈值应重发完整 system（连同平台声明）")
	}
	if since != len("[CLIENT RESULT] ok") {
		t.Fatalf("重发后累计应清零重算: %d", since)
	}

	nt.conv.system = "changed"
	nt.conv.extra = "<context_checkpoint>x</context_checkpoint>"
	b.sinceSys = 0
	items, h, _ := nt.deltaItems(b, rest, 96<<10, "")
	if got := itemText(items[0]); got != "changed\n\n"+nt.conv.extra || h != textFingerprint("changed") {
		t.Fatalf("system 变了应重发，附加指令每轮照发且不计入指纹: %q", got)
	}
}

// 本轮消息之前的新条目放在本轮消息前；放不下时按折叠规则裁剪，整条不超上限。
func TestNativeDeltaItems_PriorEntries(t *testing.T) {
	nt := &nativeTurn{strong: true, conv: nativeConv("S", "[CLIENT RESULT] last")}
	b := &nativeBinding{sysHash: textFingerprint("S")}
	rest := []historyEntry{{"User", "[CLIENT RESULT] first"}, {"User", "[CLIENT RESULT] last"}}
	items, _, _ := nt.deltaItems(b, rest, 0, "")
	user := itemText(items[1])
	if !strings.HasPrefix(user, nativeSinceHeader+"User: [CLIENT RESULT] first\n\n") || !strings.HasSuffix(user, "[CLIENT RESULT] last") {
		t.Fatalf("新条目应按序放在本轮消息之前: %q", user)
	}

	var big []historyEntry
	for i := 0; i < 20; i++ {
		big = append(big, historyEntry{"User", "[CLIENT RESULT] " + strings.Repeat("o", 2000)})
	}
	big = append(big, historyEntry{"User", "[CLIENT RESULT] last"})
	items, _, _ = nt.deltaItems(b, big, 12000, "")
	if n := promptBytes(items); n > 12000 || !strings.Contains(itemText(items[1]), historyOmittedMark) {
		t.Fatalf("超限应裁剪并注明（%d 字节）", n)
	}
}

func TestNativeCommitAndRelease(t *testing.T) {
	b := &nativeBinding{cid: "old", account: "a", project: "p", delivered: []uint64{1}}
	nt := &nativeTurn{conv: nativeConv("", "q")}
	b.mu.Lock()
	nt.plan = &nativePlan{b: b, cid: "old", continued: true}
	nt.release(true, nil)
	if b.cid != "" || len(b.delivered) != 0 || !b.mu.TryLock() {
		t.Fatal("作废后应清空并解锁")
	}
	b.mu.Unlock()

	// 本轮无状态（没有会话 ID）：成功后绑定清空，下一轮重新建会话。
	b.cid = "old"
	b.mu.Lock()
	nt.plan = &nativePlan{b: b}
	nt.commit(&RunResult{Text: "x"}, nil)
	if b.cid != "" || !b.mu.TryLock() {
		t.Fatal("无状态轮次应清空绑定并解锁")
	}
}

func TestIsConversationGone(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
	}{
		{"conversation_too_large", true},
		{"Conversation not found", true},
		{"Error while processing conversation (403 Forbidden)", false},
		{"sandbox_reconnecting", false},
	} {
		if got := isConversationGone(errString(c.msg)); got != c.want {
			t.Errorf("%q: got %v want %v", c.msg, got, c.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestItemsConversation(t *testing.T) {
	items := []prism.InputItem{
		prism.NewSystemItem("bridge"),
		prism.NewUserItem("# AGENTS.md instructions for /x\nrules"),
		prism.NewUserItem("任务"),
		prism.NewAssistantItem("call"),
		prism.NewUserItem("[CLIENT RESULT] ok"),
		prism.NewSystemItem("tail reminder"),
	}
	c := itemsConversation(items)
	if c == nil || c.system != "bridge\n\ntail reminder" || itemText(c.current) != "[CLIENT RESULT] ok" {
		t.Fatalf("拆分不对: %+v", c)
	}
	if len(c.history) != 2 || c.history[0].text != "任务" || c.history[1].speaker != "Assistant" {
		t.Fatalf("往轮对话应与折叠路径一致（滤掉静态指令）: %+v", c.history)
	}
}

func TestChatConversation(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: stringContent("S")},
		{Role: "user", Content: stringContent("q1")},
		{Role: "assistant", Content: stringContent("a1")},
		{Role: "user", Content: stringContent("q2")},
	}
	c := chatConversation(msgs, "D")
	if c == nil || c.system != "S" || len(c.history) != 2 || itemText(c.current) != "q2" {
		t.Fatalf("拆分不对: %+v", c)
	}
	// 用户消息之后还有工具往返：交给全量路径。
	msgs = append(msgs, ChatMessage{Role: "assistant", Content: stringContent("")}, ChatMessage{Role: "tool", Content: stringContent("r")})
	if chatConversation(msgs, "D") != nil {
		t.Fatal("工具调用回合不走原生续接")
	}
}

// 新建会话时历史一条放不下：切成若干段补种，每段不超过上限，只补最近的部分。
func TestNativeSeedTurns(t *testing.T) {
	var hist []historyEntry
	for i := 0; i < 40; i++ {
		hist = append(hist, historyEntry{"User", fmt.Sprintf("Q%02d %s", i, strings.Repeat("长", 1000))})
	}
	nt := &nativeTurn{strong: true, conv: nativeConv("SYS", "当前问题", hist...)}
	const limit = 24000
	seeds := nt.seedTurns(limit)
	if len(seeds) < 2 {
		t.Fatalf("应切成多段: %d", len(seeds))
	}
	var all strings.Builder
	for k, s := range seeds {
		if n := promptBytes(s); n > limit {
			t.Fatalf("第 %d 段 %d 字节超过上限", k+1, n)
		}
		u := itemText(s[1])
		if !strings.HasPrefix(u, fmt.Sprintf("[Earlier conversation, part %d of %d", k+1, len(seeds))) {
			t.Fatalf("段头不对: %.80q", u)
		}
		all.WriteString(u)
	}
	for _, want := range []string{"Q00 ", "Q39 "} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("补种内容缺少 %q", want)
		}
	}
	if nt.seedTurns(0) != nil {
		t.Fatal("不限长时不补种")
	}

	// 超过补种总量：只补最近的，注明省略。
	big := make([]historyEntry, 0, 400)
	for i := 0; i < 400; i++ {
		big = append(big, historyEntry{"User", fmt.Sprintf("B%03d %s", i, strings.Repeat("x", 4000))})
	}
	nt.conv.history = big
	seeds = nt.seedTurns(96 << 10)
	first, last := itemText(seeds[0][1]), itemText(seeds[len(seeds)-1][1])
	if !strings.Contains(first, "older messages omitted") || strings.Contains(first, "B000 ") || !strings.Contains(last, "B399 ") {
		t.Fatal("应只补最近的并注明省略")
	}
}

// 弱键建的绑定凭句柄（回复 ID / 会话 ID）找回时：只发本轮的客户端直接追加，
// 带历史的仍按弱键连助手原文一起比对。
func TestNativeDelta_WeakBindingFoundByHandle(t *testing.T) {
	b := committed(&nativeTurn{conv: nativeConv("", "你好")}, "你好！我是 A")
	if !b.weak {
		t.Fatal("弱键建的绑定应记为弱键")
	}
	single := &nativeTurn{strong: true, weak: b.weak, conv: nativeConv("", "第二问")}
	if rest, ok := single.delta(b, single.conv.entries()); !ok || len(rest) != 1 || rest[0].text != "第二问" {
		t.Fatalf("凭句柄找回、只发本轮的客户端应直接追加: ok=%v %+v", ok, rest)
	}
	full := &nativeTurn{strong: true, weak: b.weak, conv: nativeConv("", "第二问",
		historyEntry{"User", "你好"}, historyEntry{"Assistant", "你好！我是 A"})}
	if rest, ok := full.delta(b, full.conv.entries()); !ok || len(rest) != 1 || rest[0].text != "第二问" {
		t.Fatalf("带历史时应按弱键比对并只发本轮: ok=%v %+v", ok, rest)
	}
}

// 新会话提交后，回传给客户端的会话 ID（"cid:" 键，按租户隔离）指向同一个绑定；
// 会话链键也能挂上来（Responses 凭 previous_response_id 找回）。
func TestNativeCommit_RegistersAliases(t *testing.T) {
	key := "k:feedfacefeedface|f:alias-test"
	b := nativeBindingFor(key)
	b.mu.Lock()
	nt := &nativeTurn{key: key, conv: nativeConv("", "hi")}
	nt.plan = &nativePlan{b: b, cid: "cdx1_alias", delivered: nt.fingerprints(nt.conv.entries())}
	nt.commit(&RunResult{AccountID: "a", ProjectID: "p", Text: "yo"}, nil)

	if got := nativeBindingPeek("k:feedfacefeedface|cid:cdx1_alias"); got != b {
		t.Fatal("回传的会话 ID 应能找回绑定")
	}
	if nativeBindingPeek("cid:cdx1_alias") != nil {
		t.Fatal("会话 ID 别名不应跨租户")
	}
	nativeAlias("k:feedfacefeedface|r:resp_1", key)
	if nativeBindingPeek("k:feedfacefeedface|r:resp_1") != b {
		t.Fatal("会话链键应指向同一个绑定")
	}
}

// memNativeStore 是测试用的内存版 NativeStore。
type memNativeStore struct {
	mu   sync.Mutex
	recs map[string]account.NativeBindingRecord
}

func (m *memNativeStore) SaveNativeBinding(rec account.NativeBindingRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[rec.Key] = rec
	return nil
}

func (m *memNativeStore) DeleteNativeBinding(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, key)
	return nil
}

func (m *memNativeStore) LoadNativeBindings(since time.Time) ([]account.NativeBindingRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []account.NativeBindingRecord
	for _, r := range m.recs {
		if !r.Updated.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// 绑定落盘：网关重启（内存表清空）后载入，客户端会话接回原来的上游会话，别名也在；
// 上游会话作废时连同记录删除。
func TestNativeStore_SurvivesRestart(t *testing.T) {
	st := &memNativeStore{recs: map[string]account.NativeBindingRecord{}}
	r := &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), nativeStore: st}
	key := "k:0badc0de0badc0de|h:persist-test"
	b := nativeBindingFor(key)
	b.mu.Lock()
	nt := &nativeTurn{key: key, strong: true, conv: nativeConv("SYS", "记住 PERSIST-9")}
	nt.plan = &nativePlan{b: b, cid: "cdx1_persist", delivered: nt.fingerprints(nt.conv.entries()), sysHash: textFingerprint("SYS"), sinceSys: 42}
	nt.commit(&RunResult{AccountID: "acct", ProjectID: "proj", Text: "好"}, r)
	r.AliasNative("k:0badc0de0badc0de|r:resp_p", key)
	if rec, ok := st.recs[key]; !ok || rec.CID != "cdx1_persist" || len(rec.Aliases) != 2 {
		t.Fatalf("提交后应落盘（含会话 ID 与会话链两个别名）: %+v", st.recs[key])
	}

	// 模拟重启：内存表清空，再从存储载入。
	nativeBindings.mu.Lock()
	for _, k := range []string{key, "k:0badc0de0badc0de|cid:cdx1_persist", "k:0badc0de0badc0de|r:resp_p"} {
		delete(nativeBindings.m, k)
	}
	nativeBindings.mu.Unlock()
	r.UseNativeStore(st)
	got := nativeBindingPeek(key)
	if got == nil || got.cid != "cdx1_persist" || got.account != "acct" || got.project != "proj" || got.sinceSys != 42 ||
		len(got.delivered) != 1 || got.delivered[0] != entryFingerprint(historyEntry{"User", "记住 PERSIST-9"}) {
		t.Fatalf("重启后应恢复绑定: %+v", got)
	}
	if nativeBindingPeek("k:0badc0de0badc0de|cid:cdx1_persist") != got || nativeBindingPeek("k:0badc0de0badc0de|r:resp_p") != got {
		t.Fatal("别名应指向恢复出的同一个绑定")
	}
	next := &nativeTurn{key: key, strong: true, conv: nativeConv("SYS", "暗号是什么？", historyEntry{"User", "记住 PERSIST-9"}, historyEntry{"Assistant", "好"})}
	if rest, ok := next.delta(got, next.conv.entries()); !ok || len(rest) != 1 || rest[0].text != "暗号是什么？" {
		t.Fatalf("恢复后应只发增量: ok=%v %+v", ok, rest)
	}

	got.mu.Lock()
	gone := &nativeTurn{key: key, strong: true, conv: next.conv, plan: &nativePlan{b: got, cid: got.cid, continued: true}}
	gone.release(true, r)
	if _, ok := st.recs[key]; ok {
		t.Fatal("上游会话作废时应删除落盘记录")
	}
}

// 会话链键每轮都可能新增：只留最近的若干个（淘汰的表项一并删除），上游会话 ID 的别名不淘汰。
func TestNativeAlias_Bounded(t *testing.T) {
	key := "k:ababababababab|f:alias-cap"
	b := nativeBindingFor(key)
	nativeBindings.mu.Lock()
	b.addAliasLocked("k:ababababababab|cid:cdx1_cap")
	nativeBindings.mu.Unlock()
	for i := 0; i < chainMaxAliases*3; i++ {
		nativeAlias(fmt.Sprintf("k:ababababababab|r:resp_%d", i), key)
	}
	nativeBindings.mu.Lock()
	n := len(b.aliases)
	nativeBindings.mu.Unlock()
	if n > chainMaxAliases {
		t.Fatalf("别名 %d 个，超过上限 %d", n, chainMaxAliases)
	}
	if nativeBindingPeek("k:ababababababab|cid:cdx1_cap") != b {
		t.Fatal("上游会话 ID 的别名不应被淘汰")
	}
	if nativeBindingPeek("k:ababababababab|r:resp_0") != nil || nativeBindingPeek(fmt.Sprintf("k:ababababababab|r:resp_%d", chainMaxAliases*3-1)) != b {
		t.Fatal("应淘汰最旧的会话链键、保留最新的")
	}
}
