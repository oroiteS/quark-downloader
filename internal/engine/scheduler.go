// 多任务调度（§3.13）：任务级并发信号量（默认 3）、同路径互斥登记表
// （owned 语义）、连接预算按文件大小比例分配。三件事彼此独立：Go 只
// 管「同时在跑几个」；TryLockDest/UnlockDest 只管「同一路径只有一个
// 写者」；AllocateBudget 是纯函数（Manager 装配 Threads 档位时调用）。
package engine

import (
	"context"
	"sync"
)

// §3.13 逐字常数：budget=512、minPerFile=4；任务级并发默认 3。
const (
	totalBudget          = 512
	minPerFile           = 4
	defaultMaxConcurrent = 3
)

// Scheduler 是多任务调度器：任务级信号量 + 同路径登记表。
// 信号量用带缓冲 channel 实现（零依赖计数方案，Ruling D 注记）：
// 缓冲 = 可同时在跑的任务数，满则发送阻塞 = 排队。
type Scheduler struct {
	sem         chan struct{}       // 任务级信号量：占坑 = 在跑
	mu          sync.Mutex          // activeDests 的锁
	activeDests map[string]struct{} // 绝对路径小写 → 登记中（§3.13 ConcurrentDictionary 等价物）
	wg          sync.WaitGroup      // 已提交任务的 goroutine 跟踪（Wait 收尾）
}

// NewScheduler 构造调度器。maxConcurrent < 1 时取 §3.13 默认 3
// （与 Task 14 Config「零值默认 3 任务」对齐——设计 G5）。
func NewScheduler(maxConcurrent int) *Scheduler {
	if maxConcurrent < 1 {
		maxConcurrent = defaultMaxConcurrent
	}
	return &Scheduler{
		sem:         make(chan struct{}, maxConcurrent),
		activeDests: make(map[string]struct{}),
	}
}

// Go 提交任务执行（§3.13 任务级并发）：信号量限同时在跑任务数——
// 排队等待，非跳过（槽满则阻塞在 sem 上，前序任务让出槽位后依序开工）。
// 占到槽位后按任务落盘模式分发 runA/runB；任务级 ctx 与错误事件上报
// 归 Task 14（Manager）装配，本方法只锚定调度本身。
func (s *Scheduler) Go(t *Task) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.sem <- struct{}{}        // 排队等待（非跳过）
		defer func() { <-s.sem }() // finally 归还槽位（先于 wg.Done）
		if t.modeB {
			_ = t.runB(context.Background())
			return
		}
		_ = t.runA(context.Background())
	}()
}

// Wait 阻塞到所有已提交任务跑完（优雅收尾/测试用）。
func (s *Scheduler) Wait() { s.wg.Wait() }

// TryLockDest 尝试登记目标路径（§3.13 同路径互斥，数据安全非体验）。
// absLower 须为绝对路径小写（调用方归一）。false = 已有任务在写同一
// 路径，调用方应跳过该任务（报「同一文件正在下载中」），不排队——
// 场景：删完重下时旧任务尚未真正退出，两个下载器交叉写同一文件 →
// 文件损坏。
func (s *Scheduler) TryLockDest(absLower string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.activeDests[absLower]; dup {
		return false
	}
	s.activeDests[absLower] = struct{}{}
	return true
}

// UnlockDest 任务结束（finally）时释放登记。locked 必须传 TryLockDest
// 的返回值（owned 标记）：
//
//	⚠️ 必须用 owned 标记：抢失败的一方若也去 remove，会把别人的锁
//	   删掉，互斥形同虚设（比不做更危险——给出「已保护」的假象）。
//
// 因此仅登记成功者（locked=true）才 remove；locked=false 时无操作。
func (s *Scheduler) UnlockDest(absLower string, locked bool) {
	if !locked {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.activeDests, absLower)
}

// AllocateBudget 多任务连接预算分配（§3.13 逐字）：
//
//	输入: sizes[]（各文件字节）、cap（用户单文件并发档位）、
//	      concurrency（同时任务数）；budget=512、minPerFile=4。
//	单文件 → 直接给 cap
//	多文件:
//	  waves    = ⌈N / concurrency⌉   # 任务分批跑完的批数
//	  effBudget = budget × waves      # 预算约束"同时在跑"的量级 → 按批数放大，比例不变
//	  Σsize = 0 则全部退化平分（round(effBudget/N) 夹取）
//	  threads[i] = clamp( round(effBudget × size[i] / Σsize), minPerFile, cap )
//	  超额回收: while Σthreads > effBudget: 从"大于下限的最大项"扣 1
//	      # 下限优先于预算——宁可微超也不能让小文件下不动
//
// 回收的 tie-break（多项并列最大）扣索引最小者；全部顶到下限仍超预算
// 时止步微超（规格明示「宁可微超」）。cap 遮蔽内建函数名系简报签名
// 逐字要求；函数体内不用内建 cap。
func AllocateBudget(sizes []int64, cap, concurrency int) []int {
	n := len(sizes)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return []int{max(cap, 1)} // §3.13：单文件直接给 cap（非法档位防御为 1）
	}
	if concurrency < 1 {
		concurrency = 1
	}
	waves := (n + concurrency - 1) / concurrency // ⌈N/concurrency⌉
	effBudget := totalBudget * waves

	hi := max(cap, minPerFile) // cap < 下限属装配错误：夹取区间退化为单点，不 panic
	threads := make([]int, n)
	var sum int64
	for _, sz := range sizes {
		sum += sz
	}
	for i, sz := range sizes {
		v := roundDiv(int64(effBudget), int64(n)) // 全零退化平分
		if sum != 0 {
			v = roundDiv(int64(effBudget)*sz, sum)
		}
		threads[i] = int(clamp(int64(v), minPerFile, int64(hi)))
	}
	// 超额回收：从大于下限（4）的最大项扣 1；找不到可扣项（全在下限）
	// 则带着微超退出——下限优先于预算。
	for total := sumInts(threads); total > effBudget; total-- {
		best := -1
		for i, v := range threads {
			if v > minPerFile && (best < 0 || v > threads[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		threads[best]--
	}
	return threads
}

// roundDiv 整数四舍五入 a/b（a,b>0）：(a + b/2)/b。整数算术避开浮点
// 误差；量级安全：effBudget×size ≤ 512×N×size，任务数 N（~10⁴）×
// 文件大小（≤TB=2⁴⁰）≪ 2⁶³。
func roundDiv(a, b int64) int {
	return int((a + b/2) / b)
}

// sumInts 求和（回收循环的 Σthreads）。
func sumInts(xs []int) int {
	total := 0
	for _, v := range xs {
		total += v
	}
	return total
}
