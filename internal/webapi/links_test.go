// links_test.go POST /api/links 与 POST /api/links/export 的行为契约
// （Task 13）：drive/share 两模式（share 三参从会话补齐）、打分透出+
// expires_hint、参数 400、导出五格式文本、空 items/未知格式 400、
// 未登录 401。离线：假上游 download 响应由 fakeUpstream 生成。
package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// linksDriveUpstream drive 模式假上游：两文件直链。
func linksDriveUpstream() *fakeUpstream {
	return &fakeUpstream{
		links: map[string]string{
			"fa": "https://dl.example.com/fa?entry=1",
			"fb": "https://dl.example.com/fb?entry=1",
		},
	}
}

// linksResponse POST /api/links 成功响应形状。
type linksResponse struct {
	Items []struct {
		FileName string `json:"file_name"`
		Size     int64  `json:"size"`
		URL      string `json:"url"`
		Score    int    `json:"score"`
	} `json:"items"`
	ExpiresHint string `json:"expires_hint"`
}

// TestLinksDriveMode drive：body 只有 fids（无免转存三参），items 带
// file_name/size/url/score，响应带 expires_hint。
func TestLinksDriveMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := linksDriveUpstream()
	app := newTasksTestApp(t, up, &fakeManager{}, taskTestSettings("/tmp/d"), taskTestPaths(t))

	w := doReq(t, app.Router(), http.MethodPost, "/api/links",
		map[string]any{"source": "drive", "fids": []string{"fa", "fb"}})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var lr linksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(lr.Items) != 2 {
		t.Fatalf("items 数 = %d, 期望 2", len(lr.Items))
	}
	if lr.Items[0].FileName != "fa.bin" || lr.Items[0].URL == "" {
		t.Fatalf("item[0] = %+v, 期望假上游回填的名称与 URL", lr.Items[0])
	}
	if lr.Items[0].Score == 0 && lr.Items[1].Score == 0 {
		t.Fatal("score 全 0, 期望透出打分（§2.9 ScoreLink 非零分）")
	}
	if lr.ExpiresHint == "" {
		t.Fatal("expires_hint 缺失（brief：直链有实效，响应带提示）")
	}
	// drive 模式 download body 只有 fids（dlBody 三参为空）。
	reqs := up.downloadRequests()
	if len(reqs) == 0 {
		t.Fatal("假上游未收到 download 请求")
	}
	if reqs[0].PwdID != "" || reqs[0].Stoken != "" || len(reqs[0].FIDsToken) != 0 {
		t.Fatalf("drive 模式不应带免转存三参: %+v", reqs[0])
	}
}

// TestLinksShareMode share：sid 会话补 pwd_id/stoken，fids_token 从会话
// token 缓存补齐（parse 根列表时并入）。
func TestLinksShareMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := shareTestUpstream()
	up.links = map[string]string{"f1": "https://dl.example.com/f1?entry=1"}
	app := newTasksTestApp(t, up, &fakeManager{}, taskTestSettings("/tmp/d"), taskTestPaths(t))

	pr := mustParse(t, app) // 根列表 f1(tok-f1)/d1(tok-d1) 并入 token 缓存
	w := doReq(t, app.Router(), http.MethodPost, "/api/links",
		map[string]any{"source": "share", "sid": pr.SID, "fids": []string{"f1"}})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var lr linksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(lr.Items) != 1 || lr.Items[0].URL == "" {
		t.Fatalf("items = %+v, 期望 f1 直链一项", lr.Items)
	}
	reqs := up.downloadRequests()
	if len(reqs) == 0 {
		t.Fatal("假上游未收到 download 请求")
	}
	r := reqs[0]
	if r.PwdID != "abc123" || r.Stoken != "st-fresh" {
		t.Fatalf("免转存 pwd_id/stoken 未从会话补齐: %+v", r)
	}
	if len(r.FIDsToken) != 1 || r.FIDsToken[0] != "tok-f1" {
		t.Fatalf("fids_token 未从会话缓存补齐: %+v", r.FIDsToken)
	}
}

// TestLinksShareBadSID share 模式坏 sid → 404 no_share_session。
func TestLinksShareBadSID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, linksDriveUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))

	w := doReq(t, app.Router(), http.MethodPost, "/api/links",
		map[string]any{"source": "share", "sid": "nope", "fids": []string{"f1"}})
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404", w.Code)
	}
}

// TestLinksInvalidRequest 空 fids / 未知 source → 400 invalid_links_request。
func TestLinksInvalidRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, linksDriveUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))
	r := app.Router()

	for _, body := range []map[string]any{
		{"source": "drive", "fids": []string{}},
		{"source": "mystery", "fids": []string{"fa"}},
	} {
		w := doReq(t, r, http.MethodPost, "/api/links", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%v 状态码 = %d, 期望 400", body, w.Code)
		}
		var eb struct {
			Error struct{ Code string }
		}
		_ = json.Unmarshal(w.Body.Bytes(), &eb)
		if eb.Error.Code != "invalid_links_request" {
			t.Fatalf("code = %q, 期望 invalid_links_request", eb.Error.Code)
		}
	}
}

// TestLinksNotLoggedIn 未登录 → 401。
func TestLinksNotLoggedIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, linksDriveUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))
	app.resetSession()

	w := doReq(t, app.Router(), http.MethodPost, "/api/links",
		map[string]any{"source": "drive", "fids": []string{"fa"}})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", w.Code)
	}
}

// TestLinksExportFormats 五格式各透出一段非空文本，且格式特征对得上
// （plain 纯 URL 行 / named 带 tab / aria2 含 aria2c / curl 含 curl /
// motrix 含【文件名】段）。
func TestLinksExportFormats(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, linksDriveUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))
	r := app.Router()

	mk := func(format string) *httptest.ResponseRecorder {
		return doReq(t, r, http.MethodPost, "/api/links/export", map[string]any{
			"items":  []map[string]string{{"rel_path": "dir/a.bin", "url": "https://dl.example.com/x"}},
			"format": format,
		})
	}
	cases := []struct {
		format, marker string
	}{
		{"plain", "https://dl.example.com/x"},
		{"named", "dir/a.bin\thttps://dl.example.com/x"},
		{"aria2", "aria2c"},
		{"curl", "curl"},
		{"motrix", "【文件名】a.bin"},
	}
	for _, tc := range cases {
		w := mk(tc.format)
		if w.Code != http.StatusOK {
			t.Fatalf("format=%s 状态码 = %d, 期望 200（body=%s）", tc.format, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), tc.marker) {
			t.Fatalf("format=%s 输出缺特征 %q（body=%s）", tc.format, tc.marker, w.Body)
		}
	}
}

// TestLinksExportInvalid 空 items / 未知格式 → 400 invalid_links_request。
func TestLinksExportInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, linksDriveUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))
	r := app.Router()

	w := doReq(t, r, http.MethodPost, "/api/links/export", map[string]any{
		"items": []map[string]string{}, "format": "plain"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 items 状态码 = %d, 期望 400", w.Code)
	}
	w = doReq(t, r, http.MethodPost, "/api/links/export", map[string]any{
		"items": []map[string]string{{"rel_path": "a", "url": "u"}}, "format": "txt"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知格式状态码 = %d, 期望 400", w.Code)
	}
	var eb struct {
		Error struct{ Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &eb)
	if eb.Error.Code != "invalid_links_request" {
		t.Fatalf("code = %q, 期望 invalid_links_request", eb.Error.Code)
	}
}

// TestLinksExportNotLoggedIn 未登录 → 401（导出需 cookie 串）。
func TestLinksExportNotLoggedIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, linksDriveUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))
	app.resetSession()

	w := doReq(t, app.Router(), http.MethodPost, "/api/links/export", map[string]any{
		"items":  []map[string]string{{"rel_path": "a", "url": "u"}},
		"format": "plain"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", w.Code)
	}
}
