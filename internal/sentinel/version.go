package sentinel

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// 指纹多久没重新采集就提醒（Chrome 大约每 4 周一个大版本，120 天约落后 4 个）。
const (
	staleAfter    = 120 * 24 * time.Hour
	staleVersions = 3
)

var (
	uaChromeRe  = regexp.MustCompile(`Chrome/(\d+)\.0\.0\.0`)
	fullVerRe   = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	greaseRe    = regexp.MustCompile(`^Not.A.Brand$`)
	plistVerRe  = regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>([^<]+)</string>`)
	chromeVerRe = regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+)`)
)

// ChromeMajor 是指纹自称的 Chrome 大版本号（从 userAgent 解析），解析不了返回 0。
func (p *Profile) ChromeMajor() int {
	m := uaChromeRe.FindStringSubmatch(p.UserAgent())
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// SetChromeVersion 把指纹换成另一个 Chrome 版本（不重新采集时的简单做法）：
// userAgent / appVersion 换大版本号，品牌列表按 Chrome 自己的算法重排，
// highEntropy 里的完整版本号一并更新。version 可以是 "157" 或 "157.0.7512.80"；
// 指纹里存了完整版本号（highEntropy）时必须给完整版本。
func (p *Profile) SetChromeVersion(version string) error {
	major, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if err != nil || major < 100 {
		return fmt.Errorf("Chrome 版本号不对: %q", version)
	}
	full := ""
	if strings.Contains(version, ".") {
		if !fullVerRe.MatchString(version) {
			return fmt.Errorf("完整版本号应形如 157.0.7512.80: %q", version)
		}
		full = version
	}
	he := p.Navigator.UAData.HighEntropy
	if full == "" && he != nil && (he["fullVersionList"] != nil || he["uaFullVersion"] != nil) {
		return fmt.Errorf("指纹里有完整版本号，请给出完整版本（chrome://version 第一行，如 %d.0.7512.80）", major)
	}
	if p.ChromeMajor() == 0 {
		return fmt.Errorf("指纹的 userAgent 里找不到 Chrome 版本")
	}
	repl := fmt.Sprintf("Chrome/%d.0.0.0", major)
	for i, it := range p.Navigator.Proto {
		if k, _ := it[0].(string); k == "userAgent" || k == "appVersion" {
			if s, ok := it[2].(string); ok {
				p.Navigator.Proto[i][2] = uaChromeRe.ReplaceAllString(s, repl)
			}
		}
	}
	brand := p.brandName()
	p.Navigator.UAData.Brands = BrandList(major, brand, strconv.Itoa(major), false)
	if he != nil {
		if he["brands"] != nil {
			he["brands"] = p.Navigator.UAData.Brands
		}
		if he["fullVersionList"] != nil {
			he["fullVersionList"] = BrandList(major, brand, full, true)
		}
		if he["uaFullVersion"] != nil {
			he["uaFullVersion"] = full
		}
	}
	return nil
}

// brandName 是品牌列表里除 GREASE 与 Chromium 外的那个（Google Chrome / Microsoft Edge），没有返回空串。
func (p *Profile) brandName() string {
	for _, b := range p.Navigator.UAData.Brands {
		if b.Brand != "Chromium" && !greaseRe.MatchString(b.Brand) {
			return b.Brand
		}
	}
	return ""
}

// BrandList 复现 Chromium 的 GenerateBrandVersionList（components/embedder_support/user_agent_utils.cc）：
// 以大版本号为种子挑 GREASE 品牌名、版本和三者的排列。fullVersion 为 true 时是 fullVersionList 的形式。
func BrandList(major int, brand, version string, fullVersion bool) []Brand {
	chars := []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	versions := []string{"8", "99", "24"}
	grease := Brand{
		Brand:   "Not" + chars[major%len(chars)] + "A" + chars[(major+1)%len(chars)] + "Brand",
		Version: versions[major%len(versions)],
	}
	if fullVersion {
		grease.Version += ".0.0.0"
	}
	chromium := Brand{Brand: "Chromium", Version: version}
	if brand == "" {
		out := make([]Brand, 2)
		out[major%2] = grease
		out[(major+1)%2] = chromium
		return out
	}
	orders := [6][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	o := orders[major%6]
	out := make([]Brand, 3)
	out[o[0]] = grease
	out[o[1]] = chromium
	out[o[2]] = Brand{Brand: brand, Version: version}
	return out
}

// LocalChrome 找本机安装的 Google Chrome，返回版本号（如 154.0.8037.95）与可执行文件；找不到返回空串。
func LocalChrome() (version, exe string) {
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			base := os.Getenv(env)
			if base == "" {
				continue
			}
			dir := filepath.Join(base, "Google", "Chrome", "Application")
			if _, err := os.Stat(filepath.Join(dir, "chrome.exe")); err != nil {
				continue
			}
			// 版本号目录：升级未重启时新旧两个并存，取新的（重启后就是它）
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.IsDir() && fullVerRe.MatchString(e.Name()) && newerVersion(e.Name(), version) {
					version = e.Name()
				}
			}
			return version, filepath.Join(dir, "chrome.exe")
		}
	case "darwin":
		app := "/Applications/Google Chrome.app"
		if b, err := os.ReadFile(app + "/Contents/Info.plist"); err == nil {
			if m := plistVerRe.FindSubmatch(b); m != nil {
				version = string(m[1])
			}
			return version, app + "/Contents/MacOS/Google Chrome"
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable"} {
			if p, err := exec.LookPath(name); err == nil {
				if out, err := exec.Command(p, "--version").Output(); err == nil {
					version = chromeVerRe.FindString(string(out))
				}
				return version, p
			}
		}
	}
	return "", ""
}

func newerVersion(a, b string) bool {
	if b == "" {
		return true
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return len(pa) > len(pb)
}

// Staleness 检查指纹是否过时：落后本机 Chrome 3 个大版本以上，或采集已超过 120 天。
// 不过时返回空串，否则返回给人看的提醒。
func Staleness(p *Profile, now time.Time) string {
	v, _ := LocalChrome()
	return staleness(p, now, v)
}

func staleness(p *Profile, now time.Time, v string) string {
	have := p.ChromeMajor()
	if v != "" {
		local, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
		if local-have >= staleVersions {
			return fmt.Sprintf("浏览器指纹是 Chrome %d，本机 Chrome 已是 %d，建议运行 oaiprism profile -capture（或 -bump）更新", have, local)
		}
		return ""
	}
	if t, err := time.Parse("2006-01-02", p.CapturedAt); err == nil && now.Sub(t) > staleAfter {
		return fmt.Sprintf("浏览器指纹（Chrome %d）采集于 %s，已超过 %d 天，建议在装有 Chrome 的电脑上运行 oaiprism profile -capture 重新采集",
			have, p.CapturedAt, int(staleAfter.Hours()/24))
	}
	return ""
}
