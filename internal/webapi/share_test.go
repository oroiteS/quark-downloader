// share_test.go POST /api/share/parse 与 GET /api/share/files 的行为契约
// （Task 13）：口令如实提示、无链接 400、正常链建内存会话（sid 引用，
// stoken 不透出）、files 经 sid 导航、坏 sid 404、未登录 401。
// 全部离线：真 *protocol.Session + transport 重定向假上游（tasks_test.go
// 同基建）。
package webapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"quark-downloader/internal/protocol"
)

// shareTestUpstream parse/files 链的标准假上游：根目录一文件一目录，
// 子目录 d1 一文件；stoken 现换值固定。
func shareTestUpstream() *fakeUpstream {
	return &fakeUpstream{
		stoken: "st-fresh",
		share: map[string][]protocol.FileEntry{
			"0": {
				{FID: "f1", FileName: "a.mp4", Size: 100, Dir: false, ShareFIDToken: "tok-f1"},
				{FID: "d1", FileName: "dir1", Size: 0, Dir: true, ShareFIDToken: "tok-d1"},
			},
			"d1": {
				{FID: "f2", FileName: "b.mp4", Size: 200, Dir: false, ShareFIDToken: "tok-f2"},
			},
		},
	}
}

// parseResponse POST /api/share/parse 成功响应形状（stoken 不在其中）。
type parseResponse struct {
	SID      string               `json:"sid"`
	PwdID    string               `json:"pwd_id"`
	Passcode string               `json:"passcode"`
	Files    []protocol.FileEntry `json:"files"`
	Kouling  bool                 `json:"kouling"`
	Hint     string               `json:"hint"`
}

// TestShareParseKouling 口令文本 → 200 {kouling:true, hint}，不建会话
// 不打网络（brief 逐字：口令请改用分享链接）。
func TestShareParseKouling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := shareTestUpstream()
	app := newTasksTestApp(t, up, &fakeManager{}, taskTestSettings("/tmp/d"), taskTestPaths(t))

	w := doReq(t, app.Router(), http.MethodPost, "/api/share/parse",
		map[string]string{"text": "復制这条信息打开夸克网盘/~a1b2c3d4~:/"})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var pr parseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !pr.Kouling || pr.Hint != "口令请改用分享链接" {
		t.Fatalf("kouling=%v hint=%q, 期望 true 与逐字提示", pr.Kouling, pr.Hint)
	}
	if len(up.requests) != 0 {
		t.Fatalf("口令路径不应打网络, 实际 %d 个请求", len(up.requests))
	}
}

// TestShareParseNoLink 无链接文本 → 400 invalid_share_text。
func TestShareParseNoLink(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, shareTestUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))

	w := doReq(t, app.Router(), http.MethodPost, "/api/share/parse",
		map[string]string{"text": "今天天气不错"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", w.Code)
	}
	var body struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误体: %v", err)
	}
	if body.Error.Code != "invalid_share_text" {
		t.Fatalf("code = %q, 期望 invalid_share_text", body.Error.Code)
	}
}

// TestShareParseFullChain 正常链：链接+提取码 → ShareToken 现换 → 根目录
// 列表 → 200 {sid, pwd_id, passcode, files}；stoken 不透出；假上游收到
// 提取码。
func TestShareParseFullChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := shareTestUpstream()
	app := newTasksTestApp(t, up, &fakeManager{}, taskTestSettings("/tmp/d"), taskTestPaths(t))

	w := doReq(t, app.Router(), http.MethodPost, "/api/share/parse",
		map[string]string{"text": "https://pan.quark.cn/s/abc123 提取码：xp99"})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var pr parseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if pr.SID == "" {
		t.Fatal("sid 为空, 期望会话引用 id")
	}
	if pr.PwdID != "abc123" || pr.Passcode != "xp99" {
		t.Fatalf("pwd_id=%q passcode=%q, 期望 abc123/xp99", pr.PwdID, pr.Passcode)
	}
	if len(pr.Files) != 2 {
		t.Fatalf("根目录文件数 = %d, 期望 2", len(pr.Files))
	}
	// stoken 绝不透出（§6.2 安全取舍：内存会话引用）。
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if _, ok := raw["stoken"]; ok {
		t.Fatal("响应透出了 stoken 明文, 违反契约")
	}
	// 假上游的 token 请求带提取码与 pwd_id。
	found := false
	for _, r := range up.requests {
		if r.Path == "/1/clouddrive/share/sharepage/token" &&
			strings.Contains(r.Body, `"passcode":"xp99"`) &&
			strings.Contains(r.Body, `"pwd_id":"abc123"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("假上游未收到带提取码的 token 请求")
	}
}

// TestShareParseMixedKoulingWithLink 混合文本（口令标记+有效链接+提取码）
// → 正常解析链：有 PwdID 即解析，IsKouling 不再抢先（口令提示只留给
// 「仅口令、无链接可解析」的文本——brief 契约本意）。
func TestShareParseMixedKoulingWithLink(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := shareTestUpstream()
	app := newTasksTestApp(t, up, &fakeManager{}, taskTestSettings("/tmp/d"), taskTestPaths(t))

	w := doReq(t, app.Router(), http.MethodPost, "/api/share/parse",
		map[string]string{"text": "復制这条信息打开夸克网盘/~a1b2c3d4~:/ 链接：https://pan.quark.cn/s/abc123 提取码：xp99"})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var pr parseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if pr.Kouling {
		t.Fatal("混合文本含有效链接却被当口令拒绝（kouling=true）——PwdID 优先级应高于 IsKouling")
	}
	if pr.SID == "" {
		t.Fatal("sid 为空, 期望会话引用 id")
	}
	if pr.PwdID != "abc123" || pr.Passcode != "xp99" {
		t.Fatalf("pwd_id=%q passcode=%q, 期望 abc123/xp99", pr.PwdID, pr.Passcode)
	}
	if len(pr.Files) != 2 {
		t.Fatalf("根目录文件数 = %d, 期望 2", len(pr.Files))
	}
}

// TestShareFilesBySession files 经 sid 导航子目录（会话持有 stoken）。
func TestShareFilesBySession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := shareTestUpstream()
	app := newTasksTestApp(t, up, &fakeManager{}, taskTestSettings("/tmp/d"), taskTestPaths(t))

	pr := mustParse(t, app)
	w := doReq(t, app.Router(), http.MethodGet,
		"/api/share/files?sid="+pr.SID+"&pdir_fid=d1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var fr struct {
		Files []protocol.FileEntry `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fr); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(fr.Files) != 1 || fr.Files[0].FID != "f2" {
		t.Fatalf("子目录列表 = %+v, 期望 f2 一项", fr.Files)
	}
	// 会话请求必须带会话 stoken（假上游 query 断言）。
	found := false
	for _, r := range up.requests {
		if r.Path == "/1/clouddrive/share/sharepage/detail" &&
			strings.Contains(r.Query, "pdir_fid=d1") &&
			strings.Contains(r.Query, "stoken=st-fresh") {
			found = true
		}
	}
	if !found {
		t.Fatal("files 导航未携带会话 stoken")
	}
}

// TestShareFilesBadSID 坏 sid → 404 no_share_session。
func TestShareFilesBadSID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, shareTestUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))

	w := doReq(t, app.Router(), http.MethodGet,
		"/api/share/files?sid=nope&pdir_fid=0", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404", w.Code)
	}
	var body struct {
		Error struct{ Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != "no_share_session" {
		t.Fatalf("code = %q, 期望 no_share_session", body.Error.Code)
	}
}

// TestShareParseNotLoggedIn 未登录 → 401 not_logged_in（网络会话端点墙）。
func TestShareParseNotLoggedIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, shareTestUpstream(), &fakeManager{},
		taskTestSettings("/tmp/d"), taskTestPaths(t))
	app.resetSession() // 空 jar = 未登录

	w := doReq(t, app.Router(), http.MethodPost, "/api/share/parse",
		map[string]string{"text": "https://pan.quark.cn/s/abc123"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", w.Code)
	}
	var body struct {
		Error struct{ Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != "not_logged_in" {
		t.Fatalf("code = %q, 期望 not_logged_in", body.Error.Code)
	}
}

// mustParse 辅助：parse 成功并返回响应（多个用例的公共前置）。
func mustParse(t *testing.T, app *App) parseResponse {
	t.Helper()
	w := doReq(t, app.Router(), http.MethodPost, "/api/share/parse",
		map[string]string{"text": "https://pan.quark.cn/s/abc123 提取码：xp99"})
	if w.Code != http.StatusOK {
		t.Fatalf("parse 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	var pr parseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pr); err != nil {
		t.Fatalf("解析 parse 响应: %v", err)
	}
	return pr
}
