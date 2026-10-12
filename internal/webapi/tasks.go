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

// taskManager 是 webapi 消费的 manager 面（本包接口化以支持测试
// fake 注入：直接断言 Submit 收到的 Threads 档位与控制面调用）。
// *engine.Manager 满足之。
type taskManager interface {
	// Submit 提交下载任务（签名与 engine.Manager.Submit 逐字一致）：
	// 互斥拒绝（TryLockDest 失败→wrap ErrDestBusy）同步返回错误，
	// 不排队（§3.13）。
	Submit(lr protocol.LinkResult, relDir, cookieStr string, mode engine.Mode, opts ...engine.SubmitOpt) (string, error)
	// Get 查询任务进度快照（不存在/已淘汰返回 wrap ErrTaskNotFound）。
	Get(id string) (engine.Progress, error)
	// Pause/Resume/Cancel/Retry 控制面（engine 语义：不存在时静默
	// no-op；Close 后不复活——复活职责在 webapi 的 retry 复活路径）。
	Pause(id string)
	Resume(id string)
	Cancel(id string)
	Retry(id string)
	// Close 停止全部任务并保留断点（E6）；幂等且并发安全。
	Close()
	// SetDestDir 热更新下载根目录（Task 13, Ruling T13-R1：PUT
	// /api/settings 热应用——与 webapi 侧 absDest 同源一致）。
	SetDestDir(dir string)
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

// errDestBusyMark 是引擎同路径互斥拒绝的对外文案（§3.13 逐字）；判定
// 走 errors.Is(err, engine.ErrDestBusy)（Task 10 起 engine 以 wrap
// sentinel 返回，文案不变）。
const errDestBusyMark = "同一文件正在下载中"

// handleCreateTasks POST /api/tasks：见文件头流程。错误映射——请求参数
// →400；未登录→401；上游协议（展开/换 stoken/取链）→502；manager 已关
// 闭→503；落盘失败→500（任务已提交，message 如实说明）。Submit 的互斥
// 拒绝与逐项失败进 skipped（批量语义：单项失败不毁整批），ErrClosed
// 例外——manager 已关后续必失败，中断并 503。
func (a *App) handleCreateTasks(c *gin.Context) {
	var req createTasksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, codeInvalidTasksRequest,
			"请求体须为 {source, fids, pwd_id?, stoken?, passcode?, dest?, mode?} 的 JSON 对象")
		return
	}
	if req.Source != "share" && req.Source != "drive" {
		writeError(c, http.StatusBadRequest, codeInvalidTasksRequest, "source 须为 share 或 drive")
		return
	}
	if len(req.FIDs) == 0 {
		writeError(c, http.StatusBadRequest, codeInvalidTasksRequest, "fids 不能为空")
		return
	}
	mode, err := parseTaskMode(req.Mode)
	if err != nil {
		writeError(c, http.StatusBadRequest, codeInvalidTasksRequest, err.Error())
		return
	}
	destPrefix, err := cleanDestPrefix(req.Dest)
	if err != nil {
		writeError(c, http.StatusBadRequest, codeInvalidTasksRequest, err.Error())
		return
	}
	if req.Source == "share" {
		if req.PwdID == "" {
			writeError(c, http.StatusBadRequest, codeInvalidTasksRequest, "share 模式须带 pwd_id")
			return
		}
		if req.Stoken == "" && req.Passcode == "" {
			writeError(c, http.StatusBadRequest, codeInvalidTasksRequest, "share 模式须带 stoken 或 passcode（提取码）")
			return
		}
	}
	if a.manager == nil {
		writeError(c, http.StatusInternalServerError, codeInternal, "下载管理器未装配")
		return
	}

	// 会话快照（与 handleAccount 同模式）：logout 并发重建会话时用本帧
	// 的 session/jar/auth，网络调用不持锁。
	a.authMu.Lock()
	sess, auth := a.session, a.auth
	cookieStr := a.jar.String()
	a.authMu.Unlock()
	if sess == nil || !auth.LoginState() {
		writeError(c, http.StatusUnauthorized, codeNotLoggedIn, "未登录")
		return
	}

	ctx := c.Request.Context()
	stoken := req.Stoken
	if req.Source == "share" && stoken == "" {
		st, err := sess.ShareToken(ctx, req.PwdID, req.Passcode)
		if err != nil {
			writeError(c, http.StatusBadGateway, codeShareTokenFailed, "换取分享凭证失败: "+err.Error())
			return
		}
		stoken = st
	}

	// ① 目录递归展开（G11：后端负责，分页拉全在协议层）。
	items, err := expandFIDs(ctx, sess, req.Source, req.PwdID, stoken, req.FIDs)
	if err != nil {
		writeError(c, http.StatusBadGateway, codeExpandFailed, "展开目录失败: "+err.Error())
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
		writeError(c, http.StatusBadGateway, codeLinksFailed, "批量获取下载直链失败: "+err.Error())
		return
	}
	byFID := make(map[string]protocol.LinkResult, len(results))
	for _, r := range results {
		byFID[r.FID] = r
	}

	// ③ 预算分配（E7/§3.13）：cap=用户并发档位、concurrency=同时任务数；
	// 零值 settings 防御为 ≥1（AllocateBudget 对 cap<1 只保下限）。
	// settings 快照（Task 13：PUT 热应用与创建并发时的读写一致）。
	snap := a.settingsSnapshot()
	sizes := make([]int64, len(items))
	for i, it := range items {
		sizes[i] = it.size
	}
	budget := engine.AllocateBudget(sizes,
		max(snap.Threads, 1), max(snap.MaxConcurrentTasks, 1))

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
			Dest:      absDest(snap.DestDir, relDir, it.name),
			Size:      it.size,
			Mode:      modeString(mode), // Task 10：DTO mode 与 retry 复活读回
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
				writeError(c, http.StatusServiceUnavailable, codeManagerClosed,
					fmt.Sprintf("下载管理器已关闭（已提交 %d 个任务，其余中断）: %v", len(taskIDs), err))
				return
			}
			if errors.Is(err, engine.ErrInvalidThreads) {
				// 参数错误整请求拒绝（Task 12 错误码冻结：400 不进
				// skipped——档位来自 AllocateBudget 恒合法，此为防御接线）。
				writeError(c, http.StatusBadRequest, codeInvalidThreads, err.Error())
				return
			}
			reason := err.Error()
			if errors.Is(err, engine.ErrDestBusy) {
				reason = errDestBusyMark // §3.13 逐字文案（sentinel 判定，非子串匹配）
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
			writeError(c, http.StatusInternalServerError, codeSaveTasksFailed,
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

// ---- Task 10：任务查询与控制端点 ----

// taskDTO GET /api/tasks 响应条目（brief 字段清单逐字；error 字段当前
// 无稳定来源——引擎 Progress 不携带错误文本，失败详情走 SSE 终态帧
// （Task 12），此处恒缺省）。
type taskDTO struct {
	ID           string  `json:"id"`
	FileName     string  `json:"file_name"`
	RelDir       string  `json:"rel_dir"`
	Size         int64   `json:"size"`
	State        string  `json:"state"`
	Done         int64   `json:"done"`
	SpeedBPS     float64 `json:"speed_bps"`
	EtaMS        int64   `json:"eta_ms"`
	Mode         string  `json:"mode"`
	Error        string  `json:"error,omitempty"`
	NeedsReparse bool    `json:"needs_reparse,omitempty"`
	CreatedAt    int64   `json:"created_at"`
}

// handleListTasks GET /api/tasks：以 tasks.json（TaskCtx）为主表逐条
// 聚合——引擎在册→进度字段来自 Get 快照；引擎淘汰→CanRefresh 可重取的
// 标 needs_reparse（前端引导重新解析/retry 复活），否则历史终态
// finished；Exists 条目→「已存在」终态（合成 id "ctx:"+fid，不可控制）。
// 纯本地读不查登录；LoadTasks 不持 tasksFileMu（SaveTasks 原子 rename，
// 读者见旧或新完整文件）。引擎在册但 TaskCtx 缺失（落盘中断的边缘
// 场景）不透出——无文件名/模式等上下文，且持久化已断需重新创建。
func (a *App) handleListTasks(c *gin.Context) {
	c.JSON(http.StatusOK, a.tasksSnapshot())
}

// tasksSnapshot 全量任务快照（GET /api/tasks 响应体；SSE hello 帧载荷
// 同源——Task 12 断线对账与 REST 拉取一套数据两种投递）。
func (a *App) tasksSnapshot() []taskDTO {
	ctxs := store.LoadTasks(a.paths)
	out := make([]taskDTO, 0, len(ctxs))
	for _, tc := range ctxs {
		out = append(out, a.taskDTOFromCtx(tc))
	}
	return out
}

// taskDTOFromCtx 单条 TaskCtx → DTO。
func (a *App) taskDTOFromCtx(tc store.TaskCtx) taskDTO {
	d := taskDTO{
		FileName:  tc.FileName,
		RelDir:    tc.RelDir,
		Size:      tc.Size,
		Mode:      modeLabel(tc.Mode),
		CreatedAt: tc.CreatedAt,
	}
	if tc.Exists { // G11「已存在」终态：done=size，速度/ETA 无意义
		d.ID = "ctx:" + tc.FID
		d.State = "exists"
		d.Done = tc.Size
		return d
	}
	if tc.NeedsReparse { // 恢复链如实标注（Ruling T11-R1）：不可重取或
		// 恢复期重取失败的条目——needs_reparse 终态，绝不静默（Focus #3）。
		d.ID = "ctx:" + tc.FID
		d.State = "needs_reparse"
		d.NeedsReparse = true
		return d
	}
	d.ID = tc.EngineID
	if tc.EngineID == "" { // 残缺条目（落盘中断等）：按历史终态透出
		d.State = "finished"
		return d
	}
	if a.manager != nil {
		if p, err := a.manager.Get(tc.EngineID); err == nil {
			d.State = p.State
			d.Done = p.Done
			d.SpeedBPS = p.SpeedBPS
			d.EtaMS = etaMS(p)
			return d
		}
	}
	// 引擎 miss（淘汰/重启后未恢复）：D9 语义——可重取标 needs_reparse，
	// 否则历史终态。
	if tc.CanRefresh() {
		d.State = "needs_reparse"
		d.NeedsReparse = true
		return d
	}
	d.State = "finished"
	return d
}

// handleTaskPause/handleTaskResume/handleTaskCancel 控制端点：引擎在册
// 直接控制（幂等，200）；淘汰/历史任务→409 needs_reparse（brief 逐字，
// 前端引导重新解析）；双 miss→404。Exists 条目 EngineID 空，任何 id
// 匹配不上→404（不可控制）。
func (a *App) handleTaskPause(c *gin.Context) {
	a.controlTask(c, func(id string) { a.manager.Pause(id) })
}

func (a *App) handleTaskResume(c *gin.Context) {
	a.controlTask(c, func(id string) { a.manager.Resume(id) })
}

func (a *App) handleTaskCancel(c *gin.Context) {
	a.controlTask(c, func(id string) { a.manager.Cancel(id) })
}

// controlTask pause/resume/cancel 公共流：Get 探测在册→fn 控制；
// ErrTaskNotFound→TaskCtx 匹配 EngineID（有→409 needs_reparse，无→
// 404）。Get 与控制之间有竞态窗口（Get 后被淘汰），控制方法对不存在
// 任务是静默 no-op——响应 200 但下次列表即见真实状态（最终一致）。
func (a *App) controlTask(c *gin.Context, fn func(id string)) {
	if a.manager == nil {
		writeError(c, http.StatusInternalServerError, codeInternal, "下载管理器未装配")
		return
	}
	id := c.Param("id")
	if _, err := a.manager.Get(id); err == nil {
		fn(id)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if ctxTaskExists(a.paths, id) {
		writeError(c, http.StatusConflict, codeNeedsReparse,
			"任务已不在下载引擎中（已淘汰或进程重启），需重新解析后重试")
		return
	}
	writeError(c, http.StatusNotFound, codeTaskNotFound, "任务不存在: "+id)
}

// handleTaskRetry 引擎在册→引擎 Retry（对运行中/排队中幂等无效，记
// 日志——engine 语义）；淘汰/历史失败→**重走创建路径复活**（brief：
// 用 TaskCtx 重走创建，Refresher 复活）——可重取的先 makeRefresher
// 产新链再 Submit（mode 从 TaskCtx.Mode 读回，档位取 settings.Threads
// ——单任务复活无批量预算可言），TaskCtx.EngineID 换新（持 tasksFileMu
// 读-改-写，与创建端点同一临界区）；链取不到（stoken 过期等）→409
// needs_reparse 如实相告（G12 红线：绝不静默）。
func (a *App) handleTaskRetry(c *gin.Context) {
	if a.manager == nil {
		writeError(c, http.StatusInternalServerError, codeInternal, "下载管理器未装配")
		return
	}
	id := c.Param("id")
	if _, err := a.manager.Get(id); err == nil {
		a.manager.Retry(id)
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	ctxs := store.LoadTasks(a.paths)
	idx := -1
	for i := range ctxs {
		if ctxs[i].EngineID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(c, http.StatusNotFound, codeTaskNotFound, "任务不存在: "+id)
		return
	}
	tc := ctxs[idx]
	if !tc.CanRefresh() {
		writeError(c, http.StatusConflict, codeNeedsReparse,
			"任务上下文不足以重取直链（缺分享凭证），需重新解析分享")
		return
	}

	// 登录态快照（与创建端点同模式：复活要走协议取新链）。
	a.authMu.Lock()
	sess, auth := a.session, a.auth
	cookieStr := a.jar.String()
	a.authMu.Unlock()
	if sess == nil || !auth.LoginState() {
		writeError(c, http.StatusUnauthorized, codeNotLoggedIn, "未登录")
		return
	}

	refresher := a.makeRefresher(tc)
	url, err := refresher(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusConflict, codeNeedsReparse, "重取直链失败（需重新解析）: "+err.Error())
		return
	}
	mode, _ := parseTaskMode(tc.Mode) // 空值→ModeA（与 modeLabel 缺省一致）
	newID, err := a.manager.Submit(
		protocol.LinkResult{FID: tc.FID, FileName: tc.FileName, Size: tc.Size, URL: url},
		tc.RelDir, cookieStr, mode,
		engine.SubmitOpt{Threads: max(a.settingsSnapshot().Threads, 1), Refresher: refresher})
	if err != nil {
		switch {
		case errors.Is(err, engine.ErrClosed):
			writeError(c, http.StatusServiceUnavailable, codeManagerClosed, "下载管理器已关闭: "+err.Error())
		case errors.Is(err, engine.ErrDestBusy):
			writeError(c, http.StatusConflict, codeDestBusy, errDestBusyMark)
		case errors.Is(err, engine.ErrInvalidThreads): // Task 12 错误码冻结
			writeError(c, http.StatusBadRequest, codeInvalidThreads, err.Error())
		default:
			writeError(c, http.StatusBadGateway, codeSubmitFailed, "复活任务失败: "+err.Error())
		}
		return
	}

	// TaskCtx.EngineID 换新（持锁读-改-写：重读最新存量，防并发创建
	// 期间的丢更新——与创建端点同一临界区）。
	tasksFileMu.Lock()
	cur := store.LoadTasks(a.paths)
	for i := range cur {
		if cur[i].EngineID == id {
			cur[i].EngineID = newID
			break
		}
	}
	err = store.SaveTasks(a.paths, cur)
	tasksFileMu.Unlock()
	if err != nil {
		writeError(c, http.StatusInternalServerError, codeSaveTasksFailed,
			fmt.Sprintf("任务已复活（%s）但持久化任务上下文失败: %v", newID, err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"task_id": newID})
}

// ctxTaskExists tasks.json 里是否有 EngineID==id 的条目（控制端点的
// needs_reparse 判据）。Exists 条目 EngineID 空，天然不匹配。
func ctxTaskExists(paths store.Paths, id string) bool {
	for _, tc := range store.LoadTasks(paths) {
		if tc.EngineID == id {
			return true
		}
	}
	return false
}

// etaMS 由进度快照估算剩余毫秒：速度未知/已完成/总量为 0 → 0（前端
// 按 0 不渲染倒计时）。
func etaMS(p engine.Progress) int64 {
	if p.SpeedBPS <= 0 || p.Total <= 0 || p.Done >= p.Total {
		return 0
	}
	return int64(float64(p.Total-p.Done) / p.SpeedBPS * 1000)
}

// modeLabel TaskCtx.Mode 的 DTO 输出：非 "b" 一律 "a"（空值=旧数据，
// 按缺省语义；与 parseTaskMode("")→ModeA 对齐）。
func modeLabel(m string) string {
	if m == "b" {
		return "b"
	}
	return "a"
}

// modeString engine.Mode → TaskCtx.Mode 存储形（"a"/"b"，与 API 契约
// 同形）。
func modeString(m engine.Mode) string {
	if m == engine.ModeB {
		return "b"
	}
	return "a"
}
