// accountcache.go 实现昵称/头像的持久缓存（<DataDir>/account.json）：
// 登录链拿到 info 时写入，handleAccount 的 QuickCheck 走 probeFileSort
// 兜底拿到零值 info 且进程内 cachedAccount 为空（重启即空）时回退——
// 解决「凭据落盘已生效（登录态幸存）但昵称空白」的真机场景（T15）。
//
// 文件形态对齐 settings.json 惯例：JSON、tmp+rename 原子写、MkdirAll；
// 权限 0600（昵称/头像属账号隐私面，与 secret.bin 同级而非 settings 的
// 0644）。缺失/坏文件/字段缺失一律按零值处理不报错——缓存只是显示件，
// 任何读不出来都不该挡账号查询。
package webapi

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"quark-downloader/internal/protocol"
)

// accountJSONName 是昵称/头像缓存文件名（DataDir 下，与 settings.json /
// tasks.json / secret.bin 同层）。
const accountJSONName = "account.json"

// accountProfile 是 account.json 的内存形态（字段即 json tag，前端不直接
// 消费本文件——它是服务端重启后的昵称/头像回退源，wire 不暴露）。
type accountProfile struct {
	Nickname string `json:"nickname"`
	Avatar   string `json:"avatar"`
}

// accountMu 把「写 tmp → rename 换入」绑成不可分事务（沿用 settingsMu
// 模式）：handleQRState 登录链与 handleAccount 刷新可能并发到达，同一
// 进程内不交叉写同一个 tmp 文件。跨进程互斥不提供——单实例应用。
var accountMu sync.Mutex

// loadAccountProfile 读 <dataDir>/account.json。容错契约（对齐
// LoadSettings）：缺失/坏 JSON → 零值，不报错。
func loadAccountProfile(dataDir string) accountProfile {
	b, err := os.ReadFile(filepath.Join(dataDir, accountJSONName))
	if err != nil {
		return accountProfile{} // 文件不存在（从未登录过/首次启动）→ 零值
	}
	var p accountProfile
	if err := json.Unmarshal(b, &p); err != nil {
		return accountProfile{} // 坏 JSON（损坏/手编残缺）→ 零值
	}
	return p
}

// saveAccountProfile 把 p 原子写入 <dataDir>/account.json（tmp+rename，
// 沿用 SaveSettings 模式：写 tmp → Sync → Close → Rename 一步换入，断电
// 时要么旧文件完好、要么新内容完整）。文件权限 0600。
func saveAccountProfile(dataDir string, p accountProfile) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n') // 文本文件换行收尾

	accountMu.Lock()
	defer accountMu.Unlock()

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dataDir, accountJSONName)
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
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
	return os.Rename(tmp, target)
}

// refreshAccountCache 刷新昵称/头像双层缓存：进程内 cachedAccount（本代
// 会话）+ 盘 account.json（跨重启）。自取 authMu——仅供不持锁的调用方
// （handleAccount 的 QuickCheck 后路径，网络调用不持锁）；登录链
// （handleQRState/handleCookieImport）全程持 authMu，必须走
// refreshAccountCacheLocked，否则锁不可重入即自死锁（T15 修正）。
func (a *App) refreshAccountCache(info protocol.AccountInfo) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	a.refreshAccountCacheLocked(info)
}

// refreshAccountCacheLocked 是 refreshAccountCache 的持锁版：调用方必须
// 已持 authMu（handleQRState GotTicket 分支 / handleCookieImport 成功路径
// 的既有临界区内调用），进程内 cachedAccount 写入免二次取锁。
//
// 零值 info 只更新进程内层、不落盘——防把盘上旧缓存（上次登录的昵称）
// 抹成空，回退链将在下次登录/刷新时覆盖为真值。落盘失败不致命（缓存
// 只是显示件，重启后大不了空昵称走前端占位）：记一行不报错。DataDir
// 为空（Degenerate 配置/部分测试装配）只跳过盘层，进程内层照常。盘写
// 不依赖 authMu（saveAccountProfile 自有 accountMu 串行化），在临界区内
// 多付一次 ms 级文件 IO，与 SaveCookie 同临界区同模式，可接受。
func (a *App) refreshAccountCacheLocked(info protocol.AccountInfo) {
	a.cachedAccount = info

	if info.Nickname == "" && info.AvatarURI == "" {
		return // 零值不落盘：防把盘上旧缓存抹成空
	}
	if a.paths.DataDir == "" {
		return // 无数据目录可落：进程内缓存已够本代使用
	}
	if err := saveAccountProfile(a.paths.DataDir, accountProfile{
		Nickname: info.Nickname,
		Avatar:   info.AvatarURI,
	}); err != nil {
		log.Printf("webapi: 昵称/头像缓存落盘失败（不影响登录，重启后昵称回退占位）: %v", err)
	}
}
