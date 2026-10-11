package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withUopBase 把 uopBaseURL 指向 fake 服务,测试结束恢复。
// 扫码登录两个端点都在 uop 域,借此整体接管,不连真实服务。
func withUopBase(t *testing.T, base string) {
	t.Helper()
	orig := uopBaseURL
	uopBaseURL = base
	t.Cleanup(func() { uopBaseURL = orig })
}

// isHex32 报告 s 是否为 32 位小写 hex(request_id 格式,§2.4)。
func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// fakeUop 返回接管两个 CAS 端点的 handler:
// 取 token 路径固定回 TOKEN123;轮询路径回 pollBody,
// 并顺带断言轮询请求携带 §2.4 要求的全部查询参数。
func fakeUop(t *testing.T, pollBody string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cas/ajax/getTokenForQrcodeLogin":
			fmt.Fprint(w, `{"status":2000000,"message":"ok","data":{"members":{"token":"TOKEN123"}}}`)
		case "/cas/ajax/getServiceTicketByQrcodeToken":
			q := r.URL.Query()
			if got := q.Get("token"); got != "TOKEN123" {
				t.Errorf("轮询 token 参数 = %q, want TOKEN123", got)
			}
			if got := q.Get("client_id"); got != "532" {
				t.Errorf("轮询 client_id = %q, want 532", got)
			}
			if got := q.Get("v"); got != "1.2" {
				t.Errorf("轮询 v = %q, want 1.2", got)
			}
			if rid := q.Get("request_id"); !isHex32(rid) {
				t.Errorf("轮询 request_id = %q, want 32 位随机 hex", rid)
			}
			fmt.Fprint(w, pollBody)
		default:
			t.Errorf("uop fake 收到意外路径 %s", r.URL.Path)
		}
	})
}

func TestStartQRLogin(t *testing.T) {
	// 成功:查询参数符合 §2.4,头模板符合 §2.3,token 取自 data.members.token。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("client_id"); got != "532" {
			t.Errorf("client_id = %q, want 532", got)
		}
		if got := r.URL.Query().Get("v"); got != "1.2" {
			t.Errorf("v = %q, want 1.2", got)
		}
		if rid := r.URL.Query().Get("request_id"); !isHex32(rid) {
			t.Errorf("request_id = %q, want 32 位随机 hex", rid)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent = %q, want 客户端 UA", got)
		}
		// uop.quark.cn 不含 pan.quark.cn → Sec-Fetch-Site 应为 same-site(§2.3)。
		if got := r.Header.Get("Sec-Fetch-Site"); got != "same-site" {
			t.Errorf("Sec-Fetch-Site = %q, want same-site", got)
		}
		fmt.Fprint(w, `{"status":2000000,"message":"ok","data":{"members":{"token":"TOKEN123"}}}`)
	}))
	defer srv.Close()
	withUopBase(t, srv.URL)

	l, err := newTestSession(srv.Client(), nil).StartQRLogin(context.Background())
	if err != nil {
		t.Fatalf("StartQRLogin: %v", err)
	}
	if l.token != "TOKEN123" {
		t.Errorf("token = %q, want TOKEN123", l.token)
	}
	if l.start.IsZero() {
		t.Error("StartQRLogin 应记录开始时刻供 300s 超时判定")
	}

	// 失败:uop 外壳 status 非 2000000 → 返回携带 status 的错误。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":50031000,"message":"cas busy","data":null}`)
	}))
	defer srv2.Close()
	withUopBase(t, srv2.URL)

	if _, err := newTestSession(srv2.Client(), nil).StartQRLogin(context.Background()); err == nil {
		t.Fatal("status=50031000 应返回错误")
	} else if !strings.Contains(err.Error(), "50031000") {
		t.Errorf("错误应含原始 status,实际: %v", err)
	}

	// 成功外壳但缺 token → 同样报错。
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":2000000,"message":"ok","data":{}}`)
	}))
	defer srv3.Close()
	withUopBase(t, srv3.URL)

	if _, err := newTestSession(srv3.Client(), nil).StartQRLogin(context.Background()); err == nil {
		t.Fatal("缺 data.members.token 应返回错误")
	}
}

func TestQRLoginStates(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantState  QRState
		wantTicket string
	}{
		{
			name:      "50004001等待扫码",
			body:      `{"status":50004001,"message":"Query result is empty","data":null}`,
			wantState: QRWaiting,
		},
		{
			name:      "非空data无ticket已扫码",
			body:      `{"status":2000000,"message":"ok","data":{"members":{"service_ticket":""}}}`,
			wantState: QRScanned,
		},
		{
			name:       "ticket非空拿到票据",
			body:       `{"status":2000000,"message":"ok","data":{"members":{"service_ticket":"ST-abc-123"}}}`,
			wantState:  QRGotTicket,
			wantTicket: "ST-abc-123",
		},
		{
			name:      "50004002已失效",
			body:      `{"status":50004002,"message":"Token Not Found","data":null}`,
			wantState: QRInvalid,
		},
		{
			name:      "50004003已过期",
			body:      `{"status":50004003,"message":"Token Expired","data":null}`,
			wantState: QRExpired,
		},
		{
			name:      "空data兜底等待",
			body:      `{"status":2000000,"message":"ok","data":{}}`,
			wantState: QRWaiting,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(fakeUop(t, tc.body))
			defer srv.Close()
			withUopBase(t, srv.URL)

			l, err := newTestSession(srv.Client(), nil).StartQRLogin(context.Background())
			if err != nil {
				t.Fatalf("StartQRLogin: %v", err)
			}
			state, ticket, err := l.Poll(context.Background())
			if err != nil {
				t.Fatalf("Poll 不应报错: %v", err)
			}
			if state != tc.wantState {
				t.Errorf("state = %v, want %v (body=%s)", state, tc.wantState, tc.body)
			}
			if ticket != tc.wantTicket {
				t.Errorf("ticket = %q, want %q", ticket, tc.wantTicket)
			}
		})
	}
}

func TestQRLoginTimeout(t *testing.T) {
	// 开始时刻拨回 300s 之前 → Poll 直接 Timeout,且不得再发请求。
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()
	withUopBase(t, srv.URL)

	l := &QRLogin{
		sess:  newTestSession(srv.Client(), nil),
		token: "T",
		start: time.Now().Add(-qrLoginTimeout - time.Second),
	}
	state, ticket, err := l.Poll(context.Background())
	if err != nil {
		t.Fatalf("超时应为状态而非错误: %v", err)
	}
	if state != QRTimeout {
		t.Errorf("state = %v, want QRTimeout", state)
	}
	if ticket != "" {
		t.Errorf("ticket = %q, want 空", ticket)
	}
	if hits.Load() != 0 {
		t.Errorf("超时后不应再发请求,实际发了 %d 次", hits.Load())
	}
}

func TestQRPollNetErrorSkipped(t *testing.T) {
	// §2.4:单次轮询网络失败 → 静默跳过本轮,返回 Waiting、err==nil。
	// 变体 1:transport 直接报错。
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset by peer")
	})}
	l := &QRLogin{sess: newTestSession(client, nil), token: "T", start: time.Now()}
	state, _, err := l.Poll(context.Background())
	if err != nil {
		t.Fatalf("网络失败应静默跳过,err = %v", err)
	}
	if state != QRWaiting {
		t.Errorf("state = %v, want QRWaiting", state)
	}

	// 变体 2:HTTP 500。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withUopBase(t, srv.URL)

	l2 := &QRLogin{sess: newTestSession(srv.Client(), nil), token: "T", start: time.Now()}
	state, _, err = l2.Poll(context.Background())
	if err != nil {
		t.Fatalf("HTTP 500 应静默跳过,err = %v", err)
	}
	if state != QRWaiting {
		t.Errorf("state = %v, want QRWaiting", state)
	}

	// 变体 3:响应不是合法 JSON。
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html>gateway error</html>`)
	}))
	defer srv3.Close()
	withUopBase(t, srv3.URL)

	l3 := &QRLogin{sess: newTestSession(srv3.Client(), nil), token: "T", start: time.Now()}
	state, _, err = l3.Poll(context.Background())
	if err != nil {
		t.Fatalf("坏 JSON 应静默跳过,err = %v", err)
	}
	if state != QRWaiting {
		t.Errorf("state = %v, want QRWaiting", state)
	}

	// 变体 4:ctx 已取消 → 不掩盖取消信号,应返回 ctx 错误。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l4 := &QRLogin{sess: newTestSession(client, nil), token: "T", start: time.Now()}
	if _, _, err := l4.Poll(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 取消时 err = %v, want context.Canceled", err)
	}
}

func TestScanURL(t *testing.T) {
	l := &QRLogin{token: "tok-123"}
	got := l.ScanURL()

	// §2.2 扫码页 URL 模板逐字:token、client_id=532、ssb=weblogin、uc_biz_str 整段。
	want := "https://su.quark.cn/4_eMHBJ?token=tok-123&client_id=532" +
		"&ssb=weblogin&uc_param_str=" +
		"&uc_biz_str=S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0"
	if got != want {
		t.Errorf("ScanURL() =\n  %q\nwant\n  %q", got, want)
	}
	for _, seg := range []string{"token=tok-123", "client_id=532", "ssb=weblogin", "OPT:SAREA@0"} {
		if !strings.Contains(got, seg) {
			t.Errorf("ScanURL() 缺少 %q", seg)
		}
	}
}

func TestRandomRequestID(t *testing.T) {
	seen := make(map[string]bool, 128)
	for i := 0; i < 128; i++ {
		id := RandomRequestID()
		if len(id) != 32 {
			t.Fatalf("len(%q) = %d, want 32", id, len(id))
		}
		if !isHex32(id) {
			t.Fatalf("%q 不是 32 位 hex", id)
		}
		if seen[id] {
			t.Fatalf("RandomRequestID 出现重复: %s", id)
		}
		seen[id] = true
	}
}
