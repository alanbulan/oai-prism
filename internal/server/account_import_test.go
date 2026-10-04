package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// 管理端导入：Dashboard 发 snake_case 字段、其它网关的导出文件把凭据放在嵌套的 credentials 里。
// 早先请求体直接反序列化进没有 json tag 的 config.AccountConfig，令牌被静默丢掉。

func fakeJWT(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "RS256"}) + "." + enc(claims) + ".c2ln"
}

type adminAccount struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Email      string   `json:"email"`
	Plan       string   `json:"plan"`
	HasToken   bool     `json:"has_access_token"`
	HasRefresh bool     `json:"has_refresh_token"`
	MaxConcur  int      `json:"max_concurrency"`
	Tags       []string `json:"tags"`
}

func adminAccounts(t *testing.T, base string) map[string]adminAccount {
	t.Helper()
	code, out := doLocal(t, http.MethodGet, base+"/admin/accounts", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/accounts %d", code)
	}
	var body struct {
		Accounts []adminAccount `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatal(err)
	}
	m := map[string]adminAccount{}
	for _, a := range body.Accounts {
		m[a.ID] = a
	}
	return m
}

func TestAdmin_ImportAccountFormats(t *testing.T) {
	ts, _ := newTestServer(t, &fakeUpstream{t: t}, goodAccount(), nil)
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	exp := time.Now().Add(240 * time.Hour).Unix()
	at := fakeJWT(map[string]any{"exp": exp, "https://api.openai.com/auth": map[string]any{
		"chatgpt_account_id": "acct-1", "chatgpt_plan_type": "plus"}})

	// 1. Dashboard 的写法：snake_case
	code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts",
		fmt.Sprintf(`[{"id":"dash-1","name":"面板导入","access_token":%q,"refresh_token":"rt.1.dash","max_concurrency":3}]`, at), jsonHdr)
	if code != http.StatusCreated {
		t.Fatalf("snake_case 导入 %d: %s", code, out)
	}
	a := adminAccounts(t, ts.URL)["dash-1"]
	if !a.HasToken || !a.HasRefresh || a.MaxConcur != 3 {
		t.Fatalf("snake_case 字段被丢了: %+v", a)
	}

	// 2. sub2api 的导出文件：凭据嵌套在 credentials 里，外层 concurrency 不照搬
	export := map[string]any{"exported_at": "2026-10-04T12:44:07Z", "proxies": []any{}, "accounts": []any{map[string]any{
		"name": "gpt-x", "platform": "openai", "type": "oauth", "concurrency": 10, "priority": 1,
		"credentials": map[string]any{"access_token": fakeJWT(map[string]any{"exp": exp}), "refresh_token": "rt.1.export",
			"email": "x@example.com", "plan_type": "plus", "client_id": "app_X", "chatgpt_account_id": "acct-2"},
		"extra": map[string]any{"codex_5h_used_percent": 45},
	}}}
	raw, _ := json.Marshal(export)
	code, out = doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", string(raw), jsonHdr)
	if code != http.StatusCreated {
		t.Fatalf("导出文件导入 %d: %s", code, out)
	}
	var created struct {
		IDs []string `json:"ids"`
	}
	_ = json.Unmarshal([]byte(out), &created)
	if len(created.IDs) != 1 {
		t.Fatalf("应返回新账号 id: %s", out)
	}
	id := created.IDs[0]
	x := adminAccounts(t, ts.URL)[id]
	if !x.HasToken || !x.HasRefresh || x.Name != "gpt-x" || x.Email != "x@example.com" || x.MaxConcur != 2 ||
		len(x.Tags) != 1 || x.Tags[0] != "oauth" {
		t.Fatalf("导出文件解析不对: %+v", x)
	}

	// 同一个号再导一次：覆盖原账号，不新增
	before := len(adminAccounts(t, ts.URL))
	code, _ = doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", string(raw), jsonHdr)
	if all := adminAccounts(t, ts.URL); code != http.StatusCreated || len(all) != before {
		t.Fatalf("重复导入应覆盖原账号: %d, %d → %d", code, before, len(all))
	}

	// 3. 没有凭据 / 别的平台：拒绝，不建空账号
	for _, bad := range []string{
		`{"name":"空账号","plan":"pro"}`,
		`{"accounts":[{"name":"c","platform":"anthropic","credentials":{"access_token":"x"}}]}`,
	} {
		before := len(adminAccounts(t, ts.URL))
		if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", bad, jsonHdr); code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，得到 %d: %s", bad, code, out)
		}
		if len(adminAccounts(t, ts.URL)) != before {
			t.Fatal("被拒的导入不应留下账号")
		}
	}

	// 4. 编辑只改给了的字段：标签保留，并发上限生效
	code, out = doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/"+id, `{"name":"改名","max_concurrency":5}`, jsonHdr)
	if code != http.StatusOK {
		t.Fatalf("PUT %d: %s", code, out)
	}
	x = adminAccounts(t, ts.URL)[id]
	if x.Name != "改名" || x.MaxConcur != 5 || len(x.Tags) != 1 || !x.HasRefresh || x.Email != "x@example.com" {
		t.Fatalf("部分更新不对: %+v", x)
	}
	if code, _ := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/nope", `{"name":"x"}`, jsonHdr); code != http.StatusNotFound {
		t.Fatalf("不存在的账号应 404，得到 %d", code)
	}
}
