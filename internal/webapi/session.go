// session.go 实现会话生命周期接线（设计 D9 前半）：启动恢复（D9 的
// restoreSession 实现体——骨架与调用链在 Task 6 冻结于 server.go Start）。
package webapi

import "context"

// restoreSession 启动会话恢复（D9）：LoadCookie（§6.1 无值/坏值一律
// 空串 = 未登录）→ jar 装载 → LoginState 校验（__puus 唯一判据，与
// 导入链/扫码链跨链统一）→ 通过则启动保活心跳（onExpire 经 hub 广播
// auth 登录态事件，前端提示重新登录）。
//
// 任一环节不满足均静默返回——启动时无有效凭据是常态（首次使用/
// 登出后重启），不是错误；鉴权自愈接线随 resetSession 在 New 时完成，
// 本方法不重复挂。ctx 目前不消费（keepalive 用 App.Close 管理的独立
// 取消链），签名保留以维持 Start 调用链稳定。
func (a *App) restoreSession(context.Context) error {
	cookie := a.secrets.LoadCookie()
	if cookie == "" {
		return nil // 无凭据/解密失败（§6.1）：按未登录静默
	}

	a.authMu.Lock()
	defer a.authMu.Unlock()

	a.jar.LoadFromString(cookie)
	if !a.auth.LoginState() {
		// 存下的凭据缺 __puus（如旧版本半串 Cookie）：不给登录态，
		// 回滚脏 jar 不留半登录态（与导入链回滚同型）。
		a.resetSession()
		return nil
	}
	a.startKeepaliveLocked()
	// 恢复成功也广播 login（Task 12）：语义一致性优先——启动早期多半
	// 无订阅者，帧发了无人收，无害；前端若恰好已连 SSE 则即时获知。
	a.broadcastAuthEvent("login", true)
	return nil
}
