package facade

// 会话续接链：同一个前端会话（X-Oaiprism-Session）的多轮请求
// 缓存对话历史，供单条消息客户端注入上下文。
//
// 实测结论（2026-10-02 完整实验矩阵 B/B2/K/V/W/Y/N，勿再按直觉改回去）：
//   - 上游后端**只提取"最后一条 system + 最后一条 user"，中间的 input
//     条目全部丢弃**（见 upstream_input.go）。把历史
//     插成独立 input 条目（无论带不带 conversationId/previousResponseId、
//     无论 system 是否加扰动标记、无论加不加零宽空格）全部失忆；
//     历史折叠成 [Previous Conversation History] 文本拼进 system
//     则全部成功 —— 与 translateChatMessages 对客户端多 messages
//     的处理完全同款。
//   - 原生续接协议（同 cid + prev(resp_*) + codex_listen_snapshot +
//     增量 input，服务端按 conversation 拼历史）已在真实浏览器内
//     验证可行（tools/webui_probe4_result.json 答对暗号），但 Go
//     传输层被上游按"无状态会话"处理 —— 需浏览器代发通道
//     （tools/browser_forward.js，实验件）稳定化后才能启用。
//     在那之前，本文件的 History 缓存 + 折叠注入是生产路径。
//
// 历史注入判据：客户端本轮只发了 user 消息（没带 assistant 历史）
// 才注入 —— 已带完整历史的客户端由 translateChatMessages 统一折叠。
// 要开新话题请换会话（新 session ID 天然隔离）。
//
// 局限：内存态，网关重启后链条清空 —— 清空后首轮退化为新对话，
// 第二轮起自动重建链条，不影响正确性（只会丢一次上下文）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 会话历史的容量上限：条数与字符双预算，超限从最旧的开始丢。
// 24k 字符约等于 6k~8k token，足以覆盖日常多轮；再大就该让客户端
// 显式开新会话，而不是无限堆历史。
const (
	chainHistoryMaxItems = 20
	chainHistoryMaxChars = 24000
)

type sessionChainEntry struct {
	ProjectID        string
	ConversationID   string
	ResponseID       string // 上一轮回复句柄 —— 关联用途（上游不代管历史）
	AccountID        string
	Model            string
	ListenSnapshot   json.RawMessage
	History          []ChatMessage     // 本会话累积的对话历史（user/assistant 交替）
	SynthesizedFiles map[string]string // cleanPath -> hash(status + diff)，防止重复合成导致死循环
	UpdatedAt        time.Time

	// key 是条目的主键（会话键）；tenant 是所属调用方（API Key 指纹），别名键都按它隔离。
	key    string
	tenant string
	// aliases 是指向本条目的别名键（#prev: / #cid:），有上限：
	// 每轮都会新增 prev 别名，不设上限的话长会话的别名会无限堆积。
	aliases []string
}

var sessionChain = struct {
	mu      sync.RWMutex
	entries map[string]*sessionChainEntry
	lastGC  time.Time
}{entries: map[string]*sessionChainEntry{}}

const (
	sessionChainTTL = 30 * time.Minute
	// chainMaxAliases 是单条会话保留的别名上限（够覆盖最近若干轮的回复句柄）。
	chainMaxAliases = 16
	// chainGCInterval 是过期清理的最小间隔：每次写都全表扫描会在写锁下 O(n)。
	chainGCInterval = time.Minute
)

// tenantOfKey 从会话键里取出租户前缀（"k:<指纹>"）；无租户时返回空串。
// 会话键形如 "k:ab12cd34ef56ab78|cm:<session>"，见 scopeKey。
func tenantOfKey(key string) string {
	if strings.HasPrefix(key, "k:") {
		if i := strings.IndexByte(key, '|'); i > 0 {
			return key[:i]
		}
	}
	return ""
}

// chainAlias 构造租户隔离的别名键。'#' 开头，不可能与会话键（h:/cm:/k:...）相撞 ——
// 早期别名与会话键共用 "cid:" 命名空间，客户端带 conversation_id 时会话键本身
// 就是 "cid:<id>"，重置时会把主条目当别名删掉，整段历史随之丢失。
func chainAlias(tenant, kind, id string) string {
	return tenant + "#" + kind + ":" + id
}

// isStrongSessionKey 判断会话键是否来自客户端显式提供的会话标识。
//
// 弱键（首条消息指纹 f:、OpenAI user 字段 u:）只能标识"像同一个会话"：
// 两段以同一句话开头的对话、同一用户的多段对话都会撞到同一个弱键。
// 弱键只用于账号/项目亲和，绝不能据此继承上一轮句柄或注入历史，
// 否则就是跨会话（乃至跨用户）串上下文。
func isStrongSessionKey(key string) bool {
	k := key
	if t := tenantOfKey(k); t != "" {
		k = k[len(t)+1:]
	}
	// r: 是"每个回复链一条"的内部键（弱键客户端凭 previous_response_id 续接时使用），天然唯一。
	for _, p := range []string{"h:", "cm:", "pck:", "cid:", "sid:", "md:", "r:"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// chainAddAlias 把别名键指向条目（调用方持写锁）。超过上限时淘汰最旧的别名。
func chainAddAlias(e *sessionChainEntry, alias string) {
	if cur, ok := sessionChain.entries[alias]; ok && cur == e {
		return
	}
	sessionChain.entries[alias] = e
	e.aliases = append(e.aliases, alias)
	for len(e.aliases) > chainMaxAliases {
		old := e.aliases[0]
		e.aliases = e.aliases[1:]
		if cur, ok := sessionChain.entries[old]; ok && cur == e {
			delete(sessionChain.entries, old)
		}
	}
}

// chainDropAliases 删除条目的全部别名（调用方持写锁）。
func chainDropAliases(e *sessionChainEntry) {
	for _, a := range e.aliases {
		if cur, ok := sessionChain.entries[a]; ok && cur == e {
			delete(sessionChain.entries, a)
		}
	}
	e.aliases = nil
}

// chainGC 清理过期条目（调用方持写锁），按间隔节流。
func chainGC(now time.Time) {
	if now.Sub(sessionChain.lastGC) < chainGCInterval {
		return
	}
	sessionChain.lastGC = now
	for k, e := range sessionChain.entries {
		if now.Sub(e.UpdatedAt) > sessionChainTTL {
			delete(sessionChain.entries, k)
		}
	}
}

// chainEntryFor 取（或建）会话键对应的条目（调用方持写锁）。
func chainEntryFor(key string) *sessionChainEntry {
	e, ok := sessionChain.entries[key]
	if !ok {
		e = &sessionChainEntry{key: key, tenant: tenantOfKey(key)}
		sessionChain.entries[key] = e
	}
	return e
}

// sessionChainHistory 取本会话累积的对话历史（过期/不存在返回 nil）。
// 返回副本，调用方可安全修改。
func sessionChainHistory(key string) []ChatMessage {
	// 弱键（首条消息指纹 / user 字段）撞键概率不可忽略：两段以同一句话开头的
	// 对话会命中同一条历史。宁可不注入，也不能把别人的对话塞进来。
	if !isStrongSessionKey(key) {
		return nil
	}
	sessionChain.mu.RLock()
	defer sessionChain.mu.RUnlock()
	e, ok := sessionChain.entries[key]
	if !ok || time.Since(e.UpdatedAt) > sessionChainTTL || len(e.History) == 0 {
		return nil
	}
	out := make([]ChatMessage, len(e.History))
	copy(out, e.History)
	return out
}

// sessionChainAppend 把本轮的 user 消息与 assistant 回复追加进会话历史。
// user 文本为空（如纯工具轮）时跳过，避免历史里出现空轮次。
func sessionChainAppend(key, userText, assistantText string) {
	userText = strings.TrimSpace(userText)
	assistantText = strings.TrimSpace(assistantText)
	if strings.TrimSpace(key) == "" || (userText == "" && assistantText == "") || !isStrongSessionKey(key) {
		return
	}
	sessionChain.mu.Lock()
	defer sessionChain.mu.Unlock()
	e, ok := sessionChain.entries[key]
	if !ok || time.Since(e.UpdatedAt) > sessionChainTTL {
		return // 链已失效，历史从下一轮重建
	}
	if userText != "" {
		e.History = append(e.History, ChatMessage{Role: "user", Content: stringContent(userText)})
	}
	if assistantText != "" {
		e.History = append(e.History, ChatMessage{Role: "assistant", Content: stringContent(assistantText)})
	}
	// 双预算裁剪：先按条数，再按总字符。历史尾部是最新内容，必须保住。
	if len(e.History) > chainHistoryMaxItems {
		e.History = append([]ChatMessage{}, e.History[len(e.History)-chainHistoryMaxItems:]...)
	}
	total := 0
	for _, m := range e.History {
		total += len(m.Content.Text())
	}
	if total > chainHistoryMaxChars {
		for total > chainHistoryMaxChars && len(e.History) > 2 {
			total -= len(e.History[0].Content.Text())
			e.History = e.History[1:]
		}
		e.History = append([]ChatMessage{}, e.History...)
	}
}

// historyCarriesContext 判断客户端本轮请求是否已自带历史。
// 判据：messages 里出现过 assistant 角色（正常客户端的多轮请求
// 必然回传往轮 assistant 回复；只发 user 的说明是"单条消息"客户端）。
func historyCarriesContext(msgs []ChatMessage) bool {
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "assistant") {
			return true
		}
	}
	return false
}

// historyCarriesContextA 是 Anthropic Messages 版本（结构同形，类型不同）。
func historyCarriesContextA(msgs []AnthropicMessage) bool {
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "assistant") {
			return true
		}
	}
	return false
}

// injectChainHistory 把链历史折叠成文本块，追加到首条 system 尾部
// （无 system 则前插一条）。
//
// 为什么折叠成文本而不是插入独立 input 条目（2026-10-02 血泪实测）：
// 上游后端只提取"最后一条 system + 最后一条 user"，中间的 input 条目
// 全部丢弃 —— translate.go 头注释早就写了这个缺陷。往轮把历史插成
// 独立条目（V/W/Y/N 系列实验）全部失忆；折叠进 system（B/B2/K
// 系列实验，同款 [Previous Conversation History] 格式）全部成功。
func injectChainHistory(input []prism.InputItem, hist []ChatMessage) []prism.InputItem {
	if len(hist) == 0 {
		return input
	}
	var sb strings.Builder
	for _, m := range hist {
		txt := strings.TrimSpace(m.Content.Text())
		if txt == "" {
			continue
		}
		var speaker string
		switch {
		case strings.EqualFold(m.Role, "user"):
			speaker = "User"
		case strings.EqualFold(m.Role, "assistant"):
			speaker = "Assistant"
		default:
			continue // tool/function 等对上游无意义
		}
		sb.WriteString(speaker)
		sb.WriteString(": ")
		sb.WriteString(txt)
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return input
	}
	historyText := "\n\n[Previous Conversation History]\n" + sb.String()

	out := make([]prism.InputItem, 0, len(input)+1)
	if len(input) > 0 && input[0].Role == "system" {
		sys := input[0]
		// 复制 Content：切片与调用方共享底层数组，原地拼接会污染调用方的 input
		//（例如自愈重试时再次折叠，历史就会被拼两遍）。
		sys.Content = append([]prism.InputContent(nil), sys.Content...)
		for j := range sys.Content {
			sys.Content[j].Text += historyText
			break // 只拼第一个文本块
		}
		out = append(out, sys)
		out = append(out, input[1:]...)
	} else {
		out = append(out, prism.NewSystemItem(strings.TrimPrefix(historyText, "\n\n")))
		out = append(out, input...)
	}
	return out
}

// lastUserText 取 Input 里最后一条 user 消息的文本（本轮提问），
// 会话历史回写用。取不到返回空。
func lastUserText(input []prism.InputItem) string {
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].Role != "user" {
			continue
		}
		var sb strings.Builder
		for _, c := range input[i].Content {
			sb.WriteString(c.Text)
		}
		return sb.String()
	}
	return ""
}

// sessionChainBind 在请求开始前预先将 stickyKey 与会话 ID、项目 ID 关联，
// 确保同一客户端会话在整个生命周期内严格锁定同一个上游会话与项目句柄。
func sessionChainBind(key, convID, projectID string) {
	if strings.TrimSpace(key) == "" {
		return
	}
	sessionChain.mu.Lock()
	defer sessionChain.mu.Unlock()
	e := chainEntryFor(key)
	if convID != "" {
		e.ConversationID = convID
		chainAddAlias(e, chainAlias(e.tenant, "cid", convID))
	}
	if projectID != "" {
		e.ProjectID = projectID
	}
	e.UpdatedAt = time.Now()
}

// sessionChainRecord 统一记录本轮的上游会话句柄、快照与项目。
//
// 只记录终态 ResponseID：RequestID（start 受理号）不是续接键，上游查无此
// response 时会**静默忽略**（runner.go RunResult.RequestID 注释）—— 拿它当
// previous_response_id 会造成不报错的失忆，连自愈重试都触发不了。
func sessionChainRecord(key string, res *RunResult, model string) {
	if strings.TrimSpace(key) == "" || res == nil {
		return
	}
	now := time.Now()
	sessionChain.mu.Lock()
	defer sessionChain.mu.Unlock()
	chainGC(now)
	e := chainEntryFor(key)
	if res.ProjectID != "" {
		e.ProjectID = res.ProjectID
	}
	if res.ConversationID != "" {
		e.ConversationID = res.ConversationID
		chainAddAlias(e, chainAlias(e.tenant, "cid", res.ConversationID))
	}
	if res.ResponseID != "" {
		e.ResponseID = res.ResponseID
		chainAddAlias(e, chainAlias(e.tenant, "prev", res.ResponseID))
	}
	if res.AccountID != "" {
		e.AccountID = res.AccountID
	}
	if model != "" {
		e.Model = model
	}
	if len(res.ListenSnapshot) > 0 {
		e.ListenSnapshot = res.ListenSnapshot
	}
	e.UpdatedAt = now
}

// sessionChainRecordLocalID 将本地生成的临时 response id（如序言 response.created 中下发给下游的 ID）
// 也关联到当前会话 entry，确保客户端无论下一轮带回临时 ID 还是上游真实终态 ID 都能续接。
func sessionChainRecordLocalID(key, localRespID string) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(localRespID) == "" {
		return
	}
	sessionChain.mu.Lock()
	defer sessionChain.mu.Unlock()
	if e, ok := sessionChain.entries[key]; ok {
		chainAddAlias(e, chainAlias(e.tenant, "prev", localRespID))
	}
}

// sessionChainResetPrevious 重置会话的上一轮响应句柄（在断链或重试自愈时使用）
func sessionChainResetPrevious(key string) {
	if strings.TrimSpace(key) == "" {
		return
	}
	sessionChain.mu.Lock()
	defer sessionChain.mu.Unlock()
	if e, ok := sessionChain.entries[key]; ok {
		e.ResponseID = ""
		e.ListenSnapshot = nil
	}
}

// sessionChainResetSession 重置会话的远程续接句柄（上游会话、上轮响应、沙箱快照）。
//
// dropProject 为 true 时连项目一起解绑（项目本身失效时）；否则保留项目，
// 下一轮在同一个项目（同一份工作区文件）里开新会话。
// 本地累积的 History 不动 —— 它正是断链后重建上下文的依据。
func sessionChainResetSession(key string, dropProject bool) {
	if strings.TrimSpace(key) == "" {
		return
	}
	sessionChain.mu.Lock()
	defer sessionChain.mu.Unlock()
	if e, ok := sessionChain.entries[key]; ok {
		chainDropAliases(e)
		e.ResponseID = ""
		e.ConversationID = ""
		e.ListenSnapshot = nil
		if dropProject {
			e.ProjectID = ""
		}
		e.UpdatedAt = time.Now()
	}
}

// sessionChainLookup 取本会话已绑定的项目 ID、会话 ID、上轮响应 ID 与沙箱会话快照。
// 支持通过 key、previousResponseId 以及 conversationId 多路命中（别名按 key 的租户隔离）。
func sessionChainLookup(key, prevRespID, convID string) (projectID, foundConvID, prevResp, acctID string, snapshot json.RawMessage) {
	return sessionChainLookupT(tenantOfKey(key), key, prevRespID, convID)
}

// chainHit 是一次会话链命中的快照。
type chainHit struct {
	Key            string // 命中条目的主键（经别名命中时可能与请求的会话键不同）
	ProjectID      string
	ConversationID string
	ResponseID     string
	AccountID      string
	Snapshot       json.RawMessage
}

// sessionChainFind 按 会话键 → previous_response_id 别名 → conversation_id 别名 的顺序查找，
// 别名按租户隔离：别的调用方的回复句柄在这里查不到。
func sessionChainFind(tenant, key, prevRespID, convID string) (chainHit, bool) {
	sessionChain.mu.RLock()
	defer sessionChain.mu.RUnlock()

	fresh := func(k string) *sessionChainEntry {
		if entry, ok := sessionChain.entries[k]; ok && time.Since(entry.UpdatedAt) <= sessionChainTTL && entry.tenant == tenant {
			return entry
		}
		return nil
	}
	var e *sessionChainEntry
	if key != "" {
		e = fresh(key)
	}
	if e == nil && prevRespID != "" {
		e = fresh(chainAlias(tenant, "prev", prevRespID))
	}
	if e == nil && convID != "" {
		e = fresh(chainAlias(tenant, "cid", convID))
	}
	if e == nil {
		return chainHit{}, false
	}
	return chainHit{
		Key: e.key, ProjectID: e.ProjectID, ConversationID: e.ConversationID,
		ResponseID: e.ResponseID, AccountID: e.AccountID, Snapshot: e.ListenSnapshot,
	}, true
}

// sessionChainLookupT 同 sessionChainLookup，但显式指定租户（会话键为空时也不会跨租户命中）。
func sessionChainLookupT(tenant, key, prevRespID, convID string) (projectID, foundConvID, prevResp, acctID string, snapshot json.RawMessage) {
	h, _ := sessionChainFind(tenant, key, prevRespID, convID)
	return h.ProjectID, h.ConversationID, h.ResponseID, h.AccountID, h.Snapshot
}

// sessionChainGet 取本会话已绑定的项目 ID、会话 ID、上轮响应 ID 与沙箱会话快照。
func sessionChainGet(key string) (projectID, convID, prevRespID, acctID string, snapshot json.RawMessage) {
	return sessionChainLookup(key, "", "")
}

// sessionChainPut 记录本轮的上游会话句柄（兼容老调用点）。
func sessionChainPut(key, conversationID, responseID, accountID, model string) {
	sessionChainRecord(key, &RunResult{
		ConversationID: conversationID,
		ResponseID:     responseID,
		AccountID:      accountID,
	}, model)
}

// chainConv 安全取 RunResult 的会话句柄（res 为 nil 时返回空）。
func chainConv(res *RunResult) string {
	if res == nil {
		return ""
	}
	return res.ConversationID
}

// resAccount 安全取 RunResult 的账号 ID。
func resAccount(res *RunResult) string {
	if res == nil {
		return ""
	}
	return res.AccountID
}

// resReqID 安全取 RunResult 的回复句柄（previous_response_id 用）。
//
// 只认 ResponseID（终态 payload.id，resp_* 形态）—— 实测这才是上游认的
// 上下文续接键；RequestID（start 受理号）上游查无此 response 时会静默
// 忽略，表现为不报错的多轮失忆，所以宁可留空也不拿它兜底。
func resReqID(res *RunResult) string {
	if res == nil {
		return ""
	}
	return res.ResponseID
}

// sessionChainFilterNewDeltaFiles 过滤出会话中尚未合成过（或内容发生变化）的 DeltaFiles，
// 并将本次合成的文件内容哈希记录入库，防止跨轮次重复合成导致无限循环。
func sessionChainFilterNewDeltaFiles(key string, files []prism.CodexDeltaFile) []prism.CodexDeltaFile {
	if len(files) == 0 {
		return nil
	}
	sessionChain.mu.Lock()
	defer sessionChain.mu.Unlock()

	var e *sessionChainEntry
	if key != "" {
		if entry, ok := sessionChain.entries[key]; ok {
			e = entry
		}
	}

	// 若未找到现有 entry，但传入了有效 key，则就地建立 entry 方便记录文件合成状态
	if e == nil && key != "" {
		e = &sessionChainEntry{
			SynthesizedFiles: make(map[string]string),
			UpdatedAt:        time.Now(),
			key:              key,
			tenant:           tenantOfKey(key),
		}
		sessionChain.entries[key] = e
	}

	if e == nil {
		var valid []prism.CodexDeltaFile
		for _, f := range files {
			if !isSystemIgnoredFile(f.FilePath) {
				valid = append(valid, f)
			}
		}
		return valid
	}

	if e.SynthesizedFiles == nil {
		e.SynthesizedFiles = make(map[string]string)
	}

	var newFiles []prism.CodexDeltaFile
	for _, f := range files {
		if isSystemIgnoredFile(f.FilePath) {
			continue
		}
		cleanPath := filepath.ToSlash(filepath.Clean(strings.TrimSpace(f.FilePath)))
		diff := f.DiffString()
		sum := sha256.Sum256([]byte(f.Status + ":" + diff))
		hash := hex.EncodeToString(sum[:])

		if oldHash, exists := e.SynthesizedFiles[cleanPath]; exists && oldHash == hash {
			continue // 该文件在该状态和内容下已合成下发过，跳过
		}
		newFiles = append(newFiles, f)
		e.SynthesizedFiles[cleanPath] = hash
	}
	return newFiles
}
