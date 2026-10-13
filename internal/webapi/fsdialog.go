// fsdialog.go 系统原生目录选择器端点（计划外 Task 11：用户直接需求——
// 「选择下载目录的时候应该使用当前系统的组件，macOS 打开访达直接点击，
// Windows 打开文件夹，而不是输入地址」）：
//
//	POST /api/fs/pick-dir —— 后端调系统目录选择对话框（feiwu 后端与浏览器
//	同机；File System Access API 拿不到绝对路径且 Safari 不支持，已否决），
//	返回绝对路径供设置页 destDir 回填。
//
// 平台分支（buildPickDirCmd）：
//
//	darwin  osascript -e 'POSIX path of (choose folder …)'（访达选目录，
//	        输出 POSIX path）；用户取消 = 非零退出且 stderr 含 "(-128)"
//	        （AppleScript userCanceledErr，文案本地化——中文系统实测
//	        「用户已取消。 (-128)」，数字后缀跨语言稳定）或 "User canceled"。
//	windows powershell -NoProfile -NonInteractive -Command
//	        FolderBrowserDialog 脚本（仅 OK 时输出 SelectedPath）；
//	        取消 = 退出 0 且 stdout 空。
//	linux   zenity --file-selection --directory；取消 = 退出码 1
//	        （zenity(1)：0=OK / 1=Cancel / 255=错误）。
//
// 取消统一映射 200 {picked:false}（非错误，前端静默）；平台不支持或对话框
// 命令不存在 → 503 fs_picker_unsupported；其余失败 → 500 internal。
//
// 并发：dialogMu.TryLock 同刻仅允许一个对话框（系统对话框模态且无法排队，
// 并发触发只会互相踩）→ 409 dialog_busy。
//
// 注入安全：buildPickDirCmd 仅吃 runtime.GOOS 分支常量，不拼接任何用户
// 输入（路径由对话框返回，不传参）；exec.Command 直启不走 shell。
package webapi

import (
	"errors"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// darwinPickScript 取 POSIX path 的选目录脚本（分支常量，无用户输入）。
const darwinPickScript = `POSIX path of (choose folder with prompt "选择下载目录")`

// winPickDirScript FolderBrowserDialog 脚本：OK 才输出所选路径，取消静默
// 退出 0（stdout 空即取消信号）。
const winPickDirScript = `Add-Type -AssemblyName System.Windows.Forms | Out-Null
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = '选择下载目录'
if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { $d.SelectedPath }`

// dialogGOOS 对话框平台判定（测试注入点；生产恒 runtime.GOOS）。
var dialogGOOS = func() string { return runtime.GOOS }

// dialogMu 目录选择对话框互斥（同刻仅一个；TryLock 占用 → 409）。
var dialogMu sync.Mutex

// buildPickDirCmd 按 goos 构造对话框命令；不受支持的平台返回 ok=false。
// 仅分支常量入参——用户输入没有进入命令行的通道（注入安全）。
func buildPickDirCmd(goos string) (*exec.Cmd, bool) {
	switch goos {
	case "darwin":
		return exec.Command("osascript", "-e", darwinPickScript), true
	case "windows":
		return exec.Command("powershell", "-NoProfile", "-NonInteractive",
			"-Command", winPickDirScript), true
	case "linux":
		return exec.Command("zenity", "--file-selection", "--directory",
			"--title", "选择下载目录"), true
	default:
		return nil, false
	}
}

// dialogOutcome 对话框命令执行结果（runDialogCmd 的产出；纯数据便于
// fake 注入——测试不真弹窗）。
type dialogOutcome struct {
	stdout   string
	stderr   string
	err      error // 非 nil = 执行失败（含非零退出）
	exitCode int   // err 为 ExitError 时的退出码；非 ExitError 恒 -1
	notFound bool  // 对话框命令不存在（exec.ErrNotFound）→ 503
}

// runDialogCmd 执行对话框命令并归一为 dialogOutcome（包级注入点：测试
// 替换为 fake；Output 在 cmd.Stderr==nil 时把 stderr 收进 ExitError.Stderr）。
var runDialogCmd = func(cmd *exec.Cmd) dialogOutcome {
	out, err := cmd.Output()
	o := dialogOutcome{stdout: string(out)}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		o.err = err
		o.stderr = string(ee.Stderr)
		o.exitCode = ee.ExitCode()
	case err != nil:
		o.err = err
		o.notFound = errors.Is(err, exec.ErrNotFound)
	}
	return o
}

// parsePickResult 平台感知的取消/成功解析（goos 显式入参——三分支可在
// 任意平台上单测）：
//
//	darwin  非零退出且 stderr 含 "(-128)" 或 "User canceled" → 取消；
//	        成功但 stdout 空 → 失败（不应发生，显式不吞）；成功路径
//	        去尾随 "/"（POSIX path 恒带尾斜杠，归一为手输目录形态；
//	        根 "/" 例外保留）。
//	linux   退出码 1 → 取消；其余非零退出 → 失败。
//	windows 退出 0 且 stdout 空 → 取消（脚本仅 OK 时输出）；非零 → 失败。
func parsePickResult(goos string, o dialogOutcome) (picked bool, path string, ferr error) {
	path = strings.TrimSpace(o.stdout)
	switch goos {
	case "darwin":
		if o.err != nil {
			canceled := strings.Contains(o.stderr, "(-128)") ||
				strings.Contains(o.stderr, "User canceled")
			return false, "", wrapPickErr(canceled, o)
		}
		if path == "" {
			return false, "", errors.New("osascript 成功返回但无路径输出")
		}
		if path != "/" { // 根路径保留，其余去尾随斜杠（review fix round 1）
			path = strings.TrimSuffix(path, "/")
		}
		return true, path, nil
	case "linux":
		if o.err != nil {
			return false, "", wrapPickErr(o.exitCode == 1, o)
		}
		if path == "" {
			return false, "", errors.New("zenity 成功返回但无路径输出")
		}
		return true, path, nil
	case "windows":
		if o.err != nil {
			return false, "", wrapPickErr(false, o)
		}
		return path != "", path, nil // 空输出 = 取消（picked:false）
	default:
		return false, "", errors.New("不支持的平台: " + goos)
	}
}

// wrapPickErr 把对话框失败归一为错误（canceled 时返回 nil 由调用方走
// picked:false 通道；非取消把 stderr 带上下文透出，500 文案可诊断）。
func wrapPickErr(canceled bool, o dialogOutcome) error {
	if canceled {
		return nil
	}
	detail := strings.TrimSpace(o.stderr)
	if detail == "" {
		detail = o.err.Error()
	}
	return errors.New(detail)
}

// handlePickDir POST /api/fs/pick-dir——busy 拒绝 → 平台分支 → 执行 →
// 取消/成功/失败三态映射。鉴权与 settings handler 同款（同组直挂，本地
// 单用户应用无独立中间件）。
func (a *App) handlePickDir(c *gin.Context) {
	if !dialogMu.TryLock() {
		writeError(c, http.StatusConflict, codeDialogBusy,
			"已有目录选择对话框打开，请先完成或取消")
		return
	}
	defer dialogMu.Unlock()

	goos := dialogGOOS()
	cmd, ok := buildPickDirCmd(goos)
	if !ok {
		writeError(c, http.StatusServiceUnavailable, codeFsPickerUnsupported,
			"当前平台不支持系统目录选择器")
		return
	}
	out := runDialogCmd(cmd)
	if out.notFound { // 对话框命令缺失（zenity 未装等）→ 同不支持码
		writeError(c, http.StatusServiceUnavailable, codeFsPickerUnsupported,
			"系统目录选择器不可用（对话框命令缺失）")
		return
	}
	picked, path, ferr := parsePickResult(goos, out)
	switch {
	case ferr != nil:
		writeError(c, http.StatusInternalServerError, codeInternal,
			"目录选择失败: "+ferr.Error())
	case !picked: // 用户取消：非错误，前端静默
		c.JSON(http.StatusOK, gin.H{"picked": false})
	default:
		c.JSON(http.StatusOK, gin.H{"picked": true, "path": path})
	}
}
