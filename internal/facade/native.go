package facade

// 原生续接：会话历史交给上游保管，每轮只发增量。
//
// 上游（Prism 沙箱里的 Codex）按会话 ID 在服务端保存完整对话 —— 真实前端就是这么用的：
// 每轮只发 [system, 本轮 user] 加 conversationId。上限没测到：2026-10-04 同一会话灌入
// 230 万 tokens 随机字母，回头问最早几段的开头字母仍逐字答对，会话本身从未报错（最后是
// 账号被限流才停）；每轮耗时不随会话变长而增加，说明上游自己管理长会话（不是每轮全文
// 喂给模型）。
// 关键是会话 ID 必须经前端的 Server Action（createProjectConversation）在服务端
// 创建：2026-10-04 Go 侧实测（internal/facade 的 liveprobe）——
//
//	登记过的 cid + 增量             → 记得暗号（只带 cid 就够，previousResponseId / 快照可有可无）
//	不带 cid（上游临时分配一个）+ 增量 → 不记得；把临时 cid 带回去也不记得
//	自造 cid                        → 403（1b1a67b）
//
// 2026-10-03 "只发增量必然失忆"的结论（prism-upstream-no-server-side-history）就是
// 没登记会话造成的。
//
// 本文件维护"客户端会话 → 上游会话"的绑定，并算出每轮该发的增量：
//
//   - 客户端（Codex、Chat 客户端）每轮仍发完整历史。按条目指纹比对绑定里已送达的
//     前缀，只把之后的新条目发给上游；对不上（换账号、改写了历史）就新建上游会话：
//     历史一条放得下就随首轮发出，放不下就先分段补种。绑定落盘（native_store.go），
//     网关重启不算对不上。
//   - 强会话键（Codex 会话 ID、X-Oaiprism-Session 等）下助手条目不参与比对：它们由
//     上游生成，客户端回传的形态（工具调用项）与上游原文不同。弱键（首条消息指纹）
//     会撞键，必须连助手原文一起比对，防止两段同开头的对话串进同一个上游会话。
//     比对方式随绑定走：弱键建的绑定之后凭回复句柄 / 会话 ID 找回时也照弱键比。
//   - 只发本轮消息的客户端（Responses 的 previous_response_id、回传会话 ID 的 Chat
//     客户端）凭句柄找到绑定，直接追加：绑定同时挂在会话链键与 "cid:<上游会话 ID>" 下。
//   - Codex 本地压缩后历史被替换成"若干条 user + 摘要"。摘要正是上游在这个会话里
//     刚写的，认出它就把绑定对齐到压缩后的历史，继续用同一个上游会话（上游那边的
//     完整记忆比摘要详细得多）。
//   - system（桥指令约 13 KB）不每轮重发：上游会话里已有，重复只会挤占窗口。
//     内容变了、或距上次完整发送累计超过 nativeSystemRefresh 字节时重发一次 ——
//     上游自己管理长会话，离得太远的指令约束力会变弱，定期重发让它始终在近处。
//   - 完整 system 与本轮消息合起来放不下一条（Claude Code 的 system 约 60 KB，再加一次
//     读大文件的结果就超了）：定期重发推迟到之后放得下的轮次；system 内容变了就先单独发
//     一轮 system（nativeSystemSeed，上游答 OK），本轮照常只带精简 system。工具结果因此
//     可以接近单条上限，而不必给 system 预留位置。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/oai-prism/oaiprism/internal/account"
	"github.com/oai-prism/oaiprism/internal/creds"
	"github.com/oai-prism/oaiprism/internal/prism"
)

const (
	// nativeBindingTTL 是绑定的闲置寿命。上游会话本身不随沙箱回收而丢失（见 liveprobe），
	// 绑定也落盘（native_store.go）：隔几天 resume 的 Codex 会话照样接回原来的上游会话。
	nativeBindingTTL = 7 * 24 * time.Hour
	// nativeSystemRefresh：距上次完整发送 system 累计发出这么多字节（增量正文）就重发一次。
	nativeSystemRefresh = 48 << 10
	// nativeBriefSystem 是不重发完整 system 的轮次里代替它的一句话。
	nativeBriefSystem = "(The instructions given earlier in this conversation remain in force.)"
	// nativeSinceHeader 引出"上一轮回复之后客户端新增的其余条目"（并行工具结果等）。
	nativeSinceHeader = "[Messages since your last reply]\n"
	// nativeSeedMaxBytes 是新建会话时最多补种的历史字节。补种逐段串行（每段一轮约 10~20 秒），
	// 只补最近这么多，免得这一轮等太久；绑定落盘后只有换号、改写历史才需要补种。
	nativeSeedMaxBytes = 768 << 10
	nativeSeedSystem   = "Earlier parts of this conversation are being restored. Read them as context only: do not act on them, and reply with exactly OK."
	nativeSeedHeader   = "[Earlier conversation, part %d of %d - context only, reply OK]\n"
	// nativeSystemSeed 是单独发送 system 那一轮的 user 消息（system 与本轮消息放不下一条时）。
	nativeSystemSeed = "[The instructions above apply to the rest of this conversation. The next message continues it. Reply with exactly OK.]"

	// nativeBranchMax：上游会话末尾最多有这么多条客户端历史里没有的消息时，仍接着用这个会话
	// （见 nativeTurn.delta 的第 4 条）；再多就是历史被大段改写，新建会话。
	nativeBranchMax  = 32
	nativeBranchNote = "[Note: the last %d message(s) you received in this conversation are not part of the client's conversation " +
		"(a side request, or the user went back). Disregard them and your replies to them, and continue from here.]\n\n"
)

// nativeConversation 是一轮请求拆成的三部分：合并后的 system、往轮对话、本轮消息。
type nativeConversation struct {
	system  string
	history []historyEntry
	current prism.InputItem
	// currentText 是本轮消息到了下一轮历史里的样子（为空时取 current 的文本）：
	// 桥给本轮消息临时附加的提醒不进历史，比对时要去掉。
	currentText string
	// extra 是本轮附加的 system 指令（如 Codex 压缩指令）：每轮照发，不计入 system 指纹。
	extra string
}

// entries 返回往轮对话加本轮消息（本轮记作 User）。
func (c *nativeConversation) entries() []historyEntry {
	es := make([]historyEntry, 0, len(c.history)+1)
	es = append(es, c.history...)
	cur := c.currentText
	if cur == "" {
		cur = itemText(c.current)
	}
	return append(es, historyEntry{speaker: "User", text: cur})
}

// logicalItems 是不裁剪的完整上下文（用量按它计：上游模型每轮读的就是整段会话）。
func (c *nativeConversation) logicalItems() []prism.InputItem {
	return []prism.InputItem{prism.NewSystemItem(c.system + renderHistory(c.history, 0)), c.current}
}

// nativeTurn 是 handler 交给 runner 的原生续接请求。
type nativeTurn struct {
	key string
	// strong 表示会话键来自客户端显式标识（不会撞键）：只发本轮消息、压缩后的替换历史
	// 这类前缀对不上的情形，只在强键下才敢接续。
	strong     bool
	compaction bool
	conv       *nativeConversation
	// weak 表示绑定建在弱键上（由 planNative 从绑定取）：凭句柄找回时也要照弱键比对。
	weak bool
	// branch 是上游会话末尾、客户端历史里没有的消息条数（delta 的第 4 条，见 nativeBranchNote）。
	branch int
	// ghost 表示这一轮是网关自己追加的纠正（见 bridgeLocalRetry）：客户端历史里不会有它，
	// 提交时已送达的条目不记它，下一轮客户端的新条目照常接在后面。
	ghost bool

	plan *nativePlan // runner 在选定账号与项目后填写
}

// nativePlan 是本轮的执行方案，成功后由 commit 写回绑定。
type nativePlan struct {
	b         *nativeBinding // 已加锁；nil 表示本轮不碰绑定（并发撞车）
	cid       string         // 发给上游的会话 ID；空 = 不带会话 ID 发单条全量（回退）
	continued bool           // true = 续接已有会话发增量；false = 新会话发全量
	delivered []uint64
	sysHash   uint64
	sinceSys  int
	// seeds 是新建会话后、本轮之前要先发的历史补种消息；fallback 是补种失败时改发的全量条目。
	seeds    [][]prism.InputItem
	fallback []prism.InputItem
}

// nativeBinding 是一个客户端会话绑定的上游会话。
type nativeBinding struct {
	mu sync.Mutex // 一轮一锁：同一个上游会话不能并发追加
	// key 是绑定建立时的会话键（落盘主键）；aliases 是之后指向它的其他会话键
	// （会话链键、"cid:<上游会话 ID>"），受 nativeBindings.mu 保护。
	key       string
	aliases   []string
	account   string
	project   string
	cid       string
	delivered []uint64 // 上游会话已含的条目指纹（强键下不含助手条目）
	sysHash   uint64   // 上次完整发送的 system 指纹
	sinceSys  int      // 此后累计发出的增量字节
	summary   string   // 最近一次 Codex 压缩请求里上游写的摘要
	weak      bool     // 绑定建在弱键上：助手条目参与比对（凭句柄找回时也照此比）
	updated   time.Time
}

func (b *nativeBinding) reset() {
	b.account, b.project, b.cid, b.summary = "", "", "", ""
	b.delivered, b.sysHash, b.sinceSys = nil, 0, 0
}

var nativeBindings = struct {
	mu     sync.Mutex
	m      map[string]*nativeBinding
	lastGC time.Time
}{m: map[string]*nativeBinding{}}

// nativeBindingFor 取（或建）会话键对应的绑定。
func nativeBindingFor(key string) *nativeBinding {
	now := time.Now()
	nativeBindings.mu.Lock()
	defer nativeBindings.mu.Unlock()
	if now.Sub(nativeBindings.lastGC) > time.Minute {
		nativeBindings.lastGC = now
		for k, b := range nativeBindings.m {
			if b.mu.TryLock() {
				if now.Sub(b.updated) > nativeBindingTTL {
					delete(nativeBindings.m, k)
				}
				b.mu.Unlock()
			}
		}
	}
	b, ok := nativeBindings.m[key]
	if !ok {
		b = &nativeBinding{key: key, updated: now}
		nativeBindings.m[key] = b
	}
	return b
}

func entryFingerprint(e historyEntry) uint64 {
	if e.fp != 0 {
		return e.fp
	}
	h := fnv.New64a()
	h.Write([]byte(e.speaker))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(e.text)))
	return h.Sum64()
}

func textFingerprint(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// weakMatch 报告本轮是否连助手条目一起比对：弱键请求，或弱键建的绑定。
func (nt *nativeTurn) weakMatch() bool { return nt.weak || !nt.strong }

// matchable 报告条目是否参与前缀比对（强键下助手条目不参与，见文件头）。
func (nt *nativeTurn) matchable(e historyEntry) bool {
	return nt.weakMatch() || e.speaker != "Assistant"
}

// fingerprints 是本轮全部可比对条目的指纹（本轮成功后即为上游会话已含的内容）。
func (nt *nativeTurn) fingerprints(es []historyEntry) []uint64 {
	out := make([]uint64, 0, len(es))
	for _, e := range es {
		if nt.matchable(e) {
			out = append(out, entryFingerprint(e))
		}
	}
	return out
}

// fresh 去掉增量里由上游生成的条目（强键下的助手条目）。
func (nt *nativeTurn) fresh(es []historyEntry) []historyEntry {
	if nt.weakMatch() {
		return es
	}
	out := make([]historyEntry, 0, len(es))
	for _, e := range es {
		if e.speaker != "Assistant" {
			out = append(out, e)
		}
	}
	return out
}

// delta 算出上游会话还没见过的条目（最后一条必是本轮消息）。ok=false 表示对不上，须新建会话。
func (nt *nativeTurn) delta(b *nativeBinding, es []historyEntry) ([]historyEntry, bool) {
	// 1) 已送达的内容是本轮历史的前缀。
	i, j := 0, 0
	for ; i < len(es) && j < len(b.delivered); i++ {
		if !nt.matchable(es[i]) {
			continue
		}
		if entryFingerprint(es[i]) != b.delivered[j] {
			break
		}
		j++
	}
	if j == len(b.delivered) && i < len(es) {
		return nt.fresh(es[i:]), true
	}
	if !nt.strong {
		return nil, false
	}
	// 2) 只发本轮消息的客户端（显式会话键、不带历史）：直接追加。
	if len(nt.conv.history) == 0 {
		return es, true
	}
	// 3) Codex 压缩后的替换历史：从上游写的摘要处接上。
	if len(b.summary) >= 64 {
		needle := b.summary[:min(len(b.summary), 256)]
		for k := len(es) - 1; k >= 0; k-- {
			if es[k].speaker == "User" && strings.Contains(es[k].text, needle) {
				if k == len(es)-1 {
					return es[k:], true
				}
				return nt.fresh(es[k+1:]), true
			}
		}
	}
	// 4) 上游会话末尾多出几条客户端历史里没有的消息，其余都对得上、客户端只多了本轮这一条：
	//    同一会话里的旁路请求追加进了这个上游会话（Claude Code 的输入建议、记忆整理这类 fork
	//    请求与主对话共用前缀，看上去就是主对话的下一轮），或者用户回退了最近几步。接着用这个
	//    会话，并告诉上游忽略那几条 —— 新建会话要把整段历史重新补种进去，又慢又丢细节。
	if j > 0 && i == len(es)-1 && len(b.delivered)-j <= nativeBranchMax {
		nt.branch = len(b.delivered) - j
		return es[i:], true
	}
	return nil, false
}

// deltaItems 把增量组装成上游要的 [system, user]，并返回提交时写回的 system 状态。
// limit 是单条提示词字节上限（0 不限），notice 非空时随完整 system 前置。
// seed 非空时是要在本轮之前单独发的一轮 system（完整 system 与本轮消息放不下一条）。
func (nt *nativeTurn) deltaItems(b *nativeBinding, rest []historyEntry, limit int, notice string) (items []prism.InputItem, sysHash uint64, since int, seed []prism.InputItem) {
	conv := nt.conv
	cur := conv.current
	cur.Content = append([]prism.InputContent(nil), cur.Content...)
	curText := itemText(cur)

	prefix := ""
	if nt.branch > 0 {
		prefix = fmt.Sprintf(nativeBranchNote, nt.branch)
	}

	sysHash = textFingerprint(conv.system)
	changed := sysHash != b.sysHash
	full := changed || b.sinceSys+len(curText) > nativeSystemRefresh
	fullSystem := conv.system
	if notice != "" {
		fullSystem = notice + "\n\n" + fullSystem
	}
	if full && limit > 0 && len(fullSystem)+len(conv.extra)+len(curText)+len(prefix)+promptOverheadReserve > limit {
		// 放不下一条：只是定期重发就推迟；内容变了就单独发一轮。
		if changed {
			seed = []prism.InputItem{prism.NewSystemItem(fullSystem), prism.NewUserItem(nativeSystemSeed)}
		}
		full = false
	}
	system := nativeBriefSystem
	if full {
		system = fullSystem
	}
	if conv.extra != "" {
		system += "\n\n" + conv.extra
	}

	// 本轮消息之前的新条目（并行工具结果、客户端插入的消息）放在本轮消息前面，
	// 超出单条上限时按折叠历史的规则裁剪（compress.go）。
	if prior := rest[:len(rest)-1]; len(prior) > 0 {
		budget := historyBudget(limit, len(system)+len(curText)+len(prefix)+len(nativeSinceHeader))
		if h := strings.TrimPrefix(renderHistory(prior, budget), historyHeader); h != "" {
			prefix += nativeSinceHeader + h + "\n\n"
		}
	}
	added := len(prefix)
	if prefix != "" {
		if len(cur.Content) > 0 && (cur.Content[0].Type == "" || cur.Content[0].Type == prism.BlockInputText) {
			cur.Content[0].Text = prefix + cur.Content[0].Text
		} else {
			cur.Content = append([]prism.InputContent{{Type: prism.BlockInputText, Text: prefix}}, cur.Content...)
		}
	}

	since = b.sinceSys + len(curText) + added
	if full || seed != nil {
		since = len(curText) + added
	}
	return []prism.InputItem{prism.NewSystemItem(system), cur}, sysHash, since, seed
}

// itemText 拼出条目里全部文本块。
func itemText(it prism.InputItem) string {
	var sb strings.Builder
	for _, c := range it.Content {
		switch c.Type {
		case "input_image", "input_file":
		default:
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// nativeBindingPeek 取会话键已有的绑定（没有时为 nil，不新建）。
func nativeBindingPeek(key string) *nativeBinding {
	nativeBindings.mu.Lock()
	defer nativeBindings.mu.Unlock()
	return nativeBindings.m[key]
}

// streamConversationID 是流式响应头里能给出的会话 ID：头必须早于首帧写出，那时只知道
// 续接中的上游会话（新建的会话 ID 随结束帧 / 响应体给出）。
func streamConversationID(req *RunRequest) string {
	if req.Native == nil {
		return ""
	}
	b := nativeBindingPeek(req.Native.key)
	if b == nil || !b.mu.TryLock() {
		return ""
	}
	defer b.mu.Unlock()
	return b.cid
}

// nativeAlias 让 alias 指向 key 的绑定（同一个对象）：凭回复句柄 / 会话 ID 续接的请求
// 落在别的会话键上，也能找回原来的上游会话。返回新挂上别名的绑定（key 还没有绑定、
// 或别名已在时为 nil）。
func nativeAlias(alias, key string) *nativeBinding {
	if alias == "" || alias == key {
		return nil
	}
	nativeBindings.mu.Lock()
	defer nativeBindings.mu.Unlock()
	b, ok := nativeBindings.m[key]
	if !ok {
		return nil
	}
	return b.addAliasLocked(alias)
}

// addAliasLocked 把 alias 挂到绑定上（调用方持 nativeBindings.mu）。已挂过时返回 nil。
func (b *nativeBinding) addAliasLocked(alias string) *nativeBinding {
	if cur, ok := nativeBindings.m[alias]; ok && cur == b {
		return nil
	}
	nativeBindings.m[alias] = b
	if alias != b.key {
		b.aliases = append(b.aliases, alias)
		// 每轮都可能新增会话链键：只留最近的若干个，淘汰的连同表项一起删。
		// 上游会话 ID 的别名（客户端会带回来）不淘汰，一个会话也就一两个。
		for i := 0; len(b.aliases) > chainMaxAliases && i < len(b.aliases); {
			old := b.aliases[i]
			if strings.HasPrefix(old, "cid:") || strings.Contains(old, "|cid:") {
				i++
				continue
			}
			if nativeBindings.m[old] == b {
				delete(nativeBindings.m, old)
			}
			b.aliases = append(b.aliases[:i:i], b.aliases[i+1:]...)
		}
	}
	return b
}

// nativeConvKey 是上游会话 ID 对应的会话键（与 conversationKey 的 "cid:" 键同形，按租户隔离）：
// 客户端把我们回传的会话 ID 带回来时，会话键就是它。
func nativeConvKey(key, cid string) string {
	if t := tenantOfKey(key); t != "" {
		return t + "|cid:" + cid
	}
	return "cid:" + cid
}

// attachNative 给请求挂上原生续接，并沿用绑定里的项目：上游会话挂在项目下，
// 换了项目就只能新建会话。项目与会话都是账号私有的 —— 实际租到别的账号时，
// runner 按 BoundAccountID 作废这个项目（dropContinuation），planNative 随之新建会话。
func (h *Handler) attachNative(runReq *RunRequest, nt *nativeTurn) {
	runReq.Native = nt
	if runReq.ProjectID != "" {
		return
	}
	b := nativeBindingPeek(nt.key)
	if b == nil || !b.mu.TryLock() {
		return
	}
	project, account := b.project, b.account
	b.mu.Unlock()
	if project == "" {
		return
	}
	runReq.ProjectID = project
	runReq.MarkProjectFromChain()
	if runReq.BoundAccountID == "" {
		runReq.BoundAccountID = account
	}
}

// ---------------------------- runner 侧 ----------------------------

// planNative 在选定账号与项目后决定本轮怎么发：续接已有上游会话发增量，或新建会话发全量。
// full 是全量折叠后的条目（回退用）。返回本轮实际要发的条目，方案记在 req.Native.plan。
func (r *Runner) planNative(ctx context.Context, p prism.Principal, acctID, projectID string, req *RunRequest, full []prism.InputItem) []prism.InputItem {
	nt := req.Native
	nt.plan = &nativePlan{}
	if projectID == "" {
		return full // 会话挂在项目下：没有项目只能单条全量
	}
	b := nativeBindingFor(nt.key)
	if !b.mu.TryLock() {
		// 同一会话上一轮还没结束（并发请求）：这一轮发单条全量、不带会话 ID，不碰绑定。
		r.log.Info("原生续接：会话正忙，本轮按全量发送", "key", nt.key)
		return full
	}
	plan := nt.plan
	plan.b = b
	es := nt.conv.entries()
	// 弱键建的绑定一直按弱键比（凭句柄找回时 nt.strong 为真，但已送达的指纹里含助手条目）。
	nt.weak = b.weak

	if b.cid != "" && b.account == acctID && b.project == projectID {
		plan.delivered = nt.fingerprints(es)
		if rest, ok := nt.delta(b, es); ok {
			notice := r.noticeFor(req)
			items, sysHash, since, seed := nt.deltaItems(b, rest, r.cfg.Facade.PromptByteLimit(), notice)
			plan.cid, plan.continued, plan.sysHash, plan.sinceSys = b.cid, true, sysHash, since
			if seed != nil {
				plan.seeds, plan.fallback = [][]prism.InputItem{seed}, items
			}
			r.log.Info("原生续接：发送增量", "key", nt.key, "cid", b.cid,
				"newEntries", len(rest), "bytes", promptBytes(items), "fullSystem", textOfSystem(items) != nativeBriefSystem,
				"systemSeparately", seed != nil, "skippedBranch", nt.branch)
			return items
		}
		r.log.Info("原生续接：客户端历史与上游会话对不上，新建会话", "key", nt.key, "oldCid", b.cid)
	}
	plan.delivered = nt.fingerprints(es)

	cid, err := r.createConversation(ctx, p, projectID)
	if err != nil {
		r.log.Warn("原生续接：创建上游会话失败，本轮发单条全量（下一轮再登记）", "project", projectID, "err", err)
		r.app.ConversationOps.Inc("create", "error")
		return full
	}
	r.app.ConversationOps.Inc("create", "ok")
	plan.cid = cid
	plan.sysHash = textFingerprint(nt.conv.system)
	plan.sinceSys = len(itemText(nt.conv.current))

	// 历史一条放不下（全量折叠时被裁过）：先分段把历史补种进新会话，再发本轮 ——
	// 换号、网关重启、旧会话作废之后，上游照样拿到客户端手里的完整历史。
	// 连 system 与本轮消息都放不下一条时，system 再单独占一轮（见 deltaItems）。
	limit := r.cfg.Facade.PromptByteLimit()
	if historyTrimmed(full) || (limit > 0 && promptBytes(full) > limit) {
		seeds := nt.seedTurns(limit)
		notice := r.noticeFor(req)
		items := nt.currentWithSystem(notice)
		separate := limit > 0 && promptBytes(items)+promptOverheadReserve > limit
		if separate {
			seeds = append(seeds, nt.systemSeed(notice))
			items = nt.currentWithBrief()
		}
		if len(seeds) > 0 {
			plan.seeds, plan.fallback = seeds, full
			r.log.Info("原生续接：新建上游会话，历史分段补种后发送本轮", "key", nt.key, "cid", cid,
				"parts", len(seeds), "bytes", promptBytes(items), "systemSeparately", separate)
			return items
		}
	}
	r.log.Info("原生续接：新建上游会话，首轮发送全量", "key", nt.key, "cid", cid, "bytes", promptBytes(full))
	return full
}

// currentWithSystem 是 [完整 system, 本轮消息]（补种之后的那一轮用）。
func (nt *nativeTurn) currentWithSystem(notice string) []prism.InputItem {
	system := nt.conv.system
	if notice != "" {
		system = notice + "\n\n" + system
	}
	if nt.conv.extra != "" {
		system += "\n\n" + nt.conv.extra
	}
	return []prism.InputItem{prism.NewSystemItem(system), nt.conv.current}
}

// systemSeed 是单独发送完整 system 的那一轮（system 与本轮消息放不下一条时）。
func (nt *nativeTurn) systemSeed(notice string) []prism.InputItem {
	system := nt.conv.system
	if notice != "" {
		system = notice + "\n\n" + system
	}
	return []prism.InputItem{prism.NewSystemItem(system), prism.NewUserItem(nativeSystemSeed)}
}

// currentWithBrief 是 [精简 system, 本轮消息]（完整 system 已单独发过一轮）。
func (nt *nativeTurn) currentWithBrief() []prism.InputItem {
	system := nativeBriefSystem
	if nt.conv.extra != "" {
		system += "\n\n" + nt.conv.extra
	}
	return []prism.InputItem{prism.NewSystemItem(system), nt.conv.current}
}

// nativeMinBytes 是原生续接下本轮至少要发的字节：精简 system + 本轮消息
// （完整 system 放不下时单独发一轮，往轮的新条目可以裁掉）。
func nativeMinBytes(conv *nativeConversation) int {
	return len(nativeBriefSystem) + len(conv.extra) + 2 + promptBytes([]prism.InputItem{conv.current})
}

// seedTurns 把往轮对话切成若干段补种消息，每段不超过单条上限；只补最近的
// nativeSeedMaxBytes 字节（再早的上游窗口也装不下，会被它自己压缩掉）。limit <= 0 时不补种。
func (nt *nativeTurn) seedTurns(limit int) [][]prism.InputItem {
	budget := limit - len(nativeSeedSystem) - 160 - promptOverheadReserve
	if limit <= 0 || budget < 4096 {
		return nil
	}
	var lines []string
	for _, e := range nt.conv.history {
		t := strings.TrimSpace(e.text)
		if t == "" {
			continue
		}
		l := e.speaker + ": " + t
		if len(l) > budget {
			l = cutUTF8(l, budget-32) + " …[truncated]"
		}
		lines = append(lines, l)
	}
	start, total := len(lines), 0
	for start > 0 && total+len(lines[start-1])+1 <= nativeSeedMaxBytes {
		start--
		total += len(lines[start]) + 1
	}
	omitted := start
	lines = lines[start:]

	var chunks [][]string
	var cur []string
	size := 0
	for _, l := range lines {
		if size+len(l)+1 > budget && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		cur = append(cur, l)
		size += len(l) + 1
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	out := make([][]prism.InputItem, 0, len(chunks))
	for k, c := range chunks {
		head := fmt.Sprintf(nativeSeedHeader, k+1, len(chunks))
		if k == 0 && omitted > 0 {
			head += fmt.Sprintf("[%d older messages omitted]\n", omitted)
		}
		out = append(out, []prism.InputItem{prism.NewSystemItem(nativeSeedSystem), prism.NewUserItem(head + strings.Join(c, "\n"))})
	}
	return out
}

// cutUTF8 把 s 截到不超过 n 字节，且不切开多字节字符。
func cutUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// seedConversation 依次发送补种消息（每段一轮，等上游答完再发下一段）。
func (r *Runner) seedConversation(ctx context.Context, acct *account.Account, req *RunRequest, projectID string) error {
	plan := req.Native.plan
	for i, seed := range plan.seeds {
		sreq := &RunRequest{
			Input: seed, Model: req.Model, Effort: req.Effort, UserID: req.UserID,
			ConversationID: plan.cid, ProjectID: projectID, StickyKey: req.StickyKey,
			API: req.API, Bridge: true, ExtraHeaders: req.ExtraHeaders, Deadline: req.Deadline,
		}
		if _, err := r.runOnce(ctx, acct, sreq, nil); err != nil {
			return fmt.Errorf("补种第 %d/%d 段: %w", i+1, len(plan.seeds), err)
		}
	}
	return nil
}

// textOfSystem 返回规整条目里 system 的文本（没有 system 时为空）。
func textOfSystem(items []prism.InputItem) string {
	if len(items) == 0 || !isSystemRole(items[0].Role) {
		return ""
	}
	return itemText(items[0])
}

// createConversation 经 Server Action 在项目下登记一个上游会话。
func (r *Runner) createConversation(ctx context.Context, p prism.Principal, projectID string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		raw, err := r.client.ServerAction(ctx, p, prism.ActionCreateProjectConversation, projectID)
		if err == nil {
			var cid string
			if json.Unmarshal(raw, &cid) == nil && strings.HasPrefix(cid, "cdx") {
				return cid, nil
			}
			return "", errors.New("createProjectConversation 返回值不是会话 ID: " + truncateRunes(string(raw), 120))
		}
		lastErr = err
		if !isSentinelThrottle(err) {
			break
		}
		if serr := sleepCtx(ctx, time.Duration(attempt)*time.Second); serr != nil {
			return "", serr
		}
	}
	return "", lastErr
}

// commit 把成功的一轮写回绑定（并落盘）后解锁。r 为 nil 时不落盘。
func (nt *nativeTurn) commit(res *RunResult, r *Runner) {
	plan := nt.plan
	if plan == nil || plan.b == nil {
		return
	}
	b := plan.b
	plan.b = nil
	defer b.mu.Unlock()
	defer r.persistNative(b)
	if plan.cid == "" || res == nil {
		b.reset()
		return
	}
	newConv := b.cid != plan.cid
	b.account, b.project, b.cid, b.weak = res.AccountID, res.ProjectID, plan.cid, nt.weakMatch()
	b.delivered = plan.delivered
	switch {
	case nt.ghost && len(b.delivered) > 0:
		b.delivered = b.delivered[:len(b.delivered)-1] // 纠正消息（本轮消息）客户端没有
	case nt.weakMatch():
		b.delivered = append(b.delivered, entryFingerprint(historyEntry{speaker: "Assistant", text: res.Text}))
	}
	if newConv {
		// 回传给客户端的会话 ID 也能找回这个绑定（见 nativeConvKey）。
		nativeBindings.mu.Lock()
		b.addAliasLocked(nativeConvKey(nt.key, plan.cid))
		nativeBindings.mu.Unlock()
	}
	b.sysHash, b.sinceSys = plan.sysHash, plan.sinceSys
	if nt.compaction {
		b.summary = strings.TrimSpace(res.Text)
	}
	b.updated = time.Now()
}

// release 在失败时解锁绑定；drop 为 true 时作废它（上游会话已不可用，连同落盘的记录）。
func (nt *nativeTurn) release(drop bool, r *Runner) {
	plan := nt.plan
	if plan == nil || plan.b == nil {
		return
	}
	b := plan.b
	plan.b = nil
	if drop {
		b.reset()
		r.persistNative(b)
	}
	b.mu.Unlock()
}

// isConversationGone 判断续接失败是否意味着上游会话本身已不可用（须换新会话重来）。
//
// 比 isContinuationError 严：上游的瞬时失败文案多含 "conversation"（如
// "Error while processing conversation (403)"），据此作废绑定会白白丢掉整段记忆。
func isConversationGone(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrPollTimeout) || errors.Is(err, ErrContextTooLarge) {
		return false
	}
	var ae *creds.APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusGone) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, k := range []string{"conversation_too_large", "conversation too large", "not found", "no longer", "expired"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}
