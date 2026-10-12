//go:build manual

// manual_api_test.go 手动 API 真机集成用例（Task 14 / Step 1）。
//
// 本文件不进常规测试构建（//go:build manual），真机执行须显式开启：
//
//	FEIWU_MANUAL=1 go test -tags manual ./internal/webapi \
//	    -run TestManualAPI -v -count=1
//
// 流程（brief Step 1 逐字）：真机起 server（随机端口，读日志取 URL）→
// 终端打 QR（PNG 落盘 + scan_url 文本）→ 轮询 state 至 logged_in（120s+，
// 期间提示扫码）→ account（昵称/容量）→ FEIWU_SHARE_TEXT 提供则
// parse → share/files 选最小文件 → POST /api/tasks（dest 相对前缀落
// 临时子目录）→ SSE 读帧至 task_final → dest 逐字节校验（size 断言 +
// 抽样 hash 存证）→ pause/resume/cancel 各演练一次 → 打印全链路耗时。
//
// 安全：Cookie 由服务端管理不经手；token 类输出打码（Ruling A 模式：
// 前 4 位+总长）；scan_url 与 PNG 是给用户扫码的功能输出，明文属功能
// 需求。FEIWU_SHARE_TEXT 未提供时跳过分享段并明示（登录链仍完整验证）。
package webapi_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestManualAPI 真机全链路。t.Skip 守卫在首行：未设 FEIWU_MANUAL=1 时
// 说明启用方式后跳过（本文件也用于验证 skip 路径可编译可执行）。
func TestManualAPI(t *testing.T) {
	if os.Getenv("FEIWU_MANUAL") != "1" {
		t.Skip("真机手动用例：设 FEIWU_MANUAL=1 并带 -tags manual 启用；需手机扫码登录，可选 FEIWU_SHARE_TEXT 提供真实分享链接文本")
	}
	start := time.Now()
	baseURL := startServer(t)

	// ---- 扫码登录 ----
	_ = qrStart(t, baseURL) // QR 展示（PNG+链接）在 qrStart 内完成
	waitLoggedIn(t, baseURL)

	// ---- account ----
	acct := apiGetJSON(t, baseURL, "/api/account")
	if acct["logged_in"] != true {
		t.Fatalf("account: logged_in 应为 true，响应: %s", maskJSON(acct))
	}
	nick, _ := acct["nickname"].(string)
	capMap, _ := acct["capacity"].(map[string]any)
	t.Logf("account: 昵称=%q 容量=%v", nick, capMap)

	// ---- 分享段（可选） ----
	shareText := os.Getenv("FEIWU_SHARE_TEXT")
	if shareText == "" {
		t.Log("FEIWU_SHARE_TEXT 未提供——跳过 parse/tasks/SSE/文件校验/控制演练段（登录链已完整验证）")
		printElapsed(t, start)
		return
	}
	parsed := shareParse(t, baseURL, shareText)
	pwdID, _ := parsed["pwd_id"].(string)
	passcode, _ := parsed["passcode"].(string)
	sid, _ := parsed["sid"].(string)
	t.Logf("parse: sid=%s pwd_id=%s passcode=%s", mask(sid), mask(pwdID), mask(passcode))

	// 选最小文件：根层优先，全目录则逐个下钻一层。
	file := pickSmallestFile(t, baseURL, sid, parsed["files"])
	if file == nil {
		t.Fatal("分享内未找到文件（含下钻一层）")
	}
	t.Logf("选中文件: name=%q size=%d fid=%s", file.name, file.size, mask(file.fid))

	// ---- 建任务（dest 相对前缀，最终落在 settings.destDir 下）----
	destDir := settingsDestDir(t, baseURL)
	relPrefix := filepath.ToSlash(filepath.Join("feiwu-manual-test", randHex(t, 4)))
	defer os.RemoveAll(filepath.Join(destDir, "feiwu-manual-test"))

	taskID := createShareTask(t, baseURL, pwdID, passcode, file, relPrefix)

	// ---- SSE 至 task_final ----
	finalState := readSSEUntilFinal(t, baseURL, taskID)

	// ---- dest 校验：size 断言 + 抽样 hash 存证 ----
	dest := filepath.Join(destDir, filepath.FromSlash(relPrefix), file.name)
	sampleHashDest(t, dest, file.size)

	// ---- pause/resume/cancel 演练（终态任务：语义存在即可，状态以列表为准）----
	for _, action := range []string{"pause", "resume", "cancel"} {
		code, _ := apiPost(t, baseURL, fmt.Sprintf("/api/tasks/%s/%s", taskID, action), nil)
		if code != http.StatusOK && code != http.StatusConflict && code != http.StatusNotFound {
			t.Errorf("%s: 响应码 %d 不在预期 {200,409,404}", action, code)
		} else {
			t.Logf("%s → %d", action, code)
		}
	}
	list := apiGetJSON(t, baseURL, "/api/tasks")
	t.Logf("任务列表终态: %s（final=%s）", maskJSON(list), finalState)
	if finalState != "done" {
		t.Errorf("task_final state=%q，期望 done", finalState)
	}
	printElapsed(t, start)
}

// ---- server 进程 ----

// startServer 构建并启动真机进程（go run ./cmd/feiwu），从日志
// 「feiwu 监听中 … url=http://127.0.0.1:PORT」取实际端口。
func startServer(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := filepath.Dir(filepath.Dir(wd))          // internal/webapi → 仓库根
	cmd := exec.Command("go", "run", "./cmd/feiwu") // 生命周期归 t.Cleanup（SIGINT 优雅关→Kill 兜底）
	cmd.Dir = root
	// 隔离数据目录：真机测试不污染用户真实数据目录（settings/cookies/
	// tasks 全落临时目录）。用户显式传入 FEIWU_DATA_DIR 时尊重之。
	env := append(os.Environ(),
		"FEIWU_NO_BROWSER=1",
		"GOCACHE="+filepath.Join(root, ".gocache"), // 环境契约：离线本地缓存
		"GOPROXY=off",
	)
	if os.Getenv("FEIWU_DATA_DIR") == "" {
		env = append(env, "FEIWU_DATA_DIR="+t.TempDir())
	}
	cmd.Env = env
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 go run: %v（在 %s 下执行）", err, root)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGINT) // 优雅关（E6 保留断点）
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	// 日志扫描：slog 文本行 … msg="feiwu 监听中" url=http://127.0.0.1:N
	urlRe := regexp.MustCompile(`url=(http://127\.0\.0\.1:\d+)`)
	type logLine struct {
		text string
		err  error
	}
	lines := make(chan logLine, 64)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lines <- logLine{text: sc.Text()}
		}
		if sc.Err() != nil {
			lines <- logLine{err: sc.Err()}
		}
		close(lines)
	}()
	t.Cleanup(func() {
		go func() {
			for range lines {
			}
		}()
	}) // 排空防阻塞

	deadline := time.Now().Add(60 * time.Second) // go run 首次编译给足
	for {
		select {
		case ln, ok := <-lines:
			if !ok {
				t.Fatal("server 日志流提前结束（进程退出？）——检查上方输出")
			}
			if ln.err != nil {
				t.Fatalf("读 server 日志: %v", ln.err)
			}
			t.Logf("[server] %s", ln.text)
			if m := urlRe.FindStringSubmatch(ln.text); m != nil {
				return m[1]
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("60s 内未见到「feiwu 监听中」日志行")
		}
	}
}

// ---- 扫码登录 ----

type qrStartResp struct {
	Token     string `json:"token"`
	QrcodePNG string `json:"qrcode_png"`
	ScanURL   string `json:"scan_url"`
}

func qrStart(t *testing.T, base string) qrStartResp {
	t.Helper()
	code, body := apiPost(t, base, "/api/login/qr/start", nil)
	if code != http.StatusOK {
		t.Fatalf("qr/start: %d %s", code, truncate(string(body)))
	}
	var r qrStartResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("qr/start 响应解析: %v", err)
	}
	png, err := base64.StdEncoding.DecodeString(r.QrcodePNG)
	if err != nil || len(png) == 0 {
		t.Fatalf("qrcode_png 非有效 base64 PNG（len=%d err=%v）", len(r.QrcodePNG), err)
	}
	pngPath := filepath.Join(t.TempDir(), "feiwu-login-qr.png")
	if err := os.WriteFile(pngPath, png, 0o600); err != nil {
		t.Fatalf("写 QR PNG: %v", err)
	}
	fmt.Printf("\n════ 请用手机（夸克 App 已登录态）扫码登录 ════\n  二维码 PNG: %s\n  扫码链接:  %s\n（打开 PNG 扫描，或在本机浏览器打开链接展示二维码；链接含一次性 token=%s）\n══════════════════════════════════════\n",
		pngPath, r.ScanURL, mask(r.Token))
	return r
}

// waitLoggedIn 轮询 qr/state 至 logged_in（六态：waiting/scanned 继续；
// got_ticket+logged_in 成功；invalid/expired/timeout/400 失败重启流程）。
func waitLoggedIn(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		code, body := apiGet(t, base, "/api/login/qr/state")
		if code == http.StatusBadRequest {
			t.Fatalf("qr/state 400（无会话/终态后轮询）: %s", truncate(string(body)))
		}
		if code != http.StatusOK {
			t.Fatalf("qr/state: %d %s", code, truncate(string(body)))
		}
		var st struct {
			State    string `json:"state"`
			LoggedIn bool   `json:"logged_in"`
			Nickname string `json:"nickname"`
		}
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatalf("qr/state 解析: %v", err)
		}
		switch st.State {
		case "waiting", "scanned":
			t.Logf("等待扫码… state=%s（剩 %s）", st.State, time.Until(deadline).Round(time.Second))
		case "got_ticket":
			if !st.LoggedIn {
				t.Fatalf("got_ticket 但 logged_in=false: %s", truncate(string(body)))
			}
			t.Logf("登录成功：昵称=%q", st.Nickname)
			return
		default: // invalid / expired / timeout
			t.Fatalf("扫码会话终态 state=%q——请重新运行用例（会话已清，需重打二维码）", st.State)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("180s 内未完成扫码登录")
}

// ---- 分享 ----

type manualFile struct {
	fid  string
	name string
	size int64
	dir  bool
}

func filesFrom(raw any) []manualFile {
	arr, _ := raw.([]any)
	out := make([]manualFile, 0, len(arr))
	for _, it := range arr {
		m, _ := it.(map[string]any)
		f := manualFile{}
		f.fid, _ = m["fid"].(string)
		f.name, _ = m["name"].(string)
		if m["dir"] == true {
			f.dir = true
		}
		if sz, ok := m["size"].(float64); ok {
			f.size = int64(sz)
		}
		out = append(out, f)
	}
	return out
}

// pickSmallestFile 根层找最小非目录文件；全目录则逐个下钻一层（G11
// 目录语义由服务端建任务时展开，这里手动下钻仅为挑目标）。
func pickSmallestFile(t *testing.T, base, sid string, rawFiles any) *manualFile {
	t.Helper()
	files := filesFrom(rawFiles)
	var best *manualFile
	var dirs []manualFile
	for i := range files {
		if files[i].dir {
			dirs = append(dirs, files[i])
			continue
		}
		if best == nil || files[i].size < best.size {
			best = &files[i]
		}
	}
	if best != nil {
		return best
	}
	for _, d := range dirs {
		code, body := apiGet(t, base, "/api/share/files?sid="+sid+"&pdir_fid="+d.fid)
		if code != http.StatusOK {
			t.Logf("share/files 下钻 %s: %d（跳过）", mask(d.fid), code)
			continue
		}
		var resp struct {
			Files any `json:"files"`
		}
		_ = json.Unmarshal(body, &resp)
		for _, f := range filesFrom(resp.Files) {
			if f.dir {
				continue
			}
			if best == nil || f.size < best.size {
				cp := f
				best = &cp
			}
		}
	}
	return best
}

func shareParse(t *testing.T, base, text string) map[string]any {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"text": text})
	code, body := apiPost(t, base, "/api/share/parse", payload)
	if code != http.StatusOK {
		t.Fatalf("share/parse: %d %s", code, truncate(string(body)))
	}
	var r map[string]any
	_ = json.Unmarshal(body, &r)
	if r["kouling"] == true {
		t.Fatalf("分享文本是口令（kouling=true hint=%v）——请改用完整分享链接文本", r["hint"])
	}
	if r["sid"] == nil || r["pwd_id"] == nil {
		t.Fatalf("parse 响应缺 sid/pwd_id: %s", maskJSON(r))
	}
	if _, leaked := r["stoken"]; leaked {
		t.Error("parse 响应透出了 stoken 明文（契约：sid 引用替代）")
	}
	return r
}

// ---- 任务 ----

func settingsDestDir(t *testing.T, base string) string {
	t.Helper()
	s := apiGetJSON(t, base, "/api/settings")
	dir, _ := s["destDir"].(string)
	if dir == "" {
		t.Fatalf("settings.destDir 为空: %s", maskJSON(s))
	}
	return dir
}

func createShareTask(t *testing.T, base, pwdID, passcode string, f *manualFile, relPrefix string) string {
	t.Helper()
	req := map[string]any{
		"source":   "share",
		"fids":     []string{f.fid},
		"pwd_id":   pwdID,
		"passcode": passcode,
		"dest":     relPrefix,
		// stoken 不传：服务端走 ShareToken 现换闭环（T13 sid×POST 取舍）
	}
	payload, _ := json.Marshal(req)
	code, body := apiPost(t, base, "/api/tasks", payload)
	if code != http.StatusOK {
		t.Fatalf("tasks: %d %s", code, truncate(string(body)))
	}
	var resp struct {
		TaskIDs []string `json:"task_ids"`
		Skipped []struct {
			FID    string `json:"fid"`
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("tasks 响应解析: %v", err)
	}
	for _, s := range resp.Skipped {
		t.Errorf("skipped: %s（%s）原因: %s", s.Name, mask(s.FID), s.Reason)
	}
	if len(resp.TaskIDs) != 1 {
		t.Fatalf("期望 1 个 task_id，得 %d（skipped %d）", len(resp.TaskIDs), len(resp.Skipped))
	}
	t.Logf("任务已创建: %s", resp.TaskIDs[0])
	return resp.TaskIDs[0]
}

// readSSEUntilFinal 订阅 /api/events：hello 快照记录、task_progress 计数、
// 目标 task_final 帧返回其 state。小文件 + 真机网络，300s 上限。
func readSSEUntilFinal(t *testing.T, base, taskID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE 连接: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE: %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // hello 快照可较大
	progress := 0
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type    string          `json:"type"`
			TaskID  string          `json:"taskId"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Logf("SSE 帧解析跳过: %v", err)
			continue
		}
		switch ev.Type {
		case "hello":
			t.Logf("SSE hello 快照到达（%d 字节）", len(ev.Payload))
		case "task_progress":
			progress++
			if progress%10 == 1 {
				t.Logf("进度帧 #%d: %s", progress, truncate(string(ev.Payload)))
			}
		case "task_final":
			if ev.TaskID != taskID {
				continue
			}
			var p struct {
				State string `json:"state"`
				Error string `json:"error"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			t.Logf("task_final: state=%s err=%q（共 %d 帧进度）", p.State, p.Error, progress)
			return p.State
		case "auth":
			t.Logf("auth 帧: %s", truncate(string(ev.Payload)))
		}
	}
	t.Fatalf("SSE 流结束（%v）未见 %s 的 task_final", sc.Err(), taskID)
	return ""
}

func sampleHashDest(t *testing.T, dest string, wantSize int64) {
	t.Helper()
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("dest 文件缺失 %s: %v", dest, err)
	}
	if st.Size() != wantSize {
		t.Errorf("dest size=%d，期望 %d（%s）", st.Size(), wantSize, dest)
	} else {
		t.Logf("dest size 校验通过: %d 字节", wantSize)
	}
	f, err := os.Open(dest)
	if err != nil {
		t.Fatalf("打开 dest: %v", err)
	}
	defer f.Close()
	// 抽样 hash 存证：首/中/尾各 4KB（无源参照，hash 用于人工比对与留档）。
	for name, off := range map[string]int64{"head": 0, "mid": max64(st.Size()-4096, 0) / 2, "tail": max64(st.Size()-4096, 0)} {
		buf := make([]byte, 4096)
		n, _ := f.ReadAt(buf, off)
		sum := sha256.Sum256(buf[:n])
		t.Logf("dest %s@%d sha256(≤4KB): %s", name, off, hex.EncodeToString(sum[:]))
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---- HTTP 小工具 ----

func apiGet(t *testing.T, base, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body
}

func apiPost(t *testing.T, base, path string, payload []byte) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	resp, err := http.Post(base+path, "application/json", rd)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body
}

func apiGetJSON(t *testing.T, base, path string) map[string]any {
	t.Helper()
	code, body := apiGet(t, base, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, code, truncate(string(body)))
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("GET %s 解析: %v", path, err)
	}
	return m
}

// ---- 打码与杂项（Ruling A 模式：前 4 位 + 总长）----

func mask(s string) string {
	if len(s) <= 4 {
		return fmt.Sprintf("<%d字符>", len(s))
	}
	return fmt.Sprintf("%s…<%d字符>", s[:4], len(s))
}

// maskJSON 序列化后把长字符串值打码（Cookie/Token 防泄漏）。
func maskJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<marshal 失败>"
	}
	if len(b) > 400 {
		return string(b[:400]) + "…<截断>"
	}
	return string(b)
}

func truncate(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		return s[:300] + "…<截断>"
	}
	return s
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func printElapsed(t *testing.T, start time.Time) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Logf("全链路耗时: %s", time.Since(start).Round(time.Millisecond))
		return
	}
	t.Logf("全链路耗时: %s", time.Since(start).Round(time.Millisecond))
}
