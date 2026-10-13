package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// decodeError 解统一错误结构 {error:{code,message}}。
func decodeError(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var p struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("错误结构不是合法 JSON: %v（body=%s）", err, body)
	}
	return p.Error.Code, p.Error.Message
}

// TestRouterHealth /api/health 冒烟：200 {"status":"ok"}（Task 1 既有
// 验收语义迁移到真 router）。
func TestRouterHealth(t *testing.T) {
	app := New(Deps{})
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, 期望 application/json; charset=utf-8", ct)
	}
	if want := `{"status":"ok"}`; w.Body.String() != want {
		t.Fatalf("响应体 = %q, 期望 %q", w.Body.String(), want)
	}
}

// TestAPIErrorShape /api 未知路径 → 404 + 统一错误结构 {error:{code,message}}。
func TestAPIErrorShape(t *testing.T) {
	app := New(Deps{})
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/definitely-missing", nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404", w.Code)
	}
	code, _ := decodeError(t, w.Body.Bytes())
	if code != "not_found" {
		t.Fatalf("error.code = %q, 期望 not_found", code)
	}
}

// TestPanicRecoveryErrorShape handler panic → 500 + 统一错误结构（不泄漏
// 堆栈给客户端）。
func TestPanicRecoveryErrorShape(t *testing.T) {
	app := New(Deps{})
	r := app.Router()
	r.GET("/api/__boom", func(c *gin.Context) { panic("boom") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/__boom", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500", w.Code)
	}
	code, _ := decodeError(t, w.Body.Bytes())
	if code != "internal" {
		t.Fatalf("error.code = %q, 期望 internal", code)
	}
}

// TestStaticRoot GET / → 200 text/html，内容为 embed 的前端构建产物
// index.html（断言 SPA 挂载点，不与构建 hash 字面耦合——重构建不红）。
func TestStaticRoot(t *testing.T) {
	app := New(Deps{})
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, 期望 text/html; charset=utf-8", ct)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`<div id="root">`)) {
		t.Fatalf("响应体缺 SPA 挂载点 <div id=\"root\">: %q", w.Body.String())
	}
}

// TestSPAFallback SPA fallback：非 /api 前缀的未匹配路径回落 index.html
// （前端 history 路由）；/api 前缀绝不回落（保持 JSON 错误语义）。
func TestSPAFallback(t *testing.T) {
	app := New(Deps{})

	// 非 /api 深层路径 → index.html（200）。
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tasks/abc/detail", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/tasks/abc/detail 状态码 = %d, 期望 200（fallback）", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`<div id="root">`)) {
		t.Fatalf("/tasks/abc/detail 应回落 index.html(缺挂载点), 得到 %q", w.Body.String())
	}

	// /api 前缀 → JSON 404，绝不回落 HTML。
	w2 := httptest.NewRecorder()
	app.Router().ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/missing", nil))
	if w2.Code != http.StatusNotFound {
		t.Fatalf("/api/missing 状态码 = %d, 期望 404", w2.Code)
	}
	if code, _ := decodeError(t, w2.Body.Bytes()); code != "not_found" {
		t.Fatalf("/api/missing error.code = %q, 期望 not_found", code)
	}

	// /apifoo 是 /api 的字符串前缀但不是 API 命名空间 → 仍走 SPA fallback。
	w3 := httptest.NewRecorder()
	app.Router().ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/apifoo", nil))
	if w3.Code != http.StatusOK || !bytes.Contains(w3.Body.Bytes(), []byte(`<div id="root">`)) {
		t.Fatalf("/apifoo 应回落 index.html, 得到 %d %q", w3.Code, w3.Body.String())
	}
}

// TestStaticAssetFromDist dist 内存在的文件按静态资产返回（embed 路径
// 前缀透传），占位阶段 dist 只有 index.html——用 SPA fallback 的反例
// 锚定：不存在的资产路径回落 index.html，存在则返回文件本身。
func TestStaticAssetFromDist(t *testing.T) {
	app := New(Deps{})
	// 占位 dist 无资产文件：任意资产路径回落 index.html（200）。
	w := httptest.NewRecorder()
	app.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/assets/app.js 状态码 = %d, 期望 200（占位阶段回落）", w.Code)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("html")) {
		t.Fatalf("/assets/app.js 应回落 index.html, 得到 %q", w.Body.String())
	}
}

// TestAppLifecycleHooks App 生命周期入口（裁决 P1）：New 装配、Router
// 可用、Start 恢复链在已取消 ctx/无凭据下也无害（D9 后 restoreSession
// 有实义，凭据面按装配前置条件注入 fake）、Close 幂等。
func TestAppLifecycleHooks(t *testing.T) {
	app := New(Deps{})
	app.secrets = &fakeSecrets{} // Start 恢复链读凭据面（与 main 装配对齐）
	if app.Router() == nil {
		t.Fatal("Router() 返回 nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消的 ctx：恢复链不感知取消（keepalive 走独立取消链）
	if err := app.Start(ctx); err != nil {
		t.Fatalf("Start = %v, 期望 nil（无凭据静默）", err)
	}

	app.Close()
	app.Close() // 幂等
}
