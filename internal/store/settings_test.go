package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testPaths(dir string) Paths {
	return Paths{
		Root:         dir,
		DataDir:      dir,
		SettingsFile: filepath.Join(dir, "settings.json"),
		TasksFile:    filepath.Join(dir, "tasks.json"),
	}
}

// TestDefaultSettings 缺省值逐项对照:DestDir=用户 Downloads/feiwu、
// Threads=32(G5)、MaxConcurrentTasks=3(G13/§3.13)、ThemeLock=""(空=跟随系统,D7)。
func TestDefaultSettings(t *testing.T) {
	s := defaultSettings(func() (string, error) { return "/home/tester", nil })
	want := Settings{
		DestDir:            "/home/tester/Downloads/feiwu",
		Threads:            32,
		MaxConcurrentTasks: 3,
		ThemeLock:          "",
	}
	if s != want {
		t.Errorf("defaultSettings() = %+v, want %+v", s, want)
	}
}

// TestDefaultSettingsHomeUnavailable HOME 拿不到的极端情形:DestDir 退化为
// 相对路径 Downloads/feiwu(仍可用,不 panic 不报错)。
func TestDefaultSettingsHomeUnavailable(t *testing.T) {
	s := defaultSettings(failWith(os.ErrNotExist))
	if s.DestDir != filepath.Join("Downloads", "feiwu") {
		t.Errorf("home 不可得时 DestDir = %q, want 相对 Downloads/feiwu", s.DestDir)
	}
	if s.Threads != 32 || s.MaxConcurrentTasks != 3 || s.ThemeLock != "" {
		t.Errorf("home 不可得不应影响其余缺省: %+v", s)
	}
}

// TestDefaultSettingsProduction 生产入口 wiring(真 HOME)。
func TestDefaultSettingsProduction(t *testing.T) {
	s := DefaultSettings()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("无 HOME,跳过生产入口断言: %v", err)
	}
	if s.DestDir != filepath.Join(home, "Downloads", "feiwu") {
		t.Errorf("DefaultSettings().DestDir = %q, want %q", s.DestDir, filepath.Join(home, "Downloads", "feiwu"))
	}
	if s.Threads != 32 || s.MaxConcurrentTasks != 3 || s.ThemeLock != "" {
		t.Errorf("DefaultSettings() 数值缺省不对: %+v", s)
	}
}

// TestSettingsRoundTrip 保存→读回全等(JSON 往返不丢字段)。
func TestSettingsRoundTrip(t *testing.T) {
	p := testPaths(t.TempDir())
	s := Settings{DestDir: "/dl/quark", Threads: 64, MaxConcurrentTasks: 5, ThemeLock: "dark"}
	if err := SaveSettings(p, s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if got := LoadSettings(p); got != s {
		t.Errorf("往返不等: got %+v, want %+v", got, s)
	}
}

// TestLoadSettingsMissingFile 文件缺失 → 默认值,不报错。
func TestLoadSettingsMissingFile(t *testing.T) {
	p := testPaths(t.TempDir())
	if got := LoadSettings(p); got != DefaultSettings() {
		t.Errorf("缺失文件: got %+v, want %+v", got, DefaultSettings())
	}
}

// TestLoadSettingsBadFile 坏文件(非法 JSON/空文件/字段类型错)→ 默认值,不报错。
func TestLoadSettingsBadFile(t *testing.T) {
	cases := map[string]string{
		"非 JSON":  "not json at all {{{",
		"空文件":     "",
		"字段类型错":   `{"threads": "sixty-four"}`,
		"截断 JSON": `{"destDir": "/dl", "thr`,
		"顶层数组":    `[1,2,3]`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := testPaths(dir)
			if err := os.WriteFile(p.SettingsFile, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := LoadSettings(p); got != DefaultSettings() {
				t.Errorf("坏文件 %q: got %+v, want 默认 %+v", content, got, DefaultSettings())
			}
		})
	}
}

// TestLoadSettingsPartialFile 存量文件缺字段(旧版本/手编)→ 缺的字段取默认,
// 在场的字段照读——「默认值打底 + Unmarshal 覆盖」语义。
func TestLoadSettingsPartialFile(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := os.WriteFile(p.SettingsFile, []byte(`{"threads":64}`), 0o644); err != nil {
		t.Fatal(err)
	}
	def := DefaultSettings()
	got := LoadSettings(p)
	if got.Threads != 64 {
		t.Errorf("在场字段应照读: Threads = %d, want 64", got.Threads)
	}
	if got.DestDir != def.DestDir || got.MaxConcurrentTasks != def.MaxConcurrentTasks || got.ThemeLock != def.ThemeLock {
		t.Errorf("缺字段应取默认: got %+v(def=%+v)", got, def)
	}
}

// TestSaveSettingsCreatesDataDir DataDir 不存在 → Save 负责建目录。
func TestSaveSettingsCreatesDataDir(t *testing.T) {
	base := t.TempDir()
	p := testPaths(filepath.Join(base, "a", "b"))
	if err := SaveSettings(p, Settings{DestDir: "/dl", Threads: 32, MaxConcurrentTasks: 3}); err != nil {
		t.Fatalf("SaveSettings 应自建 DataDir: %v", err)
	}
	if _, err := os.Stat(p.SettingsFile); err != nil {
		t.Errorf("落盘文件缺失: %v", err)
	}
}

// TestSaveSettingsAtomic 原子性可观察面:换入完成后不残留 .tmp;写入内容完整合法。
func TestSaveSettingsAtomic(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := SaveSettings(p, Settings{DestDir: "/dl", Threads: 32, MaxConcurrentTasks: 3, ThemeLock: "light"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if _, err := os.Stat(p.SettingsFile + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("换入后不应残留 .tmp: %v", err)
	}
	b, err := os.ReadFile(p.SettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Errorf("落盘内容非法 JSON: %q", b)
	}
	// 人可读格式(MarshalIndent 带换行)——设置文件用户可能手编。
	if len(b) > 0 && b[len(b)-1] != '\n' {
		t.Errorf("文本文件应以换行收尾: %q", b)
	}
}

// TestSaveSettingsReplacesExisting 覆盖已有文件(rename 原子替换语义):
// 第二次 Save 后读回即新值,旧内容不残留。
func TestSaveSettingsReplacesExisting(t *testing.T) {
	p := testPaths(t.TempDir())
	v1 := Settings{DestDir: "/old", Threads: 16, MaxConcurrentTasks: 1, ThemeLock: ""}
	v2 := Settings{DestDir: "/new", Threads: 128, MaxConcurrentTasks: 8, ThemeLock: "dark"}
	if err := SaveSettings(p, v1); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(p, v2); err != nil {
		t.Fatal(err)
	}
	if got := LoadSettings(p); got != v2 {
		t.Errorf("覆盖写: got %+v, want %+v", got, v2)
	}
}

// TestSaveSettingsOverwritesStaleTmp 残留的旧 .tmp(上次换入前崩溃)无害:
// O_TRUNC 重写后照常换入,不留垃圾。
func TestSaveSettingsOverwritesStaleTmp(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := os.WriteFile(p.SettingsFile+".tmp", []byte("上一次崩溃的残骸"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Settings{DestDir: "/dl", Threads: 32, MaxConcurrentTasks: 3}
	if err := SaveSettings(p, s); err != nil {
		t.Fatalf("残留 .tmp 不应阻碍保存: %v", err)
	}
	if got := LoadSettings(p); got != s {
		t.Errorf("got %+v, want %+v", got, s)
	}
	if _, err := os.Stat(p.SettingsFile + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("换入后 .tmp 应消失: %v", err)
	}
}

// TestSaveSettingsRejectsBadThemeLock ThemeLock 契约 ""|light|dark(D7):
// 非法值在写盘前拒绝,不落脏数据。
func TestSaveSettingsRejectsBadThemeLock(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := SaveSettings(p, Settings{DestDir: "/dl", Threads: 32, MaxConcurrentTasks: 3, ThemeLock: "blue"}); err == nil {
		t.Error("ThemeLock=blue 应返回 error")
	}
	if _, err := os.Stat(p.SettingsFile); !os.IsNotExist(err) {
		t.Errorf("拒绝时不应落盘: %v", err)
	}
	for _, ok := range []string{"", "light", "dark"} {
		if err := SaveSettings(p, Settings{DestDir: "/dl", Threads: 32, MaxConcurrentTasks: 3, ThemeLock: ok}); err != nil {
			t.Errorf("ThemeLock=%q 应合法: %v", ok, err)
		}
	}
}
