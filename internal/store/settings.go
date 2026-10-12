package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Settings 是 settings.json 的内存形态,也是 webapi GET|PUT /api/settings
// (Task 13)直接透传给前端的 JSON 形状(字段名即 json tag,前端依赖,
// 不可偏移)。
type Settings struct {
	DestDir            string `json:"destDir"`            // 下载目录(全局默认,建任务时可改 G5)
	Threads            int    `json:"threads"`            // 并发档位,默认 32(G5)
	MaxConcurrentTasks int    `json:"maxConcurrentTasks"` // 同时下载任务数,默认 3(§3.13/G13,可调 1–8)
	ThemeLock          string `json:"themeLock"`          // ""|light|dark;空=跟随系统(D7)
}

// DefaultSettings 缺省设置:DestDir=用户 Downloads/feiwu、Threads=32(G5)、
// MaxConcurrentTasks=3(G13/§3.13)、ThemeLock=""(跟随系统,D7)。
// Downloads 目录三平台统一取 $HOME/Downloads:Windows 的 known folder 即
// %USERPROFILE%\Downloads 与之一致;Linux 不解析 user-dirs.dirs(需额外
// 依赖),$HOME/Downloads 是通行缺省——零依赖取舍,注释说明。
func DefaultSettings() Settings {
	return defaultSettings(os.UserHomeDir)
}

// defaultSettings 是 DefaultSettings 的可注入实现体(home 目录可替换,
// 离线测试)。HOME 拿不到时 DestDir 退化为相对 Downloads/feiwu——仍可用,
// 不 panic 不报错(可用性优先)。
func defaultSettings(homeDir func() (string, error)) Settings {
	home := ""
	if h, err := homeDir(); err == nil {
		home = h
	}
	return Settings{
		DestDir:            filepath.Join(home, "Downloads", "feiwu"),
		Threads:            32,
		MaxConcurrentTasks: 3,
		ThemeLock:          "",
	}
}

// LoadSettings 读 p.SettingsFile。容错设计(计划逐字):缺失/坏文件 →
// 默认,不报错——设置文件不是关键数据,任何读不出来都不该挡启动。
// 反序列化以默认值打底:存量文件缺字段(旧版本升级/用户手编)时缺的
// 取默认、在场的照读。
func LoadSettings(p Paths) Settings {
	def := DefaultSettings()
	b, err := os.ReadFile(p.SettingsFile)
	if err != nil {
		return def // 文件不存在(首次启动)或不可读 → 默认
	}
	s := def
	if err := json.Unmarshal(b, &s); err != nil {
		return def // 坏 JSON → 默认
	}
	return s
}

// settingsMu 把「写 tmp → rename 换入」绑成不可分事务(沿用 qmeta 模式,
// 见 internal/engine/qmeta.go 的 qmetaMu):webapi 的 PUT /api/settings
// 可能并发到达,同一进程内不交叉写同一个 tmp 文件。跨进程互斥不提供
// ——单实例应用。
var settingsMu sync.Mutex

// SaveSettings 把 s 原子写入 p.SettingsFile(tmp+rename,沿用 qmeta 模式):
// 写 <file>.tmp → f.Sync(数据先落盘,断电时要么旧文件完好、要么新内容
// 完整,不会出现目录项已换而 tmp 未刷盘的空壳)→ Close → os.Rename 一步
// 换入(POSIX rename(2) / Windows MoveFileEx(REPLACE_EXISTING),目标存在
// 直接原子替换,绝无「先删旧再改名」的断电窗口)。残留旧 tmp 无害:
// O_TRUNC 重写后照常换入。DataDir 不存在则先建(MkdirAll)。
//
// ThemeLock 契约 ""|light|dark(D7)在写盘前把关,非法值返回 error 不落
// 脏数据;Threads/Max 的档位与上限校验不在此重复——单一职责,由消费端
// (webapi PUT 校验 / engine Submit 钳制,见 Task 8/13)负责。
func SaveSettings(p Paths, s Settings) error {
	switch s.ThemeLock {
	case "", "light", "dark":
	default:
		return fmt.Errorf("store: ThemeLock %q 非法(须 \"\"|light|dark)", s.ThemeLock)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n') // 文本文件换行收尾(用户可能手编)

	settingsMu.Lock()
	defer settingsMu.Unlock()

	if err := os.MkdirAll(p.DataDir, 0o755); err != nil {
		return err
	}
	tmp := p.SettingsFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // 数据先落盘,再换目录项
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p.SettingsFile)
}
