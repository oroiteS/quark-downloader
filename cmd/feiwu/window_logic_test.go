// 关窗确认核心逻辑测试（评审第 3 项下沉 + 评审第 1 项平台映射）。
//
// 本文件刻意不带 build tag：ubuntu CI（go test ./...）同样编译执行，
// 把关窗确认的核心判定关进 CI 质量门——此前三分支断言只在
// window_test.go（//go:build darwin || windows）里，CI 上根本不编译。
// 被测对象全部是 window_logic.go 的无 wails 依赖纯函数，任何平台可编译。
package main

import "testing"

// TestDefaultCloseConfirmButton 关窗确认默认键（回车键）平台映射
// （评审第 1 项，保守策略：默认键永远不能等价于「退出」）。表驱动对
// 每个分支的期望值逐项断言——windows 期望值 "no" 在任意平台（含
// ubuntu CI）都被验证，不依赖在本机跑 windows：
//
//   - darwin：MessageDialog 返回被点按钮 label，默认键须为 "取消"
//     → 回车=取消=留在窗口；
//   - windows：QuestionDialog 固定 MB_YESNO 两键（返回 "Yes"/"No"），
//     wails 仅在 strings.ToLower(DefaultButton)=="no" 时设
//     MB_DEFBUTTON2——必须传小写 "no"，回车=No=不退出；
//   - 其余平台：窗口模式不编译，取 "取消" 保守兜底。
func TestDefaultCloseConfirmButton(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{"darwin", "取消"},
		{"windows", "no"},
		{"linux", "取消"},
	}
	for _, tc := range cases {
		if got := defaultCloseConfirmButton(tc.goos); got != tc.want {
			t.Errorf("defaultCloseConfirmButton(%q) = %q, 期望 %q", tc.goos, got, tc.want)
		}
	}
}

// TestShouldPreventClose OnBeforeClose 三分支判定纯函数逐分支核对
// （评审第 3 项：自 window_test.go 的 TestBuildAppOptionsOnBeforeClose
// 迁入，判定逻辑下沉平台无关；wails 语义 true=阻止关闭）：
// ①hasActiveTasks 为 nil → 放行（false）且确认函数未被调用（防御短路，
// nil 语义保持既有行为）；
// ②无活动任务 → 放行（false）且确认函数未被调用；
// ③有活动任务 + confirmClose false（用户取消）→ true 阻止（窗口保留）；
// ④有活动任务 + confirmClose true（用户确认退出）→ false 放行（进程退出）。
// buildAppOptions 对该函数的接线（含 ctx 透传）仍由平台 tag 内
// window_test.go 的 TestBuildAppOptionsOnBeforeCloseWiring 守住。
func TestShouldPreventClose(t *testing.T) {
	inactive := func() bool { return false }
	active := func() bool { return true }

	t.Run("hasActiveTasks 为 nil 防御放行", func(t *testing.T) {
		called := false
		confirm := func() bool { called = true; return true }
		if prevent := shouldPreventClose(nil, confirm); prevent {
			t.Fatal("hasActiveTasks == nil shouldPreventClose = true, 期望 false（防御短路放行关闭）")
		}
		if called {
			t.Fatal("hasActiveTasks == nil 仍调用了确认函数（防御短路缺失）")
		}
	})
	t.Run("无活动任务放行且不弹框", func(t *testing.T) {
		called := false
		confirm := func() bool { called = true; return true }
		if prevent := shouldPreventClose(inactive, confirm); prevent {
			t.Fatal("无活动任务 shouldPreventClose = true, 期望 false（直接放行关闭）")
		}
		if called {
			t.Fatal("无活动任务仍调用了确认函数（应短路放行）")
		}
	})
	t.Run("用户取消阻止关闭", func(t *testing.T) {
		if prevent := shouldPreventClose(active, func() bool { return false }); !prevent {
			t.Fatal("用户取消 shouldPreventClose = false, 期望 true（窗口保留）")
		}
	})
	t.Run("用户确认放行退出", func(t *testing.T) {
		if prevent := shouldPreventClose(active, func() bool { return true }); prevent {
			t.Fatal("用户确认 shouldPreventClose = true, 期望 false（进程退出）")
		}
	})
}
