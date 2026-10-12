// resume_test.go 验证重启自动续传链（Task 11 / D9/G12）：restoreTasks
// 读回 tasks.json 逐项恢复——drive 直续（Submit 参数/EngineID 换新）、
// share 重取 stoken 成功续（token 端点 + 免转存三参）、重取失败如实标
// needs_reparse（Review Focus #3 红线：绝不静默——恢复失败在
// GET /api/tasks 必须可见）、CanRefresh=false 同样如实标注、磁盘已
// 完整文件跳过（G11 同语义）、恢复计数写日志；断点复用集成测试用
// 真引擎 + httptest Range server 断言「从断点续传而非重下」。
//
// 全部离线：协议面真 *protocol.Session + transport 重定向假上游（与
// tasks_test.go 同模式）；滴漏 server 遵循「最后一块后不 sleep」节奏
// （收尾尾巴会系统性抬高观测值）。
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"quark-downloader/internal/engine"
	"quark-downloader/internal/protocol"
	"quark-downloader/internal/store"
)

// newResumeApp 装配带真 session（指向假上游）+ 任意 taskManager（fake
// 或真引擎）的 App——与 newTasksTestApp 同模式，但 manager 面向接口
// （断点复用集成测试注入真 *engine.Manager）。
func newResumeApp(t *testing.T, up *fakeUpstream, m taskManager, settings store.Settings, paths store.Paths) *App {
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
	app.resetSession()
	app.manager = m
	app.jar.LoadFromString("__puus=tok") // 进入登录态（与 tasks_test.go 同款）
	return app
}

// mustSaveTasks 落盘测试前置的 TaskCtx 集（失败即 Fatal）。
func mustSaveTasks(t *testing.T, paths store.Paths, ts []store.TaskCtx) {
	t.Helper()
	if err := store.SaveTasks(paths, ts); err != nil {
		t.Fatalf("落盘 TaskCtx: %v", err)
	}
}

// logCapture 捕获 slog 默认日志（恢复计数断言用）。安装后必须
// t.Cleanup 恢复默认 logger，避免污染同包其他测试。
type logCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}

func (c *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(name string) slog.Handler       { return c }

// findMessage 返回首条消息含 sub 的记录（无则 nil）。
func (c *logCapture) findMessage(sub string) *slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.records {
		if strings.Contains(c.records[i].Message, sub) {
			return &c.records[i]
		}
	}
	return nil
}

// attrString 记录里取字符串属性值（记录或键缺省时空串）。
func attrString(r *slog.Record, key string) string {
	var out string
	if r == nil {
		return out
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
		}
		return true
	})
	return out
}

// ---- ① drive 直续 ----

// TestRestoreTasksDriveResumes drive 条目：CanRefresh=true → refresher
// 取新链（假上游 download 端点）→ Submit（URL/档位=settings.Threads/
// mode 读回/Refresher 注入）→ TaskCtx.EngineID 换新落盘。
func TestRestoreTasksDriveResumes(t *testing.T) {
	paths := taskTestPaths(t)
	dest := filepath.Join(paths.DataDir, "dl")
	settings := taskTestSettings(dest)
	fm := &fakeManager{}
	up := &fakeUpstream{links: map[string]string{"fd1": "http://dl.example/fd1.bin"}}
	app := newResumeApp(t, up, fm, settings, paths)
	mustSaveTasks(t, paths, []store.TaskCtx{
		{EngineID: "old1", FileName: "a.bin", FID: "fd1", RelDir: "sub",
			Dest: filepath.Join(dest, "sub", "a.bin"), Size: 100, Mode: "b", CreatedAt: 1},
	})

	if err := app.restoreTasks(context.Background()); err != nil {
		t.Fatalf("restoreTasks: %v", err)
	}

	calls := fm.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Submit 调用数 = %d, 期望 1（got=%+v）", len(calls), calls)
	}
	c := calls[0]
	if c.LR.FID != "fd1" || c.LR.FileName != "a.bin" || c.LR.Size != 100 {
		t.Fatalf("Submit LinkResult = %+v", c.LR)
	}
	if c.LR.URL != "http://dl.example/fd1.bin" {
		t.Fatalf("Submit URL = %q, 期望假上游新链", c.LR.URL)
	}
	if c.RelDir != "sub" || c.Mode != engine.ModeB {
		t.Fatalf("Submit relDir/mode = %q/%v（期望 sub/ModeB——Mode 读回）", c.RelDir, c.Mode)
	}
	if c.Threads != settings.Threads {
		t.Fatalf("Submit Threads = %d, 期望 settings.Threads=%d（单任务恢复无批量预算）", c.Threads, settings.Threads)
	}
	if !c.HasRefresh {
		t.Fatal("Submit 未注入 Refresher（自愈复活）")
	}

	got := store.LoadTasks(paths)
	if len(got) != 1 {
		t.Fatalf("恢复后条目数 = %d, 期望 1", len(got))
	}
	if got[0].EngineID == "" || got[0].EngineID == "old1" {
		t.Fatalf("EngineID = %q, 期望换新（非空且非旧值）", got[0].EngineID)
	}
	if got[0].NeedsReparse {
		t.Fatal("恢复成功的条目不应标 NeedsReparse")
	}
}

// ---- ② share 重取 stoken 成功续 ----

// TestRestoreTasksShareRetokenResumes share 条目（stoken 已过期置空、
// pwd_id+passcode 齐）：恢复先打 sharepage/token 重取 stoken，再以免转存
// 三参（pwd_id/stoken/fids_token）取新链 Submit。
func TestRestoreTasksShareRetokenResumes(t *testing.T) {
	paths := taskTestPaths(t)
	dest := filepath.Join(paths.DataDir, "dl")
	settings := taskTestSettings(dest)
	fm := &fakeManager{}
	up := &fakeUpstream{
		stoken: "fresh-st",
		links:  map[string]string{"fs1": "http://dl.example/fs1.bin"},
	}
	app := newResumeApp(t, up, fm, settings, paths)
	mustSaveTasks(t, paths, []store.TaskCtx{
		{EngineID: "old2", FileName: "s.bin", FID: "fs1", Token: "tok-s1",
			FromShare: true, PwdID: "pw1", Passcode: "pc1", Stoken: "", // stoken 过期（G12 场景）
			Dest: filepath.Join(dest, "s.bin"), Size: 200, Mode: "a", CreatedAt: 1},
	})

	if err := app.restoreTasks(context.Background()); err != nil {
		t.Fatalf("restoreTasks: %v", err)
	}

	if calls := fm.snapshot(); len(calls) != 1 || calls[0].LR.URL != "http://dl.example/fs1.bin" {
		t.Fatalf("Submit = %+v（期望 1 次且带新链）", calls)
	}
	dls := up.downloadRequests()
	if len(dls) == 0 {
		t.Fatal("假上游未收到 download 请求（重取链未发生）")
	}
	last := dls[len(dls)-1]
	if last.PwdID != "pw1" || last.Stoken != "fresh-st" {
		t.Fatalf("免转存三参 pwd_id/stoken = %q/%q, 期望 pw1/fresh-st（重取的 stoken）", last.PwdID, last.Stoken)
	}
	if len(last.FIDsToken) != 1 || last.FIDsToken[0] != "tok-s1" {
		t.Fatalf("fids_token = %v, 期望 [tok-s1]", last.FIDsToken)
	}
	got := store.LoadTasks(paths)
	if len(got) != 1 || got[0].EngineID == "" || got[0].EngineID == "old2" {
		t.Fatalf("恢复后 EngineID = %+v（期望换新）", got)
	}
}

// ---- ③ 重取失败如实标 needs_reparse（Review Focus #3 红线）----

// TestRestoreTasksShareFailureMarksNeedsReparse share 条目 stoken 重取
// 失败（假上游返回空 stoken）：不 Submit、NeedsReparse=true 落盘、
// GET /api/tasks 透出 needs_reparse——磁盘无完整文件而显示「完成」
// 正是红线要防的静默失败形态。
func TestRestoreTasksShareFailureMarksNeedsReparse(t *testing.T) {
	paths := taskTestPaths(t)
	dest := filepath.Join(paths.DataDir, "dl")
	settings := taskTestSettings(dest)
	fm := &fakeManager{}
	up := &fakeUpstream{stoken: ""} // token 端点返回空 → 重取失败
	app := newResumeApp(t, up, fm, settings, paths)
	mustSaveTasks(t, paths, []store.TaskCtx{
		{EngineID: "old3", FileName: "x.bin", FID: "fs2", Token: "tok-s2",
			FromShare: true, PwdID: "pw2", Passcode: "pc2",
			Dest: filepath.Join(dest, "x.bin"), Size: 300, Mode: "a", CreatedAt: 1},
	})

	if err := app.restoreTasks(context.Background()); err != nil {
		t.Fatalf("restoreTasks: %v（单条恢复失败不该挡启动）", err)
	}
	if calls := fm.snapshot(); len(calls) != 0 {
		t.Fatalf("重取失败仍 Submit = %+v", calls)
	}
	got := store.LoadTasks(paths)
	if len(got) != 1 || !got[0].NeedsReparse || got[0].EngineID != "" {
		t.Fatalf("恢复后条目 = %+v（期望 NeedsReparse=true、EngineID 清空）", got)
	}

	// 红线断言：列表必须可见 needs_reparse（绝不静默）。
	w := doReq(t, app.Router(), http.MethodGet, "/api/tasks", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/tasks = %d（body=%s）", w.Code, w.Body)
	}
	var list []taskDTO
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表响应解析: %v", err)
	}
	if len(list) != 1 || list[0].State != "needs_reparse" || !list[0].NeedsReparse {
		t.Fatalf("列表条目 = %+v（期望 needs_reparse 如实透出）", list)
	}
	if list[0].ID != "ctx:fs2" {
		t.Fatalf("needs_reparse 条目 id = %q, 期望 ctx:fs2", list[0].ID)
	}
}

// ---- ④ CanRefresh=false 如实标注 ----

// TestRestoreTasksNoRefreshContextMarksNeedsReparse 上下文不足以重取
// （share 缺全部凭证）：不碰网络、不 Submit、NeedsReparse=true 落盘
// （brief 逐字「如实」；Ruling T11-R1）。
func TestRestoreTasksNoRefreshContextMarksNeedsReparse(t *testing.T) {
	paths := taskTestPaths(t)
	dest := filepath.Join(paths.DataDir, "dl")
	settings := taskTestSettings(dest)
	fm := &fakeManager{}
	up := &fakeUpstream{}
	app := newResumeApp(t, up, fm, settings, paths)
	mustSaveTasks(t, paths, []store.TaskCtx{
		{EngineID: "old4", FileName: "z.bin", FID: "fz", FromShare: true, // 无 pwd/passcode/token/stoken
			Dest: filepath.Join(dest, "z.bin"), Size: 400, Mode: "a", CreatedAt: 1},
	})

	if err := app.restoreTasks(context.Background()); err != nil {
		t.Fatalf("restoreTasks: %v", err)
	}
	if calls := fm.snapshot(); len(calls) != 0 {
		t.Fatalf("不可重取仍 Submit = %+v", calls)
	}
	if got := up.downloadRequests(); len(got) != 0 {
		t.Fatalf("不可重取仍发网络请求 = %+v", got)
	}
	got := store.LoadTasks(paths)
	if len(got) != 1 || !got[0].NeedsReparse || got[0].EngineID != "" {
		t.Fatalf("恢复后条目 = %+v（期望 NeedsReparse=true、EngineID 清空）", got)
	}
}

// ---- ⑤ 磁盘已完整文件跳过（G11 同语义，恢复路径变体）----

// TestRestoreTasksSkipsExistingCompleteFile dest 已存在同名完整文件
// （size 相符且**无 .qmeta 断点凭证**）：不 Submit、不取链、TaskCtx 标
// Exists=true 终态。对照条目：同样完整但有 .qmeta（模式 A 预分配断点
// 文件）→ 不误判，交引擎对账（Submit 发生）。
func TestRestoreTasksSkipsExistingCompleteFile(t *testing.T) {
	paths := taskTestPaths(t)
	dest := filepath.Join(paths.DataDir, "dl")
	settings := taskTestSettings(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("complete-file-bytes")
	if err := os.WriteFile(filepath.Join(dest, "e.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// 对照：bp.bin 同样「完整大小」但带 .qmeta——模式 A 预分配断点，
	// size 相符是假象，必须交引擎对账而非标已存在。
	if err := os.WriteFile(filepath.Join(dest, "bp.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "bp.bin.qmeta"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm := &fakeManager{}
	up := &fakeUpstream{links: map[string]string{
		"fe1": "http://dl.example/e.bin", "fbp": "http://dl.example/bp.bin"}}
	app := newResumeApp(t, up, fm, settings, paths)
	mustSaveTasks(t, paths, []store.TaskCtx{
		{EngineID: "old5", FileName: "e.bin", FID: "fe1",
			Dest: filepath.Join(dest, "e.bin"), Size: int64(len(data)), Mode: "a", CreatedAt: 1},
		{EngineID: "old6", FileName: "bp.bin", FID: "fbp",
			Dest: filepath.Join(dest, "bp.bin"), Size: int64(len(data)), Mode: "a", CreatedAt: 1},
	})

	if err := app.restoreTasks(context.Background()); err != nil {
		t.Fatalf("restoreTasks: %v", err)
	}
	calls := fm.snapshot()
	if len(calls) != 1 || calls[0].LR.FID != "fbp" {
		t.Fatalf("Submit = %+v（期望仅有 .qmeta 的 bp 条目走引擎）", calls)
	}
	got := store.LoadTasks(paths)
	if len(got) != 2 {
		t.Fatalf("恢复后条目数 = %d, 期望 2", len(got))
	}
	byFID := map[string]store.TaskCtx{}
	for _, tc := range got {
		byFID[tc.FID] = tc
	}
	if e := byFID["fe1"]; !e.Exists || e.EngineID != "" {
		t.Fatalf("无凭证完整条目 = %+v（期望 Exists=true、EngineID 清空）", e)
	}
	if bp := byFID["fbp"]; bp.Exists || bp.EngineID == "" || bp.NeedsReparse {
		t.Fatalf("有 .qmeta 条目 = %+v（期望交引擎恢复成功，不误判已存在）", bp)
	}
}

// ---- ⑥ 恢复计数日志 ----

// TestRestoreTasksLogsCounts 混合四态条目（恢复成功/重取失败/不可重取/
// 已存在）一条龙：恢复结束后 slog Info 写明各计数。
func TestRestoreTasksLogsCounts(t *testing.T) {
	paths := taskTestPaths(t)
	dest := filepath.Join(paths.DataDir, "dl")
	settings := taskTestSettings(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "ok-existing.bin"), []byte("full"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm := &fakeManager{}
	up := &fakeUpstream{
		stoken: "",
		links:  map[string]string{"fok": "http://dl.example/ok.bin", "fex": "http://dl.example/ex.bin"},
	}
	app := newResumeApp(t, up, fm, settings, paths)
	mustSaveTasks(t, paths, []store.TaskCtx{
		{EngineID: "o1", FileName: "ok.bin", FID: "fok", // → 恢复成功
			Dest: filepath.Join(dest, "ok.bin"), Size: 10, Mode: "a", CreatedAt: 1},
		{EngineID: "o2", FileName: "fail.bin", FID: "ff", Token: "tf", FromShare: true,
			PwdID: "pw", Passcode: "pc", // → stoken 重取失败（stoken=""）
			Dest: filepath.Join(dest, "fail.bin"), Size: 20, Mode: "a", CreatedAt: 1},
		{EngineID: "o3", FileName: "norefresh.bin", FID: "fn", FromShare: true, // → CanRefresh=false
			Dest: filepath.Join(dest, "norefresh.bin"), Size: 30, Mode: "a", CreatedAt: 1},
		{EngineID: "o4", FileName: "ok-existing.bin", FID: "fex", // → 磁盘已完整 → Exists
			Dest: filepath.Join(dest, "ok-existing.bin"), Size: 4, Mode: "a", CreatedAt: 1},
	})

	lc := &logCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(lc))
	t.Cleanup(func() { slog.SetDefault(old) })

	if err := app.restoreTasks(context.Background()); err != nil {
		t.Fatalf("restoreTasks: %v", err)
	}

	rec := lc.findMessage("恢复任务完成")
	if rec == nil {
		t.Fatal("未找到恢复计数日志（期望「恢复任务…」Info 一条）")
	}
	if got := attrString(rec, "resumed"); got != "1" {
		t.Fatalf("resumed 计数 = %q, 期望 1（rec=%v）", got, rec)
	}
	if got := attrString(rec, "needs_reparse"); got != "2" {
		t.Fatalf("needs_reparse 计数 = %q, 期望 2（重取失败+不可重取）", got)
	}
	if got := attrString(rec, "exists"); got != "1" {
		t.Fatalf("exists 计数 = %q, 期望 1", got)
	}
}

// ---- ⑦ 断点复用集成（真引擎 + httptest Range server）----

// resumeRangeServer 滴灌 Range server：记录每个请求区间与累计发送字节
// （断点复用断言——第二段传输量必须远小于全量，证明从断点续传）。
// 块间隔滴灌、**最后一块后不 sleep**（收尾尾巴会抬高观测值）。
type resumeRangeServer struct {
	mu     sync.Mutex
	ranges [][2]int64
	sent   int64
}

func (s *resumeRangeServer) add(a, b, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ranges = append(s.ranges, [2]int64{a, b})
	s.sent += n
}

// snapshot 返回累计请求区间与发送字节。
func (s *resumeRangeServer) snapshot() ([][2]int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]int64(nil), s.ranges...), s.sent
}

// resumePattern 确定性测试数据（与位置相关，防错位拼接假绿）。
func resumePattern(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/251)
	}
	return b
}

// waitEngineState 轮询引擎任务至目标状态（超时 Fatal）。
func waitEngineState(t *testing.T, m *engine.Manager, id, state string, timeout time.Duration) engine.Progress {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p, err := m.Get(id)
		if err == nil && p.State == state {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	p, _ := m.Get(id)
	t.Fatalf("任务 %s 未在 %v 内到 %q（终态快照=%+v）", id, timeout, state, p)
	return p
}

// mustEqualFile 断言文件内容与期望逐字节一致。
func mustEqualFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读结果文件: %v", err)
	}
	if int64(len(got)) != int64(len(want)) {
		t.Fatalf("文件大小 = %d, 期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("文件字节 [%d] = %d, 期望 %d（内容不一致）", i, got[i], want[i])
		}
	}
}

// TestRestoreTasksBreakpointReuse 断点复用（G4：引擎对账 total 吻合即
// 续传）：第一段真引擎下载到 ≥1/4 后 Close（E6 保留断点）→ 模拟重启
// （新引擎 + restoreTasks 走假上游新链）→ 断言最终文件完整、且第二段
// 服务器传输量 < 全量 3/4（重下则 = 全量）——断点被读回续传而非重下。
func TestRestoreTasksBreakpointReuse(t *testing.T) {
	total := int64(2 << 20) // 2MB
	data := resumePattern(total)
	const block = int64(32 << 10)
	const drip = 8 * time.Millisecond

	rec := &resumeRangeServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Errorf("畸形 Range 头 %q: %v", r.Header.Get("Range"), err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, total))
		w.WriteHeader(http.StatusPartialContent)
		written := int64(0)
		for s := a; s <= b; s += block {
			e := min(s+block-1, b)
			if _, err := w.Write(data[s : e+1]); err != nil {
				return // 客户端取消（Close 停止在飞请求）
			}
			written += e - s + 1
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if e < b {
				time.Sleep(drip) // 块间隔滴灌；最后一块后不睡
			}
		}
		rec.add(a, b, written)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()

	// 第一段：真引擎直下到部分完成，Close 保留断点。
	m1 := engine.NewManager(engine.Config{Threads: 4, Mode: engine.ModeA, MaxConcurrentTasks: 2, DestDir: dir}, nil)
	id1, err := m1.Submit(protocol.LinkResult{FID: "bp", FileName: "bp.bin", Size: total, URL: srv.URL}, "", "", engine.ModeA)
	if err != nil {
		t.Fatalf("第一段 Submit: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var done1 int64
	for time.Now().Before(deadline) {
		p, gerr := m1.Get(id1)
		if gerr == nil {
			done1 = p.Done
			if done1 >= total/4 {
				break
			}
			if p.State == "done" {
				t.Fatal("第一段已跑完全量——测试节奏失控，加大 total 或滴灌时长")
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	if done1 < total/4 {
		t.Fatalf("第一段进度 = %d, 期望 ≥ %d", done1, total/4)
	}
	m1.Close() // E6：优雅停止保留断点（.qmeta/分片在盘）
	_, baseSent := rec.snapshot()

	// 模拟重启：tasks.json 落盘该任务上下文 + 新引擎 + 假上游新链。
	paths := taskTestPaths(t)
	settings := taskTestSettings(dir)
	up := &fakeUpstream{links: map[string]string{"bp": srv.URL}} // 重启后 refresher 取的「新」直链
	m2 := engine.NewManager(engine.Config{Threads: 4, Mode: engine.ModeA, MaxConcurrentTasks: 2, DestDir: dir}, nil)
	t.Cleanup(m2.Close)
	app := newResumeApp(t, up, m2, settings, paths)
	mustSaveTasks(t, paths, []store.TaskCtx{
		{EngineID: id1, FileName: "bp.bin", FID: "bp", RelDir: "",
			Dest: filepath.Join(dir, "bp.bin"), Size: total, Mode: "a", CreatedAt: time.Now().Unix()},
	})

	if err := app.restoreTasks(context.Background()); err != nil {
		t.Fatalf("restoreTasks: %v", err)
	}
	got := store.LoadTasks(paths)
	// 两代引擎各自从 t1 起编号，id 值可能巧合相同——「换新」以非空 +
	// m2 可查询为准（下方 waitEngineState 即验证）。
	if len(got) != 1 || got[0].EngineID == "" || got[0].NeedsReparse {
		t.Fatalf("恢复后 TaskCtx = %+v（期望 EngineID 非空且成功）", got)
	}
	p := waitEngineState(t, m2, got[0].EngineID, "done", 60*time.Second)
	if p.Done != total {
		t.Fatalf("终态 Done = %d, 期望 %d", p.Done, total)
	}
	mustEqualFile(t, filepath.Join(dir, "bp.bin"), data)

	// 断点复用核心断言：第二段传输量 < 全量 3/4（重下=全量；续传≤total-done1≤3/4）。
	_, finalSent := rec.snapshot()
	secondSent := finalSent - baseSent
	if secondSent >= total*3/4 {
		t.Fatalf("第二段传输 %d 字节 ≥ 全量 3/4（疑似重下；done1=%d total=%d）", secondSent, done1, total)
	}
	if secondSent == 0 && done1 < total {
		t.Fatalf("第二段传输 0 字节但第一段仅完成 %d/%d（断点未被消费）", done1, total)
	}
}
