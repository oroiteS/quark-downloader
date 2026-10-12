package webapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"quark-downloader/internal/engine"
	"quark-downloader/internal/protocol"
	"quark-downloader/internal/store"
)

// TestHubBroadcastDelivers 基线语义：Subscribe 后 Broadcast 的事件可被该
// 订阅者收到（多订阅者各自独立收到）。
func TestHubBroadcastDelivers(t *testing.T) {
	h := NewHub()
	ch1, cancel1 := h.Subscribe()
	defer cancel1()
	ch2, cancel2 := h.Subscribe()
	defer cancel2()

	h.Broadcast(Event{Type: "auth"})

	for name, ch := range map[string]<-chan Event{"订阅者1": ch1, "订阅者2": ch2} {
		select {
		case e := <-ch:
			if e.Type != "auth" {
				t.Fatalf("%s 收到 Type=%q, 期望 auth", name, e.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s 未收到广播事件", name)
		}
	}
}

// TestHubSlowSubscriberDoesNotBlock 慢消费者取舍（简报逐字语义）：订阅者
// 不读、chan 打满后，Broadcast 绝不阻塞——满则丢弃该订阅者本帧，保住
// 全局广播。快订阅者不受慢订阅者影响，照常收到事件。配合 -race。
func TestHubSlowSubscriberDoesNotBlock(t *testing.T) {
	h := NewHub()
	slowCh, cancelSlow := h.Subscribe()
	defer cancelSlow()
	fastCh, cancelFast := h.Subscribe()
	defer cancelFast()

	const frames = 200 // 远超订阅者缓冲深度
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < frames; i++ {
			h.Broadcast(Event{Type: "task_progress", TaskID: "t1"})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("慢订阅者阻塞了 Broadcast（广播不应等待消费者）")
	}

	// 快订阅者应能收到事件（慢者的存在不影响快者的投递）。
	select {
	case <-fastCh:
	default:
		t.Fatal("快订阅者未收到事件（被慢订阅者拖累）")
	}

	// 慢订阅者丢弃了部分帧（收到数 < 发送数），且读光后不再有数据。
	n := 0
	for {
		select {
		case <-slowCh:
			n++
		default:
			goto drained
		}
	}
drained:
	if n >= frames {
		t.Fatalf("慢订阅者收到全部 %d 帧——与「chan 满丢帧」语义矛盾", frames)
	}
}

// TestHubCancelIdempotent cancel 幂等：重复调用不 panic；cancel 后
// Broadcast 不投递（也不向已关闭 channel 发送）。
func TestHubCancelIdempotent(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	cancel()
	cancel() // 第二次必须幂等

	h.Broadcast(Event{Type: "auth"}) // 不应 panic
	select {
	case e, ok := <-ch:
		if ok && e.Type != "" {
			t.Fatal("cancel 后仍投递了事件")
		}
	default:
		// channel 已关闭且无残留事件：可接受。
	}
}

// TestHubCloseClosesAll Hub.Close（App.Close/服务停机链使用）：关闭全部
// 订阅者 channel、幂等，Close 后 Broadcast 安全。
func TestHubCloseClosesAll(t *testing.T) {
	h := NewHub()
	ch1, cancel1 := h.Subscribe()
	defer cancel1()
	ch2, cancel2 := h.Subscribe()
	defer cancel2()

	h.Close()
	h.Close() // 幂等

	for name, ch := range map[string]<-chan Event{"ch1": ch1, "ch2": ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("%s 在 Close 后仍可读出数据", name)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s 在 Close 后未关闭", name)
		}
	}
	h.Broadcast(Event{Type: "auth"}) // 不应 panic
	if got := h.subscriberCount(); got != 0 {
		t.Fatalf("Close 后订阅表非空: %d", got)
	}
}

// TestSSEStream SSE 端点全链路（httptest 真连接读流）：
//  1. Content-Type text/event-stream；
//  2. 心跳注释行（间隔注入为短值）先行到达——同时证明 handler 已订阅；
//  3. Broadcast 后读到 data: {json} 事件帧；
//  4. 客户端断开（关 body）→ hub 退订无泄漏，随后 Broadcast 安全。
func TestSSEStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	origHeartbeat := sseHeartbeat
	sseHeartbeat = 25 * time.Millisecond // 注入短心跳：先于心跳证明订阅已建立
	defer func() { sseHeartbeat = origHeartbeat }()

	app := New(Deps{})
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	ctx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q, 期望 text/event-stream", ct)
	}

	// 读 goroutine + select 总超时：任何一段 3 秒无数据都算失败（不依赖
	// resp.Body 内部类型实现 SetReadDeadline）。
	lines := make(chan string, 64)
	readErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				readErr <- err
				return
			}
			lines <- strings.TrimRight(line, "\n")
		}
	}()
	readLine := func() string {
		t.Helper()
		select {
		case line := <-lines:
			return line
		case err := <-readErr:
			t.Fatalf("读 SSE 流: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("读 SSE 流超时（3 秒无数据）")
		}
		return "" // 不可达（t.Fatal 即退出）
	}

	// 1) 等心跳注释行：handler 已进入收发循环（订阅必然完成）。
	sawHeartbeat := false
	for i := 0; i < 20 && !sawHeartbeat; i++ {
		if line := readLine(); strings.HasPrefix(line, ":") {
			sawHeartbeat = true
		}
	}
	if !sawHeartbeat {
		t.Fatal("3 秒内未读到心跳注释行")
	}

	// 2) 广播 → 读到 data: {json} 帧（与 Event JSON 序列化逐字节一致）。
	want, err := json.Marshal(Event{Type: "auth", Payload: json.RawMessage(`{"ok":true}`)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	app.hub.Broadcast(Event{Type: "auth", Payload: json.RawMessage(`{"ok":true}`)})
	sawData := false
	for i := 0; i < 20 && !sawData; i++ {
		line := readLine()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if got := strings.TrimPrefix(line, "data: "); got != string(want) {
			t.Fatalf("事件帧 = %q, 期望 %q", got, want)
		}
		sawData = true
	}
	if !sawData {
		t.Fatal("未读到 data: 事件帧")
	}

	// 3) 客户端断开 → 退订（轮询订阅表归零），随后 Broadcast 安全。
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for app.hub.subscriberCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := app.hub.subscriberCount(); got != 0 {
		t.Fatalf("客户端断开后未退订（订阅表残留 %d 条——泄漏）", got)
	}
	app.hub.Broadcast(Event{Type: "auth"}) // 退订后广播不应 panic
}

// ---- Task 12：SSE 事件流与对账兜底（G3）----

// sseFrame 一帧 SSE 事件（data: 行 JSON）的解析形态：Event 头字段 +
// Payload 二次解析的快捷访问。
type sseFrame struct {
	Type    string          `json:"type"`
	TaskID  string          `json:"taskId"`
	Payload json.RawMessage `json:"payload"`
}

// openSSE 建立真实 SSE 连接并返回读帧辅助（心跳注释行自动跳过）。
// 每帧 3 秒读超时——任何挂起都算测试失败而非死等。
func openSSE(t *testing.T, h http.Handler) (*sseReader, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	srv := httptest.NewServer(h)
	ctx, cancelReq := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		cancelReq()
		srv.Close()
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancelReq()
		srv.Close()
		t.Fatalf("GET /api/events: %v", err)
	}
	r := &sseReader{t: t, lines: make(chan string, 64), readErr: make(chan error, 1)}
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				r.readErr <- err
				return
			}
			r.lines <- strings.TrimRight(line, "\n")
		}
	}()
	return r, func() {
		cancelReq()
		resp.Body.Close()
		srv.Close()
	}
}

type sseReader struct {
	t       *testing.T
	lines   chan string
	readErr chan error
}

// next 读下一帧事件（跳过心跳注释行/空行），3 秒超时。
func (r *sseReader) next() sseFrame {
	r.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		var line string
		select {
		case line = <-r.lines:
		case err := <-r.readErr:
			r.t.Fatalf("读 SSE 流: %v", err)
		case <-deadline:
			r.t.Fatal("读 SSE 流超时（3 秒无帧）")
		}
		if !strings.HasPrefix(line, "data: ") {
			continue // 心跳注释行/空行
		}
		var f sseFrame
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f); err != nil {
			r.t.Fatalf("帧不是合法 JSON: %v（line=%q）", err, line)
		}
		return f
	}
}

// expect 读下一帧并断言 type（失败即 Fatal 带实际帧内容）。
func (r *sseReader) expect(typ string) sseFrame {
	r.t.Helper()
	f := r.next()
	if f.Type != typ {
		r.t.Fatalf("帧 type = %q, 期望 %q（payload=%s）", f.Type, typ, f.Payload)
	}
	return f
}

// TestEngineEventForwarding 引擎事件转发端到端：a.engineEvent（装配处
// 适配 NewManager onEvent）→ SSE 客户端读到帧。进度帧带 done/total/
// speed_bps/eta_ms/state/merging；终态帧（done/error/canceled）为
// task_final，Err 文本透出到 payload.error。
func TestEngineEventForwarding(t *testing.T) {
	app := New(Deps{})
	r, closeSSE := openSSE(t, app.Router())
	defer closeSSE()

	r.expect("hello") // 订阅即发（空任务表也发——对账帧契约）

	app.EngineEvent(engine.Event{TaskID: "t1", Progress: engine.Progress{
		Done: 5, Total: 10, SpeedBPS: 2, State: "running",
	}})
	f := r.expect("task_progress")
	if f.TaskID != "t1" {
		t.Fatalf("taskId = %q, 期望 t1", f.TaskID)
	}
	var p struct {
		Done     int64   `json:"done"`
		Total    int64   `json:"total"`
		SpeedBPS float64 `json:"speed_bps"`
		EtaMS    int64   `json:"eta_ms"`
		State    string  `json:"state"`
		Merging  bool    `json:"merging"`
	}
	if err := json.Unmarshal(f.Payload, &p); err != nil {
		t.Fatalf("progress payload 解析: %v（%s）", err, f.Payload)
	}
	if p.Done != 5 || p.Total != 10 || p.SpeedBPS != 2 || p.State != "running" {
		t.Fatalf("progress payload = %+v, 期望 done=5 total=10 speed=2 state=running", p)
	}
	if p.EtaMS != 2500 { // 剩 5 字节 @ 2 B/s = 2.5s
		t.Fatalf("eta_ms = %d, 期望 2500", p.EtaMS)
	}

	app.EngineEvent(engine.Event{TaskID: "t1", Progress: engine.Progress{
		Done: 10, Total: 10, State: "done",
	}})
	f = r.expect("task_final")
	var fin struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(f.Payload, &fin); err != nil {
		t.Fatalf("final payload 解析: %v（%s）", err, f.Payload)
	}
	if fin.State != "done" || fin.Error != "" {
		t.Fatalf("final payload = %+v, 期望 state=done 无 error", fin)
	}

	app.EngineEvent(engine.Event{TaskID: "t2", Progress: engine.Progress{
		State: "error",
	}, Err: fmt.Errorf("boom: %w", context.Canceled)})
	f = r.expect("task_final")
	if err := json.Unmarshal(f.Payload, &fin); err != nil {
		t.Fatalf("final payload 解析: %v（%s）", err, f.Payload)
	}
	if fin.State != "error" || fin.Error == "" {
		t.Fatalf("final payload = %+v, 期望 state=error 带 error 文本", fin)
	}
	if !strings.Contains(fin.Error, "boom") {
		t.Fatalf("error 文本 = %q, 期望含 \"boom\"", fin.Error)
	}
}

// TestSSEHelloSnapshot 断线对账（Review Focus #4）：SSE 订阅首发 hello
// 帧带全量任务快照（与 GET /api/tasks 同源）；断开重连后再读 hello，
// 客户端视图可恢复。
func TestSSEHelloSnapshot(t *testing.T) {
	up := &fakeUpstream{}
	fm := &fakeManager{gets: map[string]engine.Progress{
		"t1": {Done: 3, Total: 9, SpeedBPS: 50, State: "running"},
	}}
	paths := taskTestPaths(t)
	app := newTasksTestApp(t, up, fm, taskTestSettings(paths.DataDir), paths)
	if err := store.SaveTasks(paths, []store.TaskCtx{
		{EngineID: "t1", FileName: "a.bin", FID: "f1", Size: 9, CreatedAt: 100},
		{FID: "f2", FileName: "b.bin", Size: 7, Exists: true, CreatedAt: 101},
	}); err != nil {
		t.Fatalf("落盘测试 TaskCtx: %v", err)
	}

	r, closeSSE := openSSE(t, app.Router())
	f := r.expect("hello")
	var snaps []taskDTO
	if err := json.Unmarshal(f.Payload, &snaps); err != nil {
		t.Fatalf("hello payload 解析: %v（%s）", err, f.Payload)
	}
	if len(snaps) != 2 {
		t.Fatalf("hello 快照条目 = %d, 期望 2（%+v）", len(snaps), snaps)
	}
	byState := map[string]taskDTO{}
	for _, s := range snaps {
		byState[s.State] = s
	}
	if s, ok := byState["running"]; !ok || s.ID != "t1" || s.Done != 3 {
		t.Fatalf("running 条目缺失或不符: %+v", s)
	}
	if s, ok := byState["exists"]; !ok || s.Done != 7 {
		t.Fatalf("exists 条目缺失或不符: %+v", s)
	}
	closeSSE()

	// 断开重连：hello 再发，视图可恢复（Focus #4）。
	r2, closeSSE2 := openSSE(t, app.Router())
	defer closeSSE2()
	f2 := r2.expect("hello")
	var snaps2 []taskDTO
	if err := json.Unmarshal(f2.Payload, &snaps2); err != nil {
		t.Fatalf("重连 hello payload 解析: %v（%s）", err, f2.Payload)
	}
	if len(snaps2) != 2 {
		t.Fatalf("重连 hello 快照条目 = %d, 期望 2", len(snaps2))
	}
}

// TestAuthEvents 登录态事件（auth 帧三型）：cookie 导入成功 →
// {event:"login",logged_in:true}；logout → {event:"logout"}；保活
// onExpire → {event:"expired",logged_in:false}。
func TestAuthEvents(t *testing.T) {
	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	r, closeSSE := openSSE(t, app.Router())
	defer closeSSE()
	r.expect("hello")

	assertAuth := func(event string, loggedIn bool) {
		t.Helper()
		f := r.expect("auth")
		var a struct {
			Event    string `json:"event"`
			LoggedIn bool   `json:"logged_in"`
		}
		if err := json.Unmarshal(f.Payload, &a); err != nil {
			t.Fatalf("auth payload 解析: %v（%s）", err, f.Payload)
		}
		if a.Event != event || a.LoggedIn != loggedIn {
			t.Fatalf("auth 帧 = %+v, 期望 event=%q logged_in=%v", a, event, loggedIn)
		}
	}

	// 导入成功 → login 帧。
	w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie",
		map[string]any{"text": "__puus=tok123"})
	if w.Code != http.StatusOK {
		t.Fatalf("导入状态码 = %d（body=%s）", w.Code, w.Body)
	}
	assertAuth("login", true)

	// 登出 → logout 帧。
	if w := doReq(t, app.Router(), http.MethodPost, "/api/logout", nil); w.Code != http.StatusOK {
		t.Fatalf("logout 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	assertAuth("logout", false)

	// 再登录 → 手动触发保活过期（App 层驱动，T7 模式）→ expired 帧。
	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie",
		map[string]any{"text": "__puus=tok456"}); w.Code != http.StatusOK {
		t.Fatalf("二次导入状态码 = %d（body=%s）", w.Code, w.Body)
	}
	assertAuth("login", true)
	fa.mu.Lock()
	onExpire := fa.kaOnExpire
	fa.mu.Unlock()
	if onExpire == nil {
		t.Fatal("keepalive 未捕获 onExpire（登录应启动保活）")
	}
	onExpire()
	assertAuth("expired", false)
}

// TestBroadcastAfterClientGone 退订后广播不 panic：客户端断开 → 退订
// → engineEvent/Broadcast 均安全，无泄漏订阅者。
func TestBroadcastAfterClientGone(t *testing.T) {
	app := New(Deps{})
	r, closeSSE := openSSE(t, app.Router())
	r.expect("hello")
	closeSSE()

	// 退订经 handler 的 request ctx Done 异步完成；轮询上限 2 秒。
	for i := 0; i < 40; i++ {
		if app.hub.subscriberCount() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := app.hub.subscriberCount(); n != 0 {
		t.Fatalf("断开后订阅者 = %d, 期望 0（泄漏）", n)
	}
	app.EngineEvent(engine.Event{TaskID: "t1", Progress: engine.Progress{State: "running"}}) // 不得 panic
	app.hub.Broadcast(Event{Type: "ping"})                                                   // 不得 panic
}

// TestSubmitInvalidThreadsMapped400 错误码冻结：engine.ErrInvalidThreads
// （wrap 形态）→ 400 {code:"invalid_threads"}（参数错误不进 skipped）。
func TestSubmitInvalidThreadsMapped400(t *testing.T) {
	up := &fakeUpstream{
		share: map[string][]protocol.FileEntry{
			"d0": {{FID: "fmv", FileName: "movie.mkv", Size: 4096, ShareFIDToken: "tok"}},
		},
		links: map[string]string{"fmv": "https://d.quark.cn/fmv?Expires=1"},
	}
	fm := &fakeManager{submitF: func(fakeSubmitCall) (string, error) {
		return "", fmt.Errorf("threads 档位非法: %w", engine.ErrInvalidThreads)
	}}
	paths := taskTestPaths(t)
	app := newTasksTestApp(t, up, fm, taskTestSettings(paths.DataDir), paths)

	w := doReq(t, app.Router(), http.MethodPost, "/api/tasks", map[string]any{
		"source": "share", "fids": []string{"d0"},
		"pwd_id": "pw1", "stoken": "st0",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400（body=%s）", w.Code, w.Body)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, w.Body)
	}
	if body.Error.Code != "invalid_threads" {
		t.Fatalf("code = %q, 期望 invalid_threads", body.Error.Code)
	}
}

// TestErrorCodeConstantsFrozen 冻结的错误码契约（系列 4 前端依赖）：
// 常量值即 wire 契约，重命名须同步前端——此表是唯一权威清单。
func TestErrorCodeConstantsFrozen(t *testing.T) {
	frozen := map[string]string{
		"not_logged_in":         codeNotLoggedIn,
		"internal":              codeInternal,
		"not_found":             codeNotFound,
		"task_not_found":        codeTaskNotFound,
		"needs_reparse":         codeNeedsReparse,
		"dest_busy":             codeDestBusy,
		"submit_failed":         codeSubmitFailed,
		"invalid_threads":       codeInvalidThreads,
		"manager_closed":        codeManagerClosed,
		"invalid_tasks_request": codeInvalidTasksRequest,
		"save_tasks_failed":     codeSaveTasksFailed,
		"save_cookie_failed":    codeSaveCookieFailed,
		"logout_failed":         codeLogoutFailed,
		"exchange_failed":       codeExchangeFailed,
		"qr_start_failed":       codeQRStartFailed,
		"qr_poll_failed":        codeQRPollFailed,
		"no_qr_session":         codeNoQRSession,
		"invalid_cookie":        codeInvalidCookie,
		"share_token_failed":    codeShareTokenFailed,
		"links_failed":          codeLinksFailed,
		"expand_failed":         codeExpandFailed,
		"capacity_failed":       codeCapacityFailed,
	}
	for wire, constant := range frozen {
		if wire != constant {
			t.Errorf("错误码常量漂移: wire=%q 常量=%q（重命名须同步系列 4 前端）", wire, constant)
		}
	}
	if len(frozen) != 22 {
		t.Fatalf("冻结表条目 = %d, 期望 22（增删须过契约评审）", len(frozen))
	}
}
