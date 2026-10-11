package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// §3.6「恢复对账算法」·模式 A（直写）逐字：
//
//	若 plan 缺失/total 不符/分片划分不符 → 丢弃，全新开始
//	① 若磁盘文件长度 > total → 视为残留，从 W 记账出发（截断到 total）
//	② ⚠️ 绝不能把"目标文件长度"当进度——模式 A 会预分配满尺寸，
//	   长度==total 但可能一个字节都没下。进度只认 W/done。

// reconcileFixture 造一份自洽对账输入：total=100MB、threads=64（200 片），
// W 用确定性伪随机填 0..片大小。dest 由各用例自备。
func reconcileFixture() (total int64, segs []Segment, w []int64) {
	total = 100 * tMB
	segs = Plan(total, 64)
	w = make([]int64, len(segs))
	for i, s := range segs {
		size := s.End - s.Start + 1
		w[i] = int64(uint(i*7919+13) % uint(size+1))
	}
	return total, segs, w
}

// growFile 造一个 size 字节的文件（稀疏 Truncate——对账只关心长度语义）。
func growFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("Truncate(%d): %v", size, err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return fi.Size()
}

func destPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "file.bin")
}

// 三条丢弃判据（§3.6 逐字）：plan 缺失 / total 不符 / 分片划分不符 →
// ok=false、w=nil、磁盘一个字节都不动（丢弃 = 全新开始，清理是 runA 的事）。
func TestReconcileARejectsBadMeta(t *testing.T) {
	total, segs, w := reconcileFixture()
	wShort := w[:len(w)-1]
	wLong := append(slices.Clone(w), 0)
	segBad := slices.Clone(segs) // 尾片不动（total 仍符）、片数仍符，
	segBad[0].End = segBad[0].End + 1 // 但首片边界被改 → 与 layout 重推划分不符

	cases := []struct {
		name string
		m    *QMeta
		segs []Segment
	}{
		{"meta 为 nil（plan 缺失）", nil, segs},
		{"total 不符（偏小）", &QMeta{Total: total - 1, W: w}, segs},
		{"total 不符（偏大）", &QMeta{Total: total + 1, W: w}, segs},
		{"片数不符（少一片）", &QMeta{Total: total, W: wShort}, segs},
		{"片数不符（多一片）", &QMeta{Total: total, W: wLong}, segs},
		{"当前 plan 无分片", &QMeta{Total: total, W: w}, nil},
		{"segs 与 layout 重推划分不符", &QMeta{Total: total, W: w}, segBad},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := destPath(t)
			residue := total + 4096 // 故意放个超长残留：丢弃路径也不许碰它
			growFile(t, path, residue)
			got, ok := ReconcileA(path, tt.m, tt.segs)
			if ok {
				t.Fatalf("ok = true, want false")
			}
			if got != nil {
				t.Errorf("w = %v, want nil（丢弃 → 全新开始）", got)
			}
			if sz := fileSize(t, path); sz != residue {
				t.Errorf("丢弃路径不应动磁盘: 文件长度 %d, want %d", sz, residue)
			}
		})
	}
}

// 正常续传：满尺寸预分配文件 + 与 plan 匹配的 W → W 原样保留，文件不动。
func TestReconcileAKeepsW(t *testing.T) {
	total, segs, w := reconcileFixture()
	path := destPath(t)
	growFile(t, path, total)
	got, ok := ReconcileA(path, &QMeta{Total: total, W: w}, segs)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if !slices.Equal(got, w) {
		t.Errorf("W 记账被改动: got len=%d want len=%d", len(got), len(w))
	}
	if sz := fileSize(t, path); sz != total {
		t.Errorf("文件长度 %d, want %d（合法文件不该被动过）", sz, total)
	}
}

// ⚠️ 预分配陷阱（规格 §3.6 模式 A ②，Review Focus 第 1 条）：
// 模式 A 会预分配满尺寸——文件长度==total 但可能一个字节都没下。
// 进度只认 W/done：W 全零 → 进度必须为 0，绝不能拿文件长度充进度。
func TestReconcileAPreallocationTrap(t *testing.T) {
	total, segs, w := reconcileFixture()
	for i := range w {
		w[i] = 0 // 一个字节都没下，只有预分配的空壳文件
	}
	path := destPath(t)
	growFile(t, path, total)
	got, ok := ReconcileA(path, &QMeta{Total: total, W: w}, segs)
	if !ok {
		t.Fatalf("满尺寸空壳文件 + 零 W 应可续传（ok=true），got ok=false")
	}
	var done int64
	for _, x := range got {
		done += x
	}
	if done != 0 {
		t.Errorf("进度 = %d 字节, want 0——把目标文件长度当进度了（预分配陷阱）", done)
	}
	if !slices.Equal(got, w) {
		t.Errorf("W = %v, want 全零", got)
	}
}

// 模式 A ① 逐字：磁盘文件长度 > total → 视为残留，截断到 total，从 W 记账出发。
func TestReconcileATruncatesResidue(t *testing.T) {
	total, segs, w := reconcileFixture()
	path := destPath(t)
	growFile(t, path, total+8192)
	got, ok := ReconcileA(path, &QMeta{Total: total, W: w}, segs)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if sz := fileSize(t, path); sz != total {
		t.Errorf("残留未截断: 文件长度 %d, want %d", sz, total)
	}
	if !slices.Equal(got, w) {
		t.Errorf("截断残留时 W 记账被改动（应从原 W 出发）")
	}
}

// 文件比 total 短（预分配中断/半截文件）：长度同样不是进度——W 原样出发，
// 文件既不扩容也不创建（预分配是 runA 的职责，对账只判残留）。
func TestReconcileAShortFileIgnoresLength(t *testing.T) {
	total, segs, w := reconcileFixture()
	path := destPath(t)
	growFile(t, path, total/2)
	got, ok := ReconcileA(path, &QMeta{Total: total, W: w}, segs)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if !slices.Equal(got, w) {
		t.Errorf("半截文件长度渗进了 W——模式 A 进度只认 W 记账")
	}
	if sz := fileSize(t, path); sz != total/2 {
		t.Errorf("文件长度 %d, want %d（对账不该扩容/补齐）", sz, total/2)
	}
}

// 目标文件不存在（换目录/被清理）：无残留可对，W 记账照常出发；
// 对账本身不创建文件。
func TestReconcileAMissingDest(t *testing.T) {
	total, segs, w := reconcileFixture()
	path := destPath(t)
	got, ok := ReconcileA(path, &QMeta{Total: total, W: w}, segs)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if !slices.Equal(got, w) {
		t.Errorf("W 记账被改动")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("对账不该创建 dest 文件, err=%v", err)
	}
}

// W 越界防御：QMeta 的 W 不设上界（SaveQMeta 只拦负值），记账超片大小
// （缓冲未落盘被杀 → 记账>盘上）时必须钳回片边界，否则 worker 会从片尾
// 之外 pwrite、越界写坏相邻片；负值兜底钳 0。
func TestReconcileAClampW(t *testing.T) {
	total, segs, _ := reconcileFixture()
	w := make([]int64, len(segs))
	for i, s := range segs {
		w[i] = (s.End - s.Start + 1) + 13 // 片大小 + 13
	}
	w[0] = -7 // 负值兜底（正常经 LoadQMeta 进不来，防御直传）

	path := destPath(t)
	growFile(t, path, total)
	got, ok := ReconcileA(path, &QMeta{Total: total, W: w}, segs)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if got[0] != 0 {
		t.Errorf("W[0] = %d, want 0（负值钳 0）", got[0])
	}
	for i, s := range segs {
		if i == 0 {
			continue
		}
		if want := s.End - s.Start + 1; got[i] != want {
			t.Errorf("W[%d] = %d, want %d（钳到片大小）", i, got[i], want)
		}
	}
}
