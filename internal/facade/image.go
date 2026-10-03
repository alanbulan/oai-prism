package facade

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

var httpClientForImage = &http.Client{
	Timeout: 15 * time.Second,
}

// preprocessInputImages 将输入中的图片（Base64/URL/本地路径）真实上传至项目工作区，
// 并按官方 WebUI 规范转换为 input_file（路径为 /prism-uploads/<filename>），使上游模型能完整读取图片像素。
func preprocessInputImages(ctx context.Context, client *prism.Client, p prism.Principal, projectID string, items []prism.InputItem) ([]prism.InputItem, bool) {
	if len(items) == 0 {
		return items, false
	}

	var hasUpload bool
	outItems := make([]prism.InputItem, len(items))
	for i, item := range items {
		outItems[i] = item
		hasImage := false
		for _, c := range item.Content {
			if c.Type == "input_image" && c.ImageURL != "" {
				hasImage = true
				break
			}
		}
		if !hasImage {
			continue
		}

		newContents := make([]prism.InputContent, 0, len(item.Content))
		for _, c := range item.Content {
			if c.Type != "input_image" || c.ImageURL == "" {
				newContents = append(newContents, c)
				continue
			}

			data, ext, ok := extractImageData(ctx, c.ImageURL)
			if !ok || len(data) == 0 {
				newContents = append(newContents, c)
				continue
			}

			mime := "image/png"
			switch ext {
			case ".jpg", ".jpeg":
				mime = "image/jpeg"
			case ".webp":
				mime = "image/webp"
			case ".gif":
				mime = "image/gif"
			}

			// 若当前具备 projectID，直接上传到项目存储并注入官方 input_file
			if projectID != "" && client != nil {
				filename := "image_" + randHex(6) + ext
				err := client.UploadRawProjectFile(ctx, p, projectID, filename, mime, data)
				if err != nil {
					// 兜底回退到 multipart 上传
					_, err = client.UploadFile(ctx, p, prism.FileUpload{
						ProjectID:   projectID,
						Path:        filename,
						Filename:    filename,
						ContentType: mime,
						Data:        data,
					})
				}
				if err == nil {
					hasUpload = true
					newContents = append(newContents, prism.InputContent{
						Type:        "input_file",
						Filename:    filename,
						ProjectPath: filename,
					})
				}
			}

			// 同时保留原生 input_image（Base64 Data URI），兼顾纯视觉模型与沙箱环境
			b64 := base64.StdEncoding.EncodeToString(data)
			newContents = append(newContents, prism.InputContent{
				Type:     "input_image",
				ImageURL: "data:" + mime + ";base64," + b64,
				Detail:   c.Detail,
			})
		}
		outItems[i].Content = newContents
	}
	return outItems, hasUpload
}

func extractImageData(ctx context.Context, imgURL string) ([]byte, string, bool) {
	// 1. Base64 Data URI
	if strings.HasPrefix(imgURL, "data:image/") {
		comma := strings.IndexByte(imgURL, ',')
		if comma == -1 {
			return nil, "", false
		}
		header := imgURL[:comma]
		b64Data := imgURL[comma+1:]

		ext := ".png"
		if strings.Contains(header, "image/jpeg") || strings.Contains(header, "image/jpg") {
			ext = ".jpg"
		} else if strings.Contains(header, "image/webp") {
			ext = ".webp"
		} else if strings.Contains(header, "image/gif") {
			ext = ".gif"
		}

		decoded, err := base64.StdEncoding.DecodeString(b64Data)
		if err != nil {
			return nil, "", false
		}
		return decoded, ext, true
	}

	// 2. 本地文件路径（支持 file:/// 或直接绝对路径）
	localPath := imgURL
	if strings.HasPrefix(localPath, "file:///") {
		localPath = strings.TrimPrefix(localPath, "file:///")
	} else if strings.HasPrefix(localPath, "file://") {
		localPath = strings.TrimPrefix(localPath, "file://")
	}
	if fi, err := os.Stat(localPath); err == nil && !fi.IsDir() {
		data, err := os.ReadFile(localPath)
		if err == nil && len(data) > 0 {
			ext := strings.ToLower(filepath.Ext(localPath))
			if ext == "" {
				ext = ".png"
			}
			return data, ext, true
		}
	}

	// 3. 外部 HTTP/HTTPS 链接（排除 prism 内部域名）
	if (strings.HasPrefix(imgURL, "http://") || strings.HasPrefix(imgURL, "https://")) && !strings.Contains(imgURL, "prism.openai.com") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil)
		if err != nil {
			return nil, "", false
		}
		resp, err := httpClientForImage.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			return nil, "", false
		}
		defer func() { _ = resp.Body.Close() }()

		data, err := io.ReadAll(io.LimitReader(resp.Body, 15<<20)) // 限制 15MB
		if err != nil {
			return nil, "", false
		}

		ext := ".png"
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if strings.Contains(ct, "jpeg") || strings.Contains(ct, "jpg") {
			ext = ".jpg"
		} else if strings.Contains(ct, "webp") {
			ext = ".webp"
		}
		return data, ext, true
	}

	return nil, "", false
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
