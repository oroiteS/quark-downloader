package store

import (
	"os"
	"path/filepath"
	"testing"
)

// —— 注入工具:resolvePaths 的环境全可注入,测试离线且无真 HOME/exe 依赖 ——

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// fakeExe 返回「dir 下的可执行文件」路径(带扩展名,与 os.Executable 同形)。
func fakeExe(dir string) func() (string, error) {
	return func() (string, error) { return filepath.Join(dir, "feiwu.exe"), nil }
}

func fakeCfg(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

func failWith(err error) func() (string, error) {
	return func() (string, error) { return "", err }
}

// TestResolvePathsEnvDirOverridesEverything 验 G7「FEIWU_DATA_DIR 覆盖一切」:
// 优先级最高——三平台注入下均直接采用,Root=DataDir=该值,不触发任何平台分支
// (portable 判据、UserConfigDir 都不该被碰到)。
func TestResolvePathsEnvDirOverridesEverything(t *testing.T) {
	const dir = "/data/feiwu-override"
	for _, goos := range []string{"windows", "darwin", "linux"} {
		env := map[string]string{
			"FEIWU_DATA_DIR": dir,
			"FEIWU_PORTABLE": "1", // 即使 portable 判据同时满足,env 目录仍优先
		}
		p := resolvePaths(goos, fakeEnv(env), fakeExe(t.TempDir()), fakeCfg("/cfg/base"))
		if p.Root != dir || p.DataDir != dir {
			t.Errorf("goos=%s: Root/DataDir = %q/%q, want %q/%q", goos, p.Root, p.DataDir, dir, dir)
		}
		if p.SettingsFile != filepath.Join(dir, "settings.json") {
			t.Errorf("goos=%s: SettingsFile = %q", goos, p.SettingsFile)
		}
		if p.TasksFile != filepath.Join(dir, "tasks.json") {
			t.Errorf("goos=%s: TasksFile = %q", goos, p.TasksFile)
		}
	}
}

// TestResolvePathsWindowsPortableByEnv portable 判据之一:FEIWU_PORTABLE=1。
// G7 逐字「Windows portable 模式存 exe 同目录(./data/)」:Root=exe 所在目录,
// DataDir=其下 data 子目录。
func TestResolvePathsWindowsPortableByEnv(t *testing.T) {
	exeDir := t.TempDir()
	p := resolvePaths("windows", fakeEnv(map[string]string{"FEIWU_PORTABLE": "1"}), fakeExe(exeDir), fakeCfg("/cfg/base"))
	if p.Root != exeDir || p.DataDir != filepath.Join(exeDir, "data") {
		t.Errorf("portable(env): Root/DataDir = %q/%q, want %q/%q", p.Root, p.DataDir, exeDir, filepath.Join(exeDir, "data"))
	}
	if p.SettingsFile != filepath.Join(exeDir, "data", "settings.json") {
		t.Errorf("portable(env): SettingsFile = %q", p.SettingsFile)
	}
}

// TestResolvePathsWindowsPortableByMarker portable 判据之二:exe 同目录存在
// feiwu.portable 标记文件(选型报告只说 portable,标记机制=本文件+env 覆盖)。
func TestResolvePathsWindowsPortableByMarker(t *testing.T) {
	exeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(exeDir, "feiwu.portable"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := resolvePaths("windows", fakeEnv(nil), fakeExe(exeDir), fakeCfg("/cfg/base"))
	if p.Root != exeDir || p.DataDir != filepath.Join(exeDir, "data") {
		t.Errorf("portable(marker): Root/DataDir = %q/%q, want %q/%q", p.Root, p.DataDir, exeDir, filepath.Join(exeDir, "data"))
	}
}

// TestResolvePathsWindowsNonPortable 无 env、无标记 → 走平台用户配置目录。
func TestResolvePathsWindowsNonPortable(t *testing.T) {
	exeDir := t.TempDir()
	p := resolvePaths("windows", fakeEnv(nil), fakeExe(exeDir), fakeCfg("/cfg/winbase"))
	if p.Root != "/cfg/winbase" || p.DataDir != filepath.Join("/cfg/winbase", "feiwu") {
		t.Errorf("windows 非便携: Root/DataDir = %q/%q", p.Root, p.DataDir)
	}
}

// TestResolvePathsDarwinDefault G7 逐字:macOS ~/Library/Application Support/feiwu/
// (cfgDir 注入的值即 os.UserConfigDir() 在 darwin 的返回)。
func TestResolvePathsDarwinDefault(t *testing.T) {
	p := resolvePaths("darwin", fakeEnv(nil), fakeExe(t.TempDir()), fakeCfg("/home/u/Library/Application Support"))
	if p.Root != "/home/u/Library/Application Support" {
		t.Errorf("darwin Root = %q", p.Root)
	}
	if p.DataDir != "/home/u/Library/Application Support/feiwu" {
		t.Errorf("darwin DataDir = %q, want .../Application Support/feiwu", p.DataDir)
	}
	if p.TasksFile != filepath.Join(p.DataDir, "tasks.json") {
		t.Errorf("darwin TasksFile = %q", p.TasksFile)
	}
}

// TestResolvePathsLinuxDefault G7 逐字:Linux ~/.config/feiwu/。
func TestResolvePathsLinuxDefault(t *testing.T) {
	p := resolvePaths("linux", fakeEnv(nil), fakeExe(t.TempDir()), fakeCfg("/home/u/.config"))
	if p.Root != "/home/u/.config" || p.DataDir != "/home/u/.config/feiwu" {
		t.Errorf("linux Root/DataDir = %q/%q, want /home/u/.config//home/u/.config/feiwu", p.Root, p.DataDir)
	}
}

// TestResolvePathsPortableOnlyOnWindows portable 是 Windows 打包形态的语义
// (设计 D6:仅 Windows 出 portable zip);macOS/Linux 即使误带 env/标记文件
// 也忽略,保证桌面平台落点稳定可预测。
func TestResolvePathsPortableOnlyOnWindows(t *testing.T) {
	exeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(exeDir, "feiwu.portable"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, goos := range []string{"darwin", "linux"} {
		env := map[string]string{"FEIWU_PORTABLE": "1"}
		p := resolvePaths(goos, fakeEnv(env), fakeExe(exeDir), fakeCfg("/cfg/base"))
		if p.Root != "/cfg/base" || p.DataDir != filepath.Join("/cfg/base", "feiwu") {
			t.Errorf("goos=%s 应忽略 portable 判据: Root/DataDir = %q/%q", goos, p.Root, p.DataDir)
		}
	}
}

// TestResolvePathsWindowsPortableExeUnavailable portable 判据满足但拿不到 exe
// 路径(os.Executable 失败的极端情形)→ 退回平台用户配置目录,不落不确定路径。
func TestResolvePathsWindowsPortableExeUnavailable(t *testing.T) {
	p := resolvePaths("windows", fakeEnv(map[string]string{"FEIWU_PORTABLE": "1"}), failWith(os.ErrNotExist), fakeCfg("/cfg/base"))
	if p.Root != "/cfg/base" || p.DataDir != filepath.Join("/cfg/base", "feiwu") {
		t.Errorf("exe 不可得时应退回用户配置目录: Root/DataDir = %q/%q", p.Root, p.DataDir)
	}
}

// TestResolvePathsCfgDirUnavailableFallsToExeDir HOME/配置目录都拿不到
// (无头/裁剪环境的极端情形)→ 退到 exe 同目录 data(与 portable 同形,
// 至少随二进制走,不散落 CWD)。
func TestResolvePathsCfgDirUnavailableFallsToExeDir(t *testing.T) {
	exeDir := t.TempDir()
	p := resolvePaths("linux", fakeEnv(nil), fakeExe(exeDir), failWith(os.ErrNotExist))
	if p.Root != exeDir || p.DataDir != filepath.Join(exeDir, "data") {
		t.Errorf("cfg 不可得时应退回 exe 同目录: Root/DataDir = %q/%q, want %q/%q", p.Root, p.DataDir, exeDir, filepath.Join(exeDir, "data"))
	}
}

// TestResolvePaths 生产入口接线:env 覆盖真实生效 + 字段派生关系恒成立
// + 定位无副作用(指向不存在的深层目录,解析后也不该被创建)。
func TestResolvePathsWiring(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	t.Setenv("FEIWU_DATA_DIR", dir)
	p := ResolvePaths()
	if p.DataDir != dir {
		t.Errorf("生产入口 DataDir = %q, want FEIWU_DATA_DIR=%q", p.DataDir, dir)
	}
	if p.SettingsFile != filepath.Join(p.DataDir, "settings.json") || p.TasksFile != filepath.Join(p.DataDir, "tasks.json") {
		t.Errorf("字段派生关系断裂: %+v", p)
	}
	if _, err := os.Stat(p.DataDir); !os.IsNotExist(err) {
		t.Errorf("定位不应有创建目录副作用: %v", err)
	}
}
