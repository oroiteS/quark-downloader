// 慢连接抢占（§3.7「收尾能跑完的关键机制」）：监视线程两遍扫描。
//
// 要治的病：CDN 按连接限速，个别连接落在慢节点（实测 64 路中多数
// 40–80KB/s、慢的 3–7KB/s），任务收尾时被一两条慢连接拖死。与「片数≫
// 并发」互补：后者让快连接有活干，前者让慢连接别拖后腿。
//
// 结构：采样状态按 worker 编号（一条连接同一时刻只处理一片）：
//
//	每 worker: curPart(当前片, -1=空闲), startMs(领片时刻), lastTickMs, lastW, lastIdx
//	每片:      preemptCount(被抢次数), lastPreemptMs
//
// 第一遍采样：换片重置基线本轮无样本；instBps=(W−lastW)/Δt；年龄 < 门槛
// （常规 15s/收尾 3s）无样本；threshold = max(12KB/s, 本轮实测样本均值/2)。
// 整体限速判定（防越抢越慢）：非收尾 sampled<4 不动作；slowCount×2 ≥
// sampled 本轮不抢；收尾（在飞 ≤3 路）跳过两条判定。第二遍执行：每轮
// ≤2 路，命中即 cancel 片级令牌——已写字节保留在 W、片重新入队、不计
// 失败不退避（Task 8 的 errPreempted 路径消化）。
package engine

import (
	"sync"
	"time"
)

// §3.7 常数（速率/计数类恒定；时间类在 Preemptor 字段上，测试压缩注入）。
const (
	// preemptScanEvery 抢占扫描周期（规格逐字「每 5s 执行一轮」）：监视线程
	// 200ms 节拍内的分频（Task.preemptEvery 的生产默认）。
	preemptScanEvery = 5 * time.Second
	// preemptFloorBPS threshold 地板 12KB/s：均值/2 低于它时以它为准——
	// 全体都慢（服务端配额低）的场景由整体限速判定兜住，不靠抬地板。
	preemptFloorBPS = 12.0 * 1024
	// preemptMinSampled 非收尾最少样本数（规格逐字 sampled < 4 → 不动作）：
	// 样本太少结论不可靠。
	preemptMinSampled = 4
	// preemptPerRound 每轮最多抢 2 路（规格逐字）：一轮掐太多会同时制造
	// 一批重连+重爬坡，瞬间吞吐塌方。
	preemptPerRound = 2
	// preemptMaxCount 常规片被抢上限 3 次 / preemptTailCount 收尾 30 次
	// （§3.7 收尾放宽依据：常规 3 次在收尾 30s 内烧完，剩下的片只能干等
	// 停滞看门狗中止——用户实测卡在 99%）。
	preemptMaxCount  = 3
	preemptTailCount = 30
	// preemptTailInflight 收尾判定：在飞 ≤3 路。只剩几路时，那几路的速度
	// 就是用户看到的总速度，正是最需要抢占的时候。
	preemptTailInflight = 3
	// preemptMinRemain 非收尾执行条件：该片剩余 ≥128KB——快下完的片
	// 重连不值（重连+爬坡的开销比剩下的活还大）；收尾跳过本判定。
	preemptMinRemain = int64(128 * 1024)
)

// sampleState 单个 worker 的采样状态（§3.7 结构逐字）。curPart/startMs 由
// worker 生命周期钩子（begin/end）维护；lastTickMs/lastW/lastIdx 由 Scan
// 推进——lastIdx 存上轮采样时的片号，curPart != lastIdx 即「换了片」。
type sampleState struct {
	curPart    int   // 当前片；-1=空闲（不计在飞、不采样）
	startMs    int64 // 领片时刻（年龄锚点：now-startMs）
	lastTickMs int64 // 上轮采样时刻
	lastW      int64 // 上轮采样时该片已写字节
	lastIdx    int   // 上轮采样时的片号；-1=新领的片（下轮必重置基线）
}

// Preemptor 是慢连接抢占看门狗（§3.7 两遍扫描的执行体）：监视线程按
// preemptEvery 周期调 Scan(now)（now 注入，纯逻辑测试用假时钟）。
// 时间类常数（年龄门槛/冷却）做成字段：生产默认在 newPreemptor，集成
// 测试压成毫秒级。
type Preemptor struct {
	t  *Task
	mu sync.Mutex
	// states 按 worker 编号的采样表；preemptCount/lastPreemptMs 按片。
	states        []sampleState
	preemptCount  []int
	lastPreemptMs []int64
	ageGate       time.Duration // 年龄门槛：常规 15s / 收尾 3s
	ageGateTail   time.Duration
	cooldown      time.Duration // 同片两次抢占的冷却：常规 10s / 收尾 3s
	cooldownTail  time.Duration
}

// newPreemptor 构造 workers 路的抢占看门狗（§3.7 生产默认：15s/3s 门槛、
// 10s/3s 冷却）。
func newPreemptor(t *Task, workers, segs int) *Preemptor {
	p := &Preemptor{
		t:             t,
		states:        make([]sampleState, workers),
		preemptCount:  make([]int, segs),
		lastPreemptMs: make([]int64, segs),
		ageGate:       15 * time.Second,
		ageGateTail:   3 * time.Second,
		cooldown:      10 * time.Second,
		cooldownTail:  3 * time.Second,
	}
	for i := range p.states {
		p.states[i].curPart = -1
		p.states[i].lastIdx = -1
	}
	return p
}

// begin 登记 worker w 领片 idx（workerA 出队后调用）：记录领片时刻（年龄
// 锚点）、标记新片（lastIdx=-1 → 下轮 Scan 重置基线，本轮无样本）。
func (p *Preemptor) begin(w, idx int, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[w] = sampleState{curPart: idx, startMs: now.UnixMilli(), lastIdx: -1}
}

// end 登记 worker w 释放片（fetchSeg 返回后调用，成功/失败/被抢皆然）：
// 该 worker 转空闲，不计在飞、不采样。
func (p *Preemptor) end(w int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[w].curPart = -1
}

// Scan 执行一轮两遍扫描（§3.7 逐字；now 注入）。cancel 在锁外批量执行
// （cancelSeg 拿 Task.segMu，避免与 worker 侧注册形成锁序嵌套）。
func (p *Preemptor) Scan(now time.Time) {
	ms := now.UnixMilli()

	p.mu.Lock()
	// 收尾判定（在飞 ≤3 路）：决定门槛/上限/冷却取常规组还是收尾放宽组。
	inflight := 0
	for i := range p.states {
		if p.states[i].curPart >= 0 {
			inflight++
		}
	}
	tail := inflight <= preemptTailInflight
	ageGate, maxCount, cooldown := p.ageGate, preemptMaxCount, p.cooldown
	if tail {
		ageGate, maxCount, cooldown = p.ageGateTail, preemptTailCount, p.cooldownTail
	}

	// ---- 第一遍：采样 ----
	// cand 是有有效样本的 worker（执行遍的候选）。
	type cand struct {
		w, idx  int
		instBps float64
	}
	var cands []cand
	sum, sampled := 0.0, 0
	for w := range p.states {
		st := &p.states[w]
		if st.curPart < 0 {
			continue // 空闲 worker：不在飞、不采样
		}
		idx := st.curPart
		curW := p.t.wAt(idx) // W 快照（worker 在 wmu 内写）
		if idx != st.lastIdx {
			// 换了片 → 重置基线，本轮无样本：新基线从当前 W 起算，
			// 上一片的增量绝不跨界（错位会算出荒谬瞬时速度）。
			st.lastIdx = idx
			st.lastTickMs = ms
			st.lastW = curW
			continue
		}
		dt := ms - st.lastTickMs
		inst := -1.0
		if dt > 0 {
			inst = float64(curW-st.lastW) * 1000 / float64(dt)
		}
		// lastTick/lastW 推进（无样本的轮同样推进——年龄判定的语义是
		// 「这一轮窗口」的速度，不是累计平均）。
		st.lastTickMs = ms
		st.lastW = curW
		// 年龄 < 门槛 → 无样本：新连接还在 TCP 慢启动，速度天然≈0，
		// 不排除会把启动爬坡期全体误判成「整体限速」。
		if inst < 0 || ms-st.startMs < ageGate.Milliseconds() {
			continue
		}
		sum += inst
		sampled++
		cands = append(cands, cand{w: w, idx: idx, instBps: inst})
	}
	if sampled == 0 {
		p.mu.Unlock()
		return
	}
	// ⚠️ 平均值必须用「本轮实测样本」算，不能用 全局EWMA速度/在飞数——
	// EWMA 带滞后、在飞数是瞬时值，两者错位会算出荒谬阈值（实测踩坑：
	// 把 6 路全部误判慢、抢占彻底失效）。
	threshold := max(preemptFloorBPS, sum/float64(sampled)/2)

	// ---- 整体限速判定（防「越抢越慢」）----
	slow := make([]cand, 0, len(cands))
	for _, c := range cands {
		if c.instBps < threshold {
			slow = append(slow, c)
		}
	}
	if !tail {
		if sampled < preemptMinSampled {
			p.mu.Unlock()
			return // 样本太少，结论不可靠
		}
		if len(slow)*2 >= sampled {
			p.mu.Unlock()
			return // 全体都慢 = 服务端配额低，不是个别坏节点；抢占只会造成重连风暴
		}
	} // 收尾阶段跳过以上两条判定

	// ---- 第二遍：执行抢占（每轮最多 2 路）----
	var hits []int
	for _, c := range slow {
		if len(hits) >= preemptPerRound {
			break
		}
		// 年龄 ≥ 门槛：候选有样本即已保证（采样遍同一门槛过滤），按规格
		// 列出以示完整。
		if ms-p.states[c.w].startMs < ageGate.Milliseconds() {
			continue
		}
		if !tail && p.t.segRemain(c.idx) < preemptMinRemain {
			continue // 非收尾：该片剩余 ≥128KB 才值得重连
		}
		if p.preemptCount[c.idx] >= maxCount {
			continue // 片被抢次数 < 上限（常规 3 / 收尾 30）
		}
		if p.lastPreemptMs[c.idx] != 0 && ms-p.lastPreemptMs[c.idx] < cooldown.Milliseconds() {
			continue // 距该片上次被抢 ≥ 冷却（常规 10s / 收尾 3s）
		}
		p.preemptCount[c.idx]++
		p.lastPreemptMs[c.idx] = ms
		hits = append(hits, c.idx)
	}
	p.mu.Unlock()

	for _, idx := range hits {
		p.t.cancelSeg(idx) // 只掐该片在飞令牌：全局取消语义不动（§3.7 核心语义）
	}
}

// wAt 返回 W[idx] 快照（wmu 内读：worker 逐片独占写、监视全量读）。
func (t *Task) wAt(idx int) int64 {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	return t.W[idx]
}

// segRemain 返回片 idx 剩余字节（片大小 − W[idx]）。
func (t *Task) segRemain(idx int) int64 {
	s := t.Segs[idx]
	t.wmu.Lock()
	defer t.wmu.Unlock()
	return s.End - s.Start + 1 - t.W[idx]
}
