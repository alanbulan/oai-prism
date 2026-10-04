package facade

// 会话链：把客户端的会话标识（会话键、previous_response_id、conversation_id）
// 关联到同一条记录，记下项目与账号亲和，供 Responses 客户端凭回复句柄找回会话。
//
// 对话历史不在这里：上游按登记过的会话 ID 保管完整对话，网关每轮只发增量
// （见 native.go）。会话链只负责"这一轮属于哪个会话"。
//
// 局限：内存态，网关重启后清空 —— 原生续接随之为客户端新建上游会话，
// 客户端自带的历史会补种进去（见 native.go seedTurns）。

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

type sessionChainEntry struct {
	ProjectID        string
	ConversationID   string
	ResponseID       string // 上一轮回复句柄（终态 payload.id）
	AccountID        string
	Model            string
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
// 弱键只用于账号/项目亲和，绝不能据此继承上一轮的会话，否则就是跨会话
// （乃至跨用户）串上下文 —— 原生续接在弱键下连助手原文一起比对（见 native.go）。
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

// sessionChainBind 在请求开始前预先把会话键与项目关联，
// 同一客户端会话在整个生命周期内锁定同一个项目。
func sessionChainBind(key, projectID string) {
	if strings.TrimSpace(key) == "" {
		return
	}
	sessionChain.mu.Lock()
	defer sessionChain.mu.Unlock()
	e := chainEntryFor(key)
	if projectID != "" {
		e.ProjectID = projectID
	}
	e.UpdatedAt = time.Now()
}

// sessionChainRecord 统一记录本轮的上游会话句柄与项目。
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

// chainHit 是一次会话链命中的快照。
type chainHit struct {
	Key            string // 命中条目的主键（经别名命中时可能与请求的会话键不同）
	ProjectID      string
	ConversationID string
	ResponseID     string
	AccountID      string
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
		ResponseID: e.ResponseID, AccountID: e.AccountID,
	}, true
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
