package protocol

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoginStatePuusOnly 验证登录判定(§2.6):唯一可靠判据是 __puus,
// _UP_* 前缀 Cookie 不可作判据;__puus 被登出类 Set-Cookie 置空同样不算。
func TestLoginStatePuusOnly(t *testing.T) {
	s := newTestSession(nil, NewCookieJar())
	s.jar.LoadFromString("_UP_A=1; _UP_D=2")
	if s.LoginState() {
		t.Error("只有 _UP_* 时不应视为已登录(§2.6:__puus 是唯一可靠判据)")
	}

	s.jar.LoadFromString("__puus=tok")
	if !s.LoginState() {
		t.Error("Cookie 串含 __puus 应视为已登录")
	}

	// 置空的 __puus 不算存在(Task 1:Has 只认存在且值非空)。
	s.jar.Merge("pan.quark.cn", []string{"__puus=; Path=/"})
	if s.LoginState() {
		t.Error("__puus 被置空后不应视为已登录")
	}
}

// TestQuickCheckInvalid 验证快速校验(§2.6):401/412 → 登录已失效;
// 请求须经 wall 客户端发往 pan 域 account/info?fr=pc&platform=pc。
func TestQuickCheckInvalid(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusPreconditionFailed} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/account/info" {
				t.Errorf("HTTP %d: 路径 = %q, want /account/info", status, r.URL.Path)
			}
			q := r.URL.Query()
			if q.Get("fr") != "pc" || q.Get("platform") != "pc" {
				t.Errorf("HTTP %d: 查询串 = %q, want fr=pc&platform=pc", status, r.URL.RawQuery)
			}
			w.WriteHeader(status)
		}))

		s := newTestSession(nil, NewCookieJar())
		// 业务客户端一律失败:证明 QuickCheck 走 wall 客户端(§2.6 需过墙域)。
		s.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("业务客户端不应被 QuickCheck 使用")
		})}
		s.wall = srv.Client()
		s.base.pan = srv.URL

		ok, info, err := s.QuickCheck(context.Background())
		if ok || err != nil {
			t.Errorf("HTTP %d: got (%v, %v), want (false, nil)", status, ok, err)
		}
		if info != (AccountInfo{}) {
			t.Errorf("HTTP %d: info 应为零值, got %+v", status, info)
		}
		srv.Close()
	}
}

// TestQuickCheckConservative 验证保守语义(§2.6):wall 域失败后业务域
// file/sort 兜底也拿不到确定性 401/412 时,保守视为仍有效,
// 返回 (true, 零值, nil),避免网络抖动误杀登录态。
func TestQuickCheckConservative(t *testing.T) {
	// wall 域与业务域兜底都传输层失败 → 保守 true。
	failClient := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("网络不可达")
	})}
	s := newTestSession(failClient, NewCookieJar())
	s.wall = failClient
	ok, info, err := s.QuickCheck(context.Background())
	if !ok || err != nil {
		t.Errorf("两级传输层失败: got (%v, %v), want (true, nil)", ok, err)
	}
	if info != (AccountInfo{}) {
		t.Errorf("两级传输层失败: info 应为零值, got %+v", info)
	}

	// wall 域 HTTP 500(拿不到 401/412/200 判定)→ 转业务域兜底,
	// 兜底同样传输层失败 → 仍保守 true。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	s.wall = srv.Client()
	s.base.pan = srv.URL

	ok, info, err = s.QuickCheck(context.Background())
	if !ok || err != nil {
		t.Errorf("HTTP 500 + 兜底失败: got (%v, %v), want (true, nil)", ok, err)
	}
	if info != (AccountInfo{}) {
		t.Errorf("HTTP 500 + 兜底失败: info 应为零值, got %+v", info)
	}
}

// TestQuickCheckFileSortFallback 验证 §2.6 业务域兜底:wall 域 account/info
// 网络失败时,应再用业务客户端(s.client)GET file/sort——401/412 →
// (false, 零值, nil);200 → 保守 (true, 零值, nil)(file/sort 响应
// 无账号数据)。兜底 URL 与保活心跳同形态。
func TestQuickCheckFileSortFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusPreconditionFailed, http.StatusOK} {
		want := status != http.StatusUnauthorized && status != http.StatusPreconditionFailed
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/1/clouddrive/file/sort" {
				t.Errorf("file/sort HTTP %d: 兜底路径 = %q, want /1/clouddrive/file/sort", status, r.URL.Path)
			}
			q := r.URL.Query()
			if q.Get("pr") != "ucpro" || q.Get("fr") != "pc" {
				t.Errorf("file/sort HTTP %d: 兜底查询串 = %q, 必带 pr=ucpro&fr=pc(§2.8)", status, r.URL.RawQuery)
			}
			if q.Get("pdir_fid") != "0" || q.Get("_page") != "1" || q.Get("_size") != "1" {
				t.Errorf("file/sort HTTP %d: 兜底查询串 = %q, want pr=ucpro&fr=pc&uc_param_str=&pdir_fid=0&_page=1&_size=1", status, r.URL.RawQuery)
			}
			w.WriteHeader(status)
		}))

		s := newTestSession(srv.Client(), NewCookieJar())
		// wall 客户端必错:逼 QuickCheck 走 §2.6 的业务域兜底。
		s.wall = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("wall 域不可达")
		})}
		s.base.pan = srv.URL
		s.base.drivePC = srv.URL

		ok, info, err := s.QuickCheck(context.Background())
		if ok != want || err != nil {
			t.Errorf("file/sort HTTP %d: got (%v, %v), want (%v, nil)", status, ok, err, want)
		}
		if info != (AccountInfo{}) {
			t.Errorf("file/sort HTTP %d: 兜底路径无账号信息, got %+v", status, info)
		}
		srv.Close()
	}
}

// TestQuickCheckSuccess 验证校验通过路径(§2.6):HTTP 200 时从 data 取
// 昵称与头像;头模板完整带上(Cookie 整串 + 客户端 UA)。
func TestQuickCheckSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent = %q, want %q", got, UserAgent)
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "__puus=alive") {
			t.Errorf("Cookie = %q, 应带完整 Cookie 串", got)
		}
		fmt.Fprint(w, `{"status":200,"code":0,"message":"ok",`+
			`"data":{"nickname":"飞舞用户","avatarUri":"https://avatar.example/u.png"}}`)
	}))
	defer srv.Close()

	s := newTestSession(nil, NewCookieJar())
	s.jar.LoadFromString("__puus=alive")
	s.wall = srv.Client()
	s.base.pan = srv.URL

	ok, info, err := s.QuickCheck(context.Background())
	if !ok || err != nil {
		t.Fatalf("got (%v, %v), want (true, nil)", ok, err)
	}
	if info.Nickname != "飞舞用户" || info.AvatarURI != "https://avatar.example/u.png" {
		t.Errorf("账号信息 = %+v, want 昵称/头像取自 data", info)
	}
}

// TestQuickCheckTimeoutCap 证明快速校验的超时(§2.6 规定 10s,此处注入
// 40ms)约束整个流程:wall 域请求睡超超时被打断,业务域兜底在同一已到期
// ctx 下立即失败 → 按网络失败保守视为有效,不真实发第二个请求。
func TestQuickCheckTimeoutCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := newTestSession(nil, NewCookieJar())
	s.wall = srv.Client()
	s.base.pan = srv.URL
	// 业务域兜底必错:超时到期后探测立即失败,零真实网络。
	s.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("业务域不可达")
	})}
	s.quickCheckTimeout = 40 * time.Millisecond

	start := time.Now()
	ok, _, err := s.QuickCheck(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("QuickCheck 耗时 %v, 应被超时截断在亚秒级", elapsed)
	}
	if !ok || err != nil {
		t.Errorf("超时属网络失败, got (%v, %v), want (true, nil)", ok, err)
	}
}

// TestKeepaliveTwoStrikes 验证保活心跳(§2.6):连续 2 次失败 → onExpire
// 被调一次且心跳停止(不再发第 3 个请求);心跳 URL 查询参数逐字。
func TestKeepaliveTwoStrikes(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Path != "/1/clouddrive/file/sort" {
			t.Errorf("心跳路径 = %q, want /1/clouddrive/file/sort", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("pr") != "ucpro" || q.Get("fr") != "pc" {
			t.Errorf("心跳查询串 = %q, 必带 pr=ucpro&fr=pc(§2.8)", r.URL.RawQuery)
		}
		if q.Get("pdir_fid") != "0" || q.Get("_page") != "1" || q.Get("_size") != "1" {
			t.Errorf("心跳查询串 = %q, want pr=ucpro&fr=pc&uc_param_str=&pdir_fid=0&_page=1&_size=1", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), NewCookieJar())
	s.base = sessionBase{pan: srv.URL, drivePC: srv.URL, drive: srv.URL}
	s.keepalivePeriod = 15 * time.Millisecond

	expired := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartKeepalive(ctx, func() { expired <- struct{}{} })

	select {
	case <-expired:
	case <-time.After(5 * time.Second):
		t.Fatal("连续 2 次失败应触发 onExpire")
	}
	// onExpire 仅一次、心跳已停:周期 15ms 内再无第 3 个请求。
	time.Sleep(150 * time.Millisecond)
	if got := n.Load(); got != 2 {
		t.Errorf("心跳停止后请求数应停留在 2, 实际 %d", got)
	}
	select {
	case <-expired:
		t.Error("onExpire 应只被调用一次")
	default:
	}
}

// TestKeepaliveCancelNotExpire 验证取消竞态修复:strike 1(第一次真实失败)
// 之后,下一次 ping 在途时 ctx 被取消——ping 必因取消失败,但取消不是
// 登录过期,不该被计为第二振误触 onExpire。构造:首个请求真实 500,
// 第二个请求在 handler 内挂起制造在途窗口,等到它在途后 cancel。
func TestKeepaliveCancelNotExpire(t *testing.T) {
	var n atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // 真实失败 → strike 1
			return
		}
		<-release // 后续请求挂起:制造 ping 在途窗口
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), NewCookieJar())
	s.base = sessionBase{pan: srv.URL, drivePC: srv.URL, drive: srv.URL}
	s.keepalivePeriod = 15 * time.Millisecond

	var expired atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartKeepalive(ctx, func() { expired.Store(true) })

	// 等第二个请求已发出且在途(n==2,handler 正挂起)——此刻 cancel,
	// 在途 ping 必以 context canceled 失败,当前实现会把它计为 strike 2。
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n.Load() < 2 {
		t.Fatal("5s 内第二个心跳未发出,无法构造在途取消窗口")
	}
	cancel()
	close(release) // 放行挂起的 handler,避免拖住 srv.Close

	// 等待远超多个周期:若误触 onExpire,expired 会被置位。
	time.Sleep(300 * time.Millisecond)
	if expired.Load() {
		t.Error("ctx 取消导致的 ping 失败不应计为第二振,onExpire 被误触发")
	}
	settled := n.Load()
	time.Sleep(120 * time.Millisecond)
	if got := n.Load(); got != settled {
		t.Errorf("取消后请求数仍增长: %d → %d,心跳 goroutine 应已退出", settled, got)
	}
}

// TestKeepaliveRecover 验证失败计数清零与 ctx 取消即停(§2.6):
// 1 次失败后紧跟成功 → 计数清零,不触发 onExpire;ctx 取消后心跳停止。
func TestKeepaliveRecover(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // 首次失败
			return
		}
		fmt.Fprint(w, `{"status":200,"code":0,"data":{}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), NewCookieJar())
	s.base = sessionBase{pan: srv.URL, drivePC: srv.URL, drive: srv.URL}
	s.keepalivePeriod = 15 * time.Millisecond

	var expired atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartKeepalive(ctx, func() { expired.Store(true) })

	// 等至少 4 次心跳:1 败 3 成,失败计数被清零,不应触发 onExpire。
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := n.Load(); got < 4 {
		t.Fatalf("心跳请求不足: %d 次(5s 内应 ≥ 4)", got)
	}
	if expired.Load() {
		t.Error("单次失败被成功清零,不应触发 onExpire")
	}

	// ctx 取消即停:取消后请求数不再增长。
	cancel()
	settled := n.Load()
	time.Sleep(120 * time.Millisecond)
	if got := n.Load(); got != settled {
		t.Errorf("ctx 取消后心跳应停止, 请求数 %d → %d", settled, got)
	}
	if expired.Load() {
		t.Error("ctx 取消不应触发 onExpire")
	}
}
