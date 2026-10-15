// 关窗确认核心逻辑（评审采纳项：下沉为无 build tag 纯函数）。本文件
// 不 import wails、不含平台 tag——ubuntu CI（go test ./...）直接编译
// 执行 window_logic_test.go，把此前只活在 darwin||windows 平台 tag 里
// 的关窗确认判定关进 CI 质量门。行为与抽取前逐字节等价（既有测试全绿
// 为证），装配接线仍在 window.go。
package main

// defaultCloseConfirmButton 关窗确认弹框默认键（回车键）的平台映射
// （评审第 1 项）。保守策略：默认键永远不能等价于「退出」——用户狂按
// 回车必须停在窗口，绝不误杀下载任务。
//
//   - darwin：MessageDialog 返回被点按钮 label，DefaultButton 取 "取消"
//     → 回车=取消=留在窗口；
//   - windows：QuestionDialog 固定 MB_YESNO 两键（返回 "Yes"/"No"），
//     Buttons/CancelButton 不参与渲染；wails v2.16.0 windows dialog.go
//     的 calculateMessageDialogFlags 仅在
//     strings.ToLower(DefaultButton)=="no" 时设 MB_DEFBUTTON2——必须传
//     小写 "no" → 回车=No=不退出。修复点：此前固定 "取消" 在 Windows
//     静默不生效（ToLower("取消")≠"no"），回车=Yes=确认退出，违背保守
//     策略且与 macOS（回车=取消）不一致；
//   - 其余平台：窗口模式不编译（window_other.go 落无窗口模式），映射取
//     "取消" 保守兜底。
//
// 参数化 goos（而非直接读 runtime.GOOS）使其成为纯函数：表驱动测试在
// 任意平台断言全部分支（见 window_logic_test.go）。
func defaultCloseConfirmButton(goos string) string {
	if goos == "windows" {
		return "no"
	}
	return "取消"
}

// shouldPreventClose OnBeforeClose 三分支判定（评审第 3 项：自
// window.go 的 OnBeforeClose 闭包下沉，平台无关纯函数；wails 语义
// true=阻止关闭）。分支与抽取前逐字等价，行为零变化：
//
//  1. hasActiveTasks 为 nil（测试/装配空位防御）或返回 false（无活动
//     任务）→ false 放行关闭，confirmClose 不被调用（短路）；
//  2. 有活动任务 + confirmClose false（用户取消）→ true 阻止（窗口保留）；
//  3. 有活动任务 + confirmClose true（用户确认退出）→ false 放行（进程退出）。
//
// confirmClose 以无参形态注入——OnBeforeClose 的 ctx 由 window.go 侧
// 闭包包住后传入，本函数不依赖任何平台/wails 类型，headless 可测。
// nil 语义保持既有行为：hasActiveTasks == nil 视为放行。
func shouldPreventClose(hasActiveTasks func() bool, confirmClose func() bool) bool {
	if hasActiveTasks == nil || !hasActiveTasks() {
		return false // 无活动任务：直接放行关闭
	}
	return !confirmClose() // 确认退出 → false 放行；取消 → true 阻止
}
