package facade

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/oai-prism/oaiprism/internal/prism"
)

// 图片抓取的安全边界。
//
// image_url 完全由调用方控制。早期实现把它当本地路径直接 os.ReadFile，
// 并无条件抓取任意 http(s) 地址 —— 等于把网关主机的任意文件（含
// secrets/accounts.json 里的账号凭据）和内网地址（含云元数据端点）
// 开放给每一个调用方：读到的内容会上传进项目，再经模型或原样反代取回。
//
// 现在只接受两种来源：data:image/* 内联数据，以及解析到公网地址的
// http(s) 链接；且抓回来的必须真的是图片。
const (
	imageFetchTimeout  = 15 * time.Second
	imageFetchMaxBytes = 15 << 20
	imageMaxRedirects  = 5
)

var errImageBlocked = errors.New("图片地址指向内网或保留地址，已拒绝")

// isPublicIP 判断地址是否可以作为图片抓取目标。
func isPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, cidr := range reservedCIDRs {
		if cidr.Contains(ip) {
			return false
		}
	}
	return true
}

var reservedCIDRs = func() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range []string{
		"0.0.0.0/8",       // "本网络"
		"100.64.0.0/10",   // 运营商级 NAT
		"192.0.0.0/24",    // IETF 协议分配
		"192.0.2.0/24",    // 文档
		"198.18.0.0/15",   // 基准测试
		"198.51.100.0/24", // 文档
		"203.0.113.0/24",  // 文档
		"240.0.0.0/4",     // 保留
		"64:ff9b::/96",    // NAT64（可映射到内网 IPv4）
	} {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// checkImageURL 校验 URL 的协议与解析结果：所有解析出的地址都必须是公网地址。
func checkImageURL(ctx context.Context, u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("不支持的图片协议: %s", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("图片地址缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return errImageBlocked
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("解析图片主机失败: %w", err)
	}
	for _, a := range addrs {
		if !isPublicIP(a.IP) {
			return errImageBlocked
		}
	}
	return nil
}

// safeDialControl 在真正建连时再校验一次目标地址：
// 防 DNS rebinding（校验时解析到公网、建连时解析到内网）。
func safeDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if !isPublicIP(net.ParseIP(host)) {
		return errImageBlocked
	}
	return nil
}

func newImageClient(useProxy bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: imageFetchTimeout,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
	}
	if useProxy {
		// 走出站代理时建连对象是代理本身（常在本机），无法在 dial 层校验目标；
		// 此时依赖 checkImageURL 的解析校验（含每一跳重定向）。
		tr.Proxy = http.ProxyFromEnvironment
	} else {
		dialer.Control = safeDialControl
	}
	return &http.Client{
		Timeout:   imageFetchTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= imageMaxRedirects {
				return errors.New("图片地址重定向次数过多")
			}
			return checkImageURL(req.Context(), req.URL)
		},
	}
}

var (
	imageDirectClient = newImageClient(false)
	imageProxyClient  = newImageClient(true)
)

// fetchRemoteImage 抓取公网图片。返回的数据保证是图片（按内容嗅探，不信任响应头）。
func fetchRemoteImage(ctx context.Context, raw string) ([]byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	if err := checkImageURL(ctx, u); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	client := imageDirectClient
	if p, _ := http.ProxyFromEnvironment(req); p != nil {
		client = imageProxyClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, "", fmt.Errorf("图片地址返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, imageFetchMaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > imageFetchMaxBytes {
		return nil, "", fmt.Errorf("图片超过 %d 字节上限", imageFetchMaxBytes)
	}
	ext, ok := imageExtOf(data)
	if !ok {
		// 只认真正的图片：否则这里就成了"让网关代取任意 URL 内容"的通道。
		return nil, "", errors.New("图片地址返回的不是图片")
	}
	return data, ext, nil
}

// imageExtOf 按内容嗅探图片类型。
func imageExtOf(data []byte) (string, bool) {
	switch http.DetectContentType(data) {
	case "image/png":
		return ".png", true
	case "image/jpeg":
		return ".jpg", true
	case "image/gif":
		return ".gif", true
	case "image/webp":
		return ".webp", true
	case "image/bmp":
		return ".bmp", true
	}
	return "", false
}

// uploadCache 记录"某账号某项目里已上传过的图片"。
//
// 客户端每轮回传完整历史，历史里的图片也一并回传；不做缓存的话，
// 每一轮都会把所有旧图片重新上传一遍，并因"有新上传"强制沙箱整套重新同步。
type uploadCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]uploadEntry
}

type uploadEntry struct {
	filename string
	expires  time.Time
}

func newUploadCache(ttl time.Duration) *uploadCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &uploadCache{ttl: ttl, m: make(map[string]uploadEntry, 16)}
}

func uploadKey(accountID, projectID string, data []byte) string {
	sum := sha256.Sum256(data)
	return accountID + "|" + projectID + "|" + hex.EncodeToString(sum[:])
}

func (c *uploadCache) Get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Now().After(e.expires) {
		delete(c.m, key)
		return "", false
	}
	return e.filename, true
}

func (c *uploadCache) Put(key, filename string) {
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 1024 {
		for k, e := range c.m {
			if now.After(e.expires) {
				delete(c.m, k)
			}
		}
	}
	c.m[key] = uploadEntry{filename: filename, expires: now.Add(c.ttl)}
}

// preprocessInputImages 将输入中的图片（data URI / 公网 URL）上传至项目工作区，
// 并按官方 WebUI 规范转换为 input_file（路径为 /prism-uploads/<filename>），使上游模型能完整读取图片像素。
// 返回值 hasUpload 仅在本轮确有**新**上传时为 true（调用方据此重做沙箱同步）。
func preprocessInputImages(ctx context.Context, client *prism.Client, p prism.Principal, cache *uploadCache, accountID, projectID string, items []prism.InputItem) ([]prism.InputItem, bool) {
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
				// 取不到（含被安全策略拒绝）就原样交给上游，由上游决定能否访问。
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
			case ".bmp":
				mime = "image/bmp"
			}

			// 若当前具备 projectID，上传到项目存储并注入官方 input_file（已传过的直接复用）。
			if projectID != "" && client != nil {
				key := uploadKey(accountID, projectID, data)
				if filename, ok := cache.Get(key); ok {
					newContents = append(newContents, prism.InputContent{
						Type: "input_file", Filename: filename, ProjectPath: filename,
					})
				} else {
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
						cache.Put(key, filename)
						newContents = append(newContents, prism.InputContent{
							Type: "input_file", Filename: filename, ProjectPath: filename,
						})
					}
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

// extractImageData 取图片字节。只支持 data:image/* 与公网 http(s) 链接；
// 本地路径（含 file://）一律拒绝 —— 网关主机的文件系统不是调用方能读的东西。
func extractImageData(ctx context.Context, imgURL string) ([]byte, string, bool) {
	if strings.HasPrefix(imgURL, "data:image/") {
		comma := strings.IndexByte(imgURL, ',')
		if comma == -1 {
			return nil, "", false
		}
		header := imgURL[:comma]
		if !strings.Contains(header, ";base64") {
			return nil, "", false
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(imgURL[comma+1:]))
		if err != nil {
			return nil, "", false
		}
		ext, ok := imageExtOf(decoded)
		if !ok {
			return nil, "", false
		}
		return decoded, ext, true
	}

	if strings.HasPrefix(imgURL, "http://") || strings.HasPrefix(imgURL, "https://") {
		// prism 自家的地址交给上游自己取（它有权限，我们不必中转）。
		if u, err := url.Parse(imgURL); err == nil && strings.HasSuffix(strings.ToLower(u.Hostname()), "prism.openai.com") {
			return nil, "", false
		}
		data, ext, err := fetchRemoteImage(ctx, imgURL)
		if err != nil {
			return nil, "", false
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
