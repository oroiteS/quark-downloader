// tasks.go 实现任务创建端点 POST /api/tasks（Task 9 / G11/E7/G12）：
// 目录 fid 递归展开（分页拉全，G11 后端负责）→ AllocateBudget 预算分配
// （E7）→ 批量取直链 → 逐项 manager.Submit（一文件一任务）→ TaskCtx 落盘
// （§6.2）→ 返回 {task_ids, skipped}；附 makeRefresher 自愈回调工厂。
package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"quark-downloader/internal/engine"
	"quark-downloader/internal/protocol"
	"quark-downloader/internal/store"
)

// tasksFileMu 把 tasks.json 的读-改-写（Load+append+Save）绑成临界区
// （fix round 1 发现 1）：并发 POST /api/tasks 时后写者必须在前写者
// 结果之上累加——丢更新=被覆盖方的任务上下文静默丢失（重启不续传、
// 无 needs_reparse，违反「绝不静默」红线）；同时消除并发 SaveTasks 争
// 同一 tmp 的 rename 竞态。包级锁与 store.settingsMu/tasksMu 同型
// （单实例应用前提）；临界区内无网络调用、不碰 authMu——无死锁面。
var tasksFileMu sync.Mutex

// taskManager 是任务创建消费的最小 manager 面（本包接口化以支持测试
// fake 注入：直接断言 Submit 收到的 Threads 档位；Task 10 的控制面
// pause/resume/cancel/retry 在此面上扩展）。*engine.Manager 满足之。
type taskManager interface {
	// Submit 提交下载任务（签名与 engine.Manager.Submit 逐字一致）：
	// 互斥拒绝（TryLockDest 失败）同步返回错误，不排队（§3.13）。
	Submit(lr protocol.LinkResult, relDir, cookieStr string, mode engine.Mode, opts ...engine.SubmitOpt) (string, error)
	// Close 停止全部任务并保留断点（E6）；幂等且并发安全。
	Close()
}

// createTasksRequest POST /api/tasks 请求体（字段名是 §6.2 契约）：
// source=share 时须带 pwd_id 且 stoken/passcode 至少其一（stoken 缺省时
// 用 passcode 走 ShareToken 现换）；dest 为相对 DestDir 的子目录前缀
// （G5「建任务时可改」——受 Submit 签名约束只支持子目录级覆盖，拒绝
// 逃逸）；mode "a"|"b" 缺省 ModeA。
type createTasksRequest struct {
	Source   string   `json:"source"`
	FIDs     []string `json:"fids"`
	PwdID    string   `json:"pwd_id"`
	Stoken   string   `json:"stoken"`
	Passcode string   `json:"passcode"`
	Dest     string   `json:"dest"`
	Mode     string   `json:"mode"`
}

// skippedItem 响应 skipped 数组元素：被跳过的文件及原因（当前仅同路径
// 互斥与取链失败两类；「目标已存在完整文件」的 G11 标注待任务列表端点
// 接入）。
type skippedItem struct {
	FID    string `json:"fid"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// fileItem 展开收集的文件项（G11：fid/name/size/share_fid_token/relDir）。
type fileItem struct {
	fid    string
	name   string
	size   int64
	token  string // share_fid_token（仅 share 模式非空）
	relDir string // 子树内相对目录（正斜杠；dest 前缀在 Submit 前拼入）
}

// errDestBusyMark 是引擎同路径互斥拒绝错误的稳定子串（§3.13 文案）；
// engine 侧无 sentinel，webapi 以子串识别并归一化为对外文案。
const errDestBusyMark = "同一文件正在下载中"

// handleCreateTasks POST /api/tasks：见文件头流程。错误映射——请求参数
// →400；未登录→401；上游协议（展开/换 stoken/取链）→502；manager 已关
// 闭→503；落盘失败→500（任务已提交，message 如实说明）。Submit 的互斥
// 拒绝与逐项失败进 skipped（批量语义：单项失败不毁整批），ErrClosed
// 例外——manager 已关后续必失败，中断并 503。
func (a *App) handleCreateTasks(c *gin.Context) {
	var req createTasksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_tasks_request",
			"请求体须为 {source, fids, pwd_id?, stoken?, passcode?, dest?, mode?} 的 JSON 对象")
		return
	}
	if req.Source != "share" && req.Source != "drive" {
		writeError(c, http.StatusBadRequest, "invalid_tasks_request", "source 须为 share 或 drive")
		return
	}
	if len(req.FIDs) == 0 {
		writeError(c, http.StatusBadRequest, "invalid_tasks_request", "fids 不能为空")
		return
	}
	mode, err := parseTaskMode(req.Mode)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_tasks_request", err.Error())
		return
	}
	destPrefix, err := cleanDestPrefix(req.Dest)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_tasks_request", err.Error())
		return
	}
	if req.Source == "share" {
		if req.PwdID == "" {
			writeError(c, http.StatusBadRequest, "invalid_tasks_request", "share 模式须带 pwd_id")
			return
		}
		if req.Stoken == "" && req.Passcode == "" {
			writeError(c, http.StatusBadRequest, "invalid_tasks_request", "share 模式须带 stoken 或 passcode（提取码）")
			return
		}
	}
	if a.manager == nil {
		writeError(c, http.StatusInternalServerError, "internal", "下载管理器未装配")
		return
	}

	// 会话快照（与 handleAccount 同模式）：logout 并发重建会话时用本帧
	// 的 session/jar/auth，网络调用不持锁。
	a.authMu.Lock()
	sess, auth := a.session, a.auth
	cookieStr := a.jar.String()
	a.authMu.Unlock()
	if sess == nil || !auth.LoginState() {
		writeError(c, http.StatusUnauthorized, "not_logged_in", "未登录")
		return
	}

	ctx := c.Request.Context()
	stoken := req.Stoken
	if req.Source == "share" && stoken == "" {
		st, err := sess.ShareToken(ctx, req.PwdID, req.Passcode)
		if err != nil {
			writeError(c, http.StatusBadGateway, "share_token_failed", "换取分享凭证失败: "+err.Error())
			return
		}
		stoken = st
	}

	// ① 目录递归展开（G11：后端负责，分页拉全在协议层）。
	items, err := expandFIDs(ctx, sess, req.Source, req.PwdID, stoken, req.FIDs)
	if err != nil {
		writeError(c, http.StatusBadGateway, "expand_failed", "展开目录失败: "+err.Error())
		return
	}
	if len(items) == 0 { // 全空目录：合法，空手而归
		c.JSON(http.StatusOK, gin.H{"task_ids": []string{}, "skipped": []skippedItem{}})
		return
	}

	// ② 批量取直链（三入口择优在协议层；share 免转存带三参）。
	linkReq := protocol.LinkRequest{FIDs: make([]string, 0, len(items))}
	if req.Source == "share" {
		linkReq.FIDsToken = make([]string, 0, len(items))
	}
	for _, it := range items {
		linkReq.FIDs = append(linkReq.FIDs, it.fid)
		if req.Source == "share" {
			linkReq.FIDsToken = append(linkReq.FIDsToken, it.token)
		}
	}
	linkReq.PwdID, linkReq.Stoken = "", ""
	if req.Source == "share" { // 免转存三参仅在 share 模式带上（isShareMode 判定），drive 请求乱传 pwd_id 也不误入
		linkReq.PwdID, linkReq.Stoken = req.PwdID, stoken
	}
	results, err := sess.DownloadLinks(ctx, linkReq)
	if err != nil {
		writeError(c, http.StatusBadGateway, "links_failed", "批量获取下载直链失败: "+err.Error())
		return
	}
	byFID := make(map[string]protocol.LinkResult, len(results))
	for _, r := range results {
		byFID[r.FID] = r
	}

	// ③ 预算分配（E7/§3.13）：cap=用户并发档位、concurrency=同时任务数；
	// 零值 settings 防御为 ≥1（AllocateBudget 对 cap<1 只保下限）。
	sizes := make([]int64, len(items))
	for i, it := range items {
		sizes[i] = it.size
	}
	budget := engine.AllocateBudget(sizes,
		max(a.settings.Threads, 1), max(a.settings.MaxConcurrentTasks, 1))

	// ④ 逐项 Submit（一文件一任务）＋ ⑤ TaskCtx 落盘。
	var (
		taskIDs []string
		skipped []skippedItem
		newCtxs []store.TaskCtx
	)
	createdAt := time.Now().Unix()
	for i, it := range items {
		lr, ok := byFID[it.fid]
		if !ok || lr.URL == "" {
			skipped = append(skipped, skippedItem{it.fid, it.name, "取直链失败（无可用 URL）"})
			continue
		}
		relDir := it.relDir
		if destPrefix != "" {
			relDir = path.Join(destPrefix, it.relDir)
		}
		tc := store.TaskCtx{
			FileName:  it.name,
			PwdID:     req.PwdID,
			Stoken:    stoken,
			Passcode:  req.Passcode, // G12：持久化提取码，重启后可重取 stoken
			FID:       it.fid,
			Token:     it.token,
			FromShare: req.Source == "share",
			RelDir:    relDir,
			Dest:      absDest(a.settings.DestDir, relDir, it.name),
			Size:      it.size,
			CreatedAt: createdAt,
		}
		// G11「目标已存在同名完整文件 → 跳过并在任务中标注『已存在』」：
		// dest 存在且 size 与列表值相符 → 不提交引擎（省一轮对账与登记），
		// 写一条 Exists=true 的 TaskCtx（EngineID 留空=无引擎任务，T10 的
		// GET /api/tasks 透出「已存在」终态），同时进响应 skipped。列表
		// size≤0 时判定不可信——保守提交，完整性对账交给引擎。
		if it.size > 0 {
			if st, serr := os.Stat(tc.Dest); serr == nil && st.Size() == it.size {
				tc.Exists = true
				newCtxs = append(newCtxs, tc)
				skipped = append(skipped, skippedItem{it.fid, it.name, "已存在"})
				continue
			}
		}
		// Refresher 闭包捕获 tc 值拷贝（EngineID 回填不影响刷新语义——
		// 刷新只消费 fid/分享上下文）。
		id, err := a.manager.Submit(
			protocol.LinkResult{FID: it.fid, FileName: it.name, Size: it.size, URL: lr.URL, Score: lr.Score},
			relDir, cookieStr, mode,
			engine.SubmitOpt{Threads: budget[i], Refresher: a.makeRefresher(tc)})
		if err != nil {
			if errors.Is(err, engine.ErrClosed) {
				writeError(c, http.StatusServiceUnavailable, "manager_closed",
					fmt.Sprintf("下载管理器已关闭（已提交 %d 个任务，其余中断）: %v", len(taskIDs), err))
				return
			}
			reason := err.Error()
			if strings.Contains(reason, errDestBusyMark) {
				reason = errDestBusyMark // §3.13 逐字文案
			}
			skipped = append(skipped, skippedItem{it.fid, it.name, reason})
			continue
		}
		tc.EngineID = id
		newCtxs = append(newCtxs, tc)
		taskIDs = append(taskIDs, id)
	}

	// 落盘（存量 + 新增，原子覆盖；Task 11 恢复链消费）。读-改-写整体
	// 持 tasksFileMu（fix round 1 发现 1）：并发创建请求在此串行化——
	// 后写者以前写者的落盘结果为存量累加，杜绝丢更新；亦消除并发
	// SaveTasks 争同一 tmp 的 rename 竞态。
	if len(newCtxs) > 0 {
		tasksFileMu.Lock()
		err := store.SaveTasks(a.paths, append(store.LoadTasks(a.paths), newCtxs...))
		tasksFileMu.Unlock()
		if err != nil {
			writeError(c, http.StatusInternalServerError, "save_tasks_failed",
				fmt.Sprintf("已提交 %d 个任务但持久化任务上下文失败（重启后不自动续传）: %v", len(taskIDs), err))
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"task_ids": taskIDs, "skipped": skipped})
}

// makeRefresher 直链自愈回调工厂（G12/§2.10）：任务级一次一换语义由
// 引擎保证（maybeRefresh 计数与择机），这里只产新 URL——
//
//	FromShare → ShareToken(pwd_id, passcode) 重取 stoken（失败即返回错）
//	  → DownloadLinks 免转存三参（新 stoken）取该 fid 的 URL；
//	drive → DownloadLinks({fids:[fid]})。
//
// 会话每次刷新时快照当前 a.session（logout 重建后旧代闭包不误用弃用
// session）；session 不可用即返回错（按「取不到新链」中止保留断点）。
func (a *App) makeRefresher(t store.TaskCtx) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		a.authMu.Lock()
		sess := a.session
		a.authMu.Unlock()
		if sess == nil {
			return "", errors.New("会话不可用（未登录），无法重取直链")
		}
		req := protocol.LinkRequest{FIDs: []string{t.FID}}
		if t.FromShare {
			stoken, err := sess.ShareToken(ctx, t.PwdID, t.Passcode)
			if err != nil {
				return "", fmt.Errorf("重取 stoken 失败: %w", err)
			}
			if stoken == "" {
				return "", errors.New("重取 stoken 失败: 响应为空")
			}
			req.FIDsToken = []string{t.Token}
			req.PwdID = t.PwdID
			req.Stoken = stoken
		}
		results, err := sess.DownloadLinks(ctx, req)
		if err != nil {
			return "", fmt.Errorf("重取直链失败: %w", err)
		}
		for _, r := range results {
			if r.FID == t.FID && r.URL != "" {
				return r.URL, nil
			}
		}
		return "", fmt.Errorf("重取直链失败: 结果中无 %s 的可用 URL", t.FID)
	}
}

// expandFIDs 逐个 fid 递归展开目录树，收集文件项（relDir 保留子树内
// 相对结构，G11）。visited 防环兼防 fids 重复（同一目录不二次展开）；
// 上游树结构异常成环时按已访问剪枝，不无限递归。
func expandFIDs(ctx context.Context, sess *protocol.Session, source, pwdID, stoken string, fids []string) ([]fileItem, error) {
	var items []fileItem
	visited := make(map[string]bool, len(fids))
	for _, fid := range fids {
		var err error
		if source == "share" {
			err = expandShare(ctx, sess, pwdID, stoken, fid, "", visited, &items)
		} else {
			err = expandDrive(ctx, sess, fid, "", visited, &items)
		}
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

// expandShare 分享侧展开（ShareList 分页拉全在协议层）；relDir 用正斜杠
// 拼接（远端路径语义；engine Submit 内部 filepath.Join 兼容）。
func expandShare(ctx context.Context, sess *protocol.Session, pwdID, stoken, fid, relDir string, visited map[string]bool, out *[]fileItem) error {
	if visited[fid] {
		return nil
	}
	visited[fid] = true
	entries, err := sess.ShareList(ctx, pwdID, stoken, fid)
	if err != nil {
		return fmt.Errorf("列出分享目录 %s 失败: %w", fid, err)
	}
	for _, e := range entries {
		if e.Dir {
			if err := expandShare(ctx, sess, pwdID, stoken, e.FID, path.Join(relDir, e.FileName), visited, out); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, fileItem{fid: e.FID, name: e.FileName, size: e.Size, token: e.ShareFIDToken, relDir: relDir})
	}
	return nil
}

// expandDrive 网盘侧展开（DriveList；响应无 share_fid_token）。
func expandDrive(ctx context.Context, sess *protocol.Session, fid, relDir string, visited map[string]bool, out *[]fileItem) error {
	if visited[fid] {
		return nil
	}
	visited[fid] = true
	entries, err := sess.DriveList(ctx, fid)
	if err != nil {
		return fmt.Errorf("列出网盘目录 %s 失败: %w", fid, err)
	}
	for _, e := range entries {
		if e.Dir {
			if err := expandDrive(ctx, sess, e.FID, path.Join(relDir, e.FileName), visited, out); err != nil {
				return err
			}
			continue
		}
		*out = append(*out, fileItem{fid: e.FID, name: e.FileName, size: e.Size, relDir: relDir})
	}
	return nil
}

// parseTaskMode 请求 mode 字段：""（缺省）/"a"/"A" → ModeA；"b"/"B" →
// ModeB（§3.5 两种落盘模式）；其余报参数错误。
func parseTaskMode(s string) (engine.Mode, error) {
	switch s {
	case "", "a", "A":
		return engine.ModeA, nil
	case "b", "B":
		return engine.ModeB, nil
	default:
		return engine.ModeA, fmt.Errorf("mode %q 非法（须为 a 或 b）", s)
	}
}

// cleanDestPrefix dest 前缀校验：相对 DestDir 的子目录（G5 建任务时可改
// 的子目录级覆盖）；拒绝绝对路径与 .. 逃逸（engine Submit 还有逃逸
// 拒绝兜底，这里前置报参数错误更友好）。空串（缺省）无前缀。
func cleanDestPrefix(dest string) (string, error) {
	if dest == "" {
		return "", nil
	}
	if filepath.IsAbs(dest) || strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, `\`) {
		return "", fmt.Errorf("dest %q 须为相对下载目录的子目录", dest)
	}
	clean := filepath.Clean(dest)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("dest %q 不能逃逸下载目录", dest)
	}
	return clean, nil
}

// absDest 与引擎内部目标路径组装对齐（Submit: filepath.Join(DestDir,
// relDir, FileName) + Abs）：TaskCtx.Dest 存绝对路径，恢复对账与前端
// 展示共用。
func absDest(destDir, relDir, name string) string {
	abs, err := filepath.Abs(filepath.Join(destDir, relDir, name))
	if err != nil {
		return filepath.Join(destDir, relDir, name) // Abs 失败（极端环境）退化为拼接值
	}
	return abs
}
