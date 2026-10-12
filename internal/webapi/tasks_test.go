// tasks_test.go 验证 POST /api/tasks 任务创建链（Task 9 / G11/E7/G12）：
// 目录递归展开（嵌套两层 + relDir 落位）、一文件一任务、预算分配
// （Σ≤effBudget 且小文件≥4，Review Focus #5）、同路径互斥 skipped 语义、
// makeRefresher 两模式（share 重取 stoken / drive 直取）、TaskCtx 落盘
// 字段（§6.2），以及 App.Close 对 manager.Close 的并发接线。
//
// 全部离线：协议面走真 *protocol.Session + transport 重定向到 httptest
// 假上游（sessionBase 私有无法注入，重定向是 webapi 包唯一接管点；
// 节流 260ms/请求 真实运转，测试请求数控制在个位数）；引擎面走
// fakeManager（taskManager 接口注入，直接断言 Submit 收到的档位值）。
package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"quark-downloader/internal/engine"
	"quark-downloader/internal/protocol"
	"quark-downloader/internal/store"
)

// ---- 注入 fake ---------------------------------------------------------------

// fakeManager taskManager 的假实现：记录每次 Submit 的完整参数（档位、
// Refresher 是否注入等），id 依次发 t1/t2/…；submitF 可程序化注入错误
// （如模拟同路径互斥拒绝）。
type fakeManager struct {
	mu      sync.Mutex
	submits []fakeSubmitCall
	nextID  int
	closed  int
	submitF func(call fakeSubmitCall) (string, error)
}

// fakeSubmitCall 一次 Submit 调用的完整快照。
type fakeSubmitCall struct {
	LR         protocol.LinkResult
	RelDir     string
	Cookie     string
	Mode       engine.Mode
	Threads    int
	HasRefresh bool
}

func (f *fakeManager) Submit(lr protocol.LinkResult, relDir, cookieStr string, mode engine.Mode, opts ...engine.SubmitOpt) (string, error) {
	call := fakeSubmitCall{LR: lr, RelDir: relDir, Cookie: cookieStr, Mode: mode}
	for _, o := range opts { // 与 engine.Submit 同语义：后者覆盖先者
		call.Threads = o.Threads
		if o.Refresher != nil {
			call.HasRefresh = true
		}
	}
	f.mu.Lock()
	f.submits = append(f.submits, call)
	f.nextID++
	id := fmt.Sprintf("t%d", f.nextID)
	fn := f.submitF
	f.mu.Unlock()
	if fn != nil {
		return fn(call)
	}
	return id, nil
}

func (f *fakeManager) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

func (f *fakeManager) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeManager) snapshot() []fakeSubmitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSubmitCall(nil), f.submits...)
}

// upstreamReq 假上游收到的一次请求（供断言取链 body 的免转存三参等）。
type upstreamReq struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// fakeUpstream 夸克 API 假上游：按路径前缀分发 file/sort、sharepage/detail、
// sharepage/token、file/download 四类接口；记录全部请求供断言。
type fakeUpstream struct {
	mu       sync.Mutex
	requests []upstreamReq
	sort     map[string][]protocol.FileEntry // drive: pdir_fid → 条目
	share    map[string][]protocol.FileEntry // share: pdir_fid → 条目
	stoken   string                          // sharepage/token 返回的新 stoken
	links    map[string]string               // download: fid → download_url
}

func (u *fakeUpstream) record(r *http.Request, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = append(u.requests, upstreamReq{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body,
	})
}

// dlBody download 请求 body 的解析形态（免转存三参断言用）。
type dlBody struct {
	FIDs      []string `json:"fids"`
	FIDsToken []string `json:"fids_token"`
	PwdID     string   `json:"pwd_id"`
	Stoken    string   `json:"stoken"`
}

func (u *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := ""
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err == nil {
			body = string(b)
		}
	}
	u.record(r, body)

	switch {
	case r.URL.Path == "/1/clouddrive/file/sort":
		u.writeList(w, u.sort[r.URL.Query().Get("pdir_fid")])
	case r.URL.Path == "/1/clouddrive/share/sharepage/detail":
		u.writeList(w, u.share[r.URL.Query().Get("pdir_fid")])
	case r.URL.Path == "/1/clouddrive/share/sharepage/token":
		u.mu.Lock()
		st := u.stoken
		u.mu.Unlock()
		fmt.Fprintf(w, `{"status":200,"code":0,"data":{"stoken":%q}}`, st)
	case r.URL.Path == "/1/clouddrive/file/download":
		var req dlBody
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		items := make([]map[string]any, 0, len(req.FIDs))
		for _, fid := range req.FIDs {
			url := u.links[fid]
			items = append(items, map[string]any{
				"fid": fid, "download_url": url, "file_name": fid + ".bin", "size": 1024,
			})
		}
		b, _ := json.Marshal(items)
		fmt.Fprintf(w, `{"status":200,"code":0,"data":%s}`, b)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (u *fakeUpstream) writeList(w http.ResponseWriter, entries []protocol.FileEntry) {
	if entries == nil {
		entries = []protocol.FileEntry{}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	b, _ := json.Marshal(entries)
	fmt.Fprintf(w, `{"status":200,"code":0,"data":{"list":%s,"metadata":{"_total":%d}}}`, b, len(entries))
}

// downloadRequests 返回全部 download 请求的解析后 body。
func (u *fakeUpstream) downloadRequests() []dlBody {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []dlBody
	for _, r := range u.requests {
		if r.Path != "/1/clouddrive/file/download" {
			continue
		}
		var b dlBody
		if json.Unmarshal([]byte(r.Body), &b) == nil {
			out = append(out, b)
		}
	}
	return out
}

// retargetTransport 把任意出站请求重定向到测试服务（真 *protocol.Session
// 的唯一接管点：URL 域名由私有 sessionBase 生成，这里在传输层改写目标）。
type retargetTransport struct{ host string }

func (t *retargetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = "http"
	u.Host = t.host
	r.URL = &u
	r.Host = ""
	return http.DefaultTransport.RoundTrip(r)
}

// newTasksTestApp 装配带真 session（指向假上游）+ fake manager 的 App。
// 带 session 的测试构造（交接：newAuthTestApp 的 session 为 nil 不可用，
// 参照 session_test.go 的 New+注入模式）。
func newTasksTestApp(t *testing.T, up *fakeUpstream, fm *fakeManager, settings store.Settings, paths store.Paths) *App {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: &retargetTransport{host: srv.Listener.Addr().String()}}

	fa := &fakeAuth{}
	app := New(Deps{Paths: paths, Settings: settings})
	app.newAuthPair = func(jar *protocol.CookieJar) (*protocol.Session, authAPI) {
		fa.mu.Lock()
		fa.jar = jar
		fa.mu.Unlock()
		return protocol.NewSession(client, jar), fa
	}
	app.resetSession() // 注入工厂生效（New 默认建的生产对弃用）
	app.manager = fm
	app.jar.LoadFromString("__puus=tok") // 进入登录态（__puus 唯一判据）
	return app
}

// taskTestPaths 指向临时目录的 Paths（TaskCtx 落盘断言用）。
func taskTestPaths(t *testing.T) store.Paths {
	t.Helper()
	dir := t.TempDir()
	return store.Paths{DataDir: dir, TasksFile: filepath.Join(dir, "tasks.json")}
}

// taskTestSettings 测试默认设置（显式给值——零值 Settings.Threads=0 会
// 退化预算档位）。
func taskTestSettings(destDir string) store.Settings {
	return store.Settings{DestDir: destDir, Threads: 32, MaxConcurrentTasks: 3}
}

// ---- ① 目录展开（嵌套 dir 两层，G11 后端负责）----

// TestCreateTasksExpandsNestedDirs drive 模式：fid 两层嵌套目录递归展开，
// 每个文件一个 Submit，relDir 保留子树内相对目录结构（顶层文件 ""，
// 两层深文件 "sub/deep"）；Submit 带 cookie、mode 与 Refresher。
func TestCreateTasksExpandsNestedDirs(t *testing.T) {
	up := &fakeUpstream{
		sort: map[string][]protocol.FileEntry{
			"d0": {
				{FID: "fa", FileName: "a.txt", Size: 100},
				{FID: "dsub", FileName: "sub", Size: 0, Dir: true},
			},
			"dsub": {
				{FID: "ddeep", FileName: "deep", Size: 0, Dir: true},
			},
			"ddeep": {
				{FID: "fz", FileName: "z.bin", Size: 900},
			},
		},
		links: map[string]string{
			"fa": "https://d.quark.cn/fa?auth_key=k&Expires=1&Signature=s",
			"fz": "https://d.quark.cn/fz?auth_key=k&Expires=1&Signature=s",
		},
	}
	fm := &fakeManager{}
	paths := taskTestPaths(t)
	app := newTasksTestApp(t, up, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

	w := doReq(t, app.Router(), http.MethodPost, "/api/tasks",
		map[string]any{"source": "drive", "fids": []string{"d0"}})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var body struct {
		TaskIDs []string `json:"task_ids"`
		Skipped []struct {
			FID    string `json:"fid"`
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, w.Body)
	}

	subs := fm.snapshot()
	if len(subs) != 2 {
		t.Fatalf("Submit 调用 %d 次, 期望 2（一文件一任务，目录不建任务）", len(subs))
	}
	if len(body.TaskIDs) != 2 || len(body.Skipped) != 0 {
		t.Fatalf("task_ids = %v, skipped = %v, 期望 2 个任务、0 跳过", body.TaskIDs, body.Skipped)
	}

	byName := map[string]fakeSubmitCall{}
	for _, s := range subs {
		byName[s.LR.FileName] = s
	}
	a, okA := byName["a.txt"]
	z, okZ := byName["z.bin"]
	if !okA || !okZ {
		t.Fatalf("提交的文件 = %v, 期望 a.txt 与 z.bin", byName)
	}
	if a.RelDir != "" {
		t.Fatalf("a.txt relDir = %q, 期望 \"\"（顶层文件）", a.RelDir)
	}
	if z.RelDir != "sub/deep" {
		t.Fatalf("z.bin relDir = %q, 期望 \"sub/deep\"（两层嵌套保留相对结构）", z.RelDir)
	}
	for _, s := range subs {
		if s.Cookie != "__puus=tok" {
			t.Fatalf("%s Submit cookie = %q, 期望当前 jar 整串", s.LR.FileName, s.Cookie)
		}
		if s.Mode != engine.ModeA {
			t.Fatalf("%s Submit mode = %d, 期望 ModeA（请求缺省）", s.LR.FileName, s.Mode)
		}
		if !s.HasRefresh {
			t.Fatalf("%s Submit 未注入 Refresher（G12 自愈回调）", s.LR.FileName)
		}
		if s.LR.URL == "" {
			t.Fatalf("%s Submit URL 为空（应带 DownloadLinks 取到的直链）", s.LR.FileName)
		}
	}
}

// ---- ② 一文件一任务 + relDir 落位（dest 前缀）----

// TestCreateTasksOneFileOneTaskRelDir share 模式：单文件 → 恰一个任务、
// relDir 落位 = dest 前缀 + 子树内相对路径；响应 task_ids 与 Submit 返回
// 一一对应。
func TestCreateTasksOneFileOneTaskRelDir(t *testing.T) {
	up := &fakeUpstream{
		share: map[string][]protocol.FileEntry{
			"d0": {
				{FID: "fmv", FileName: "movie.mkv", Size: 4096, ShareFIDToken: "tok-mv"},
			},
		},
		links: map[string]string{
			"fmv": "https://d.quark.cn/fmv?auth_key=k&Expires=1&Signature=s",
		},
	}
	fm := &fakeManager{}
	paths := taskTestPaths(t)
	app := newTasksTestApp(t, up, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

	w := doReq(t, app.Router(), http.MethodPost, "/api/tasks", map[string]any{
		"source": "share", "fids": []string{"d0"},
		"pwd_id": "pw1", "stoken": "st0", "dest": "Saved",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var body struct {
		TaskIDs []string `json:"task_ids"`
		Skipped []any    `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, w.Body)
	}

	subs := fm.snapshot()
	if len(subs) != 1 {
		t.Fatalf("Submit 调用 %d 次, 期望 1（一文件一任务）", len(subs))
	}
	if len(body.TaskIDs) != 1 || body.TaskIDs[0] != "t1" {
		t.Fatalf("task_ids = %v, 期望 [t1]", body.TaskIDs)
	}
	if len(body.Skipped) != 0 {
		t.Fatalf("skipped = %v, 期望空", body.Skipped)
	}
	if got := subs[0].RelDir; got != "Saved" {
		t.Fatalf("relDir = %q, 期望 \"Saved\"（dest 前缀落位）", got)
	}
	if got := subs[0].LR.FID; got != "fmv" {
		t.Fatalf("Submit fid = %q, 期望 fmv", got)
	}
	if got := subs[0].LR.Size; got != 4096 {
		t.Fatalf("Submit size = %d, 期望 4096", got)
	}

	// 免转存取链：download 请求须带 fids_token + pwd_id + stoken 三参。
	dls := up.downloadRequests()
	if len(dls) == 0 {
		t.Fatal("未发出 download 取链请求")
	}
	dl := dls[0]
	if len(dl.FIDsToken) != 1 || dl.FIDsToken[0] != "tok-mv" {
		t.Fatalf("download fids_token = %v, 期望 [tok-mv]（免转存三参）", dl.FIDsToken)
	}
	if dl.PwdID != "pw1" || dl.Stoken != "st0" {
		t.Fatalf("download pwd_id/stoken = %q/%q, 期望 pw1/st0", dl.PwdID, dl.Stoken)
	}
}

// ---- ③ 预算 Σ≤effBudget 且小文件≥4（Review Focus #5 / E7）----

// TestCreateTasksBudgetAllocation 5 个大小悬殊文件（cap=32、并发=3 →
// waves=2、effBudget=1024）：Submit 收到的档位与 AllocateBudget 对表一致，
// Σ≤effBudget 且每项 ≥ minPerFile(4)，大文件档位不低于小文件。
func TestCreateTasksBudgetAllocation(t *testing.T) {
	sizes := []struct {
		fid  string
		size int64
	}{
		{"f1", 100 << 30}, // 100GB
		{"f2", 50 << 30},
		{"f3", 1 << 30},
		{"f4", 1 << 20}, // 1MB
		{"f5", 100},     // 100B 小文件
	}
	entries := make([]protocol.FileEntry, 0, len(sizes))
	links := map[string]string{}
	for _, s := range sizes {
		entries = append(entries, protocol.FileEntry{FID: s.fid, FileName: s.fid + ".bin", Size: s.size})
		links[s.fid] = "https://d.quark.cn/" + s.fid + "?auth_key=k&Expires=1&Signature=s"
	}
	up := &fakeUpstream{
		sort:  map[string][]protocol.FileEntry{"d0": entries},
		links: links,
	}
	fm := &fakeManager{}
	paths := taskTestPaths(t)
	app := newTasksTestApp(t, up, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

	if w := doReq(t, app.Router(), http.MethodPost, "/api/tasks",
		map[string]any{"source": "drive", "fids": []string{"d0"}}); w.Code != 200 {
		t.Fatalf("状态码 = %d（body=%s）", w.Code, w.Body)
	}

	subs := fm.snapshot()
	if len(subs) != len(sizes) {
		t.Fatalf("Submit 调用 %d 次, 期望 %d", len(subs), len(sizes))
	}
	// 对表：webapi 传给 Submit 的档位 == AllocateBudget(sizes, cfgThreads, maxConcurrent)。
	var sizeSlice []int64
	got := make(map[string]int, len(subs))
	for _, s := range subs {
		sizeSlice = append(sizeSlice, s.LR.Size)
		got[s.LR.FID] = s.Threads
	}
	want := engine.AllocateBudget([]int64{100 << 30, 50 << 30, 1 << 30, 1 << 20, 100}, 32, 3)
	total := 0
	for i, s := range sizes {
		if got[s.fid] != want[i] {
			t.Fatalf("%s 档位 = %d, 期望 AllocateBudget 对表值 %d", s.fid, got[s.fid], want[i])
		}
		if got[s.fid] < 4 {
			t.Fatalf("%s 档位 = %d, 期望 ≥4（minPerFile，Focus #5）", s.fid, got[s.fid])
		}
		total += got[s.fid]
	}
	if total > 1024 { // effBudget = 512 × ⌈5/3⌉ = 1024
		t.Fatalf("Σthreads = %d, 期望 ≤1024（effBudget）", total)
	}
	if got["f1"] < got["f5"] {
		t.Fatalf("大文件档位 %d 低于小文件档位 %d（按大小比例分配）", got["f1"], got["f5"])
	}
}

// ---- ④ skipped 语义（§3.13 同路径互斥，不排队）----

// TestCreateTasksSkipsDuplicateDest 同名两文件（不同 fid）：第二个 Submit
// 被引擎以「同一文件正在下载中」拒绝 → 进 skipped 带该原因，第一个正常
// 建任务；落盘的 TaskCtx 只含成功项。
func TestCreateTasksSkipsDuplicateDest(t *testing.T) {
	up := &fakeUpstream{
		share: map[string][]protocol.FileEntry{
			"d0": {
				{FID: "fx", FileName: "dup.txt", Size: 10, ShareFIDToken: "tx"},
				{FID: "fy", FileName: "dup.txt", Size: 10, ShareFIDToken: "ty"},
			},
		},
		links: map[string]string{
			"fx": "https://d.quark.cn/fx?auth_key=k&Expires=1&Signature=s",
			"fy": "https://d.quark.cn/fy?auth_key=k&Expires=1&Signature=s",
		},
	}
	fm := &fakeManager{}
	fm.submitF = func(call fakeSubmitCall) (string, error) {
		if call.LR.FID == "fy" {
			return "", fmt.Errorf("feiwu: 同一文件正在下载中: %s", filepath.Join("dl", "dup.txt"))
		}
		return "t1", nil
	}
	paths := taskTestPaths(t)
	app := newTasksTestApp(t, up, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

	w := doReq(t, app.Router(), http.MethodPost, "/api/tasks", map[string]any{
		"source": "share", "fids": []string{"d0"}, "pwd_id": "pw", "stoken": "st",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（部分跳过不是整批失败；body=%s）", w.Code, w.Body)
	}
	var body struct {
		TaskIDs []string `json:"task_ids"`
		Skipped []struct {
			FID    string `json:"fid"`
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, w.Body)
	}
	if len(body.TaskIDs) != 1 || body.TaskIDs[0] != "t1" {
		t.Fatalf("task_ids = %v, 期望 [t1]", body.TaskIDs)
	}
	if len(body.Skipped) != 1 {
		t.Fatalf("skipped = %+v, 期望 1 项", body.Skipped)
	}
	sk := body.Skipped[0]
	if sk.FID != "fy" || sk.Name != "dup.txt" {
		t.Fatalf("skipped 条目 = {%s %s}, 期望 {fy dup.txt}", sk.FID, sk.Name)
	}
	if sk.Reason != "同一文件正在下载中" {
		t.Fatalf("skipped 原因 = %q, 期望 \"同一文件正在下载中\"（§3.13 文案）", sk.Reason)
	}

	if saved := store.LoadTasks(paths); len(saved) != 1 || saved[0].FID != "fx" {
		t.Fatalf("落盘 TaskCtx = %+v, 期望只含成功项 fx", saved)
	}
}

// ---- ⑤ Refresher 两模式（fake session / G12）----

// TestMakeRefresherShareAndDrive share：ShareToken(pwd_id, passcode) 重取
// stoken（失败即错）→ DownloadLinks 免转存三参（新 stoken）取该 fid 的
// URL；drive：DownloadLinks 只带 fids。一次一换语义由引擎保证，这里只验
// 产出新 URL。
func TestMakeRefresherShareAndDrive(t *testing.T) {
	t.Run("share 重取 stoken 后免转存三参取链", func(t *testing.T) {
		up := &fakeUpstream{
			stoken: "st-fresh",
			links:  map[string]string{"f1": "https://d.quark.cn/f1?auth_key=fresh&Expires=9&Signature=s"},
		}
		fm := &fakeManager{}
		paths := taskTestPaths(t)
		app := newTasksTestApp(t, up, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

		tc := store.TaskCtx{
			FromShare: true, PwdID: "pw1", Passcode: "pc1", Stoken: "st-old",
			FID: "f1", Token: "tok1", FileName: "a.bin", Size: 10,
		}
		url, err := app.makeRefresher(tc)(context.Background())
		if err != nil {
			t.Fatalf("refresher: %v", err)
		}
		if url != up.links["f1"] {
			t.Fatalf("新 URL = %q, 期望 %q", url, up.links["f1"])
		}
		dls := up.downloadRequests()
		if len(dls) != 3 { // 三入口各一次（免转存 entry=ft 优先）
			t.Fatalf("download 请求数 = %d, 期望 3（三入口）", len(dls))
		}
		for _, dl := range dls {
			if dl.Stoken != "st-fresh" {
				t.Fatalf("download stoken = %q, 期望 st-fresh（重取后的新 stoken）", dl.Stoken)
			}
			if len(dl.FIDsToken) != 1 || dl.FIDsToken[0] != "tok1" {
				t.Fatalf("download fids_token = %v, 期望 [tok1]", dl.FIDsToken)
			}
			if dl.PwdID != "pw1" {
				t.Fatalf("download pwd_id = %q, 期望 pw1", dl.PwdID)
			}
		}
	})
	t.Run("drive 只带 fids 取链", func(t *testing.T) {
		up := &fakeUpstream{
			links: map[string]string{"f9": "https://d.quark.cn/f9?auth_key=d&Expires=9&Signature=s"},
		}
		fm := &fakeManager{}
		paths := taskTestPaths(t)
		app := newTasksTestApp(t, up, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

		tc := store.TaskCtx{FromShare: false, FID: "f9", FileName: "b.bin", Size: 20}
		url, err := app.makeRefresher(tc)(context.Background())
		if err != nil {
			t.Fatalf("refresher: %v", err)
		}
		if url != up.links["f9"] {
			t.Fatalf("新 URL = %q, 期望 %q", url, up.links["f9"])
		}
		dls := up.downloadRequests()
		if len(dls) == 0 {
			t.Fatal("未发出 download 取链请求")
		}
		dl := dls[0]
		if len(dl.FIDs) != 1 || dl.FIDs[0] != "f9" {
			t.Fatalf("download fids = %v, 期望 [f9]", dl.FIDs)
		}
		if len(dl.FIDsToken) != 0 || dl.PwdID != "" || dl.Stoken != "" {
			t.Fatalf("download 免转存三参 = %v/%q/%q, 期望全空（drive 模式）",
				dl.FIDsToken, dl.PwdID, dl.Stoken)
		}
	})
	t.Run("ShareToken 失败即返回错误", func(t *testing.T) {
		up := &fakeUpstream{stoken: ""} // 空 stoken → 重取到的为空
		fm := &fakeManager{}
		paths := taskTestPaths(t)
		app := newTasksTestApp(t, up, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

		tc := store.TaskCtx{FromShare: true, PwdID: "pw", FID: "f1", Token: "t"}
		url, err := app.makeRefresher(tc)(context.Background())
		if err == nil {
			t.Fatalf("stoken 为空应报错, 得到 URL %q", url)
		}
	})
}

// ---- ⑥ TaskCtx 落盘字段（§6.2）----

// TestCreateTasksPersistsTaskCtx POST 成功后 tasks.json 逐字段断言：
// EngineID 对应 task_ids、分享上下文（PwdID/Stoken/Passcode/Token/
// FromShare）、RelDir（含 dest 前缀）、Dest=DestDir+relDir+FileName、
// Size、CreatedAt。
func TestCreateTasksPersistsTaskCtx(t *testing.T) {
	up := &fakeUpstream{
		share: map[string][]protocol.FileEntry{
			"d0": {
				{FID: "f1", FileName: "a.bin", Size: 777, ShareFIDToken: "tok1"},
				{FID: "dsub", FileName: "sub", Dir: true},
			},
			"dsub": {
				{FID: "f2", FileName: "b.bin", Size: 888, ShareFIDToken: "tok2"},
			},
		},
		links: map[string]string{
			"f1": "https://d.quark.cn/f1?auth_key=k&Expires=1&Signature=s",
			"f2": "https://d.quark.cn/f2?auth_key=k&Expires=1&Signature=s",
		},
	}
	fm := &fakeManager{}
	paths := taskTestPaths(t)
	settings := taskTestSettings(filepath.Join(paths.DataDir, "dl"))
	app := newTasksTestApp(t, up, fm, settings, paths)

	w := doReq(t, app.Router(), http.MethodPost, "/api/tasks", map[string]any{
		"source": "share", "fids": []string{"d0"},
		"pwd_id": "pw1", "stoken": "st0", "passcode": "pc1", "dest": "Saved",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}

	saved := store.LoadTasks(paths)
	if len(saved) != 2 {
		t.Fatalf("落盘 TaskCtx %d 条, 期望 2: %+v", len(saved), saved)
	}
	byFID := map[string]store.TaskCtx{}
	for _, tc := range saved {
		byFID[tc.FID] = tc
	}
	tc1, ok1 := byFID["f1"]
	tc2, ok2 := byFID["f2"]
	if !ok1 || !ok2 {
		t.Fatalf("落盘 fid = %v, 期望 f1/f2", byFID)
	}

	if tc1.EngineID != "t1" || tc2.EngineID != "t2" {
		t.Fatalf("EngineID = %q/%q, 期望 t1/t2（对应 task_ids）", tc1.EngineID, tc2.EngineID)
	}
	for _, tc := range []store.TaskCtx{tc1, tc2} {
		if !tc.FromShare || tc.PwdID != "pw1" || tc.Stoken != "st0" || tc.Passcode != "pc1" {
			t.Fatalf("%s 分享上下文 = {fromShare:%v pwd:%q stoken:%q passcode:%q}, 期望 G12 全持久化",
				tc.FID, tc.FromShare, tc.PwdID, tc.Stoken, tc.Passcode)
		}
		if tc.CreatedAt <= 0 {
			t.Fatalf("%s CreatedAt = %d, 期望 >0", tc.FID, tc.CreatedAt)
		}
	}
	if tc1.Token != "tok1" || tc2.Token != "tok2" {
		t.Fatalf("Token = %q/%q, 期望 tok1/tok2（share fid_token）", tc1.Token, tc2.Token)
	}
	if tc1.FileName != "a.bin" || tc1.RelDir != "Saved" || tc1.Size != 777 {
		t.Fatalf("f1 落盘 = {name:%q relDir:%q size:%d}, 期望 a.bin/Saved/777",
			tc1.FileName, tc1.RelDir, tc1.Size)
	}
	if tc2.RelDir != "Saved/sub" {
		t.Fatalf("f2 RelDir = %q, 期望 Saved/sub（dest 前缀+嵌套目录）", tc2.RelDir)
	}
	wantDest := filepath.Join(settings.DestDir, "Saved", "a.bin")
	if tc1.Dest != wantDest {
		t.Fatalf("f1 Dest = %q, 期望 %q（DestDir+relDir+FileName）", tc1.Dest, wantDest)
	}
}

// ---- ⑦ App.Close 对 manager.Close 的并发接线 ----

// TestAppCloseClosesManagerConcurrently Close 落地 manager.Close（E6 保留
// 断点）：http.Server RegisterOnShutdown 的 go f() 与主关闭链并发调用
// App.Close——不 panic、无数据竞争（-race 验证）、manager.Close 至少一次。
// 并发安全依据：engine.Manager.Close 的 closed 检查与设置同处 m.mu 临界
// 区（manager.go:699-710），重复/并发调用直接返回，App 层无需再包 once。
func TestAppCloseClosesManagerConcurrently(t *testing.T) {
	fm := &fakeManager{}
	paths := taskTestPaths(t)
	app := newTasksTestApp(t, &fakeUpstream{}, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // 并发关闭（模拟 OnShutdown go f() × 主链）
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.Close()
		}()
	}
	wg.Wait()

	if n := fm.closeCount(); n < 1 {
		t.Fatalf("manager.Close 调用 %d 次, 期望 ≥1", n)
	}
}

// ---- ⑧ 并发落盘互斥（fix round 1 发现 1）----

// TestCreateTasksConcurrentPersistsBoth 两个并发 POST 各建任务：TaskCtx
// 的读-改-写（Load+append+Save）必须绑成临界区——否则后写者覆盖前写者，
// 被覆盖方的任务上下文静默丢失（重启不续传、无 needs_reparse，违反
// 「绝不静默」红线）。gate 强制两 handler 的 Submit 同步放行，使紧随其
// 后的落盘段并发重叠（无互斥时表现为丢更新或并发 SaveTasks 争同一
// tmp 报 500）。
func TestCreateTasksConcurrentPersistsBoth(t *testing.T) {
	up := &fakeUpstream{
		sort: map[string][]protocol.FileEntry{
			"d0": {{FID: "fa", FileName: "a.bin", Size: 100}},
			"d1": {{FID: "fb", FileName: "b.bin", Size: 200}},
		},
		links: map[string]string{
			"fa": "https://d.quark.cn/fa?auth_key=k&Expires=1&Signature=s",
			"fb": "https://d.quark.cn/fb?auth_key=k&Expires=1&Signature=s",
		},
	}
	fm := &fakeManager{}
	arrived := make(chan struct{}, 16) // 各 handler 首个 Submit 到达信号
	release := make(chan struct{})
	fm.submitF = func(call fakeSubmitCall) (string, error) {
		arrived <- struct{}{}
		<-release
		return "t-" + call.LR.FID, nil
	}
	paths := taskTestPaths(t)
	app := newTasksTestApp(t, up, fm, taskTestSettings(filepath.Join(paths.DataDir, "dl")), paths)

	// 并发请求体构造（goroutine 内不触 t.Fatalf，响应由主 goroutine 断言）。
	post := func(fid string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"source": "drive", "fids": []string{fid}})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		app.Router().ServeHTTP(w, req)
		return w
	}

	const reqs = 2
	responses := make([]*httptest.ResponseRecorder, reqs)
	var wg sync.WaitGroup
	for i, fid := range []string{"d0", "d1"} {
		wg.Add(1)
		go func(i int, fid string) {
			defer wg.Done()
			responses[i] = post(fid)
		}(i, fid)
	}
	// 等两个 handler 都进入 Submit，再同时放行 → 落盘段并发开始。
	for i := 0; i < reqs; i++ {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("等待并发请求到达 Submit 超时（协议节流拖慢属预期，10s 应足够）")
		}
	}
	close(release)
	wg.Wait()

	for i, w := range responses {
		if w == nil || w.Code != http.StatusOK {
			body := "<nil>"
			if w != nil {
				body = w.Body.String()
			}
			t.Fatalf("请求 %d 状态码异常（body=%s）——并发落盘互斥失效", i, body)
		}
	}
	saved := store.LoadTasks(paths)
	if len(saved) != reqs {
		t.Fatalf("落盘 TaskCtx %d 条, 期望 %d（丢更新=覆盖丢失；got=%+v）", len(saved), reqs, saved)
	}
	got := map[string]bool{}
	for _, tc := range saved {
		got[tc.FID] = true
	}
	if !got["fa"] || !got["fb"] {
		t.Fatalf("落盘 fid = %v, 期望 fa 与 fb 都在（后写者不得覆盖前写者）", got)
	}
}

// ---- ⑨ 已存在完整文件跳过标注（fix round 1 发现 2 / G11 逐字）----

// TestCreateTasksSkipsExistingCompleteFile G11「目标已存在同名完整文件 →
// 跳过并在任务中标注『已存在』」：①size 相符 → 不 Submit、skipped 带
// reason「已存在」、TaskCtx 落盘 Exists=true 且 EngineID 空（T10 的
// GET /api/tasks 透出终态）；②size 不符（部分文件）→ 正常 Submit；
// ③列表 size≤0 → 保守不跳过（对账交给引擎）。
func TestCreateTasksSkipsExistingCompleteFile(t *testing.T) {
	// seedFile 在 dest 预置 n 字节文件（模拟已存在的下载产物）。
	seedFile := func(t *testing.T, settings store.Settings, name string, n int64) {
		t.Helper()
		if err := os.MkdirAll(settings.DestDir, 0o755); err != nil {
			t.Fatalf("预置目录: %v", err)
		}
		if err := os.WriteFile(filepath.Join(settings.DestDir, name), make([]byte, n), 0o644); err != nil {
			t.Fatalf("预置文件: %v", err)
		}
	}
	newCase := func(listSize int64) (*fakeUpstream, *fakeManager, store.Paths, store.Settings) {
		up := &fakeUpstream{
			share: map[string][]protocol.FileEntry{
				"d0": {{FID: "f1", FileName: "a.bin", Size: listSize, ShareFIDToken: "tok1"}},
			},
			links: map[string]string{"f1": "https://d.quark.cn/f1?auth_key=k&Expires=1&Signature=s"},
		}
		fm := &fakeManager{}
		paths := taskTestPaths(t)
		settings := taskTestSettings(filepath.Join(paths.DataDir, "dl"))
		return up, fm, paths, settings
	}
	post := func(app *App) *httptest.ResponseRecorder {
		return doReq(t, app.Router(), http.MethodPost, "/api/tasks", map[string]any{
			"source": "share", "fids": []string{"d0"}, "pwd_id": "pw", "stoken": "st",
		})
	}

	t.Run("同名完整文件跳过并标注已存在", func(t *testing.T) {
		up, fm, paths, settings := newCase(777)
		seedFile(t, settings, "a.bin", 777)
		app := newTasksTestApp(t, up, fm, settings, paths)

		w := post(app)
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
		}
		var body struct {
			TaskIDs []string `json:"task_ids"`
			Skipped []struct {
				FID    string `json:"fid"`
				Reason string `json:"reason"`
			} `json:"skipped"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, w.Body)
		}
		if len(body.TaskIDs) != 0 || len(fm.snapshot()) != 0 {
			t.Fatalf("task_ids = %v, Submit %d 次, 期望均 0（已存在不提交）", body.TaskIDs, len(fm.snapshot()))
		}
		if len(body.Skipped) != 1 || body.Skipped[0].FID != "f1" || body.Skipped[0].Reason != "已存在" {
			t.Fatalf("skipped = %+v, 期望 [{f1 已存在}]（与互斥拒绝文案区分）", body.Skipped)
		}
		saved := store.LoadTasks(paths)
		if len(saved) != 1 || saved[0].FID != "f1" {
			t.Fatalf("落盘 = %+v, 期望 f1 一条（跳过也落盘标注）", saved)
		}
		if !saved[0].Exists || saved[0].EngineID != "" {
			t.Fatalf("标注 = {Exists:%v EngineID:%q}, 期望 {true \"\"}（无引擎任务）", saved[0].Exists, saved[0].EngineID)
		}
	})

	t.Run("size 不符正常提交", func(t *testing.T) {
		up, fm, paths, settings := newCase(777)
		seedFile(t, settings, "a.bin", 100) // 部分文件（或同名异容）
		app := newTasksTestApp(t, up, fm, settings, paths)

		if w := post(app); w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
		}
		if n := len(fm.snapshot()); n != 1 {
			t.Fatalf("Submit %d 次, 期望 1（size 不符=非完整文件，不跳过）", n)
		}
		saved := store.LoadTasks(paths)
		if len(saved) != 1 || saved[0].Exists || saved[0].EngineID != "t1" {
			t.Fatalf("落盘 = %+v, 期望 {Exists:false EngineID:t1}", saved)
		}
	})

	t.Run("size 非正保守不跳过", func(t *testing.T) {
		up, fm, paths, settings := newCase(0) // 列表 size≤0：dest 判定不可信
		seedFile(t, settings, "a.bin", 1)     // 即使 dest 有同名文件
		app := newTasksTestApp(t, up, fm, settings, paths)

		if w := post(app); w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
		}
		if n := len(fm.snapshot()); n != 1 {
			t.Fatalf("Submit %d 次, 期望 1（size≤0 不做已存在判定，交给引擎对账）", n)
		}
		saved := store.LoadTasks(paths)
		if len(saved) != 1 || saved[0].Exists {
			t.Fatalf("落盘 = %+v, 期望 {Exists:false}（保守路径不标注）", saved)
		}
	})
}
