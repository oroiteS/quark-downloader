// settings.go 设置读写端点（Task 13）：
//
//	GET /api/settings —— 现值快照
//	PUT /api/settings —— 校验（theme 枚举在 SaveSettings 写侧把关,
//	  T2 裁决；threads 1–512 / max 1–8 / destDir 非空在本层把关）→
//	  落盘 → 热应用：destDir 同步 manager.SetDestDir（Ruling T13-R1：
//	  webapi absDest 与引擎 Join(cfg.DestDir) 两侧一致）；threads/max
//	  即时生效于后续任务的预算分配（Ruling T13-R2 webapi 侧：cap 读
//	  settings 快照——引擎调度容量构造期固化, 需重启生效, 如实标注）
//
// settingsHotMu：PUT 写 a.settings 与 tasks.go 各读处（预算 cap/absDest/
// retry 档位）的并发防护——包级 RWMutex（与 tasksFileMu 同型先例,
// 单实例应用）。
package webapi

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"quark-downloader/internal/store"
)

// settingsHotMu 保护 App.settings 的读写（PUT 写 / 业务读快照）。
var settingsHotMu sync.RWMutex

// settingsSnapshot 返回当前设置的一致快照（业务读方一律走这里,
// 不直接读 a.settings 字段）。
func (a *App) settingsSnapshot() store.Settings {
	settingsHotMu.RLock()
	defer settingsHotMu.RUnlock()
	return a.settings
}

// handleSettingsGet GET /api/settings —— {destDir, threads,
// maxConcurrentTasks, themeLock}（json tag 即前端契约）。
func (a *App) handleSettingsGet(c *gin.Context) {
	c.JSON(http.StatusOK, a.settingsSnapshot())
}

// handleSettingsPut PUT /api/settings——校验 → 落盘 → 热应用。
func (a *App) handleSettingsPut(c *gin.Context) {
	var s store.Settings
	if err := c.ShouldBindJSON(&s); err != nil {
		writeError(c, http.StatusBadRequest, codeInvalidSettings, "请求体须为 Settings 形状")
		return
	}
	// 档位/目录校验（ThemeLock 枚举同款把关——本地先校验, 使落盘阶段的
	// 错误可全部归为 IO 类 500）。
	if s.DestDir == "" {
		writeError(c, http.StatusBadRequest, codeInvalidSettings, "destDir 不可为空")
		return
	}
	if s.Threads < 1 || s.Threads > 512 {
		writeError(c, http.StatusBadRequest, codeInvalidSettings, "threads 须在 1–512")
		return
	}
	if s.MaxConcurrentTasks < 1 || s.MaxConcurrentTasks > 8 {
		writeError(c, http.StatusBadRequest, codeInvalidSettings, "maxConcurrentTasks 须在 1–8")
		return
	}
	switch s.ThemeLock {
	case "", "light", "dark": // D7 契约, 与 store.SaveSettings 同款
	default:
		writeError(c, http.StatusBadRequest, codeInvalidSettings,
			"themeLock 须为 \"\"|light|dark")
		return
	}
	// 落盘（此时错误只可能是 IO——500, 非参数问题）。
	if err := store.SaveSettings(a.paths, s); err != nil {
		writeError(c, http.StatusInternalServerError, codeSaveSettingsFailed, err.Error())
		return
	}
	// 热应用（T13-R1：引擎侧 dest 同步；运行中任务 mt.dest 已算好不动）。
	settingsHotMu.Lock()
	a.settings = s
	settingsHotMu.Unlock()
	if m := a.manager; m != nil {
		m.SetDestDir(s.DestDir)
	}
	c.JSON(http.StatusOK, s)
}
