package tokens

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

// 期望值来自官方 tiktoken：tiktoken.get_encoding("o200k_base").encode_ordinary(s)。
// 另在本地与 tiktoken 逐条对拍、零差异：仓库全部源码与文档（41 万 token）、
// "ab \n" 的全部 1～6 位组合（5460 条）、4500 条随机 Unicode（各文字、组合符、
// emoji、各类空白），以及超长同类字符串。
func TestCountMatchesTiktoken(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello world", 2},
		{"Hello, world!", 4},
		{"你好世界", 2},
		{"  多空格\n\n换行\tTab   结尾  ", 14},
		{"🙂🚀👨‍👩‍👧‍👦 emoji 测试 ✅", 19},
		{"I'm you'RE they'LL we'd it's", 9},
		{"func main() {\n\tfmt.Println(\"hi\")\n}\n", 10},
		{"user", 1},
		{"assistant", 1},
		{"system", 1},
		// 空白回溯：\s*[\r\n]+ 必须整体匹配（第三方库的代码生成正则会切成两段）
		{"\n \n", 1},
		{"a\n \n", 2},
		{" \n \n\n  ", 3},
		{"\tif x {\n\t\treturn\n\t}\n\n", 8},
		// 长片段走堆合并
		{strings.Repeat("a", 1000), 125},
		{strings.Repeat(" ", 1000) + "y", 10},
		{strings.Repeat("=", 999), 16},
		{strings.Repeat("中", 800), 800},
	}
	for _, c := range cases {
		if got := Count(c.in); got != c.want {
			t.Errorf("Count(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// 长文本走缓存：命中前后结果一致，并发读写安全。
func TestCountCacheConsistent(t *testing.T) {
	long := strings.Repeat("The quick brown fox 跳过了懒狗。\n", 64)
	want := encode(long)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 50 {
				if got := Count(long); got != want {
					t.Errorf("cached Count = %d, want %d", got, want)
					return
				}
			}
		})
	}
	wg.Wait()
}

// 官方文档给出的算例：1024×1024 高精度 765；2048×4096 高精度 1105；低精度恒为 85。
func TestImageTokens(t *testing.T) {
	cases := []struct {
		w, h   int
		detail string
		want   int
	}{
		{1024, 1024, "high", 765},
		{2048, 4096, "high", 1105},
		{4096, 8192, "low", 85},
		{512, 512, "auto", 255},
		{100, 100, "", 255},
		{0, 0, "high", imageUnknown},
	}
	for _, c := range cases {
		if got := Image(c.w, c.h, c.detail); got != c.want {
			t.Errorf("Image(%d,%d,%q) = %d, want %d", c.w, c.h, c.detail, got, c.want)
		}
	}
}

// data URI 解析真实宽高：PNG 走标准库，WebP 走手写头解析。
func TestImageURLDimensions(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1600, 900))); err != nil {
		t.Fatal(err)
	}
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	// 1600×900 → 短边缩到 768：1365×768 → 3×2 块 → 85 + 6×170
	if got := ImageURL(uri, "high"); got != 1105 {
		t.Errorf("png ImageURL = %d, want 1105", got)
	}

	// VP8X 扩展头：画布 (宽-1, 高-1) 各 24 位
	webp := make([]byte, 30)
	copy(webp, "RIFF")
	copy(webp[8:], "WEBPVP8X")
	webp[24], webp[25] = 0xff, 0x01 // 宽 512
	webp[27], webp[28] = 0xff, 0x01 // 高 512
	uri = "data:image/webp;base64," + base64.StdEncoding.EncodeToString(webp)
	if got := ImageURL(uri, "auto"); got != 255 {
		t.Errorf("webp ImageURL = %d, want 255", got)
	}

	if got := ImageURL("https://example.com/a.png", "high"); got != imageUnknown {
		t.Errorf("remote ImageURL = %d, want %d", got, imageUnknown)
	}
}

// 堆合并与 tiktoken 原算法（线性扫描）逐片段一致。
func TestHeapMergeMatchesScan(t *testing.T) {
	b := encoder()
	r := rand.New(rand.NewSource(3))
	alphabets := []string{"ab", "abcdeAB", "  \n", "=-*#", "你好世界的", "aé", "xyz0"}
	for k := range 600 {
		al := []rune(alphabets[k%len(alphabets)])
		var sb strings.Builder
		for range 1 + r.Intn(800) {
			sb.WriteRune(al[r.Intn(len(al))])
		}
		piece := sb.String()
		if got, want := b.heapMerge(piece), b.scanMerge(piece); got != want {
			t.Fatalf("k=%d len=%d: heap=%d scan=%d", k, len(piece), got, want)
		}
	}
}

// 防 CPU 钉死：2MB 连续字母是单个预切分片段，原 O(n²) 合并永远算不完。
func TestHugeRunBounded(t *testing.T) {
	start := time.Now()
	if got := Count(strings.Repeat("a", 2<<20)); got != 262144 {
		t.Fatalf("2MB 'a' = %d tokens, want 262144", got)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("2MB 连续字母耗时 %v，合并复杂度退化", el)
	}
}
