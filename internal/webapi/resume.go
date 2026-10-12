// resume.go 实现重启自动续传（Task 11 / D9/G12）：App 启动时
// restoreTasks 读回 tasks.json，逐项按「已存在 / 不可重取 / 可重取」
// 分流——可重取的先 makeRefresher 试取新链（G12：share 先重取
// stoken），成功即 Submit（断点自动复用，G4：引擎对账 total 吻合即
// 续传）；失败与不可重取的如实标 NeedsReparse（Review Focus #3：
// 绝不静默——磁盘无完整文件而显示「完成」正是红线要防的形态）。
// 恢复计数写日志；落盘持 tasksFileMu（与创建/复活同一临界区纪律）。
package webapi

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"quark-downloader/internal/engine"
	"quark-downloader/internal/protocol"
	"quark-downloader/internal/store"
)

// restoreTasks 任务恢复链（App.Start 在监听前调用——此时无 HTTP
// 并发，读-改-写以快照整体回写，锁仅保持纪律统一）。恢复语义：
//
//	Exists 条目 → 跳过（已存在终态，计数）；
//	dest 已存在同名完整文件（size 相符，G11 同语义）→ 标 Exists 终态，
//	  不 Submit——「磁盘上已完整的」不自动开跑；
//	manager 未装配 → 如实标 NeedsReparse（不挡启动）；
//	!CanRefresh（上下文字面不足以重取）→ 如实标 NeedsReparse；
//	CanRefresh → makeRefresher 试取新链：
//	  成功 → Submit（mode 从 TaskCtx.Mode 读回、Threads=settings.Threads
//	    单任务全档、Refresher 复用）→ EngineID 换新（断点由引擎 .qmeta
//	    对账续传，不重下）；
//	  失败 → 如实标 NeedsReparse（每次启动重试：成功即解除标注——
//	    临时性失败（网络/未登录）自愈，用户也可重新解析创建）。
//
// NeedsReparse 条目 EngineID 清空（旧引擎 id 在新进程无意义；GET
// /api/tasks 透出 ctx:fid 合成 id 的 needs_reparse 终态）。
func (a *App) restoreTasks(ctx context.Context) error {
	ctxs := store.LoadTasks(a.paths)
	if len(ctxs) == 0 {
		return nil
	}

	// cookie 快照一次（恢复期间不变；makeRefresher 内部另行快照
	// session，与创建端点同模式）。
	a.authMu.Lock()
	cookieStr := a.jar.String()
	a.authMu.Unlock()

	var resumed, exists, reparse int
	for i := range ctxs {
		tc := &ctxs[i]
		if tc.Exists { // 已存在终态：无需恢复
			exists++
			continue
		}
		// G11 同语义（恢复路径变体）：dest 完整且**无引擎断点凭证
		// （.qmeta）**才标「已存在」终态——模式 A 预分配（worker.go
		// preallocate：开局即 Truncate 到 total），仅凭 size 相符会把
		// 自己的断点文件误判完整（断点永不续传）。有 .qmeta 一律交
		// 引擎对账：部分断点续传、已完成秒完成（brief 逐字「引擎对账
		// 天然处理」）。size≤0 判定不可信，同样交引擎。
		if tc.Size > 0 {
			if _, qerr := os.Stat(tc.Dest + ".qmeta"); qerr != nil {
				if st, err := os.Stat(tc.Dest); err == nil && !st.IsDir() && st.Size() == tc.Size {
					tc.Exists = true
					tc.EngineID = ""
					exists++
					continue
				}
			}
		}
		if a.manager == nil || !tc.CanRefresh() {
			// 未装配（异常装配）或上下文不足以重取（share 凭证残缺）：
			// 不碰网络，如实标注（brief 逐字「如实」）。
			tc.NeedsReparse = true
			tc.EngineID = ""
			reparse++
			continue
		}
		refresher := a.makeRefresher(*tc)
		url, err := refresher(ctx)
		if err != nil {
			slog.Warn("feiwu: 恢复任务重取直链失败", "fid", tc.FID, "dest", tc.Dest, "err", err)
			tc.NeedsReparse = true
			tc.EngineID = ""
			reparse++
			continue
		}
		mode, _ := parseTaskMode(tc.Mode) // 空值/旧数据 → ModeA（与 modeLabel 对齐）
		id, err := a.manager.Submit(
			protocol.LinkResult{FID: tc.FID, FileName: tc.FileName, Size: tc.Size, URL: url},
			tc.RelDir, cookieStr, mode,
			engine.SubmitOpt{Threads: max(a.settings.Threads, 1), Refresher: refresher})
		if err != nil {
			slog.Warn("feiwu: 恢复任务提交失败", "fid", tc.FID, "dest", tc.Dest, "err", err)
			tc.NeedsReparse = true
			tc.EngineID = ""
			reparse++
			continue
		}
		tc.EngineID = id
		tc.NeedsReparse = false // 重试语境下恢复成功即解除标注
		resumed++
	}

	// 整体回写（启动期无并发；持锁保持与创建/复活端点同一纪律）。
	// 落盘失败返回错挡启动：Submit 已发生但上下文丢失会让下次启动
	// 重复恢复——错误可见优于静默降级（断点仍在盘，自愈无损）。
	tasksFileMu.Lock()
	err := store.SaveTasks(a.paths, ctxs)
	tasksFileMu.Unlock()
	if err != nil {
		return fmt.Errorf("持久化恢复结果: %w", err)
	}
	slog.Info("feiwu: 恢复任务完成",
		"resumed", resumed, "exists", exists, "needs_reparse", reparse)
	return nil
}
