// 恢复对账·模式 A/B（规格 §3.6「恢复对账算法」逐字）：
//
//	若 plan 缺失/total 不符/分片划分不符 → 丢弃，全新开始
//	模式 A（直写）:
//	 ① 若磁盘文件长度 > total → 视为残留，从 W 记账出发（截断到 total）
//	 ② ⚠️ 绝不能把"目标文件长度"当进度——模式 A 会预分配满尺寸，
//	     长度==total 但可能一个字节都没下。进度只认 W/done。
//	模式 B（独立分片+合并）:
//	 ① 由目标文件当前长度 have 反推合并进度（合法：合并线程只追加，
//	     目标文件长度 = 已合并连续前缀）
//	 ② 尾部对账：尾片分片文件长度 < mergedInHead → 目标混入来路不明
//	     字节 → 截断到前一该片边界（内容正确优先于省流量）
//	 ③ 分片对账：磁盘长度 > 片大小截断；W[i]=磁盘长度一律不信记账；
//	     == 片大小 → 置完成标记（防 worker/合并线程互相干等死锁）
//	 ④ 已合并片 W=片大小、残留分片删除
package engine

import "os"

// qmetaMatchesPlan §3.6 前置判据（A/B 共用）：plan 缺失 / total 不符 /
// 分片划分不符 → false（丢弃，全新开始）。划分只由 (total, n) 决定，
// layout 可唯一重推——S/E 不持久化在 .qmeta 里也能校验。
func qmetaMatchesPlan(m *QMeta, segs []Segment) bool {
	if m == nil || len(segs) == 0 { // plan 缺失（含当前 plan 本身无片）
		return false
	}
	total := segs[len(segs)-1].End + 1 // 闭区间尾片 → 文件总长
	if m.Total != total {              // total 不符
		return false
	}
	if len(m.W) != len(segs) { // 片数不符
		return false
	}
	rederived := layout(m.Total, len(segs))
	for i, s := range segs {
		if s != rederived[i] { // 分片划分不符
			return false
		}
	}
	return true
}

// ReconcileA 对账直写模式的目标文件：dest 为目标文件路径，m 为 .qmeta
// 读回的元数据（nil = plan 缺失），segs 为本次 Plan 的分片。
//
// 返回对账后的每片已写字节 w 与 ok。ok=false 表示元数据与当前 plan 不可
// 匹配（或磁盘操作失败），调用方丢弃一切全新开始；此时 w 为 nil 且磁盘
// 不被改动（清理/重建是 runA 的职责）。ok=true 时状态恢复后的进度起点
// 为 done = Σ w[i]（§3.6）。
func ReconcileA(dest string, m *QMeta, segs []Segment) (w []int64, ok bool) {
	// 三条丢弃判据（§3.6 逐字），全过才开始动磁盘。
	if !qmetaMatchesPlan(m, segs) {
		return nil, false
	}
	total := segs[len(segs)-1].End + 1

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

// ReconcileB 对账模式 B 的目标文件与分片目录（§3.6 逐字四步）。dest 为
// 目标文件、partsDir 为 <dest>.qparts，m 为 .qmeta 读回的元数据（nil =
// plan 缺失），segs 为本次 Plan 的分片。
//
// 返回对账后的每片已写字节 w、合并进度起点 mergedCount（第一个未合并
// 片）/mergedInHead（该片已合并字节）与 ok。ok=false 表示元数据与当前
// plan 不可匹配或磁盘操作失败：w 为 nil、磁盘不被改动，调用方丢弃一切
// 全新开始（分片目录残留由新一轮下载按文件长度覆盖/合并线程删除）。
// ok=true 时调用方装配 runB：W=w、done=Σw、mergeNext=mergedCount、
// mergeInPart=mergedInHead（§3.6「状态恢复后的进度起点」——已合并片
// 在 ④ 被置 W=片大小，故 Σw 已含已合并字节）。
func ReconcileB(dest, partsDir string, m *QMeta, segs []Segment) (w []int64, mergedCount int, mergedInHead int64, ok bool) {
	if !qmetaMatchesPlan(m, segs) {
		return nil, 0, 0, false
	}
	total := segs[len(segs)-1].End + 1
	segSize := func(i int) int64 { return segs[i].End - segs[i].Start + 1 }

	// ① 由目标文件当前长度 have 反推合并进度：依次累加分片大小，得到
	// mergedCount（已完整合并的片数）与 mergedInHead（尾片已合并字节）。
	// 合法因为合并线程只追加，目标文件长度 = 已合并连续前缀。
	var have int64
	fi, err := os.Stat(dest)
	if err != nil && !os.IsNotExist(err) {
		return nil, 0, 0, false
	}
	if err == nil {
		have = fi.Size()
	}
	if have > total { // 病态超长残留（如手工/外因篡改）：钳回 total，视作全部已合并
		if err := os.Truncate(dest, total); err != nil {
			return nil, 0, 0, false
		}
		have = total
	}
	var acc int64
	for mergedCount < len(segs) && have >= acc+segSize(mergedCount) {
		acc += segSize(mergedCount)
		mergedCount++
	}
	mergedInHead = have - acc

	// ② 尾部对账：第 mergedCount 片的分片文件长度 < mergedInHead →
	// 目标文件里混入来路不明字节（分片文件被截/损坏后目标却更长）→
	// 截断目标文件到前一该片边界，mergedInHead=0（内容正确优先于省流量）。
	if mergedCount < len(segs) && mergedInHead > 0 {
		tail, err := partOnDisk(partsDir, mergedCount)
		if err != nil {
			return nil, 0, 0, false
		}
		if tail < mergedInHead {
			if err := os.Truncate(dest, have-mergedInHead); err != nil {
				return nil, 0, 0, false
			}
			mergedInHead = 0
		}
	}

	// ③ 分片对账：对每个未合并的片，一律以磁盘为准（⚠️ 不信 W 记账——
	// 缓冲未落盘被杀 → 记账>盘上；删除失败 → 反之）。
	w = make([]int64, len(segs))
	for i := mergedCount; i < len(segs); i++ {
		sz := segSize(i)
		disk, err := partOnDisk(partsDir, i)
		if err != nil {
			return nil, 0, 0, false
		}
		if disk > sz { // 磁盘长度 > 片大小 → 截断到片大小
			if err := os.Truncate(partPath(partsDir, i), sz); err != nil {
				return nil, 0, 0, false
			}
			disk = sz
		}
		w[i] = disk
		if disk == sz {
			// 磁盘长度 == 片大小 → 置完成标记：否则 worker 看 W 满直接
			// 跳过、合并线程等标记，互相干等死锁（§3.5 5 实测事故）。
			if err := markPartDone(partsDir, i); err != nil {
				return nil, 0, 0, false
			}
		} else if partDoneMarked(partsDir, i) {
			// 假标记：盘上不完整 ≠ 完成——留着会让合并线程提前删片。
			_ = os.Remove(partDonePath(partsDir, i))
		}
	}

	// ④ 已合并的片：W=片大小（Σw 含已合并字节的进度起点），残留分片
	// 文件删除（正常流程合并线程已删；异常残留在此清扫，连同标记）。
	for i := 0; i < mergedCount; i++ {
		w[i] = segSize(i)
		_ = os.Remove(partPath(partsDir, i))
		_ = os.Remove(partDonePath(partsDir, i))
	}
	return w, mergedCount, mergedInHead, true
}
