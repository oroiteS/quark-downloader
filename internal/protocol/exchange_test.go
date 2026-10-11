package protocol

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newExchangeSession 返回 wall 客户端与三个域 base 都指向 fake 服务的 Session,
// 不连真实服务:wall 注入 test server 的普通 client,base 整体接管域名判断。
func newExchangeSession(t *testing.T, srv *httptest.Server) *Session {
	t.Helper()
	s := newTestSession(srv.Client(), nil)
	s.wall = srv.Client()
	s.base = sessionBase{pan: srv.URL, drivePC: srv.URL, drive: srv.URL}
	return s
}

// fakeExchange 接管换 Cookie 全部四个端点,按序记录到达路径:
// 步骤 1 account/info 回登录 Cookie 六件套 + 账号信息;步骤 2 三连回 __puus 等。
// 各端点顺带断言 §2.5 要求的查询参数与 §2.3 头模板。
func fakeExchange(t *testing.T) http.Handler {
	t.Helper()
	var mu sync.Mutex
	paths := make([]string, 0, 4)
	t.Cleanup(func() {
		want := []string{
			"/account/info", "/list",
			"/1/clouddrive/file/sort", "/1/clouddrive/member",
		}
		if fmt.Sprint(paths) != fmt.Sprint(want) {
			t.Errorf("请求到达顺序 = %v, want %v", paths, want)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/account/info":
			q := r.URL.Query()
			if got := q.Get("st"); got != "TICKET456" {
				t.Errorf("步骤 1 st = %q, want TICKET456", got)
			}
			if got := q.Get("lw"); got != "scan" {
				t.Errorf("步骤 1 lw = %q, want scan", got)
			}
			// §2.5 步骤 1 特例:Accept 覆盖为 text/html 形态,其余按 §2.3 模板。
			if got := r.Header.Get("Accept"); got != "text/html,application/xhtml+xml,*/*;q=0.8" {
				t.Errorf("步骤 1 Accept = %q, want text/html 形态", got)
			}
			if got := r.Header.Get("Origin"); got != "https://pan.quark.cn" {
				t.Errorf("步骤 1 Origin = %q, want https://pan.quark.cn", got)
			}
			if got := r.Header.Get("User-Agent"); got != UserAgent {
				t.Errorf("步骤 1 User-Agent = %q, want 客户端 UA", got)
			}
			for _, c := range []string{"__pus", "__kp", "__kps", "__ktd", "__uid", "ctoken"} {
				http.SetCookie(w, &http.Cookie{Name: c, Value: c + "-v"})
			}
			fmt.Fprint(w, `{"status":200,"code":0,"message":"ok",`+
				`"data":{"nickname":"飞舞用户","avatarUri":"https://avatar.example/u.png"}}`)
		case "/list":
			// 步骤 2a:Cookie 整串应带上步骤 1 刚收到的 __pus。
			if got := r.Header.Get("Cookie"); !strings.Contains(got, "__pus=__pus-v") {
				t.Errorf("步骤 2a Cookie = %q, 应含步骤 1 收到的 __pus", got)
			}
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "puus-step2a"})
			fmt.Fprint(w, `<html>list</html>`)
		case "/1/clouddrive/file/sort":
			// 步骤 2b:查询参数逐字按 §2.5。
			q := r.URL.Query()
			for k, want := range map[string]string{
				"pr": "ucpro", "fr": "pc", "uc_param_str": "",
				"pdir_fid": "0", "_page": "1", "_size": "50",
				"_fetch_total": "1", "_sort": "file_type:asc,updated_at:desc",
			} {
				if got := q.Get(k); got != want {
					t.Errorf("步骤 2b 参数 %s = %q, want %q", k, got, want)
				}
			}
			http.SetCookie(w, &http.Cookie{Name: "sort_c", Value: "sv"})
			fmt.Fprint(w, `{"status":200,"code":0,"data":{}}`)
		case "/1/clouddrive/member":
			q := r.URL.Query()
			if got := q.Get("fetch_subscribe"); got != "true" {
				t.Errorf("步骤 2c fetch_subscribe = %q, want true", got)
			}
			if got := q.Get("pr"); got != "ucpro" {
				t.Errorf("步骤 2c pr = %q, want ucpro", got)
			}
			http.SetCookie(w, &http.Cookie{Name: "member_c", Value: "mv"})
			fmt.Fprint(w, `{"status":200,"code":0,"data":{}}`)
		default:
			t.Errorf("exchange fake 收到意外路径 %s", r.URL.Path)
		}
	})
}

func TestExchangeCookieSuccess(t *testing.T) {
	srv := httptest.NewServer(fakeExchange(t))
	defer srv.Close()

	s := newExchangeSession(t, srv)
	info, err := s.ExchangeCookie(context.Background(), "TICKET456")
	if err != nil {
		t.Fatalf("ExchangeCookie: %v", err)
	}
	// 账号信息取自步骤 1 响应 data(§2.5)。
	if info.Nickname != "飞舞用户" {
		t.Errorf("Nickname = %q, want 飞舞用户", info.Nickname)
	}
	if info.AvatarURI != "https://avatar.example/u.png" {
		t.Errorf("AvatarURI = %q, want https://avatar.example/u.png", info.AvatarURI)
	}
	// 成功判据:jar 含 __pus;步骤 2 补全的 __puus 也应入串。
	if !s.jar.Has("__pus") {
		t.Errorf("成功判据失败,jar 应含 __pus,String() = %q", s.jar.String())
	}
	if !s.jar.Has("__puus") {
		t.Errorf("jar 应含步骤 2a 下发的 __puus,String() = %q", s.jar.String())
	}
}

func TestExchangeCookieFail(t *testing.T) {
	// 步骤 1 直接 500:应立即报含步骤明细的错误,不再发起步骤 2 的三个请求。
	var n sync.Mutex
	paths := make([]string, 0, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Lock()
		paths = append(paths, r.URL.Path)
		n.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newExchangeSession(t, srv)
	_, err := s.ExchangeCookie(context.Background(), "TICKET456")
	if err == nil {
		t.Fatal("步骤 1 返回 500 时应报错")
	}
	for _, sub := range []string{"步骤 1", "account/info", "500"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("错误信息应含 %q,实际: %v", sub, err)
		}
	}
	if len(paths) != 1 || paths[0] != "/account/info" {
		t.Errorf("步骤 1 失败后不应继续步骤 2,实际请求 = %v", paths)
	}
	if s.jar.Has("__pus") {
		t.Error("失败路径下 jar 不应拿到 __pus")
	}
}
