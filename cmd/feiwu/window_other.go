//go:build !darwin && !windows

// 非 darwin/windows 平台的窗口运行器空位（Task 2）：Wails linux 后端
// 依赖 webkit2gtk（非纯 Go 交叉），不进桌面构建面——这里返回 nil，
// run 的 windowRunner == nil 分支落回无窗口模式的 openBrowser 回退，
// 行为与 Task 1 之前的现状完全一致。CI go-test（ubuntu）借此只验证
// 装配不编译 wails。Task 7 起签名与桌面侧对称（ctx/hasActiveTasks 在
// 此无消费者），run 装配处无需按平台分支。
package main

import "context"

// newWailsRunner 非 darwin/windows 无窗口运行器（nil → run 走 openBrowser 回退）。
func newWailsRunner(ctx context.Context, hasActiveTasks func() bool) func(string) error {
	return nil
}
