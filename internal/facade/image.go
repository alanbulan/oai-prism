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
	"path"
	"regexp"
	"slices"
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

	// projects：项目 → 最近一次登记新文件的时刻（决定沙箱要不要换新，见 Runner.runOnce）。
	projects map[string]time.Time
}

type uploadEntry struct {
	projectPath string
	expires     time.Time
}

func newUploadCache(ttl time.Duration) *uploadCache {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &uploadCache{ttl: ttl, m: make(map[string]uploadEntry, 16), projects: map[string]time.Time{}}
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
	return e.projectPath, true
}

func (c *uploadCache) Put(key, projectPath string) {
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
	c.m[key] = uploadEntry{projectPath: projectPath, expires: now.Add(c.ttl)}
}

// MarkProject 记下项目刚登记了新文件。
func (c *uploadCache) MarkProject(projectID string) {
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, at := range c.projects {
		if now.Sub(at) > c.ttl {
			delete(c.projects, p)
		}
	}
	c.projects[projectID] = now
}

// LastUpload 返回项目最近一次登记新文件的时刻。
func (c *uploadCache) LastUpload(projectID string) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.projects[projectID]
	return at, ok
}

// attachImages 把输入里的图片（data URI / 公网 URL）按官方前端的做法交给上游：
// 上传到项目、登记进项目文件树的 prism-uploads/ 目录，再以 input_file 引用
// （project_path 为 /prism-uploads/<文件名>）。
//
// 上游不会把 input_image 的像素交给模型（会话里只剩一个 "[image]" 占位），模型看图
// 靠的是用沙箱工具打开项目里的这个文件；所以文件必须真的出现在沙箱工作区 —— 只上传
// 不登记的话，沙箱里没有它，模型会说"文件不存在"。登记成功后不再附带 input_image。
//
// 返回值 hasUpload 仅在本轮确有**新**上传时为 true（调用方据此重做沙箱同步）。
func (r *Runner) attachImages(ctx context.Context, p prism.Principal, accountID, projectID string, items []prism.InputItem) ([]prism.InputItem, bool) {
	if len(items) == 0 || projectID == "" || r.client == nil {
		return items, false
	}
	var hasUpload bool
	outItems := make([]prism.InputItem, len(items))
	for i, item := range items {
		outItems[i] = item
		if !slices.ContainsFunc(item.Content, func(c prism.InputContent) bool { return c.Type == "input_image" && c.ImageURL != "" }) {
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
			key := uploadKey(accountID, projectID, data)
			projectPath, cached := r.uploads.Get(key)
			if !cached {
				var err error
				projectPath, err = r.uploadImage(ctx, p, projectID, data, ext)
				if err != nil {
					r.log.Warn("图片登记进项目失败，改为内联发送（上游可能看不到图片）", "project", projectID, "err", err)
					newContents = append(newContents, prism.InputContent{
						Type: "input_image", ImageURL: "data:" + imageMIME(ext) + ";base64," + base64.StdEncoding.EncodeToString(data), Detail: c.Detail,
					})
					continue
				}
				hasUpload = true
				r.uploads.Put(key, projectPath)
				r.uploads.MarkProject(projectID)
			}
			stripLocalImagePath(newContents)
			newContents = append(newContents,
				prism.InputContent{Type: "input_file", Filename: path.Base(projectPath), ProjectPath: projectPath},
				// 上游把它渲染成 "[project file: /prism-uploads/x.png]"，模型会照字面去开文件系统根下的
				// /prism-uploads（不存在）；文件其实在沙箱工作目录下
				prism.InputContent{Type: prism.BlockInputText, Text: attachmentHint(projectPath)},
			)
		}
		outItems[i].Content = newContents
	}
	return outItems, hasUpload
}

// uploadImage 上传图片并登记进项目文件树，返回项目内路径。
func (r *Runner) uploadImage(ctx context.Context, p prism.Principal, projectID string, data []byte, ext string) (string, error) {
	filename := "image_" + randHex(6) + ext
	fileID, err := r.client.UploadRawProjectFile(ctx, p, projectID, filename, imageMIME(ext), data)
	if err != nil {
		return "", fmt.Errorf("上传: %w", err)
	}
	projectPath, err := r.client.RegisterProjectFile(ctx, p, projectID, fileID, filename)
	if err != nil {
		return "", fmt.Errorf("登记进文件树: %w", err)
	}
	return projectPath, nil
}

func imageMIME(ext string) string {
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	}
	return "image/png"
}

// attachmentHint 告诉模型附件在沙箱工作区里的实际位置。
func attachmentHint(projectPath string) string {
	rel := strings.TrimPrefix(projectPath, "/")
	return "(Attached image: view it with your view_image tool at the workspace-relative path `" + rel +
		"` — relative to your current working directory, not `/" + rel + "` at the filesystem root.)"
}

// localImagePathRe 匹配 Codex 给图片加的标签里的本机路径：<image name=[Image #1] path="F:\...">。
var localImagePathRe = regexp.MustCompile(`(<image\b[^>\n]*?)\s+path="[^"\n]*"`)

// stripLocalImagePath 去掉图片标签里的本机路径：上游模型看图只能用沙箱里的项目文件，
// 看到一个 Windows 本地路径反而会去沙箱里打开它，得到"文件不存在"。
func stripLocalImagePath(contents []prism.InputContent) {
	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i].Type != prism.BlockInputText {
			continue
		}
		contents[i].Text = localImagePathRe.ReplaceAllString(contents[i].Text, "$1")
		return
	}
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
