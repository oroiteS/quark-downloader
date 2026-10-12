// Package store 管理飞舞的本地持久化:数据目录定位(G7)、设置存取、
// Cookie 加密(§6.1,Task 3)与任务上下文(§6.2,Task 4)。
package store

import (
	"os"
	"path/filepath"
	"runtime"
)

// Paths 是一次数据目录定位的结果。字段语义(Task 3/4/5 的消费契约):
//
//	Root         数据根——平台用户配置根目录(如 ~/Library/Application
//	             Support);portable 时为 exe 所在目录;FEIWU_DATA_DIR 时
//	             与 DataDir 同值(用户显式指定的目录就是根,不再细分)。
//	DataDir      实际存放数据文件的目录(settings.json / tasks.json /
//	             secret.bin 都在这里)。
//	SettingsFile DataDir/settings.json。
//	TasksFile    DataDir/tasks.json(Task 4 的任务上下文)。
type Paths struct {
	Root         string
	DataDir      string
	SettingsFile string
	TasksFile    string
}

// ResolvePaths 按 G7 定位数据目录,返回各关键文件位置。只定位、不创建
// 目录(避免探测副作用;建目录是首次写入者 SaveSettings/SaveTasks 的职责)。
// 优先级:FEIWU_DATA_DIR 覆盖一切 > Windows portable 判据 > 平台默认目录。
func ResolvePaths() Paths {
	return resolvePaths(runtime.GOOS, os.Getenv, os.Executable, os.UserConfigDir)
}

// resolvePaths 是 ResolvePaths 的可注入实现体(goos / 环境变量 / exe 路径 /
// 用户配置目录全部可替换——三平台路径与 portable 判据由此离线测试,无需
// 交叉编译)。
//
// 平台默认目录(G7 逐字):macOS ~/Library/Application Support/feiwu/;
// Linux ~/.config/feiwu/——两者恰好就是 os.UserConfigDir() 加 feiwu 子目录
// (darwin 返回 ~/Library/Application Support,linux 尊重 $XDG_CONFIG_HOME、
// 缺省 $HOME/.config)。Windows G7 只定义了 portable 形态;非 portable
// (判据不满足时)按同一模式落 %AppData%\feiwu(os.UserConfigDir 在
// windows 的返回),与桌面惯例一致。
func resolvePaths(goos string, getenv func(string) string, exeDir, cfgDir func() (string, error)) Paths {
	// 1) FEIWU_DATA_DIR 覆盖一切(最高优先,测试与部署都依赖)。
	if d := getenv("FEIWU_DATA_DIR"); d != "" {
		return newPaths(d, d)
	}
	// 2) Windows portable:G7「Windows portable 模式存 exe 同目录(./data/)」。
	// 判据:FEIWU_PORTABLE=1,或 exe 同目录存在 feiwu.portable 标记文件——
	// 选型报告只说 portable,标记机制定为「标记文件 + env 覆盖」(计划
	// Task 2 裁决)。portable 是 Windows 打包形态的语义(D6:仅 Windows 出
	// portable zip),其它平台忽略判据,保证桌面平台落点稳定可预测。
	if goos == "windows" && isPortable(getenv, exeDir) {
		if exe, err := exeDir(); err == nil && exe != "" {
			root := filepath.Dir(exe) // exe 同目录
			return newPaths(root, filepath.Join(root, "data"))
		}
		// exe 路径拿不到(os.Executable 失败的极端情形):退到平台默认,
		// 不落不确定路径。
	}
	// 3) 平台默认:UserConfigDir/feiwu。
	if base, err := cfgDir(); err == nil && base != "" {
		return newPaths(base, filepath.Join(base, "feiwu"))
	}
	// HOME/配置目录都拿不到(裁剪环境的极端情形):退到 exe 同目录 data
	// (与 portable 同形,至少随二进制走,不散落 CWD)。
	if exe, err := exeDir(); err == nil && exe != "" {
		root := filepath.Dir(exe)
		return newPaths(root, filepath.Join(root, "data"))
	}
	// 双双失败:最后兜底相对路径(相对 CWD)。
	return newPaths(".", filepath.Join(".", "feiwu"))
}

// isPortable 判定 Windows portable 模式:FEIWU_PORTABLE=1 显式声明,
// 或 exe 同目录存在 feiwu.portable 标记文件(零字节即可,只判存在)。
func isPortable(getenv func(string) string, exeDir func() (string, error)) bool {
	if getenv("FEIWU_PORTABLE") == "1" {
		return true
	}
	exe, err := exeDir()
	if err != nil || exe == "" {
		return false
	}
	_, err = os.Stat(filepath.Join(filepath.Dir(exe), "feiwu.portable"))
	return err == nil
}

// newPaths 组装派生字段:SettingsFile/TasksFile 恒为 DataDir 下的
// settings.json / tasks.json。
func newPaths(root, dataDir string) Paths {
	return Paths{
		Root:         root,
		DataDir:      dataDir,
		SettingsFile: filepath.Join(dataDir, "settings.json"),
		TasksFile:    filepath.Join(dataDir, "tasks.json"),
	}
}
