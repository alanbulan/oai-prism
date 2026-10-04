package tokens

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"container/heap"
	_ "embed"
	"encoding/base64"
	"math"
	"strconv"
	"sync"

	"github.com/dlclark/regexp2/v2"
)

// o200k_base 词表：与 OpenAI 发布的 o200k_base.tiktoken 逐字节相同
// （sha256 446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d，
// 即 tiktoken 加载时校验的 expected_hash），gzip 后内嵌，运行时不联网。
//
//go:embed data/o200k_base.tiktoken.gz
var o200kGz []byte

// splitPattern 是 o200k_base 的预切分正则（tiktoken 的 _pat_str 原文）。
//
// 注意必须用 regexp2.Compile（解释执行）。第三方分词库给同一正则注册了
// 代码生成版本（MustCompile 会优先命中它），其中 \s*[\r\n]+ 的回溯有缺陷：
// "\n \n" 被切成 "\n" + " \n"，而 tiktoken 是一个整体 —— 代码缩进与空行
// 里处处都是这种序列，曾让计数偏差数个百分点。
const splitPattern = `[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+(?i:'s|'t|'re|'ve|'m|'ll|'d)?|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*(?i:'s|'t|'re|'ve|'m|'ll|'d)?|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n/]*|\s*[\r\n]+|\s+(?!\S)|\s+`

// longPieceBytes 以上的片段改用堆合并。BPE 原算法（tiktoken 同款）是 O(n²)：
// 一段 2MB 的连续字母会成为单个片段，按原算法永远算不完 —— 任何客户端都能
// 借此钉死一个 CPU 核。堆合并 O(n log n)，合并顺序与原算法完全一致。
const longPieceBytes = 256

type bpe struct {
	ranks map[string]uint
	split *regexp2.Regexp
}

var (
	bpeOnce sync.Once
	bpeInst *bpe
)

func encoder() *bpe {
	bpeOnce.Do(func() { bpeInst = loadBPE() })
	return bpeInst
}

func loadBPE() *bpe {
	zr, err := gzip.NewReader(bytes.NewReader(o200kGz))
	if err != nil {
		panic("tokens: 内嵌词表损坏: " + err.Error())
	}
	ranks := make(map[string]uint, 200_000)
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		line := sc.Bytes()
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		tok, err1 := base64.StdEncoding.DecodeString(string(line[:sp]))
		rank, err2 := strconv.ParseUint(string(line[sp+1:]), 10, 32)
		if err1 != nil || err2 != nil {
			panic("tokens: 内嵌词表格式错误")
		}
		ranks[string(tok)] = uint(rank)
	}
	if err := sc.Err(); err != nil {
		panic("tokens: 读取内嵌词表失败: " + err.Error())
	}
	re, err := regexp2.Compile(splitPattern, regexp2.None)
	if err != nil {
		panic("tokens: 预切分正则编译失败: " + err.Error())
	}
	return &bpe{ranks: ranks, split: re}
}

// count 对整段文本做预切分 + 逐片段 BPE，返回 token 数（等价 tiktoken encode_ordinary）。
func (b *bpe) count(s string) (int, error) {
	n := 0
	m, err := b.split.FindStringMatch(s)
	for err == nil && m != nil {
		n += b.pieceCount(m.String())
		m, err = b.split.FindNextMatch(m)
	}
	return n, err
}

func (b *bpe) pieceCount(piece string) int {
	if _, ok := b.ranks[piece]; ok {
		return 1
	}
	if len(piece) >= longPieceBytes {
		return b.heapMerge(piece)
	}
	return b.scanMerge(piece)
}

// scanMerge 是 tiktoken byte_pair_merge 的直译：每轮线性找最小 rank（同 rank 取最左）合并。
func (b *bpe) scanMerge(piece string) int {
	type part struct {
		start int
		rank  uint
	}
	parts := make([]part, len(piece)+1)
	rankAt := func(i int) uint {
		if i+2 < len(parts) {
			if r, ok := b.ranks[piece[parts[i].start:parts[i+2].start]]; ok {
				return r
			}
		}
		return math.MaxUint
	}
	for i := range parts {
		parts[i] = part{start: i, rank: math.MaxUint}
	}
	for i := 0; i+2 < len(parts); i++ {
		parts[i].rank = rankAt(i)
	}
	for len(parts) > 1 {
		minRank, minIdx := uint(math.MaxUint), -1
		for i := 0; i+1 < len(parts); i++ {
			if parts[i].rank < minRank {
				minRank, minIdx = parts[i].rank, i
			}
		}
		if minIdx < 0 {
			break
		}
		parts = append(parts[:minIdx+1], parts[minIdx+2:]...)
		parts[minIdx].rank = rankAt(minIdx)
		if minIdx > 0 {
			parts[minIdx-1].rank = rankAt(minIdx - 1)
		}
	}
	return len(parts) - 1
}

// heapMerge 与 scanMerge 结果相同：合并只改变左右两个相邻对的 rank，
// 用懒删除的最小堆维护候选（键为 rank、同 rank 按位置），整体 O(n log n)。
func (b *bpe) heapMerge(piece string) int {
	n := len(piece)
	// 节点 i 表示从字节 i 开始的部分；n 是末尾哨兵。
	next := make([]int, n+1)
	prev := make([]int, n+1)
	rank := make([]uint, n+1)
	alive := make([]bool, n+1)
	for i := 0; i <= n; i++ {
		next[i], prev[i], alive[i] = i+1, i-1, true
	}
	pairRank := func(i int) uint {
		j := next[i]
		if j >= n { // i 已是最后一个部分
			return math.MaxUint
		}
		if r, ok := b.ranks[piece[i:next[j]]]; ok {
			return r
		}
		return math.MaxUint
	}

	h := make(mergeHeap, 0, n)
	for i := range n {
		rank[i] = pairRank(i)
		if rank[i] != math.MaxUint {
			h = append(h, mergeCand{rank: rank[i], pos: i})
		}
	}
	heap.Init(&h)

	parts := n
	for h.Len() > 0 {
		c := heap.Pop(&h).(mergeCand)
		i := c.pos
		if !alive[i] || rank[i] != c.rank {
			continue // 过期候选
		}
		j := next[i]
		alive[j] = false
		next[i] = next[j]
		prev[next[j]] = i
		parts--

		rank[i] = pairRank(i)
		if rank[i] != math.MaxUint {
			heap.Push(&h, mergeCand{rank: rank[i], pos: i})
		}
		if p := prev[i]; p >= 0 {
			rank[p] = pairRank(p)
			if rank[p] != math.MaxUint {
				heap.Push(&h, mergeCand{rank: rank[p], pos: p})
			}
		}
	}
	return parts
}

type mergeCand struct {
	rank uint
	pos  int // 节点下标即字节偏移：同 rank 时下标小者在左
}

type mergeHeap []mergeCand

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(a, b int) bool {
	if h[a].rank != h[b].rank {
		return h[a].rank < h[b].rank
	}
	return h[a].pos < h[b].pos
}
func (h mergeHeap) Swap(a, b int) { h[a], h[b] = h[b], h[a] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(mergeCand)) }
func (h *mergeHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
