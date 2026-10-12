// events.go SSE 事件帧与 hub：webapi 的事件总线。
//
// 语义（简报逐字）：Subscribe() (ch <-chan Event, cancel func()) +
// Broadcast(e Event)；慢消费者不阻塞广播（chan 满则丢弃该订阅者本帧）；
// cancel 幂等。
package webapi

import (
	"encoding/json"
	"sync"

	"quark-downloader/internal/engine"
)

// Event 是 SSE 事件帧的应用形态，JSON 序列化即 data: 行载荷。
//
// 字段设计留扩展位：Task 12 定帧格式时 type 收敛为
// "task_progress"|"task_final"|"auth" 枚举，任务进度/终态细节走 Payload
// （json.RawMessage——帧格式演进不动 wire 结构，也避免本包对 engine 进度
// 类型的反向耦合）。
type Event struct {
	Type    string          `json:"type"`              // 事件类型（Task 12 定枚举）
	TaskID  string          `json:"taskId,omitempty"`  // 任务类事件非空
	Payload json.RawMessage `json:"payload,omitempty"` // 结构化载荷（随 type 演进）
}

// subscriberBuffer 每订阅者的投递缓冲深度。
//
// 取值权衡：进度帧按任务节流 500ms（engine eventEveryDefault），3 个并发
// 任务 + auth/心跳类杂项事件下 16 帧约等于 2 秒以上的突发余量——正常前端
// 消费者永远打不满；真打满即消费者已死/挂起，丢帧优于反压（见 Broadcast）。
const subscriberBuffer = 16

// Hub 进度/终态事件的进程内广播总线：SSE 端点订阅，engine onEvent 广播
// （Task 9 接线）。零值不可用，须 NewHub。
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{} // 在册订阅者（channel 在册 ⇔ 未关闭）
}

// NewHub 构造空 hub。
func NewHub() *Hub {
	return &Hub{subs: make(map[chan Event]struct{})}
}

// Subscribe 注册一个订阅者，返回只读事件流与退订函数。
//
// cancel 幂等（重复调用、与 Close 竞争均安全）：锁内「仍在册才删除并
// 关闭」天然幂等——关闭的 channel 绝不会再被 Broadcast 选中（在册 ⇔
// 未关闭这一不变式全程由 h.mu 保护）。cancel 会关闭 ch：订阅端可用
// `_, ok := <-ch` 感知退出。
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
	return ch, cancel
}

// Broadcast 向所有在册订阅者广播一帧。
//
// 慢消费者取舍：投递是非阻塞 select——chan 满即丢弃该订阅者本帧，绝不
// 等待。理由：进度帧是幂等快照流（丢了中间帧，最新帧随后就到），终态帧
// 虽不可丢但消费端（SSE handler 16 帧缓冲）打满意味着连接已死，下一轮
// 心跳/断开检测就会清理——阻塞广播则会把一个卡死的浏览器变成全局停摆，
// 得不偿失。全程持锁发送保证「在册 ⇔ 未关闭」不变式（绝无向已关闭
// channel 发送的 panic 窗口）。
func (h *Hub) Broadcast(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // 慢消费者：丢本帧，保全局
		}
	}
}

// Close 关闭全部订阅者（服务停机链：App.Close → http.Server OnShutdown
// 触发，让 SSE 长连接立即退出、Shutdown 排空不被拖满预算）。幂等。
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

// subscriberCount 在册订阅者数（测试断言退订/泄漏用）。
func (h *Hub) subscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// ---- 引擎事件转发与登录态事件（Task 12，G3/G12）----

// progressPayload task_progress 帧载荷（字段与 GET /api/tasks 的 DTO
// 同形：done/speed_bps/state——前端一套渲染逻辑通用）。
type progressPayload struct {
	Done     int64   `json:"done"`
	Total    int64   `json:"total"`
	SpeedBPS float64 `json:"speed_bps"`
	EtaMS    int64   `json:"eta_ms"`
	State    string  `json:"state"`
	Merging  bool    `json:"merging"`
}

// finalPayload task_final 帧载荷：终态 + 错误文本（Err 非 nil 时透出，
// GET /api/tasks 的 error 字段无稳定来源——失败详情走此帧，Task 12）。
type finalPayload struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// authPayload auth 帧载荷：登录态三事件 login/logout/expired。
type authPayload struct {
	Event    string `json:"event"`
	LoggedIn bool   `json:"logged_in"`
}

// taskFinalStates 终态枚举（engine taskState 字符串面）：命中即 task_final。
var taskFinalStates = map[string]bool{"done": true, "error": true, "canceled": true}

// EngineEvent 引擎事件适配入口（装配：NewManager(cfg, app.EngineEvent)，
// P4 Ruling——引擎 500ms 节流已在引擎层，此处纯转发不再节流）。
// 进度/终态分流：终态三枚举（done/error/canceled）→ task_final（带 Err
// 文本），其余 → task_progress。
func (a *App) EngineEvent(ev engine.Event) {
	if taskFinalStates[ev.Progress.State] {
		payload, _ := json.Marshal(finalPayload{
			State: ev.Progress.State,
			Error: errMsg(ev.Err),
		})
		a.hub.Broadcast(Event{Type: "task_final", TaskID: ev.TaskID, Payload: payload})
		return
	}
	payload, _ := json.Marshal(progressPayload{
		Done: ev.Progress.Done, Total: ev.Progress.Total,
		SpeedBPS: ev.Progress.SpeedBPS, EtaMS: etaMS(ev.Progress),
		State: ev.Progress.State, Merging: ev.Progress.Merging,
	})
	a.hub.Broadcast(Event{Type: "task_progress", TaskID: ev.TaskID, Payload: payload})
}

// errMsg 终态错误文本（nil → 空串；用户取消为 ErrUserCancel 包装——
// 文本如实透出，前端按 state=canceled 区分展示）。
func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// broadcastAuthEvent 登录态事件帧（login/logout/expired）。onExpire 只
// 通知前端重新登录，会话清理留给用户操作（重新登录覆盖或 logout）。
func (a *App) broadcastAuthEvent(event string, loggedIn bool) {
	payload, _ := json.Marshal(authPayload{Event: event, LoggedIn: loggedIn})
	a.hub.Broadcast(Event{Type: "auth", Payload: payload})
}

// tasksSnapshotJSON 全量任务快照的 JSON 载荷（hello 帧用；与 GET
// /api/tasks 同源——tasks.go 的 tasksSnapshot 组装）。
func (a *App) tasksSnapshotJSON() json.RawMessage {
	b, err := json.Marshal(a.tasksSnapshot())
	if err != nil {
		return json.RawMessage("[]") // 不应发生：DTO 全可序列化
	}
	return json.RawMessage(b)
}
