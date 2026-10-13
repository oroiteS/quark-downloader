package webapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"quark-downloader/internal/protocol"
)

// ---- 注入 fake ----

// fakeQR 编排式扫码会话：Poll 依次吐出预设序列，耗尽后兜底 waiting。
type fakeQR struct {
	mu      sync.Mutex
	scanURL string
	results []qrResult
	next    int
	polls   int
}

type qrResult struct {
	state  protocol.QRState
	ticket string
}

func (f *fakeQR) Poll(context.Context) (protocol.QRState, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.next >= len(f.results) {
		return protocol.QRWaiting, "", nil
	}
	r := f.results[f.next]
	f.next++
	return r.state, r.ticket, nil
}

func (f *fakeQR) ScanURL() string { return f.scanURL }

// fakeAuth authAPI 的假实现：可绑定真 CookieJar（LoginState 随 jar 走，
// 与真 session 一致），可注入各方法行为。
type fakeAuth struct {
	mu         sync.Mutex
	jar        *protocol.CookieJar // 绑定后 LoginState 读 __puus
	startQR    *fakeQR
	startErr   error
	exchangeF  func(ticket string) (protocol.AccountInfo, error)
	exchanged  []string
	quickF     func() (bool, protocol.AccountInfo)
	capF       func() (int64, int64, error)
	kaStarted  int
	kaCtx      context.Context
	kaOnExpire func() // 捕获 onExpire 供测试手动触发（App 层驱动登录过期）
}

func (f *fakeAuth) StartQRLogin(context.Context) (qrSession, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f.startQR, nil
}

func (f *fakeAuth) ExchangeCookie(_ context.Context, ticket string) (protocol.AccountInfo, error) {
	f.mu.Lock()
	f.exchanged = append(f.exchanged, ticket)
	fn := f.exchangeF
	f.mu.Unlock()
	if fn != nil {
		return fn(ticket)
	}
	return protocol.AccountInfo{Nickname: "默认昵称"}, nil
}

func (f *fakeAuth) QuickCheck(context.Context) (bool, protocol.AccountInfo, error) {
	if f.quickF != nil {
		ok, info := f.quickF()
		return ok, info, nil
	}
	return true, protocol.AccountInfo{}, nil
}

func (f *fakeAuth) Capacity(context.Context) (int64, int64, error) {
	if f.capF != nil {
		return f.capF()
	}
	return 0, 0, nil
}

func (f *fakeAuth) LoginState() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jar != nil {
		return f.jar.Has("__puus")
	}
	return false
}

func (f *fakeAuth) StartKeepalive(ctx context.Context, onExpire func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kaStarted++
	f.kaCtx = ctx
	f.kaOnExpire = onExpire
}

// fakeSecrets SecretStore 假实现：记录每次保存。
type fakeSecrets struct {
	mu    sync.Mutex
	saved []string
	err   error
}

func (f *fakeSecrets) SaveCookie(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, s)
	return f.err
}

func (f *fakeSecrets) LoadCookie() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) == 0 {
		return ""
	}
	return f.saved[len(f.saved)-1]
}

func (f *fakeSecrets) lastSaved() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) == 0 {
		return ""
	}
	return f.saved[len(f.saved)-1]
}

// newAuthTestApp 装配注入 fake 的 App：auth 全走 fa、凭据全走 fs。
// fakeAuth 绑定 App 的 jar（resetSession 每次重建时经工厂重绑）。
func newAuthTestApp(t *testing.T, fa *fakeAuth, fs *fakeSecrets) *App {
	t.Helper()
	app := New(Deps{})
	app.secrets = fs
	app.newAuthPair = func(jar *protocol.CookieJar) (*protocol.Session, authAPI) {
		fa.mu.Lock()
		fa.jar = jar
		fa.mu.Unlock()
		return nil, fa
	}
	app.resetSession() // 以注入工厂重建会话（New 默认建的生产对弃用）
	return app
}

// doReq 发一个 JSON 请求。
func doReq(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal 请求体: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
	return w
}

// qrScanURL 带 token 的扫码页 URL（§2.2 模板形态）。
const qrScanURL = "https://su.quark.cn/4_eMHBJ?token=tok-abc-123&client_id=532" +
	"&ssb=weblogin&uc_param_str=" +
	"&uc_biz_str=S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0"

// ---- POST /api/login/qr/start ----

// TestQRStartResponseShape qr/start 契约：{token, qrcode_png(base64), scan_url}，
// PNG 为 512×512 可解码图片。
func TestQRStartResponseShape(t *testing.T) {
	fa := &fakeAuth{startQR: &fakeQR{scanURL: qrScanURL}}
	app := newAuthTestApp(t, fa, &fakeSecrets{})

	w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	var body struct {
		Token     string `json:"token"`
		QrcodePng string `json:"qrcode_png"`
		ScanURL   string `json:"scan_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, w.Body)
	}
	if body.Token != "tok-abc-123" {
		t.Fatalf("token = %q, 期望 tok-abc-123", body.Token)
	}
	if body.ScanURL != qrScanURL {
		t.Fatalf("scan_url = %q, 期望原样返回扫码页 URL", body.ScanURL)
	}
	png, err := base64.StdEncoding.DecodeString(body.QrcodePng)
	if err != nil {
		t.Fatalf("qrcode_png 不是合法 base64: %v", err)
	}
	if magic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}; !bytes.HasPrefix(png, magic) {
		t.Fatalf("qrcode_png 解码后非 PNG（头 8 字节 = %x）", png[:8])
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(png))
	if err != nil {
		t.Fatalf("PNG 解码失败: %v", err)
	}
	if cfg.Width != 512 || cfg.Height != 512 {
		t.Fatalf("PNG 尺寸 = %dx%d, 期望 512x512（QRPNG 规格）", cfg.Width, cfg.Height)
	}
}

// TestQRStartUpstreamError 取 token 失败 → 502 + 统一错误结构。
func TestQRStartUpstreamError(t *testing.T) {
	fa := &fakeAuth{startErr: errors.New("uop HTTP 500")}
	app := newAuthTestApp(t, fa, &fakeSecrets{})

	w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", w.Code)
	}
	if code, _ := decodeError(t, w.Body.Bytes()); code != "qr_start_failed" {
		t.Fatalf("error.code = %q, 期望 qr_start_failed", code)
	}
}

// TestQRStartReplacesSession 重复 start = 刷新二维码：旧会话作废、
// 新会话接管 state 轮询。
func TestQRStartReplacesSession(t *testing.T) {
	qr1 := &fakeQR{scanURL: qrScanURL, results: []qrResult{{state: protocol.QRWaiting}}}
	qr2 := &fakeQR{scanURL: qrScanURL, results: []qrResult{{state: protocol.QRScanned}}}
	fa := &fakeAuth{startQR: qr1}
	app := newAuthTestApp(t, fa, &fakeSecrets{})

	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
		t.Fatalf("第一次 start 状态码 = %d", w.Code)
	}
	fa.mu.Lock()
	fa.startQR = qr2
	fa.mu.Unlock()
	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
		t.Fatalf("第二次 start 状态码 = %d", w.Code)
	}

	w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
	var body struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("state 响应不是合法 JSON: %v", err)
	}
	if body.State != "scanned" {
		t.Fatalf("state = %q, 期望 scanned（新会话接管）", body.State)
	}
}

// ---- GET /api/login/qr/state ----

// TestQRStateSixStatesMapping 六态映射（fake Poll）：
// waiting/scanned 非终态保留会话可继续轮询；invalid/expired/timeout 终态
// 后会话清除（再轮询 400，须重新 start）。
func TestQRStateSixStatesMapping(t *testing.T) {
	cases := []struct {
		protocol.QRState
		want string
		term bool // 是否终态
	}{
		{protocol.QRWaiting, "waiting", false},
		{protocol.QRScanned, "scanned", false},
		{protocol.QRInvalid, "invalid", true},
		{protocol.QRExpired, "expired", true},
		{protocol.QRTimeout, "timeout", true},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			fa := &fakeAuth{startQR: &fakeQR{
				scanURL: qrScanURL,
				results: []qrResult{{state: tc.QRState}, {state: tc.QRState}},
			}}
			app := newAuthTestApp(t, fa, &fakeSecrets{})
			if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
				t.Fatalf("start 状态码 = %d", w.Code)
			}

			w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
			if w.Code != 200 {
				t.Fatalf("state 状态码 = %d（body=%s）", w.Code, w.Body)
			}
			var body struct {
				State    string `json:"state"`
				LoggedIn bool   `json:"logged_in"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("state 响应不是合法 JSON: %v", err)
			}
			if body.State != tc.want {
				t.Fatalf("state = %q, 期望 %q", body.State, tc.want)
			}
			if body.LoggedIn {
				t.Fatalf("%s 态 logged_in = true, 期望 false", tc.want)
			}

			w2 := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
			if tc.term {
				if w2.Code != http.StatusBadRequest {
					t.Fatalf("%s 终态后第二次 state 状态码 = %d, 期望 400（会话已清除）", tc.want, w2.Code)
				}
				if code, _ := decodeError(t, w2.Body.Bytes()); code != "no_qr_session" {
					t.Fatalf("终态后 error.code = %q, 期望 no_qr_session", code)
				}
			} else if w2.Code != 200 {
				t.Fatalf("%s 非终态第二次 state 状态码 = %d, 期望 200（会话保留）", tc.want, w2.Code)
			}
		})
	}
}

// TestQRStateWithoutStart 未 start 直接轮询 → 400。
func TestQRStateWithoutStart(t *testing.T) {
	app := newAuthTestApp(t, &fakeAuth{}, &fakeSecrets{})
	w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", w.Code)
	}
	if code, _ := decodeError(t, w.Body.Bytes()); code != "no_qr_session" {
		t.Fatalf("error.code = %q, 期望 no_qr_session", code)
	}
}

// TestQRStateGotTicketFullChain GotTicket 自动链（简报逐字）：
// 服务端自动 ExchangeCookie → SaveCookie → StartKeepalive →
// 返回 {state:"got_ticket", logged_in:true, nickname}；ticket 一次性消费。
func TestQRStateGotTicketFullChain(t *testing.T) {
	fa := &fakeAuth{startQR: &fakeQR{
		scanURL: qrScanURL,
		results: []qrResult{{state: protocol.QRGotTicket, ticket: "ticket-xyz"}},
	}}
	fa.exchangeF = func(ticket string) (protocol.AccountInfo, error) {
		if ticket != "ticket-xyz" {
			return protocol.AccountInfo{}, errors.New("ticket 不符")
		}
		// 模拟真实 ExchangeCookie：登录态 Cookie 进 jar。
		fa.mu.Lock()
		jar := fa.jar
		fa.mu.Unlock()
		jar.LoadFromString("__puus=tok; __pus=p")
		return protocol.AccountInfo{Nickname: "飞舞用户"}, nil
	}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
		t.Fatalf("start 状态码 = %d", w.Code)
	}
	w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
	if w.Code != 200 {
		t.Fatalf("state 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	var body struct {
		State    string `json:"state"`
		LoggedIn bool   `json:"logged_in"`
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("state 响应不是合法 JSON: %v", err)
	}
	if body.State != "got_ticket" || !body.LoggedIn || body.Nickname != "飞舞用户" {
		t.Fatalf("响应 = {state:%q, logged_in:%v, nickname:%q}, 期望 {got_ticket, true, 飞舞用户}",
			body.State, body.LoggedIn, body.Nickname)
	}

	fa.mu.Lock()
	exchanged := append([]string(nil), fa.exchanged...)
	kaStarted := fa.kaStarted
	fa.mu.Unlock()
	if len(exchanged) != 1 || exchanged[0] != "ticket-xyz" {
		t.Fatalf("ExchangeCookie 调用 = %v, 期望 [ticket-xyz]", exchanged)
	}
	if kaStarted != 1 {
		t.Fatalf("StartKeepalive 调用 %d 次, 期望 1", kaStarted)
	}
	if got := fs.lastSaved(); got != "__pus=p; __puus=tok" {
		t.Fatalf("保存的 Cookie = %q, 期望 jar 整串 __pus=p; __puus=tok", got)
	}

	// ticket 已消费：会话清除，再轮询 400。
	w2 := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("got_ticket 后再 state 状态码 = %d, 期望 400", w2.Code)
	}
}

// ---- POST /api/login/cookie ----

// TestCookieImportFormats 四格式各一例（HTTP 层）：解析 → jar → 登录 →
// 保存 + keepalive → 200 {logged_in:true}。
func TestCookieImportFormats(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // 保存进 SecretStore 的整串
	}{
		{
			name: "① JSON 数组(含非 quark 条目过滤)",
			text: `[{"name":"__puus","value":"u1","domain":".quark.cn"},
			       {"name":"baidu_key","value":"x","domain":".baidu.com"}]`,
			want: "__puus=u1",
		},
		{
			name: "② JSON 对象",
			text: `{"__puus":"u2","a":"1"}`,
			want: "__puus=u2; a=1",
		},
		{
			name: "③ 嵌套 cookie 串",
			text: `{"cookie":"__puus=u3; b=2"}`,
			want: "__puus=u3; b=2",
		},
		{
			name: "④ 原始串(Cookie 前缀+跨行)",
			text: "Cookie: __puus=u4;\r\nc=3\n",
			want: "__puus=u4; c=3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeAuth{}
			fs := &fakeSecrets{}
			app := newAuthTestApp(t, fa, fs)

			w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie", map[string]string{"text": tc.text})
			if w.Code != http.StatusOK {
				t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
			}
			var body struct {
				LoggedIn bool `json:"logged_in"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.LoggedIn {
				t.Fatalf("响应 = %s, 期望 {logged_in:true}", w.Body)
			}
			if got := fs.lastSaved(); got != tc.want {
				t.Fatalf("保存的 Cookie = %q, 期望 %q", got, tc.want)
			}
			fa.mu.Lock()
			ka := fa.kaStarted
			fa.mu.Unlock()
			if ka != 1 {
				t.Fatalf("StartKeepalive 调用 %d 次, 期望 1", ka)
			}
		})
	}
}

// TestCookieImportFilters500NonQuark Review Focus #2（HTTP 层钉死）：
// 混入 500 条非 quark 条目 → jar 只含 quark 的条目。
func TestCookieImportFilters500NonQuark(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"junk%d","value":"x","domain":".example.com"}`, i)
	}
	b.WriteString(`,{"name":"__puus","value":"tok","domain":".quark.cn"}`)
	b.WriteString(`,{"name":"__pus","value":"p","domain":"pan.quark.cn"}`)
	b.WriteByte(']')

	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie", map[string]string{"text": b.String()})
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	if got := fs.lastSaved(); got != "__pus=p; __puus=tok" {
		t.Fatalf("保存的 Cookie = %q, 期望 __pus=p; __puus=tok（jar 只含 quark 条目）", got)
	}
	if got := app.jar.String(); got != "__pus=p; __puus=tok" {
		t.Fatalf("jar = %q, 期望只含 quark 条目", got)
	}
}

// TestCookieImportRejectsMissingPuus `__puus` 缺失 → 400 带原因；
// 且不留脏状态：jar 回滚为空、不保存、不启 keepalive。
func TestCookieImportRejectsMissingPuus(t *testing.T) {
	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie",
		map[string]string{"text": `{"a":"1","b":"2"}`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400（body=%s）", w.Code, w.Body)
	}
	code, msg := decodeError(t, w.Body.Bytes())
	if code != "invalid_cookie" {
		t.Fatalf("error.code = %q, 期望 invalid_cookie", code)
	}
	if !strings.Contains(msg, "__puus") {
		t.Fatalf("错误信息 %q 未说明 __puus 缺失原因", msg)
	}

	if got := app.jar.String(); got != "" {
		t.Fatalf("拒绝后 jar = %q, 期望回滚为空（不留半登录态）", got)
	}
	if n := len(fs.saved); n != 0 {
		t.Fatalf("拒绝后仍保存了 %d 次凭据, 期望 0", n)
	}
	fa.mu.Lock()
	ka := fa.kaStarted
	fa.mu.Unlock()
	if ka != 0 {
		t.Fatalf("拒绝后 StartKeepalive 调用 %d 次, 期望 0", ka)
	}
}

// TestCookieImportBadBody 请求体非 {text} 形状/空 text → 400。
func TestCookieImportBadBody(t *testing.T) {
	app := newAuthTestApp(t, &fakeAuth{}, &fakeSecrets{})

	w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie", map[string]string{"text": "   "})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 text 状态码 = %d, 期望 400", w.Code)
	}
	w2 := httptest.NewRecorder()
	app.Router().ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/api/login/cookie", strings.NewReader("不是 json")))
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("非 JSON 状态码 = %d, 期望 400", w2.Code)
	}
	if _, msg := decodeError(t, w2.Body.Bytes()); !strings.Contains(msg, "text") {
		t.Fatalf("错误信息 %q 未提及 text 字段", msg)
	}
}

// ---- POST /api/logout ----

// TestLogoutCleansAll 登出三清（简报逐字）：清 jar、删 SecretStore 凭据、
// 停 keepalive（ctx 取消）。
func TestLogoutCleansAll(t *testing.T) {
	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	// 先进入登录态：导入 Cookie 成功并启动 keepalive。
	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie",
		map[string]string{"text": "__puus=u; __pus=p"}); w.Code != 200 {
		t.Fatalf("导入 Cookie 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	fa.mu.Lock()
	kaCtx := fa.kaCtx
	fa.mu.Unlock()
	if kaCtx == nil {
		t.Fatal("导入成功后 keepalive 未启动")
	}

	w := doReq(t, app.Router(), http.MethodPost, "/api/logout", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("logout 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	var body struct {
		LoggedIn bool `json:"logged_in"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.LoggedIn {
		t.Fatalf("logout 响应 = %s, 期望 {logged_in:false}", w.Body)
	}

	// ① 清 jar。
	if got := app.jar.String(); got != "" {
		t.Fatalf("logout 后 jar = %q, 期望空", got)
	}
	// ② 删凭据（SaveCookie("") 覆盖 → LoadCookie 空 = 未登录）。
	if got := fs.lastSaved(); got != "" {
		t.Fatalf("logout 后保存的凭据 = %q, 期望空串", got)
	}
	if got := fs.LoadCookie(); got != "" {
		t.Fatalf("logout 后 LoadCookie = %q, 期望空串", got)
	}
	// ③ 停 keepalive：ctx 已取消。
	select {
	case <-kaCtx.Done():
	default:
		t.Fatal("logout 后 keepalive ctx 未取消")
	}
	// 进行中的扫码会话一并作废。
	if w2 := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil); w2.Code != 400 {
		t.Fatalf("logout 后 state 状态码 = %d, 期望 400", w2.Code)
	}
}

// ---- GET /api/account ----

// TestAccountNotLoggedIn 未登录 → 401 + code not_logged_in。
func TestAccountNotLoggedIn(t *testing.T) {
	app := newAuthTestApp(t, &fakeAuth{}, &fakeSecrets{})
	w := doReq(t, app.Router(), http.MethodGet, "/api/account", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", w.Code)
	}
	if code, _ := decodeError(t, w.Body.Bytes()); code != "not_logged_in" {
		t.Fatalf("error.code = %q, 期望 not_logged_in", code)
	}
}

// TestAccountLoggedIn 已登录 → QuickCheck + 昵称头像 + Capacity。
func TestAccountLoggedIn(t *testing.T) {
	fa := &fakeAuth{
		quickF: func() (bool, protocol.AccountInfo) {
			return true, protocol.AccountInfo{Nickname: "妮可", AvatarURI: "https://avatar/x.png"}
		},
		capF: func() (int64, int64, error) { return 1099511627776, 400 << 20, nil },
	}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)
	app.jar.LoadFromString("__puus=tok") // 进入登录态

	w := doReq(t, app.Router(), http.MethodGet, "/api/account", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（body=%s）", w.Code, w.Body)
	}
	var body struct {
		LoggedIn bool   `json:"logged_in"`
		Nickname string `json:"nickname"`
		Avatar   string `json:"avatar"`
		Capacity struct {
			Total int64 `json:"total"`
			Used  int64 `json:"used"`
		} `json:"capacity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, w.Body)
	}
	if !body.LoggedIn || body.Nickname != "妮可" || body.Avatar != "https://avatar/x.png" {
		t.Fatalf("账号信息 = %+v", body)
	}
	if body.Capacity.Total != 1099511627776 || body.Capacity.Used != 400<<20 {
		t.Fatalf("容量 = %+v, 期望 1TB/400MB", body.Capacity)
	}
}

// TestAccountQuickCheckInvalid QuickCheck 判失效 → 401。
func TestAccountQuickCheckInvalid(t *testing.T) {
	fa := &fakeAuth{quickF: func() (bool, protocol.AccountInfo) { return false, protocol.AccountInfo{} }}
	app := newAuthTestApp(t, fa, &fakeSecrets{})
	app.jar.LoadFromString("__puus=tok")

	w := doReq(t, app.Router(), http.MethodGet, "/api/account", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", w.Code)
	}
	if code, msg := decodeError(t, w.Body.Bytes()); code != "not_logged_in" || !strings.Contains(msg, "失效") {
		t.Fatalf("错误 = %q/%q, 期望 not_logged_in + 失效说明", code, msg)
	}
}

// TestAccountCapacityError Capacity 失败 → 502（账号信息查询链失败不静默）。
func TestAccountCapacityError(t *testing.T) {
	fa := &fakeAuth{capF: func() (int64, int64, error) { return 0, 0, errors.New("member 429") }}
	app := newAuthTestApp(t, fa, &fakeSecrets{})
	app.jar.LoadFromString("__puus=tok")

	w := doReq(t, app.Router(), http.MethodGet, "/api/account", nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", w.Code)
	}
	if code, _ := decodeError(t, w.Body.Bytes()); code != "capacity_failed" {
		t.Fatalf("error.code = %q, 期望 capacity_failed", code)
	}
}

// ---- GotTicket 自动链失败注入（Task 6 遗留 minor 1）----

// TestQRStateGotTicketExchangeFails GotTicket → ExchangeCookie 失败 →
// 500 exchange_failed 且错误信息透传上游明细；ticket 一次性语义不变
// （会话仍终结，再轮询 400）；不保存凭据、不启 keepalive。
func TestQRStateGotTicketExchangeFails(t *testing.T) {
	fa := &fakeAuth{startQR: &fakeQR{
		scanURL: qrScanURL,
		results: []qrResult{{state: protocol.QRGotTicket, ticket: "t-bad"}},
	}}
	fa.exchangeF = func(string) (protocol.AccountInfo, error) {
		return protocol.AccountInfo{}, errors.New("换 Cookie 步骤 1(account/info)失败: HTTP 502")
	}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
		t.Fatalf("start 状态码 = %d", w.Code)
	}
	w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("state 状态码 = %d, 期望 500（body=%s）", w.Code, w.Body)
	}
	code, msg := decodeError(t, w.Body.Bytes())
	if code != "exchange_failed" {
		t.Fatalf("error.code = %q, 期望 exchange_failed", code)
	}
	if !strings.Contains(msg, "account/info") {
		t.Fatalf("错误信息 %q 未透传上游失败明细", msg)
	}

	// ticket 已消费：会话终结，再轮询 400（失败路径同样一次性）。
	if w2 := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil); w2.Code != 400 {
		t.Fatalf("Exchange 失败后再 state 状态码 = %d, 期望 400（会话已终结）", w2.Code)
	}
	if n := len(fs.saved); n != 0 {
		t.Fatalf("失败路径仍保存了 %d 次凭据, 期望 0", n)
	}
	fa.mu.Lock()
	ka := fa.kaStarted
	fa.mu.Unlock()
	if ka != 0 {
		t.Fatalf("失败路径 StartKeepalive 调用 %d 次, 期望 0", ka)
	}
}

// TestQRStateGotTicketSaveCookieFails Exchange 成功（jar 进登录态）→
// SaveCookie 失败 → 500 save_cookie_failed；keepalive 不启动（保活跟着
// 凭据走：没存下来就不该有心跳）。
func TestQRStateGotTicketSaveCookieFails(t *testing.T) {
	fa := &fakeAuth{startQR: &fakeQR{
		scanURL: qrScanURL,
		results: []qrResult{{state: protocol.QRGotTicket, ticket: "t-ok"}},
	}}
	fa.exchangeF = func(string) (protocol.AccountInfo, error) {
		// 模拟真实 ExchangeCookie 成功：登录态 Cookie 进 jar。
		fa.mu.Lock()
		jar := fa.jar
		fa.mu.Unlock()
		jar.LoadFromString("__puus=tok; __pus=p")
		return protocol.AccountInfo{Nickname: "飞舞用户"}, nil
	}
	fs := &fakeSecrets{err: errors.New("keyring 不可用")}
	app := newAuthTestApp(t, fa, fs)

	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
		t.Fatalf("start 状态码 = %d", w.Code)
	}
	w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("state 状态码 = %d, 期望 500（body=%s）", w.Code, w.Body)
	}
	if code, _ := decodeError(t, w.Body.Bytes()); code != "save_cookie_failed" {
		t.Fatalf("error.code = %q, 期望 save_cookie_failed", code)
	}
	fa.mu.Lock()
	ka := fa.kaStarted
	fa.mu.Unlock()
	if ka != 0 {
		t.Fatalf("保存失败仍启动 keepalive（%d 次）, 期望 0", ka)
	}
}

// TestQRStateGotTicketMissingPuusNotLoggedIn 跨链判据统一（Task 6 遗留
// minor 2）：ExchangeCookie 成功返回但 jar 缺 __puus → 不给 logged_in
// （与导入链/恢复链同一判据，§2.6 __puus 唯一判据）：500 带原因、
// 不保存、不启 keepalive。
func TestQRStateGotTicketMissingPuusNotLoggedIn(t *testing.T) {
	fa := &fakeAuth{startQR: &fakeQR{
		scanURL: qrScanURL,
		results: []qrResult{{state: protocol.QRGotTicket, ticket: "t-no-puus"}},
	}}
	fa.exchangeF = func(string) (protocol.AccountInfo, error) {
		// 协议层成功返回，但 jar 未获得 __puus（上游未下发登录态）。
		return protocol.AccountInfo{Nickname: "无登录态"}, nil
	}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
		t.Fatalf("start 状态码 = %d", w.Code)
	}
	w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("state 状态码 = %d, 期望 500（body=%s）", w.Code, w.Body)
	}
	code, msg := decodeError(t, w.Body.Bytes())
	if code != "exchange_failed" {
		t.Fatalf("error.code = %q, 期望 exchange_failed", code)
	}
	if !strings.Contains(msg, "__puus") {
		t.Fatalf("错误信息 %q 未说明 __puus 缺失原因", msg)
	}
	if n := len(fs.saved); n != 0 {
		t.Fatalf("缺 __puus 仍保存了 %d 次凭据, 期望 0", n)
	}
	fa.mu.Lock()
	ka := fa.kaStarted
	fa.mu.Unlock()
	if ka != 0 {
		t.Fatalf("缺 __puus 仍启动 keepalive（%d 次）, 期望 0", ka)
	}
}

// TestResetSessionStopsKeepalive resetSession 先停旧心跳（Task 6 遗留
// minor 3）：登录成功启心跳后，resetSession 重建会话（logout 清场与
// 导入失败回滚共用的重建体）→ 旧 keepalive 的 ctx 已取消、取消开关
// 归零（弃用 session 上不再心跳空跑）。
func TestResetSessionStopsKeepalive(t *testing.T) {
	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	// 进入登录态并启心跳。
	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie",
		map[string]string{"text": "__puus=u; __pus=p"}); w.Code != 200 {
		t.Fatalf("导入 Cookie 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	fa.mu.Lock()
	kaCtx := fa.kaCtx
	fa.mu.Unlock()
	if kaCtx == nil {
		t.Fatal("导入成功后 keepalive 未启动")
	}

	// 重建会话（调用方持锁契约照做）→ 旧心跳应被停掉。
	app.authMu.Lock()
	app.resetSession()
	app.authMu.Unlock()
	select {
	case <-kaCtx.Done():
	default:
		t.Fatal("resetSession 后旧 keepalive ctx 未取消（弃用 session 上心跳空跑）")
	}
	app.authMu.Lock()
	cancel := app.keepaliveCancel
	app.authMu.Unlock()
	if cancel != nil {
		t.Fatal("resetSession 后 keepaliveCancel 应归零")
	}
}

// TestAccountNicknameFallsBackToCached 发现 B 修复（T14 真机暴露）：
// QR 登录链拿到昵称后缓存；account 的 QuickCheck 走 wall 域兜底拿不到
// info（零值）时昵称回退缓存——昵称不再空白。
func TestAccountNicknameFallsBackToCached(t *testing.T) {
	fa := &fakeAuth{startQR: &fakeQR{
		scanURL: qrScanURL,
		results: []qrResult{{state: protocol.QRGotTicket, ticket: "tk-nick"}},
	}}
	fa.exchangeF = func(string) (protocol.AccountInfo, error) {
		fa.mu.Lock()
		jar := fa.jar
		fa.mu.Unlock()
		jar.LoadFromString("__puus=tok")
		return protocol.AccountInfo{Nickname: "扫码昵称"}, nil
	}
	// 真机场景：QuickCheck wall 域失败走 probeFileSort 兜底——ok 但零值 info。
	fa.quickF = func() (bool, protocol.AccountInfo) { return true, protocol.AccountInfo{} }
	fa.capF = func() (int64, int64, error) { return 100, 1, nil }
	app := newAuthTestApp(t, fa, &fakeSecrets{})

	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
		t.Fatalf("start 状态码 = %d", w.Code)
	}
	if w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil); w.Code != 200 {
		t.Fatalf("state 状态码 = %d（body=%s）", w.Code, w.Body)
	}

	w := doReq(t, app.Router(), http.MethodGet, "/api/account", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("account 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	var body struct {
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Nickname != "扫码昵称" {
		t.Fatalf("nickname = %q, 期望回退缓存 \"扫码昵称\"（body=%s）", body.Nickname, w.Body)
	}
}

// ---- 会话凭据周期落盘（重启不丢登录：__puus 轮换型凭据持续同步持久层）----

// stopPersistGoroutine 收尾停掉 keepalive（含落盘 goroutine），防测试
// 进程内 goroutine 拿着短周期 ticker 空转。
func stopPersistGoroutine(app *App) {
	app.authMu.Lock()
	app.stopKeepaliveLocked()
	app.authMu.Unlock()
}

// waitPersisted 轮询等待 fs.saved 新增写入（n0 为基线长度），返回新增
// 切片；2 秒仍无写入则失败（落盘 goroutine 异步执行，轮询等待）。
func waitPersisted(t *testing.T, fs *fakeSecrets, n0 int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		got := append([]string(nil), fs.saved[n0:]...)
		fs.mu.Unlock()
		if len(got) > 0 {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("2 秒内未发生凭据落盘（基线 %d 条后无新增）", n0)
	return nil
}

// TestKeepalivePersistsCookiePeriodically 周期落盘（修复主链）：注入
// 短周期，登录态下断言登录同步写之后仍有落盘发生、内容非空且含
// __puus（轮换票持续进持久层，重启读回的不是登录瞬间的旧票）。
func TestKeepalivePersistsCookiePeriodically(t *testing.T) {
	orig := cookiePersistInterval
	cookiePersistInterval = 10 * time.Millisecond
	defer func() { cookiePersistInterval = orig }()

	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)
	defer stopPersistGoroutine(app)

	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie",
		map[string]string{"text": "__puus=u; __pus=p"}); w.Code != 200 {
		t.Fatalf("导入 Cookie 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	fs.mu.Lock()
	n0 := len(fs.saved) // 登录链同步写基线
	fs.mu.Unlock()

	got := waitPersisted(t, fs, n0)
	for _, s := range got {
		if s == "" || !strings.Contains(s, "__puus") {
			t.Fatalf("周期落盘内容 = %q, 期望非空且含 __puus", s)
		}
	}
}

// TestRestorePersistsCookieImmediately 启动立即落盘（消除「登录后没
// 重启过但轮换已发生」窗口）：恢复链 Start 成功后、周期 tick 到来前
// （周期拉长为 1h 排除周期写）即落盘一次恢复串。
func TestRestorePersistsCookieImmediately(t *testing.T) {
	orig := cookiePersistInterval
	cookiePersistInterval = time.Hour // 周期不可达：写入必来自启动立即轮
	defer func() { cookiePersistInterval = orig }()

	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	if err := fs.SaveCookie("__pus=p; __puus=tok"); err != nil {
		t.Fatalf("预置凭据: %v", err)
	}
	app := newAuthTestApp(t, fa, fs)
	defer stopPersistGoroutine(app)

	fs.mu.Lock()
	n0 := len(fs.saved) // 预置写入基线
	fs.mu.Unlock()

	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := waitPersisted(t, fs, n0)
	if len(got) != 1 || got[0] != "__pus=p; __puus=tok" {
		t.Fatalf("启动落盘 = %q, 期望恰一次写入恢复串 __pus=p; __puus=tok", got)
	}
}

// TestKeepalivePersistSkipsWithoutPuus jar 无 __puus（未登录）时落盘
// goroutine 不写：防把 Store 里的好凭据覆盖为空（空串只由 logout 显式
// 写入，走原路径）。
func TestKeepalivePersistSkipsWithoutPuus(t *testing.T) {
	orig := cookiePersistInterval
	cookiePersistInterval = 5 * time.Millisecond
	defer func() { cookiePersistInterval = orig }()

	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	app := newAuthTestApp(t, fa, fs)

	// 防御路径：keepalive（含落盘 goroutine）在 jar 有 Cookie 但无
	// __puus 的状态下被启动——任何轮次都不得触碰 Store。
	app.authMu.Lock()
	app.jar.LoadFromString("a=1")
	app.startKeepaliveLocked()
	app.authMu.Unlock()

	time.Sleep(25 * time.Millisecond) // 跨越 4-5 个周期轮次
	if n := len(fs.saved); n != 0 {
		t.Fatalf("无 __puus 仍落盘 %d 次, 期望 0（防好凭据被覆盖为空）", n)
	}
	stopPersistGoroutine(app)
}
