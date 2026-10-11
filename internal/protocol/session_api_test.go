package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// noThrottle 是测试用 no-op 节流器:跳过 §2.3 的 260ms 间隔等待,
// 让重试用例不必真实等待节流周期。
type noThrottle struct{}

func (noThrottle) Wait(context.Context) error { return nil }

// newTestSession 构造节流与退避睡眠均为 no-op 的 Session。
// 这是同包注入点:Session 留有未导出字段 throttle/sleep,
// 测试直接替换,无需改动 Throttler 的公开接口。
func newTestSession(client *http.Client, jar *CookieJar) *Session {
	s := NewSession(client, jar)
	s.throttle = noThrottle{}
	s.sleep = func(context.Context, time.Duration) error { return nil }
	return s
}

// roundTripperFunc 把函数适配为 http.RoundTripper,用于伪造网络层错误。
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDoAPISuccess(t *testing.T) {
	const dataRaw = `{"share_url_token":"tok","file_list":[{"fid":"f1","file_name":"a.mp4","size":1024}]}`

	// GET:成功响应返回 data 字段原文;头模板完整带上。
	jar := NewCookieJar()
	jar.LoadFromString("__puus=t; foo=1")

	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent = %q, want %q", got, UserAgent)
		}
		if got := r.Header.Get("Cookie"); got != "__puus=t; foo=1" {
			t.Errorf("Cookie = %q, want 完整 Cookie 串 %q", got, "__puus=t; foo=1")
		}
		if got := r.Header.Get("Referer"); got != "https://pan.quark.cn/" {
			t.Errorf("Referer = %q, want https://pan.quark.cn/", got)
		}
		fmt.Fprintf(w, `{"status":200,"code":0,"message":"ok","data":%s}`, dataRaw)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), jar)
	data, err := s.DoAPI(context.Background(), http.MethodGet, srv.URL+"/1/clouddrive/file/sort", nil)
	if err != nil {
		t.Fatalf("DoAPI: %v", err)
	}
	if string(data) != dataRaw {
		t.Errorf("data = %q, want 原文 %q", data, dataRaw)
	}
	if n.Load() != 1 {
		t.Errorf("请求数 = %d, want 1", n.Load())
	}

	// POST:JSON body 原样透传,Content-Type 为 application/json(§2.8)。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != `{"pdir_fid":"abc","file_fid":"def"}` {
			t.Errorf("POST body = %q, want 原样透传", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"task_id":"tk"}}`)
	}))
	defer srv2.Close()

	s2 := newTestSession(srv2.Client(), NewCookieJar())
	data, err = s2.DoAPI(context.Background(), http.MethodPost, srv2.URL+"/task", []byte(`{"pdir_fid":"abc","file_fid":"def"}`))
	if err != nil {
		t.Fatalf("POST DoAPI: %v", err)
	}
	if string(data) != `{"task_id":"tk"}` {
		t.Errorf("POST data = %q, want %q", data, `{"task_id":"tk"}`)
	}
}

func TestDoAPIRetryOn5xx(t *testing.T) {
	// 前 2 次 500、第 3 次 200 → 成功且请求计数 == 3。
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"ok":true}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	data, err := s.DoAPI(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("前两次 500 后成功: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("data = %q, want %q", data, `{"ok":true}`)
	}
	if n.Load() != 3 {
		t.Errorf("请求数 = %d, want 3", n.Load())
	}

	// 4 次全 500 → 重试耗尽(1+3 次)后返回携带状态码的 APIError。
	var m atomic.Int64
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()

	_, err = newTestSession(srv2.Client(), nil).DoAPI(context.Background(), http.MethodGet, srv2.URL+"/x", nil)
	if err == nil {
		t.Fatal("4 次全 500 应返回错误")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误应为 *APIError,实际 %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
	if m.Load() != 4 {
		t.Errorf("请求数 = %d, want 4(1 次 + 3 次重试)", m.Load())
	}

	// 网络错误同样退避重试:前 2 次 transport 报错,第 3 次成功。
	var fails atomic.Int64
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if fails.Add(1) <= 2 {
			return nil, errors.New("connection reset by peer")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"status":200,"code":0,"data":{"net":"ok"}}`)),
			Request:    r,
		}, nil
	})}
	data, err = newTestSession(client, nil).DoAPI(context.Background(), http.MethodGet, "https://drive-pc.quark.cn/fake", nil)
	if err != nil {
		t.Fatalf("网络错误后重试成功: %v", err)
	}
	if string(data) != `{"net":"ok"}` {
		t.Errorf("data = %q, want %q", data, `{"net":"ok"}`)
	}
	if fails.Load() != 3 {
		t.Errorf("尝试次数 = %d, want 3", fails.Load())
	}
}

func TestBackoffFormula(t *testing.T) {
	// attempt=0, r=0 → 600ms × 1 × 0.6 = 360ms。
	if got := BackoffDuration(0, 0); got != 360*time.Millisecond {
		t.Errorf("BackoffDuration(0,0) = %v, want 360ms", got)
	}
	// attempt=0, r=0.999 → 600ms × 1 × (0.6+0.8×0.999) ≈ 839.52ms。
	if got := BackoffDuration(0, 0.999); got < 839*time.Millisecond || got > 840*time.Millisecond {
		t.Errorf("BackoffDuration(0,0.999) = %v, want ≈839.52ms", got)
	}
	// attempt=10 → 公式值约 1593s×抖动,远超上限 → 15s 封顶。
	if got := BackoffDuration(10, 0); got != 15*time.Second {
		t.Errorf("BackoffDuration(10,0) = %v, want 15s 封顶", got)
	}
	if got := BackoffDuration(10, 0.999); got != 15*time.Second {
		t.Errorf("BackoffDuration(10,0.999) = %v, want 15s 封顶", got)
	}
	// 同 attempt 下抖动系数 r 越大退避越长。
	if BackoffDuration(1, 0.2) >= BackoffDuration(1, 0.9) {
		t.Errorf("r=0.2 (%v) 应短于 r=0.9 (%v)", BackoffDuration(1, 0.2), BackoffDuration(1, 0.9))
	}
}

func TestErrMapping(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantMsg  string
		wantCode int
	}{
		{
			name:     "未实名映射",
			body:     `{"status":500,"code":1,"message":"share file error: user NOT real name, please verify first"}`,
			wantMsg:  "账号未实名，无法分享/转存",
			wantCode: 1,
		},
		{
			name:     "容量码 23017 映射",
			body:     `{"status":500,"code":23017,"message":"save file error"}`,
			wantMsg:  "网盘容量不足",
			wantCode: 23017,
		},
		{
			name:     "容量码 23018 映射",
			body:     `{"status":500,"code":23018,"message":"save file error"}`,
			wantMsg:  "网盘容量不足",
			wantCode: 23018,
		},
		{
			name:     "容量关键词映射",
			body:     `{"status":500,"code":0,"message":"not enough space to save"}`,
			wantMsg:  "网盘容量不足",
			wantCode: 0,
		},
		{
			name:     "status 非 200 透传 message",
			body:     `{"status":32003,"code":32003,"message":"share not exist"}`,
			wantMsg:  "share not exist",
			wantCode: 32003,
		},
		{
			name:     "code 非 0 透传 message",
			body:     `{"status":200,"code":41008,"message":"分享已取消"}`,
			wantMsg:  "分享已取消",
			wantCode: 41008,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			_, err := newTestSession(srv.Client(), nil).DoAPI(context.Background(), http.MethodGet, srv.URL+"/x", nil)
			if err == nil {
				t.Fatalf("业务失败响应应返回错误,body=%s", tc.body)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("错误应为 *APIError,实际 %T: %v", err, err)
			}
			if apiErr.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", apiErr.Message, tc.wantMsg)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("Code = %d, want %d", apiErr.Code, tc.wantCode)
			}
		})
	}
}

func TestAuthHeal(t *testing.T) {
	// 先 412 后 200;heal 返回 true → 自愈重试成功,共 2 次请求、1 次校验。
	var n, heals atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"healed":true}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.SetAuthHeal(func(ctx context.Context) bool {
		heals.Add(1)
		return true
	})
	data, err := s.DoAPI(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("412 自愈后应成功: %v", err)
	}
	if string(data) != `{"healed":true}` {
		t.Errorf("data = %q, want %q", data, `{"healed":true}`)
	}
	if n.Load() != 2 || heals.Load() != 1 {
		t.Errorf("请求数 = %d, 校验次数 = %d, want 2/1", n.Load(), heals.Load())
	}

	// heal 返回 false → 直接报登录失效,不再重试。
	var n2 atomic.Int64
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv2.Close()

	s2 := newTestSession(srv2.Client(), nil)
	s2.SetAuthHeal(func(ctx context.Context) bool { return false })
	_, err = s2.DoAPI(context.Background(), http.MethodGet, srv2.URL+"/x", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误应为 *APIError,实际 %T: %v", err, err)
	}
	if !strings.Contains(apiErr.Message, "登录") {
		t.Errorf("heal 失败应报登录失效错误,实际 %q", apiErr.Message)
	}
	if n2.Load() != 1 {
		t.Errorf("请求数 = %d, want 1(heal 失败不重试)", n2.Load())
	}

	// 自愈只做一次:heal 成功但重试又遇 412 → 不再自愈,报登录失效。
	var n3, heals3 atomic.Int64
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n3.Add(1)
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer srv3.Close()

	s3 := newTestSession(srv3.Client(), nil)
	s3.SetAuthHeal(func(ctx context.Context) bool {
		heals3.Add(1)
		return true
	})
	_, err = s3.DoAPI(context.Background(), http.MethodGet, srv3.URL+"/x", nil)
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "登录") {
		t.Fatalf("自愈后仍 412 应报登录失效错误,实际 %v", err)
	}
	if n3.Load() != 2 || heals3.Load() != 1 {
		t.Errorf("请求数 = %d, 校验次数 = %d, want 2/1(自愈仅一次)", n3.Load(), heals3.Load())
	}
}

// TestAuthHeal403 验证 §2.3 状态表 403 行:"同上(触发快速校验 §2.6)"——
// 403 与 401/412 同走鉴权自愈路径(退避 → heal → true 重试一次 / false 报
// 登录失效)。直链场景 403=签名过期属引擎层语义(§2.10,本分支不实现),
// 此处只按状态表验证会话层通用自愈。
func TestAuthHeal403(t *testing.T) {
	// 403 → heal true → 重试原请求一次 → 成功;共 2 次请求、1 次校验。
	var n, heals atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"healed":true}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.SetAuthHeal(func(ctx context.Context) bool {
		heals.Add(1)
		return true
	})
	data, err := s.DoAPI(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("403 自愈后应成功: %v", err)
	}
	if string(data) != `{"healed":true}` {
		t.Errorf("data = %q, want %q", data, `{"healed":true}`)
	}
	if n.Load() != 2 || heals.Load() != 1 {
		t.Errorf("请求数 = %d, 校验次数 = %d, want 2/1", n.Load(), heals.Load())
	}

	// 403 → heal false → 报登录失效,不再重试。
	var n2 atomic.Int64
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv2.Close()

	s2 := newTestSession(srv2.Client(), nil)
	s2.SetAuthHeal(func(ctx context.Context) bool { return false })
	_, err = s2.DoAPI(context.Background(), http.MethodGet, srv2.URL+"/x", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误应为 *APIError,实际 %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Message, "登录") {
		t.Errorf("heal 失败应报登录失效错误,实际 %q", apiErr.Message)
	}
	if n2.Load() != 1 {
		t.Errorf("请求数 = %d, want 1(heal 失败不重试)", n2.Load())
	}
}

func TestDoAPIMergeSetCookiesOnRetry(t *testing.T) {
	// §2.3 规则 2:每次响应的 Set-Cookie 增量合并回 Cookie 串,同名覆盖。
	// 第 1 次:500 + Set-Cookie: st=v1 → 退避重试;
	// 第 2 次(重试):请求 Cookie 头应含 st=v1(增量来自上次响应),
	//   响应再下发 st=v2 验证同名覆盖不丢新值。
	var n atomic.Int64
	var firstCookie, secondCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			firstCookie = r.Header.Get("Cookie")
			w.Header().Set("Set-Cookie", "st=v1; Path=/; Domain=127.0.0.1")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			secondCookie = r.Header.Get("Cookie")
			w.Header().Set("Set-Cookie", "st=v2; Path=/")
			fmt.Fprint(w, `{"status":200,"code":0,"data":{"st":"ok"}}`)
		}
	}))
	defer srv.Close()

	jar := NewCookieJar()
	data, err := newTestSession(srv.Client(), jar).DoAPI(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("500 带 Set-Cookie 后重试应成功: %v", err)
	}
	if string(data) != `{"st":"ok"}` {
		t.Errorf("data = %q, want %q", string(data), `{"st":"ok"}`)
	}
	if n.Load() != 2 {
		t.Errorf("请求数 = %d, want 2(1 次 500 + 1 次重试)", n.Load())
	}
	if strings.Contains(firstCookie, "st=") {
		t.Errorf("首请求 Cookie = %q, 不应已含 st(st 应来自响应增量)", firstCookie)
	}
	if !strings.Contains(secondCookie, "st=v1") {
		t.Errorf("重试请求 Cookie = %q, 应含首次响应下发的 st=v1(jar 增量合并生效)", secondCookie)
	}
	if !strings.Contains(jar.String(), "st=v2") {
		t.Errorf("jar = %q, 第二次响应再下发的同名 st=v2 应覆盖 st=v1", jar.String())
	}
}

func TestDoAPIAuthHealCarriesSetCookies(t *testing.T) {
	// 412 自愈场景:WAF 下发的 Set-Cookie 应在重试前入 jar——
	// mergeSetCookies 必须先于 401/412 状态分发执行,
	// heal 通过后的重试请求携带新 Cookie。
	var n, heals atomic.Int64
	var retryCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Set-Cookie", "__puus=refreshed; Path=/")
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		retryCookie = r.Header.Get("Cookie")
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"ok":true}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.SetAuthHeal(func(ctx context.Context) bool {
		heals.Add(1)
		return true
	})
	data, err := s.DoAPI(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatalf("412 带 Set-Cookie 自愈后应成功: %v", err)
	}
	if string(data) != `{"ok":true}` {
		t.Errorf("data = %q, want %q", string(data), `{"ok":true}`)
	}
	if !strings.Contains(retryCookie, "__puus=refreshed") {
		t.Errorf("自愈重试请求 Cookie = %q, 应含 412 响应下发的 __puus=refreshed", retryCookie)
	}
	if n.Load() != 2 || heals.Load() != 1 {
		t.Errorf("请求数 = %d, 校验次数 = %d, want 2/1", n.Load(), heals.Load())
	}
}
