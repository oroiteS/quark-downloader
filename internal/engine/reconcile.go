// 恢复对账·模式 A（直写），规格 §3.6「恢复对账算法」逐字：
//
//	若 plan 缺失/total 不符/分片划分不符 → 丢弃，全新开始
//	模式 A（直写）:
//	 ① 若磁盘文件长度 > total → 视为残留，从 W 记账出发（截断到 total）
//	 ② ⚠️ 绝不能把"目标文件长度"当进度——模式 A 会预分配满尺寸，
//	     长度==total 但可能一个字节都没下。进度只认 W/done。
package engine

import "os"

// ReconcileA 对账直写模式的目标文件：dest 为目标文件路径，m 为 .qmeta
// 读回的元数据（nil = plan 缺失），segs 为本次 Plan 的分片。
//
// 返回对账后的每片已写字节 w 与 ok。ok=false 表示元数据与当前 plan 不可
// 匹配（或磁盘操作失败），调用方丢弃一切全新开始；此时 w 为 nil 且磁盘
// 不被改动（清理/重建是 runA 的职责）。ok=true 时状态恢复后的进度起点
// 为 done = Σ w[i]（§3.6）。
func ReconcileA(dest string, m *QMeta, segs []Segment) (w []int64, ok bool) {
	// 三条丢弃判据（§3.6 逐字），全过才开始动磁盘。
	if m == nil || len(segs) == 0 { // plan 缺失（含当前 plan 本身无片）
		return nil, false
	}
	total := segs[len(segs)-1].End + 1 // 闭区间尾片 → 文件总长
	if m.Total != total { // total 不符
		return nil, false
	}
	if len(m.W) != len(segs) { // 片数不符
		return nil, false
	}
	rederived := layout(m.Total, len(segs)) // 划分只由 (total,n) 决定，可唯一重推
	for i, s := range segs {
		if s != rederived[i] { // 分片划分不符
			return nil, false
		}
	}

	// 磁盘现状（模式 A ①）：只认「过长 = 残留」，绝不反向推断进度（②）。
	fi, err := os.Stat(dest)
	if err != nil && !os.IsNotExist(err) {
		return nil, false // stat 失败（权限等）：无从判断残留，保守丢弃
	}
	if err == nil && fi.Size() > total { // ① 视为残留 → 截断到 total，从 W 记账出发
		if err := os.Truncate(dest, total); err != nil {
			return nil, false // 带着超长残留续传会错位，截不动就丢弃
		}
	}
	// ② dest 缺失或长度 ≤ total：长度一律不构成进度信息（预分配陷阱——
	//    长度==total 可能一个字节都没下）；也不在此扩容或创建文件——
	//    预分配是 runA（Task 7）启动时的职责。

	// 从 W 记账出发，逐片钳回 [0, 片大小]：QMeta 的 W 不设上界（SaveQMeta
	// 只拦负值），记账超片大小（缓冲未落盘被杀 → 记账>盘上）若不钳，
	// worker 将从片尾之外 pwrite、越界写坏相邻片。这不是「拿文件长度当
	// 进度」，只是把记账夹回片的物理边界。
	w = make([]int64, len(segs))
	for i, s := range segs {
		w[i] = clamp(m.W[i], 0, s.End-s.Start+1)
	}
	return w, true
}
