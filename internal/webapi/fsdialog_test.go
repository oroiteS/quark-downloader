// fsdialog_test.go POST /api/fs/pick-dir 行为契约（计划外 Task 11：下载
// 目录接入系统原生目录选择器）：
//
//	① 三分支（darwin/windows/linux）命令构造正确性 + 未支持平台拒绝；
//	② 取消/成功解析（fake runner 注入 dialogOutcome，全程不真弹窗）；
//	③ busy 互斥（sync.Mutex TryLock → 409 dialog_busy）；
//	④ 对话框命令缺失 → 503 fs_picker_unsupported；其余失败 → 500 internal。
//
// 注入安全钉死：buildPickDirCmd 仅吃 goos 分支常量，无任何用户输入拼接位。
package webapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"quark-downloader/internal/store"
)

// ---- ① 命令构造：三分支 + 不支持 ----

// TestBuildPickDirCmdDarwin darwin 分支：osascript -e 取 POSIX path 的
// choose folder（脚本为分支常量，无用户输入拼接位）。
func TestBuildPickDirCmdDarwin(t *testing.T) {
	cmd, ok := buildPickDirCmd("darwin")
	if !ok {
		t.Fatal("darwin 应受支持")
	}
	// Args[0] 是构造字面量（Path 会被 LookPath 解析为绝对路径，不宜断言）。
	if cmd.Args[0] != "osascript" {
		t.Fatalf("Args[0] = %q, 期望 osascript", cmd.Args[0])
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "-e" {
		t.Fatalf("Args = %v, 期望 [-e <script>]", cmd.Args)
	}
	script := cmd.Args[2]
	if !strings.Contains(script, "choose folder") || !strings.Contains(script, "POSIX path") {
		t.Fatalf("脚本缺 choose folder/POSIX path: %q", script)
	}
}

// TestBuildPickDirCmdWindows windows 分支：powershell -NoProfile
// -NonInteractive -Command FolderBrowserDialog 脚本。
func TestBuildPickDirCmdWindows(t *testing.T) {
	cmd, ok := buildPickDirCmd("windows")
	if !ok {
		t.Fatal("windows 应受支持")
	}
	if cmd.Args[0] != "powershell" {
		t.Fatalf("Args[0] = %q, 期望 powershell", cmd.Args[0])
	}
	if len(cmd.Args) != 5 ||
		cmd.Args[1] != "-NoProfile" || cmd.Args[2] != "-NonInteractive" ||
		cmd.Args[3] != "-Command" {
		t.Fatalf("Args = %v, 期望 [-NoProfile -NonInteractive -Command <script>]", cmd.Args)
	}
	if !strings.Contains(cmd.Args[4], "FolderBrowserDialog") {
		t.Fatalf("脚本缺 FolderBrowserDialog: %q", cmd.Args[4])
	}
}

// TestBuildPickDirCmdLinux linux 分支：zenity --file-selection --directory。
func TestBuildPickDirCmdLinux(t *testing.T) {
	cmd, ok := buildPickDirCmd("linux")
	if !ok {
		t.Fatal("linux 应受支持")
	}
	if cmd.Args[0] != "zenity" {
		t.Fatalf("Args[0] = %q, 期望 zenity", cmd.Args[0])
	}
	if !reflect.DeepEqual(cmd.Args[1:3], []string{"--file-selection", "--directory"}) {
		t.Fatalf("Args = %v, 期望含 [--file-selection --directory]", cmd.Args)
	}
}

// TestBuildPickDirCmdUnsupported 其余平台 → ok=false（handler 映射 503）。
func TestBuildPickDirCmdUnsupported(t *testing.T) {
	if cmd, ok := buildPickDirCmd("plan9"); ok || cmd != nil {
		t.Fatalf("plan9 应不受支持, got ok=%v cmd=%v", ok, cmd)
	}
}

// ---- ② 取消/成功解析（fake outcome，不真弹窗） ----

// TestParsePickResult 全平台 × 成功/取消/失败 矩阵：
//
//	darwin  取消 = 非零退出且 stderr 含 (-128)（文案本地化——本机中文实测
//	        「用户已取消。 (-128)」，数字后缀跨语言稳定）或 "User canceled"；
//	linux   取消 = 退出码 1（zenity man page：0=OK / 1=Cancel）；
//	windows 取消 = 退出 0 且 stdout 空（脚本仅 OK 时输出 SelectedPath）。
func TestParsePickResult(t *testing.T) {
	cancelDarwin := dialogOutcome{
		err:      errors.New("exit status 1"),
		stderr:   "0:17: execution error: 用户已取消。 (-128)",
		exitCode: 1,
	}
	cancelDarwinEn := dialogOutcome{
		err:      errors.New("exit status 1"),
		stderr:   "23:1: execution error: User canceled. (-128)",
		exitCode: 1,
	}
	failDarwin := dialogOutcome{
		err:      errors.New("exit status 1"),
		stderr:   "execution error: 磁盘不可用。 (-50)",
		exitCode: 1,
	}
	cancelLinux := dialogOutcome{err: errors.New("exit status 1"), exitCode: 1}
	failLinux := dialogOutcome{err: errors.New("exit status 255"), exitCode: 255}

	cases := []struct {
		name     string
		goos     string
		out      dialogOutcome
		picked   bool
		path     string
		wantFail bool
	}{
		{"darwin 成功", "darwin", dialogOutcome{stdout: "/Users/x/DL\n"}, true, "/Users/x/DL", false},
		{"darwin 取消(中文环境)", "darwin", cancelDarwin, false, "", false},
		{"darwin 取消(英文环境)", "darwin", cancelDarwinEn, false, "", false},
		{"darwin 其他失败", "darwin", failDarwin, false, "", true},
		{"darwin 成功但空输出", "darwin", dialogOutcome{}, false, "", true},
		{"linux 成功", "linux", dialogOutcome{stdout: "/home/x/DL\n"}, true, "/home/x/DL", false},
		{"linux 取消(码1)", "linux", cancelLinux, false, "", false},
		{"linux 失败(码255)", "linux", failLinux, false, "", true},
		{"windows 成功(CRLF)", "windows", dialogOutcome{stdout: "C:\\Users\\x\\DL\r\n"}, true, `C:\Users\x\DL`, false},
		{"windows 取消(空输出)", "windows", dialogOutcome{}, false, "", false},
		{"windows 失败", "windows", dialogOutcome{err: errors.New("exit status 1"), exitCode: 1}, false, "", true},
	}
	for _, tc := range cases {
		picked, path, ferr := parsePickResult(tc.goos, tc.out)
		if tc.wantFail {
			if ferr == nil {
				t.Fatalf("%s: 期望失败, got picked=%v path=%q", tc.name, picked, path)
			}
			continue
		}
		if ferr != nil {
			t.Fatalf("%s: 意外失败: %v", tc.name, ferr)
		}
		if picked != tc.picked || path != tc.path {
			t.Fatalf("%s: picked=%v path=%q, 期望 picked=%v path=%q",
				tc.name, picked, path, tc.picked, tc.path)
		}
	}
}

// ---- handler 集成（路由真挂载，fake runner 注入） ----

// newFsDialogTestApp 最小装配（pick-dir 不触 session/manager/settings，
// 沿用 tasks 测试同款 harness 保证路由形态一致）。
func newFsDialogTestApp(t *testing.T) *App {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return newTasksTestApp(t, &fakeUpstream{}, &fakeManager{}, store.Settings{
		DestDir: "/tmp/dl", Threads: 32, MaxConcurrentTasks: 3,
	}, settingsTestPaths(t))
}

// withFakeRunner 注入 fake 对话框 runner，测试结束还原。
func withFakeRunner(t *testing.T, fake func(*exec.Cmd) dialogOutcome) *[]*exec.Cmd {
	t.Helper()
	var seen []*exec.Cmd
	prev := runDialogCmd
	runDialogCmd = func(cmd *exec.Cmd) dialogOutcome {
		seen = append(seen, cmd)
		return fake(cmd)
	}
	t.Cleanup(func() { runDialogCmd = prev })
	return &seen
}

// TestPickDirHandlerSuccess 成功 → 200 {picked:true, path}；runner 收到的
// 就是按本平台构造的命令（Path/Args 与 buildPickDirCmd 一致）。
func TestPickDirHandlerSuccess(t *testing.T) {
	app := newFsDialogTestApp(t)
	seen := withFakeRunner(t, func(*exec.Cmd) dialogOutcome {
		return dialogOutcome{stdout: "/Users/x/DL\n"}
	})

	w := doReq(t, app.Router(), http.MethodPost, "/api/fs/pick-dir", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"picked":true`) || !strings.Contains(body, `"/Users/x/DL"`) {
		t.Fatalf("body = %s, 期望 {picked:true, path:/Users/x/DL}", body)
	}
	if len(*seen) != 1 {
		t.Fatalf("runner 调用次数 = %d, 期望 1", len(*seen))
	}
	want, ok := buildPickDirCmd(dialogGOOS())
	if !ok || (*seen)[0].Path != want.Path || !reflect.DeepEqual((*seen)[0].Args, want.Args) {
		t.Fatalf("runner 收到的命令 = %s %v, 期望构造产物 %s %v",
			(*seen)[0].Path, (*seen)[0].Args, want.Path, want.Args)
	}
}

// TestPickDirHandlerCancel 用户取消 → 200 {picked:false}（非错误，静默语义）。
func TestPickDirHandlerCancel(t *testing.T) {
	app := newFsDialogTestApp(t)
	withFakeRunner(t, func(*exec.Cmd) dialogOutcome {
		return dialogOutcome{
			err:      errors.New("exit status 1"),
			stderr:   "execution error: 用户已取消。 (-128)",
			exitCode: 1,
		}
	})

	w := doReq(t, app.Router(), http.MethodPost, "/api/fs/pick-dir", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	if body := w.Body.String(); !strings.Contains(body, `"picked":false`) {
		t.Fatalf("body = %s, 期望 {picked:false}", body)
	}
}

// TestPickDirHandlerBusy 占用中（TryLock 失败）→ 409 dialog_busy。
func TestPickDirHandlerBusy(t *testing.T) {
	app := newFsDialogTestApp(t)
	if !dialogMu.TryLock() {
		t.Fatal("测试预占 dialogMu 失败（前测未正确释放？）")
	}
	defer dialogMu.Unlock()

	w := doReq(t, app.Router(), http.MethodPost, "/api/fs/pick-dir", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d, 期望 409（body=%s）", w.Code, w.Body)
	}
	var eb struct {
		Error struct{ Code string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &eb); err != nil {
		t.Fatalf("解析错误体: %v", err)
	}
	if eb.Error.Code != "dialog_busy" {
		t.Fatalf("code = %q, 期望 dialog_busy", eb.Error.Code)
	}
}

// TestPickDirHandlerUnsupported 平台不支持 → 503 fs_picker_unsupported。
func TestPickDirHandlerUnsupported(t *testing.T) {
	app := newFsDialogTestApp(t)
	prev := dialogGOOS
	dialogGOOS = func() string { return "plan9" }
	t.Cleanup(func() { dialogGOOS = prev })

	w := doReq(t, app.Router(), http.MethodPost, "/api/fs/pick-dir", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503（body=%s）", w.Code, w.Body)
	}
	var eb struct {
		Error struct{ Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &eb)
	if eb.Error.Code != "fs_picker_unsupported" {
		t.Fatalf("code = %q, 期望 fs_picker_unsupported", eb.Error.Code)
	}
}

// TestPickDirHandlerCmdMissing 对话框命令不存在 → 503 fs_picker_unsupported
// （zenity 未装/osascript 缺失同码，与「其余失败」的 500 internal 区分）。
func TestPickDirHandlerCmdMissing(t *testing.T) {
	app := newFsDialogTestApp(t)
	withFakeRunner(t, func(*exec.Cmd) dialogOutcome {
		return dialogOutcome{
			err:      &exec.Error{Name: "zenity", Err: exec.ErrNotFound},
			notFound: true,
		}
	})

	w := doReq(t, app.Router(), http.MethodPost, "/api/fs/pick-dir", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503（body=%s）", w.Code, w.Body)
	}
	var eb struct {
		Error struct{ Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &eb)
	if eb.Error.Code != "fs_picker_unsupported" {
		t.Fatalf("code = %q, 期望 fs_picker_unsupported", eb.Error.Code)
	}
}

// TestPickDirHandlerOtherError 其余对话框失败 → 500 internal。
func TestPickDirHandlerOtherError(t *testing.T) {
	app := newFsDialogTestApp(t)
	withFakeRunner(t, func(*exec.Cmd) dialogOutcome {
		return dialogOutcome{err: errors.New("boom"), stderr: "boom", exitCode: 2}
	})

	w := doReq(t, app.Router(), http.MethodPost, "/api/fs/pick-dir", nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500（body=%s）", w.Code, w.Body)
	}
	var eb struct {
		Error struct{ Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &eb)
	if eb.Error.Code != "internal" {
		t.Fatalf("code = %q, 期望 internal", eb.Error.Code)
	}
}
