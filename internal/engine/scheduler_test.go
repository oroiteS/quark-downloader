// 多任务调度（§3.13）测试：任务级并发信号量（排队等待而非跳过）、
// 同路径互斥 owned 语义（⚠️ 抢失败方误 Unlock 不得删别人的锁——规格
// 事故注）、连接预算分配表驱动（规格例/档位收敛/全零平分/超额回收）。
package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// ---- AllocateBudget（§3.13 预算公式表驱动） --------------------------------
//
// TestAllocateBudget 逐字核对 §3.13：单文件→cap；多文件 waves=⌈N/c⌉、
// effBudget=512×waves；Σsize=0 全部退化平分；clamp(round(effBudget×
// size[i]/Σsize), 4, cap)；超额回收从"大于下限的最大项"扣 1。
//
// ⚠️ 规格例笔误裁决（Ruling E3，沿 Ruling E2"公式权威"先例）：规格例
// 首项写 460，但逐字公式 round(512×900/999)=round(461.26)=461（90MB→
// round(46.13)=46、9MB→round(4.61)=5 均与 round 一致；460 仅在把 Σ 误
// 当 1000 且用 floor 时成立，与 9MB→5 不自洽——无一致舍入规则能同时
// 产出 [460,46,5]）。以公式为准断言 461。
func TestAllocateBudget(t *testing.T) {
	mb := int64(1024 * 1024)
	cases := []struct {
		name    string
		sizes   []int64
		cap     int
		conc    int
		want    []int
		explain string
	}{
		{
			name:  "规格例三文件按比例",
			sizes: []int64{900 * mb, 90 * mb, 9 * mb},
			cap:   512, conc: 3,
			want: []int{461, 46, 5},
			explain: "waves=1、effBudget=512、Σ=999MB：round(512×900/999)=461、" +
				"round(512×90/999)=46、round(512×9/999)=5，Σ=512 恰满不回收（规格例 460 系笔误，见测试头裁决）",
		},
		{
			name:  "cap32档收敛",
			sizes: []int64{900 * mb, 90 * mb, 9 * mb},
			cap:   32, conc: 3,
			want:    []int{32, 32, 5},
			explain: "同上比例，前两项 461/46 超档夹到 32，9MB→5 不动",
		},
		{
			name: "单文件直给cap", sizes: []int64{123456}, cap: 32, conc: 1,
			want: []int{32}, explain: "§3.13 逐字：单文件 → 直接给 cap",
		},
		{
			name: "全零平分整除", sizes: []int64{0, 0}, cap: 512, conc: 2,
			want:    []int{256, 256},
			explain: "Σsize=0 退化平分：round(512/2)=256（夹取不触）",
		},
		{
			name: "全零平分触发回收", sizes: []int64{0, 0, 0}, cap: 512, conc: 3,
			want:    []int{170, 171, 171},
			explain: "round(512/3)=171×3=513>512 → 从首个最大项扣 1（tie 扣索引最小者）",
		},
		{
			name: "超额回收下限优先", sizes: []int64{501, 499, 0}, cap: 512, conc: 3,
			want: []int{254, 254, 4},
			explain: "round 得 [257,255,0→夹4]=516>512，逐轮扣最大项：" +
				"256/255/255/254/254 → [254,254,4]；下限项 4 全程不扣（下限优先于预算）",
		},
		{
			name: "waves按批数放大", sizes: []int64{100, 100, 100, 100}, cap: 512, conc: 2,
			want:    []int{256, 256, 256, 256},
			explain: "waves=⌈4/2⌉=2、effBudget=1024：round(1024×100/400)=256×4=1024 恰满",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AllocateBudget(tc.sizes, tc.cap, tc.conc)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("AllocateBudget(%v, cap=%d, conc=%d) = %v, want %v\n  推导: %s",
					tc.sizes, tc.cap, tc.conc, got, tc.want, tc.explain)
			}
			if len(got) != len(tc.sizes) {
				t.Fatalf("结果长度 %d 与输入 %d 不符", len(got), len(tc.sizes))
			}
		})
	}
}

// ---- 同路径互斥 owned 语义 ---------------------------------------------------

// TestTryLockDestOwned §3.13 事故注逐字场景：A 持有 → B TryLock 失败 →
// B 若按"结束就 remove"误调 UnlockDest，不得把 A 的锁删掉（否则互斥
// 形同虚设，给出"已保护"的假象）。
func TestTryLockDestOwned(t *testing.T) {
	s := NewScheduler(3)
	const p = "/data/movie.iso"

	if !s.TryLockDest(p) {
		t.Fatal("空表首次 TryLockDest 应成功（A 登记）")
	}
	if s.TryLockDest(p) {
		t.Fatal("A 持有中 B TryLockDest 应失败（tryAdd 语义）")
	}
	// B 抢失败仍误调 UnlockDest（owned=false）：必须是无操作。
	s.UnlockDest(p, false)
	// C 再试：若 B 的误 Unlock 删了 A 的锁，这里会成功 → 互斥失效。
	if s.TryLockDest(p) {
		t.Fatal("B 误 Unlock 不应影响 A 的登记（owned 标记缺失，互斥形同虚设）")
	}
	// 登记成功者（A，owned=true）才是唯一能释放的人；释放后可再锁。
	s.UnlockDest(p, true)
	if !s.TryLockDest(p) {
		t.Fatal("A 正常释放后应可重新登记")
	}
	s.UnlockDest(p, true)
	// owned=false 对无登记路径也无害（不 panic、不写表）。
	s.UnlockDest("/data/never.bin", false)
}

// TestTryLockDestIndependentPaths 不同路径互不干扰。
func TestTryLockDestIndependentPaths(t *testing.T) {
	s := NewScheduler(2)
	if !s.TryLockDest("/a.iso") || !s.TryLockDest("/b.iso") {
		t.Fatal("不同路径应可同时登记")
	}
	if s.TryLockDest("/a.iso") {
		t.Fatal("同路径第二个登记仍应失败")
	}
}

// ---- 任务级并发信号量 --------------------------------------------------------

// TestNewSchedulerDefaultConcurrency §3.13 逐字：默认 3（非法值回退 3）。
func TestNewSchedulerDefaultConcurrency(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := cap(NewScheduler(n).sem); got != 3 {
			t.Fatalf("NewScheduler(%d) 并发上限 = %d, want 3（默认）", n, got)
		}
	}
	if got := cap(NewScheduler(7).sem); got != 7 {
		t.Fatalf("NewScheduler(7) 并发上限 = %d, want 7", got)
	}
}

// TestSchedulerGoLimitsConcurrency 信号量限同时在跑任务数：4 任务、上限
// 2——任意时刻 running ≤2、后提交者在排队（pending，等待而非跳过），
// 放行后全部到达 done 终态（排队的也跑了）。服务器用 release 闸门撑住
// 第一波请求，让采样窗口由测试控制而非靠 sleep 赌时序。
func TestSchedulerGoLimitsConcurrency(t *testing.T) {
	data := []byte("feiwu-scheduler-semaphore-probe!!") // 32B：Threads=1 → 单片单请求
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		<-release // 第一波拿槽的任务卡在此处：并发窗口由测试持有
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[a : b+1])
	}))
	defer srv.Close()

	s := NewScheduler(2)
	dir := t.TempDir()
	tasks := make([]*Task, 4)
	for i := range tasks {
		dest := filepath.Join(dir, fmt.Sprintf("f%d.bin", i))
		tasks[i] = newFastTask(t, dest, srv.URL, 1, int64(len(data)))
		s.Go(tasks[i])
	}

	sawFull, sawQueued := false, false
	for range 30 { // 150ms 采样窗口：本地起任务远快于 5ms/次
		run, pend := 0, 0
		for _, tk := range tasks {
			switch taskState(tk.state.Load()) {
			case stateRunning:
				run++
			case statePending:
				pend++
			}
		}
		if run > 2 {
			t.Fatalf("同时在跑 %d 个任务, 超过信号量上限 2", run)
		}
		if run == 2 {
			sawFull = true
			if pend > 0 {
				sawQueued = true // 满槽 + 有人在排队：排队等待的现场证据
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawFull {
		t.Fatal("150ms 内未观察到 2 个任务并行（信号量未放行?）")
	}
	if !sawQueued {
		t.Fatal("满槽时应有任务在排队等待（非跳过）")
	}

	close(release) // 放行：排队的任务应依序开工并全部完成
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		done := true
		for _, tk := range tasks {
			if taskState(tk.state.Load()) != stateDone {
				done = false
				break
			}
		}
		if done {
			s.Wait()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i, tk := range tasks {
		t.Errorf("任务 %d 终态 = %s, want done", i, taskState(tk.state.Load()))
	}
	t.Fatal("5s 内任务未全部完成（排队任务未被调度?）")
}
