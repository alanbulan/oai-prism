package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 账号的启用 / 停用与优先级（Dashboard 编辑）：停用的账号不进调度池，但列表里仍在、能启用回来。
func TestAdmin_AccountEnableAndPriority(t *testing.T) {
	ts, _, srv := newTestServerWithSrv(t, &fakeUpstream{t: t}, goodAccount(), nil)
	if srv.sqlite == nil {
		t.Skip("测试服务器没有 SQLite")
	}
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	at := fakeJWT(map[string]any{"exp": time.Now().Add(240 * time.Hour).Unix()})
	if code, out := doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", `{"id":"acc-x","access_token":"`+at+`"}`, jsonHdr); code != http.StatusCreated {
		t.Fatalf("导入失败 %d: %s", code, out)
	}

	type row struct {
		ID       string `json:"id"`
		Disabled bool   `json:"disabled"`
		State    string `json:"state"`
		Priority int    `json:"priority"`
	}
	list := func() map[string]row {
		t.Helper()
		_, out := doLocal(t, http.MethodGet, ts.URL+"/admin/accounts", "", nil)
		var body struct{ Accounts []row }
		if err := json.Unmarshal([]byte(out), &body); err != nil {
			t.Fatal(err)
		}
		m := map[string]row{}
		for _, a := range body.Accounts {
			m[a.ID] = a
		}
		return m
	}
	put := func(body string) {
		t.Helper()
		if code, out := doLocal(t, http.MethodPut, ts.URL+"/admin/accounts/acc-x", body, jsonHdr); code != http.StatusOK {
			t.Fatalf("PUT %s → %d: %s", body, code, out)
		}
	}

	if r := list()["acc-x"]; r.Disabled || r.State != "ok" {
		t.Fatalf("新导入的账号应启用: %+v", r)
	}
	put(`{"enabled":false}`)
	r, ok := list()["acc-x"]
	if !ok || !r.Disabled || r.State != "disabled" {
		t.Fatalf("停用后仍应出现在列表里并标为停用: %v %+v", ok, r)
	}
	for _, a := range srv.pool.Accounts() {
		if a.ID == "acc-x" {
			t.Fatal("停用的账号不应留在调度池里")
		}
	}

	put(`{"enabled":true,"priority":8}`)
	if r := list()["acc-x"]; r.Disabled || r.Priority != 8 {
		t.Fatalf("应启用回来并带上优先级: %+v", r)
	}
	if first := srv.pool.Accounts()[0].ID; first != "acc-x" {
		t.Fatalf("优先级最高的账号应排在调度池最前: %s", first)
	}

	// 重新导入同一个账号（导入文件不带优先级与启用状态）：沿用原来的
	put(`{"enabled":false}`)
	doLocal(t, http.MethodPost, ts.URL+"/admin/accounts", `{"id":"acc-x","access_token":"`+at+`"}`, jsonHdr)
	if r := list()["acc-x"]; !r.Disabled || r.Priority != 8 {
		t.Fatalf("重新导入不应改掉启用状态与优先级: %+v", r)
	}
}

// OAuth 授权完成后（回调由本地监听器处理）：控制台按会话 ID 仍能查到结果，手动粘贴回调也直接给出结果。
// 早先完成即删会话：回调页显示授权成功，控制台却一直转圈，粘贴回调地址报会话已过期。
func TestOAuth_SessionResultAfterCallback(t *testing.T) {
	s := &Server{}
	sess := &oauthSession{ID: "oa_test1", State: "st-1", ClientID: "c-test-1", CreatedAt: time.Now()}
	oauthSessions.put(sess)
	s.finishOAuthSession(sess, "oauth-abcd", nil)

	rec := httptest.NewRecorder()
	s.handleOAuthStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/oauth/status?session_id=oa_test1", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"success"`) || !strings.Contains(rec.Body.String(), "oauth-abcd") {
		t.Fatalf("完成后应能查到成功结果: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	s.handleOAuthExchange(rec, httptest.NewRequest(http.MethodPost, "/admin/oauth/exchange",
		strings.NewReader(`{"session_id":"oa_test1","callback":"http://localhost:1455/auth/callback?code=x&state=st-1"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "oauth-abcd") {
		t.Fatalf("已完成的授权再粘贴回调应直接给出结果: %d %s", rec.Code, rec.Body.String())
	}

	// 同一个回调不能再用一次（state 已摘掉）
	if oauthSessions.getByState("st-1") != nil {
		t.Fatal("完成后 state 不应再能认领会话")
	}

	// 失败的授权：查得到原因，手动粘贴给出明确提示
	bad := &oauthSession{ID: "oa_test2", State: "st-2", ClientID: "c-test-2", CreatedAt: time.Now()}
	oauthSessions.put(bad)
	s.finishOAuthSession(bad, "", errTest("invalid_grant"))
	rec = httptest.NewRecorder()
	s.handleOAuthStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/oauth/status?session_id=oa_test2", nil))
	if !strings.Contains(rec.Body.String(), `"status":"error"`) || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("失败的授权应查得到原因: %s", rec.Body.String())
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
