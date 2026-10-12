// session_test.go 验证会话生命周期接线（设计 D9 前半）：
// 启动恢复（有 cookie→keepalive 启动；无→静默）、onExpire 经 hub 推
// SSE 登录态事件、SetAuthHeal 接线后 DoAPI 401 路径自愈一次。
//
// 全部离线：恢复链走 fakeAuth/fakeSecrets；自愈链走真 *protocol.Session
// + httptest fake 上游——heal 闭包经注入的 fake authAPI.QuickCheck 供值，
// 不触达真实夸克域名。keepalive 循环本身的两振判定/计数清零/取消语义
// 由 protocol 包 authcheck_test 覆盖，本文件以 fakeAuth 捕获 onExpire
// 手动驱动（App 层可测方式，不注入 protocol 内部周期）。
package webapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quark-downloader/internal/protocol"
)

// ---- restoreSession（启动恢复）----

// TestRestoreWithCookieStartsKeepalive 有凭据：LoadCookie → jar 装载 →
// LoginState（__puus）→ StartKeepalive 启动；jar 恢复出整串登录态。
func TestRestoreWithCookieStartsKeepalive(t *testing.T) {
	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	if err := fs.SaveCookie("__pus=p; __puus=tok"); err != nil {
		t.Fatalf("预置凭据: %v", err)
	}
	app := newAuthTestApp(t, fa, fs)

	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fa.mu.Lock()
	ka := fa.kaStarted
	fa.mu.Unlock()
	if ka != 1 {
		t.Fatalf("StartKeepalive 调用 %d 次, 期望 1（恢复出登录态即启心跳）", ka)
	}
	if got := app.jar.String(); got != "__pus=p; __puus=tok" {
		t.Fatalf("恢复后 jar = %q, 期望整串登录态 Cookie", got)
	}
	if !app.auth.LoginState() {
		t.Fatal("恢复后应处于登录态（jar 含 __puus）")
	}
}

// TestRestoreWithoutCookieSilent 无凭据（或解密失败空串）：静默——
// 无错返回、不启 keepalive、jar 保持空。
func TestRestoreWithoutCookieSilent(t *testing.T) {
	fa := &fakeAuth{}
	app := newAuthTestApp(t, fa, &fakeSecrets{})

	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("无凭据应静默返回, 实际错误: %v", err)
	}
	fa.mu.Lock()
	ka := fa.kaStarted
	fa.mu.Unlock()
	if ka != 0 {
		t.Fatalf("无凭据仍启动 keepalive（%d 次）, 期望 0", ka)
	}
	if got := app.jar.String(); got != "" {
		t.Fatalf("无凭据后 jar = %q, 期望空", got)
	}
}

// TestRestoreCookieMissingPuusRollsBack 存下的凭据缺 __puus（如旧版本
// 半串 Cookie）：与导入链同一判据，按未登录静默处理——无错、脏 jar
// 回滚为空、不启 keepalive。
func TestRestoreCookieMissingPuusRollsBack(t *testing.T) {
	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	if err := fs.SaveCookie("_UP_A=1; _UP_D=2"); err != nil {
		t.Fatalf("预置凭据: %v", err)
	}
	app := newAuthTestApp(t, fa, fs)

	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("缺 __puus 应静默按未登录处理, 实际错误: %v", err)
	}
	fa.mu.Lock()
	ka := fa.kaStarted
	fa.mu.Unlock()
	if ka != 0 {
		t.Fatalf("缺 __puus 仍启动 keepalive（%d 次）, 期望 0", ka)
	}
	if got := app.jar.String(); got != "" {
		t.Fatalf("缺 __puus 后 jar = %q, 期望回滚为空（不留半登录态）", got)
	}
}

// ---- onExpire → SSE 登录态事件 ----

// TestKeepaliveExpirePushesSSEAuthEvent 心跳判过期 → hub 广播 auth 事件 →
// SSE 流收到 data: {"type":"auth","payload":{"event":"expired","logged_in":false}}
// （帧格式 Task 12 已冻结：auth 帧 payload={event,logged_in}）。
// fakeAuth 捕获 onExpire 后手动触发模拟两振过期（App 层驱动）。
func TestKeepaliveExpirePushesSSEAuthEvent(t *testing.T) {
	// 注入短心跳: 等到心跳注释行即证明 SSE handler 已完成订阅。
	origHeartbeat := sseHeartbeat
	sseHeartbeat = 25 * time.Millisecond
	defer func() { sseHeartbeat = origHeartbeat }()

	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	if err := fs.SaveCookie("__puus=tok"); err != nil {
		t.Fatalf("预置凭据: %v", err)
	}
	app := newAuthTestApp(t, fa, fs)
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fa.mu.Lock()
	onExpire := fa.kaOnExpire
	fa.mu.Unlock()
	if onExpire == nil {
		t.Fatal("恢复后 keepalive 未启动（未捕获到 onExpire）")
	}

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
	defer resp.Body.Close()

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
		case <-readErr:
			t.Fatalf("读 SSE 流: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("读 SSE 流超时（3 秒无数据）")
		}
		return "" // 不可达
	}

	// 1) 等心跳注释行: handler 已订阅 hub。
	sawHeartbeat := false
	for i := 0; i < 20 && !sawHeartbeat; i++ {
		if line := readLine(); strings.HasPrefix(line, ":") {
			sawHeartbeat = true
		}
	}
	if !sawHeartbeat {
		t.Fatal("3 秒内未读到心跳注释行（订阅未建立）")
	}

	// 2) 触发 onExpire → SSE 流读到 auth 登录态事件帧。
	onExpire()
	want, err := json.Marshal(Event{
		Type: "auth",
		// Task 12 冻结契约：auth 帧 payload={event,logged_in}（旧帧
		// 只有 logged_in——T12 起带 event 枚举 login/logout/expired）。
		Payload: json.RawMessage(`{"event":"expired","logged_in":false}`),
	})
	if err != nil {
		t.Fatalf("marshal 期望帧: %v", err)
	}
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
		t.Fatal("onExpire 后未读到 auth 登录态事件帧")
	}
	cancelReq()
}

// ---- SetAuthHeal 接线 ----

// TestSessionAuthHealWiredOn401 接线验证（D9）：resetSession 装配的
// session 挂上 SetAuthHeal，heal → 本代 authAPI.QuickCheck。真
// *protocol.Session + httptest fake 上游：DoAPI 首遇 401 → heal（fake
// QuickCheck true）→ 重试原请求成功——2 次请求、1 次校验、自愈仅一次。
//
// 注: 401 自愈前的退避由 protocol 内部常量决定（2–4s 随机, authHealBackoff）,
// 本测试真实等待以覆盖完整接线链路（离线, 耗时数秒属预期）。
func TestSessionAuthHealWiredOn401(t *testing.T) {
	var n, quicks atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"healed":true}}`)
	}))
	defer srv.Close()

	fa := &fakeAuth{quickF: func() (bool, protocol.AccountInfo) {
		quicks.Add(1)
		return true, protocol.AccountInfo{}
	}}
	app := New(Deps{})
	app.newAuthPair = func(jar *protocol.CookieJar) (*protocol.Session, authAPI) {
		fa.mu.Lock()
		fa.jar = jar
		fa.mu.Unlock()
		return protocol.NewSession(srv.Client(), jar), fa
	}
	app.resetSession() // 注入工厂生效（New 时的生产工厂产物弃用）

	data, err := app.session.DoAPI(context.Background(), http.MethodGet,
		srv.URL+"/1/clouddrive/file/sort", nil)
	if err != nil {
		t.Fatalf("401 自愈后应成功: %v", err)
	}
	if string(data) != `{"healed":true}` {
		t.Fatalf("data = %q, 期望自愈重试后的成功 data", data)
	}
	if n.Load() != 2 {
		t.Fatalf("请求数 = %d, 期望 2（401 → 自愈 → 重试）", n.Load())
	}
	if quicks.Load() != 1 {
		t.Fatalf("QuickCheck 调用 %d 次, 期望 1（自愈仅一次）", quicks.Load())
	}
}
