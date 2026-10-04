package account

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oai-prism/oaiprism/internal/config"
)

// Store 负责把凭据从磁盘加载进来，并在文件变化时热重载。
//
// 为什么要单独做一层：凭据是"运行期会变的东西"，
// 让用户改完文件去重启服务是不能接受的——
// 尤其当 access_token 只有几天寿命时，重启就成了运维负担。
// 所以这里用 mtime+size 轮询做变更检测，成本极低（一次 stat），
// 却换来"改文件即生效"的体验。
type Store struct {
	path string
	log  *slog.Logger

	mu      sync.Mutex
	lastMod time.Time
	lastSz  int64
	curr    []config.AccountConfig
	loaded  bool
}

// NewStore 构造文件凭据仓库。
func NewStore(path string, log *slog.Logger) *Store {
	return &Store{path: path, log: log}
}

// Path 返回凭据文件路径。
func (s *Store) Path() string { return s.path }

// Accounts 返回最近一次成功加载的账号配置。
func (s *Store) Accounts() []config.AccountConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]config.AccountConfig(nil), s.curr...)
}

// Exists 判断凭据文件是否存在。
func (s *Store) Exists() bool {
	if s.path == "" {
		return false
	}
	_, err := os.Stat(s.path)
	return err == nil
}

// Load 读取凭据文件。文件不存在时返回 (nil, nil)——
// 这是刻意设计：服务应当能先起来，凭据后补。
func (s *Store) Load() ([]config.AccountConfig, error) {
	if s.path == "" {
		return nil, nil
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}

	list, err := ParseAccounts(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 %s: %w", s.path, err)
	}

	s.mu.Lock()
	s.lastMod = fi.ModTime()
	s.lastSz = fi.Size()
	s.curr = list
	s.loaded = true
	s.mu.Unlock()

	return list, nil
}

// Changed 判断文件是否自上次加载后发生了变化。
func (s *Store) Changed() bool {
	if s.path == "" {
		return false
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		// 文件被删掉了：算作变化，让上层把池清空。
		s.mu.Lock()
		prev := s.loaded
		s.loaded = false
		s.mu.Unlock()
		return prev
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return true
	}
	return !fi.ModTime().Equal(s.lastMod) || fi.Size() != s.lastSz
}

// Watch 轮询文件变化并回调。
//
// 之所以用轮询而不是 fsnotify：跨平台行为差异（尤其容器挂载卷的
// inotify 事件不可靠）会引入难以排查的"改了没生效"。一次 stat 的
// 成本在微秒级，5 秒一轮完全无感。
func (s *Store) Watch(ctx context.Context, interval time.Duration, onChange func([]config.AccountConfig)) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.Changed() {
				continue
			}
			list, err := s.Load()
			if err != nil {
				s.log.Error("凭据文件重载失败，保留旧凭据", "path", s.path, "err", err)
				continue
			}
			s.log.Info("凭据文件已变更，热重载", "path", s.path, "count", len(list))
			if onChange != nil {
				onChange(list)
			}
		}
	}
}

// Persist 把账号列表原子写回磁盘（用于刷新后的 token 回写）。
func (s *Store) Persist(list []config.AccountConfig) error {
	if s.path == "" {
		return fmt.Errorf("未配置 creds.file")
	}

	doc := fileDoc{Accounts: make([]fileAccount, 0, len(list))}
	for _, a := range list {
		fa := fileAccount{
			ID:             a.ID,
			Name:           a.Name,
			Cookies:        a.Cookies,
			CookieMap:      a.CookieMap,
			SessionToken:   a.SessionToken,
			AccessToken:    a.AccessToken,
			RefreshToken:   a.RefreshToken,
			AccountID:      a.AccountID,
			Email:          a.Email,
			Plan:           a.Plan,
			Proxy:          a.Proxy,
			MaxConcurrency: a.MaxConcurrency,
			RatePerSecond:  a.RatePerSecond,
			RateBurst:      a.RateBurst,
			Weight:         a.Weight,
			Headers:        a.Headers,
			Tags:           a.Tags,
			UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
		}
		if a.ExpiresAt != nil {
			fa.ExpiresAt = a.ExpiresAt.UTC().Format(time.RFC3339)
		}
		doc.Accounts = append(doc.Accounts, fa)
	}

	buf, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	// 权限 0600：这里存的是可以直接登录你账号的凭据。
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// 写回后同步内部状态，避免 Watcher 把自己写的当成外部变更再刷一遍。
	if fi, err := os.Stat(s.path); err == nil {
		s.mu.Lock()
		s.lastMod = fi.ModTime()
		s.lastSz = fi.Size()
		s.curr = append([]config.AccountConfig(nil), list...)
		s.loaded = true
		s.mu.Unlock()
	}
	return nil
}

// ---------------------------- 文件格式 ----------------------------

type fileDoc struct {
	Accounts []fileAccount `json:"accounts"`
}

type fileAccount struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Enabled        *bool             `json:"enabled"`
	Cookies        string            `json:"cookies"`
	CookieMap      map[string]string `json:"cookie_map"`
	SessionToken   string            `json:"session_token"`
	AccessToken    string            `json:"access_token"`
	RefreshToken   string            `json:"refresh_token"`
	ExpiresAt      string            `json:"expires_at"`
	AccountID      string            `json:"account_id"`
	Email          string            `json:"email"`
	Plan           string            `json:"plan"`
	Proxy          string            `json:"proxy"`
	MaxConcurrency int               `json:"max_concurrency"`
	RatePerSecond  float64           `json:"rate_per_second"`
	RateBurst      int               `json:"rate_burst"`
	Weight         int               `json:"weight"`
	Headers        map[string]string `json:"headers"`
	Tags           []string          `json:"tags"`
	UpdatedAt      string            `json:"updated_at,omitempty"`
}

// ParseAccounts 解析凭据文件，同时接受几种写法：
//   - {"accounts":[...]}   推荐，可带元数据（其它网关的导出文件也是这个形状）
//   - [...]                裸数组，手写更省事
//   - {...}                单个账号
//
// 并且字段名大小写/下划线不敏感，"access_token" / "accessToken" / "Access-Token" 都能识别。
// 这是刻意的宽容：凭据文件是用户手写的，让人对着文档数下划线是糟糕的体验。
// 没写 id 的账号按位置编号（file-1、file-2…），没写名字的用 id。
func ParseAccounts(raw []byte) ([]config.AccountConfig, error) {
	list, err := ParseAccountList(raw)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == "" {
			list[i].ID = fmt.Sprintf("file-%d", i+1)
		}
		if list[i].Name == "" {
			list[i].Name = list[i].ID
		}
	}
	return list, nil
}

// ParseAccountList 与 ParseAccounts 相同，但不补默认 id / 名字（由调用方决定，见管理端导入）。
//
// 认得 sub2api 一类网关的导出格式：账号的凭据放在嵌套的 credentials 对象里
//（access_token、refresh_token、email、plan_type…），platform 标明平台。
func ParseAccountList(raw []byte) ([]config.AccountConfig, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var (
		rawAccounts []json.RawMessage
		err         error
	)
	switch raw[0] {
	case '[':
		err = json.Unmarshal(raw, &rawAccounts)
	case '{':
		var probe map[string]json.RawMessage
		if err = json.Unmarshal(raw, &probe); err != nil {
			break
		}
		if list, ok := probe["accounts"]; ok {
			err = json.Unmarshal(list, &rawAccounts)
		} else if len(probe) > 0 {
			rawAccounts = []json.RawMessage{raw}
		}
	default:
		return nil, fmt.Errorf("无法识别的 JSON 结构")
	}
	if err != nil {
		return nil, err
	}

	out := make([]config.AccountConfig, 0, len(rawAccounts))
	for i, ra := range rawAccounts {
		m, err := decodeLoose(ra)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个账号: %w", i+1, err)
		}
		ac, err := accountFromMap(m)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个账号: %w", i+1, err)
		}
		out = append(out, ac)
	}
	return out, nil
}

// accountFromMap 把归一化 key 的账号对象转成配置。
func accountFromMap(m map[string]any) (config.AccountConfig, error) {
	// 导出格式：凭据在 credentials 里，外层的 concurrency / priority / extra 是导出方自己的
	// 调度参数，不照搬 —— 那边常设 10 并发，Prism 对同一账号的并发很敏感，沿用本网关的默认值。
	if creds, ok := m["credentials"].(map[string]any); ok {
		if p := str(m, "platform"); p != "" && !strings.EqualFold(p, "openai") {
			return config.AccountConfig{}, fmt.Errorf("不是 OpenAI 账号（platform=%s）", p)
		}
		flat := make(map[string]any, len(creds)+4)
		for k, v := range creds {
			flat[normKey(k)] = v
		}
		for _, k := range []string{"id", "name", "enabled", "tags", "proxy"} {
			if v, ok := m[k]; ok {
				flat[k] = v
			}
		}
		if len(strs(flat, "tags")) == 0 && strings.EqualFold(str(m, "type"), "oauth") {
			flat["tags"] = []any{"oauth"}
		}
		m = flat
	}

	ac := config.AccountConfig{
		ID:             str(m, "id"),
		Name:           str(m, "name"),
		Cookies:        str(m, "cookies", "cookie"),
		SessionToken:   str(m, "sessiontoken", "session"),
		AccessToken:    str(m, "accesstoken", "token", "jwt", "bearertoken"),
		RefreshToken:   str(m, "refreshtoken"),
		AccountID:      str(m, "accountid", "chatgptaccountid", "deviceid"),
		Email:          str(m, "email"),
		Plan:           str(m, "plan", "plantype"),
		Proxy:          str(m, "proxy"),
		MaxConcurrency: integer(m, "maxconcurrency", "concurrency"),
		RatePerSecond:  number(m, "ratepersecond", "rate"),
		RateBurst:      integer(m, "rateburst", "burst"),
		Weight:         integer(m, "weight"),
		Tags:           strs(m, "tags"),
	}
	if v, ok := boolp(m, "enabled"); ok {
		ac.Enabled = &v
	}
	if cm := strmap(m, "cookiemap"); cm != nil {
		ac.CookieMap = cm
	}
	if hm := strmap(m, "headers"); hm != nil {
		ac.Headers = hm
	}
	if ts := str(m, "expiresat"); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			ac.ExpiresAt = &t
		}
	}
	return ac, nil
}

// HasCredentials 判断账号配置里有没有任何能用来认证的东西。
func HasCredentials(a config.AccountConfig) bool {
	return a.AccessToken != "" || a.RefreshToken != "" || a.SessionToken != "" ||
		a.Cookies != "" || len(a.CookieMap) > 0
}

// MergeAccountPatch 把管理端的部分更新（只含要改的字段，字段名同样宽容）合并到已有账号上。
//
// 必须逐字段合并：整体反序列化时没给的字段都是零值，存回去就把已有的标签、并发上限
// 覆盖掉了（Dashboard 的编辑框只发 name / email / plan / max_concurrency）。
func MergeAccountPatch(base config.AccountConfig, raw []byte) (config.AccountConfig, error) {
	m, err := decodeLoose(raw)
	if err != nil {
		return base, err
	}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := m[k]; ok {
				return true
			}
		}
		return false
	}
	out := base
	if has("name") {
		out.Name = str(m, "name")
	}
	if has("email") {
		out.Email = str(m, "email")
	}
	if has("plan", "plantype") {
		out.Plan = str(m, "plan", "plantype")
	}
	if has("maxconcurrency", "concurrency") {
		out.MaxConcurrency = integer(m, "maxconcurrency", "concurrency")
	}
	if has("tags") {
		out.Tags = strs(m, "tags")
	}
	// 凭据只在给了非空值时替换（SaveAccount 对空凭据本来就保留旧值）
	for _, f := range []struct {
		dst  *string
		keys []string
	}{
		{&out.Cookies, []string{"cookies", "cookie"}},
		{&out.AccessToken, []string{"accesstoken", "token", "jwt", "bearertoken"}},
		{&out.RefreshToken, []string{"refreshtoken"}},
	} {
		if v := str(m, f.keys...); v != "" {
			*f.dst = v
		}
	}
	return out, nil
}

// decodeLoose 把对象反序列化成归一化 key 的 map。
func decodeLoose(raw json.RawMessage) (map[string]any, error) {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	norm := make(map[string]any, len(m))
	for k, v := range m {
		norm[normKey(k)] = v
	}
	return norm, nil
}

func normKey(k string) string {
	var sb strings.Builder
	sb.Grow(len(k))
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'A' && c <= 'Z':
			sb.WriteByte(c + 32)
		case c == '_' || c == '-' || c == ' ':
			// 丢弃分隔符
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func integer(m map[string]any, keys ...string) int {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i)
			}
		case float64:
			return int(n)
		case string:
			var i int
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
				return i
			}
		}
	}
	return 0
}

func number(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case json.Number:
			if f, err := n.Float64(); err == nil {
				return f
			}
		case float64:
			return n
		}
	}
	return 0
}

func boolp(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		return strings.EqualFold(b, "true") || b == "1", true
	}
	return false, false
}

func strs(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func strmap(m map[string]any, key string) map[string]string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	src, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, e := range src {
		if s, ok := e.(string); ok {
			out[k] = s
			continue
		}
		out[k] = fmt.Sprint(e)
	}
	return out
}
