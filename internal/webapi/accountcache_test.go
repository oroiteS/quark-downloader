// accountcache_test.go 昵称/头像持久缓存（<DataDir>/account.json）钉死：
// 登录链（扫码/导入）写盘、重启后（新 App 实例）handleAccount 空昵称
// 回退到盘缓存、损坏文件不炸、QuickCheck 非空 info 刷新盘。全部离线：
// auth 走 fakeAuth、凭据走 fakeSecrets、数据目录走 t.TempDir。
package webapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"quark-downloader/internal/protocol"
	"quark-downloader/internal/store"
)

// newAuthTestAppAt 在指定数据目录装配注入 fake 的 App（newAuthTestApp
// 的落盘版：paths.DataDir 指向临时目录，缓存读写有真实落点）。
func newAuthTestAppAt(t *testing.T, dir string, fa *fakeAuth, fs *fakeSecrets) *App {
	t.Helper()
	app := newAuthTestApp(t, fa, fs)
	app.paths = store.Paths{DataDir: dir}
	return app
}

// qrLoginVia 扫码登录到 GotTicket 终态（exchangeF 注入昵称/头像进 jar），
// 返回 state 响应 recorder。
func qrLoginVia(t *testing.T, app *App, fa *fakeAuth, nickname, avatar string) {
	t.Helper()
	fa.startQR = &fakeQR{
		scanURL: qrScanURL,
		results: []qrResult{{state: protocol.QRGotTicket, ticket: "tk-disk"}},
	}
	fa.exchangeF = func(string) (protocol.AccountInfo, error) {
		fa.mu.Lock()
		jar := fa.jar
		fa.mu.Unlock()
		jar.LoadFromString("__puus=tok")
		return protocol.AccountInfo{Nickname: nickname, AvatarURI: avatar}, nil
	}
	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/qr/start", nil); w.Code != 200 {
		t.Fatalf("start 状态码 = %d", w.Code)
	}
	if w := doReq(t, app.Router(), http.MethodGet, "/api/login/qr/state", nil); w.Code != 200 {
		t.Fatalf("state 状态码 = %d（body=%s）", w.Code, w.Body)
	}
}

// readAccountProfileFromDisk 读盘缓存文件原文（测试断言用）。
func readAccountProfileFromDisk(t *testing.T, dir string) accountProfile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, accountJSONName))
	if err != nil {
		t.Fatalf("读 account.json: %v", err)
	}
	var p accountProfile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("account.json 不是合法 JSON: %v（body=%s）", err, b)
	}
	return p
}

// TestQRLoginPersistsAccountProfile 登录链成功写缓存：ExchangeCookie 拿到
// info 后 account.json 落盘（昵称/头像），权限 0600。
func TestQRLoginPersistsAccountProfile(t *testing.T) {
	dir := t.TempDir()
	fa := &fakeAuth{}
	fs := &fakeSecrets{}
	app := newAuthTestAppAt(t, dir, fa, fs)
	defer stopPersistGoroutine(app)

	qrLoginVia(t, app, fa, "扫码昵称", "https://avatar/y.png")

	p := readAccountProfileFromDisk(t, dir)
	if p.Nickname != "扫码昵称" || p.Avatar != "https://avatar/y.png" {
		t.Fatalf("account.json = %+v, 期望 {扫码昵称, https://avatar/y.png}", p)
	}
	fi, err := os.Stat(filepath.Join(dir, accountJSONName))
	if err != nil {
		t.Fatalf("stat account.json: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("account.json 权限 = %o, 期望 600（账号隐私面）", got)
	}
}

// TestAccountFallsBackToDiskCacheAfterRestart 重启回退链（T15 主链）：
// app1 扫码登录写下盘缓存 → app2（新 App 实例，模拟重启：进程内
// cachedAccount 为空）QuickCheck 走兜底返回零值 info → 昵称/头像回退盘
// 缓存，登录态与昵称同时幸存。
func TestAccountFallsBackToDiskCacheAfterRestart(t *testing.T) {
	dir := t.TempDir()
	fa1 := &fakeAuth{}
	app1 := newAuthTestAppAt(t, dir, fa1, &fakeSecrets{})
	qrLoginVia(t, app1, fa1, "盘上昵称", "https://avatar/z.png")
	stopPersistGoroutine(app1)

	// 重启：全新 App + 全新 fakeAuth，凭据从 SecretStore 恢复（jar 含 __puus）。
	fa2 := &fakeAuth{
		// 真机场景：QuickCheck wall 域失败走 probeFileSort 兜底——ok 但零值 info。
		quickF: func() (bool, protocol.AccountInfo) { return true, protocol.AccountInfo{} },
		capF:   func() (int64, int64, error) { return 100, 1, nil },
	}
	app2 := newAuthTestAppAt(t, dir, fa2, &fakeSecrets{})
	app2.jar.LoadFromString("__puus=tok")

	w := doReq(t, app2.Router(), http.MethodGet, "/api/account", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("account 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	var body struct {
		LoggedIn bool   `json:"logged_in"`
		Nickname string `json:"nickname"`
		Avatar   string `json:"avatar"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.LoggedIn || body.Nickname != "盘上昵称" || body.Avatar != "https://avatar/z.png" {
		t.Fatalf("账号信息 = %+v, 期望昵称/头像回退盘缓存", body)
	}
}

// TestAccountCorruptDiskCacheNotFatal 损坏文件不炸：account.json 为坏
// JSON 时 handleAccount 照常 200（昵称空 → 前端占位兜底），不 5xx 不
// panic；后续登录链刷新可自愈覆盖。
func TestAccountCorruptDiskCacheNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, accountJSONName), []byte("{不是 json"), 0o600); err != nil {
		t.Fatalf("预置损坏文件: %v", err)
	}
	fa := &fakeAuth{
		quickF: func() (bool, protocol.AccountInfo) { return true, protocol.AccountInfo{} },
		capF:   func() (int64, int64, error) { return 100, 1, nil },
	}
	app := newAuthTestAppAt(t, dir, fa, &fakeSecrets{})
	app.jar.LoadFromString("__puus=tok")

	w := doReq(t, app.Router(), http.MethodGet, "/api/account", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("account 状态码 = %d, 期望 200（损坏缓存按空处理不报错）（body=%s）", w.Code, w.Body)
	}
	var body struct {
		Nickname string `json:"nickname"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Nickname != "" {
		t.Fatalf("nickname = %q, 期望空（损坏缓存按零值处理）", body.Nickname)
	}

	// 自愈：重新登录链刷新盘缓存为真值。
	qrLoginVia(t, app, fa, "自愈昵称", "")
	stopPersistGoroutine(app)
	if p := readAccountProfileFromDisk(t, dir); p.Nickname != "自愈昵称" {
		t.Fatalf("损坏后重新登录未覆盖盘缓存 = %+v, 期望 自愈昵称", p)
	}
}

// TestCookieImportPersistsAccountProfile 导入链写缓存：handleCookieImport
// 成功后经 QuickCheck 补取 info 落盘（导入没有 ExchangeCookie 的顺带
// info）；QuickCheck 失败不阻断导入（200 照常返回）。
func TestCookieImportPersistsAccountProfile(t *testing.T) {
	dir := t.TempDir()
	fa := &fakeAuth{
		quickF: func() (bool, protocol.AccountInfo) {
			return true, protocol.AccountInfo{Nickname: "导入昵称", AvatarURI: "https://avatar/i.png"}
		},
	}
	app := newAuthTestAppAt(t, dir, fa, &fakeSecrets{})
	defer stopPersistGoroutine(app)

	if w := doReq(t, app.Router(), http.MethodPost, "/api/login/cookie",
		map[string]string{"text": "__puus=u; __pus=p"}); w.Code != http.StatusOK {
		t.Fatalf("导入 Cookie 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	if p := readAccountProfileFromDisk(t, dir); p.Nickname != "导入昵称" || p.Avatar != "https://avatar/i.png" {
		t.Fatalf("account.json = %+v, 期望 {导入昵称, https://avatar/i.png}", p)
	}

	// QuickCheck 失败：导入不受阻，盘缓存保留旧值。
	fa2 := &fakeAuth{
		quickF: func() (bool, protocol.AccountInfo) { return false, protocol.AccountInfo{} },
	}
	app2 := newAuthTestAppAt(t, dir, fa2, &fakeSecrets{})
	defer stopPersistGoroutine(app2)
	if w := doReq(t, app2.Router(), http.MethodPost, "/api/login/cookie",
		map[string]string{"text": "__puus=u2"}); w.Code != http.StatusOK {
		t.Fatalf("QuickCheck 失败时导入状态码 = %d, 期望 200（昵称补取不阻断导入）（body=%s）", w.Code, w.Body)
	}
	if p := readAccountProfileFromDisk(t, dir); p.Nickname != "导入昵称" {
		t.Fatalf("QuickCheck 失败不应覆盖盘缓存 = %+v, 期望保留 导入昵称", p)
	}
}

// TestAccountQuickCheckRefreshesDiskCache handleAccount 拿到非空 info 时
// 刷新盘缓存（旧值被协议层现值覆盖）。
func TestAccountQuickCheckRefreshesDiskCache(t *testing.T) {
	dir := t.TempDir()
	fa := &fakeAuth{}
	app := newAuthTestAppAt(t, dir, fa, &fakeSecrets{})
	defer stopPersistGoroutine(app)

	qrLoginVia(t, app, fa, "旧昵称", "")
	fa.quickF = func() (bool, protocol.AccountInfo) {
		return true, protocol.AccountInfo{Nickname: "新昵称", AvatarURI: "https://avatar/n.png"}
	}
	fa.capF = func() (int64, int64, error) { return 100, 1, nil }

	if w := doReq(t, app.Router(), http.MethodGet, "/api/account", nil); w.Code != http.StatusOK {
		t.Fatalf("account 状态码 = %d（body=%s）", w.Code, w.Body)
	}
	if p := readAccountProfileFromDisk(t, dir); p.Nickname != "新昵称" || p.Avatar != "https://avatar/n.png" {
		t.Fatalf("account.json = %+v, 期望被 QuickCheck 现值刷新为 {新昵称, https://avatar/n.png}", p)
	}
}

// TestRefreshAccountCacheSkipsZeroInfo 零值 info 不落盘：防把盘上旧缓存
// （上次登录的昵称）抹成空（refreshAccountCache 内建守卫）。
func TestRefreshAccountCacheSkipsZeroInfo(t *testing.T) {
	dir := t.TempDir()
	fa := &fakeAuth{}
	app := newAuthTestAppAt(t, dir, fa, &fakeSecrets{})

	qrLoginVia(t, app, fa, "旧昵称", "")
	stopPersistGoroutine(app)

	app.refreshAccountCache(protocol.AccountInfo{}) // 零值：只动进程内层
	if p := readAccountProfileFromDisk(t, dir); p.Nickname != "旧昵称" {
		t.Fatalf("零值 info 覆盖了盘缓存 = %+v, 期望保留 旧昵称", p)
	}
}
