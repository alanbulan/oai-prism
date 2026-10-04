package tokens

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	_ "image/gif" // 注册解码器：只读取图片头取宽高
	_ "image/jpeg"
	_ "image/png"
	"io"
	"strings"
)

// OpenAI 视觉计费规则（GPT-4o / GPT-4.1 / GPT-5 系列）：
//
//	detail=low：固定 85
//	detail=high/auto：先等比缩放到 2048×2048 以内，再把短边缩到 768，
//	                  按 512×512 切块，每块 170，另加 85 基础
const (
	imageBase    = 85
	imagePerTile = 170
	// 取不到宽高（远程链接未下载、格式不认识）时按 1024×1024 高精度计：765
	imageUnknown = imageBase + 4*imagePerTile
)

// Image 计算一张图片的 token 数。width/height 未知时传 0。
func Image(width, height int, detail string) int {
	if strings.EqualFold(detail, "low") {
		return imageBase
	}
	if width <= 0 || height <= 0 {
		return imageUnknown
	}
	w, h := float64(width), float64(height)
	if w > 2048 || h > 2048 {
		s := 2048 / max(w, h)
		w, h = w*s, h*s
	}
	if short := min(w, h); short > 768 {
		s := 768 / short
		w, h = w*s, h*s
	}
	tiles := ceilDiv(w, 512) * ceilDiv(h, 512)
	return imageBase + imagePerTile*tiles
}

func ceilDiv(v, d float64) int {
	n := int(v / d)
	if float64(n)*d < v {
		n++
	}
	return n
}

// ImageURL 计算图片块的 token 数：data URI 会解析图片头拿到真实宽高，
// 其余（http 链接等）按未知尺寸处理。
func ImageURL(url, detail string) int {
	w, h := dataURIDimensions(url)
	return Image(w, h, detail)
}

// dataURIDimensions 只解码图片头部：image.DecodeConfig 读到宽高就停，
// 不会把几 MB 的 base64 全部解码。
func dataURIDimensions(url string) (int, int) {
	if !strings.HasPrefix(url, "data:image/") {
		return 0, 0
	}
	meta, payload, ok := strings.Cut(url, ",")
	if !ok || !strings.Contains(meta, ";base64") {
		return 0, 0
	}
	r := base64.NewDecoder(base64.StdEncoding, strings.NewReader(payload))
	head := make([]byte, 256<<10) // JPEG 的 EXIF 段可能把 SOF 推到较后位置
	n, _ := io.ReadFull(r, head)
	head = head[:n]
	if w, h, ok := webpDimensions(head); ok {
		return w, h
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
		return cfg.Width, cfg.Height
	}
	return 0, 0
}

// webpDimensions 解析 WebP 头（标准库不带 WebP 解码器）。
func webpDimensions(b []byte) (int, int, bool) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ": // 有损：帧头后紧跟 14 位宽、14 位高
		w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
		return w, h, w > 0 && h > 0
	case "VP8L": // 无损：签名 0x2f 后 14 位 (宽-1)、14 位 (高-1)
		if b[20] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(b[21:25])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, true
	case "VP8X": // 扩展：24 位 (画布宽-1)、24 位 (画布高-1)
		w := int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		h := int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return w + 1, h + 1, true
	}
	return 0, 0, false
}
