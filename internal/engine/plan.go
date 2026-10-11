// Package engine 实现飞舞（feiwu）下载引擎，规格见
// docs/核心算法原理.md §3「下载引擎完整规格」。
package engine

import "time"

// 分片规划常数，逐字出自规格 §3.3；「参数依据」注释摘自规格原文，改动前先读。
const (
	segCountMax = 16384     // MAX_SEG：片数硬上限
	segBytesMin = 512 << 10 // MIN_SEG：单片最小字节数。片太小则连接频繁换片，每次换片重新 TCP+TLS 建连（期间零吞吐），速度锯齿
	effMax      = 512       // 有效并发上限。实测 1024 路只比 512 快约 5%，但单连接速度减半、异常率升高；再大会打爆家用路由器 NAT 会话表

	// want 倍率：尾部掉速窗口 =「剩余片数 < 并发数」的区间。×8 时 9.5GB 文件的窗口是
	// 最后 1.2GB（12%），用户实测越下越慢到 0；×32 缩到约 300MB（3%）。片数必须远大于
	// 并发数，让快连接干完立刻抢下一片（工作窃取），慢连接只拖自己那一小片。
	wantPerEff = 32

	bufMin = 64 << 10  // BufSize 下限
	bufMax = 256 << 10 // BufSize 上限
	bufPer = 64 << 20  // BufSize 分母基准：min/max 以 64MB/n 内插

	rampFast = 3 * time.Second // n≤256 的启动爬坡
	rampSlow = 6 * time.Second // n>256 的启动爬坡
)

// Segment 是一个下载分片，[Start, End] 为闭区间。
type Segment struct {
	Index int
	Start int64
	End   int64
}

// Plan 按规格 §3.3 把 total 字节切成分片：
//
//	want  = clamp(max(bySize, eff×32), 1, MAX_SEG)
//	seg   = clamp(⌈total/want⌉, MIN_SEG, maxSeg)
//	n     = clamp(⌈total/seg⌉, 1, MAX_SEG)
//	finalSeg = ⌈total/n⌉            // 重新对齐，避免最后一片畸形
//	第 i 片 = [i×finalSeg, min((i+1)×finalSeg, total)−1]
//
// total ≤ 0 返回 nil。纯函数，无副作用。
func Plan(total int64, threads int) []Segment {
	if total <= 0 {
		return nil
	}
	eff := min(int64(threads), int64(effMax))
	want := clamp(max(baseCount(total), eff*wantPerEff), 1, segCountMax)
	seg := clamp(ceilDiv(total, want), segBytesMin, segCap(total))
	n := clamp(ceilDiv(total, seg), 1, segCountMax)
	finalSeg := ceilDiv(total, n)

	segs := make([]Segment, n)
	for i := range segs {
		start := int64(i) * finalSeg
		end := start + finalSeg - 1
		if total-start <= finalSeg { // 等价 min((i+1)×finalSeg, total)−1，减法形式防溢出
			end = total - 1
		}
		segs[i] = Segment{Index: i, Start: start, End: end}
	}
	return segs
}

// BufSize 返回读缓冲大小：max(64KB, min(256KB, 64MB/n))（§3.3）。
// n<1 钳到 1，仅防除零（规格未定义该输入）。
func BufSize(n int) int {
	if n < 1 {
		n = 1
	}
	return int(clamp((bufPer)/int64(n), bufMin, bufMax))
}

// Ramp 返回并发爬坡时长：n>256 ? 6s : 3s（§3.3）。
func Ramp(n int) time.Duration {
	if n > 256 {
		return rampSlow
	}
	return rampFast
}

// baseCount 按文件大小选基础片数 bySize（§3.3 表）。
// 分档依据：小文件不该开一堆连接抢几片——每条连接跑不到一片，光握手就耗完了。
func baseCount(total int64) int64 {
	switch {
	case total < 5<<20:
		return 1
	case total < 32<<20:
		return 4
	case total < 256<<20:
		return 16
	case total < 2<<30:
		return 32
	default:
		return 64
	}
}

// segCap 按文件大小选单片上限 maxSeg（§3.3 表）。
func segCap(total int64) int64 {
	switch {
	case total < 32<<20:
		return 1 << 20
	case total < 256<<20:
		return 2 << 20
	case total < 2<<30:
		return 8 << 20
	default:
		return 16 << 20
	}
}

// clamp 把 v 夹进 [lo, hi]。
func clamp(v, lo, hi int64) int64 {
	return max(lo, min(hi, v))
}

// ceilDiv 计算 ⌈a/b⌉（a≥0, b≥1），用先除后加避免 a+b 溢出。
func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}
