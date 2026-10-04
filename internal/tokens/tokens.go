// Package tokens 用 o200k_base 分词表精确计算 token 数。
//
// 上游从不返回 usage（轮询响应顶层与 payload 均无此键，抓包实证），
// 早期用"CJK 1 字 1 token、拉丁 4 字符 1 token"估算，误差动辄 30% 以上。
// o200k_base 是 GPT-4o / GPT-4.1 / GPT-5 系列的分词表；这里用官方词表
// 与官方预切分正则做真实 BPE 计数，与 tiktoken 的 encode_ordinary 逐 token 一致。
package tokens

import (
	"hash/maphash"
	"sync"
)

// 消息帧开销，沿用 OpenAI 官方计数口径（openai-cookbook num_tokens_from_messages）：
// 每条消息 <|start|>{role}<|message|>{content}<|end|> 除角色名与正文外占 3 个 token，
// 整段对话末尾的回复引导 <|start|>assistant<|message|> 再占 3 个。
const (
	PerMessage   = 3
	ReplyPriming = 3
)

// Warmup 在后台加载词表（20 万条映射，首次加载需要数百毫秒），
// 避免第一笔请求承担这段延迟。
func Warmup() {
	go encoder()
}

// Codex 每一轮都会重发完整历史：系统指令与早先的消息块逐字相同。
// 按内容寻址缓存长文本的计数，重复块不再重新编码。
const (
	cacheMinLen  = 256 // 短文本直接编码，比查缓存更划算
	cacheEntries = 8192
)

type cacheEntry struct {
	size  int // 原文长度，与哈希一起比对，进一步排除碰撞
	count int
}

var (
	cacheSeed = maphash.MakeSeed()
	cacheMu   sync.Mutex
	cache     = make(map[uint64]cacheEntry, cacheEntries)
)

// Count 返回 s 的 o200k_base token 数。
func Count(s string) int {
	if s == "" {
		return 0
	}
	if len(s) < cacheMinLen {
		return encode(s)
	}
	key := maphash.String(cacheSeed, s)
	cacheMu.Lock()
	e, ok := cache[key]
	cacheMu.Unlock()
	if ok && e.size == len(s) {
		return e.count
	}
	n := encode(s)
	cacheMu.Lock()
	if len(cache) >= cacheEntries {
		// 满了整体清空：历史块的复用集中在相邻几轮之间，不值得维护 LRU 链表
		clear(cache)
	}
	cache[key] = cacheEntry{size: len(s), count: n}
	cacheMu.Unlock()
	return n
}

func encode(s string) int {
	n, err := encoder().count(s)
	if err != nil {
		// 只有正则匹配超时会走到这里（未设置超时，实际不可达）；
		// 宁可给出保守估计，也不让计费口径变成 0。
		return len(s) / 4
	}
	return n
}
