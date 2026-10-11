package engine

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// 期望内容全部按规格 §3.6 格式逐字推演：行1 total、行2 n、行3..n+2 每片 S,E,W。

func qmetaPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "file.bin.qmeta")
}

// 写读往返：各种规模 total/threads 下 Plan 切片 → 伪随机 W → Save → Load，
// Total 与 W 逐字节一致。
func TestQMetaRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		total   int64
		threads int
	}{
		{"单片小文件", 10, 1},
		{"1000003/16 非整除重对齐", 1000003, 16},
		{"100MB/64", 100 * tMB, 64},
		{"规格例 9.5GB/512", int64(9.5 * float64(tGB)), 512},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			segs := Plan(tt.total, tt.threads)
			w := make([]int64, len(segs))
			for i, s := range segs {
				size := s.End - s.Start + 1
				w[i] = int64(uint(i*7919+13) % uint(size+1)) // 确定性伪随机，覆盖 0..size
			}
			path := qmetaPath(t)
			m := &QMeta{Total: tt.total, W: w}
			if err := SaveQMeta(path, m); err != nil {
				t.Fatalf("SaveQMeta: %v", err)
			}
			got, err := LoadQMeta(path)
			if err != nil {
				t.Fatalf("LoadQMeta: %v", err)
			}
			if got.Total != m.Total {
				t.Errorf("Total = %d, want %d", got.Total, m.Total)
			}
			if !slices.Equal(got.W, m.W) {
				t.Errorf("W 往返不一致: got len=%d, want len=%d", len(got.W), len(m.W))
			}
		})
	}
}

// 文本格式逐字：total=10 → 单片 [0,9]，W=3 → "10\n1\n0,9,3\n"。
func TestQMetaFileFormat(t *testing.T) {
	path := qmetaPath(t)
	if err := SaveQMeta(path, &QMeta{Total: 10, W: []int64{3}}); err != nil {
		t.Fatalf("SaveQMeta: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "10\n1\n0,9,3\n"; string(b) != want {
		t.Errorf("文件内容 = %q, want %q", b, want)
	}
}

// 上次保存中途被杀 → tmp 残留垃圾：旧文件必须原样可读，且下次保存照常成功。
func TestQMetaResidualTmp(t *testing.T) {
	path := qmetaPath(t)
	v1 := &QMeta{Total: 100, W: []int64{40}}
	if err := SaveQMeta(path, v1); err != nil {
		t.Fatalf("SaveQMeta v1: %v", err)
	}
	if err := os.WriteFile(path+".tmp", []byte("写到一半的垃圾"), 0o644); err != nil {
		t.Fatalf("模拟 tmp 残留: %v", err)
	}

	got, err := LoadQMeta(path)
	if err != nil {
		t.Fatalf("tmp 残留不该影响旧文件: %v", err)
	}
	if got.Total != v1.Total || !slices.Equal(got.W, v1.W) {
		t.Errorf("旧文件被改动: got {Total:%d W:%v}, want {Total:%d W:%v}", got.Total, got.W, v1.Total, v1.W)
	}

	v2 := &QMeta{Total: 100, W: []int64{90}}
	if err := SaveQMeta(path, v2); err != nil {
		t.Fatalf("下次保存应照常成功: %v", err)
	}
	if got, err := LoadQMeta(path); err != nil || got.Total != v2.Total || !slices.Equal(got.W, v2.W) {
		t.Errorf("v2 未生效: got %v, err %v", got, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("成功保存后 tmp 应已被换入（rename 走人）, err=%v", err)
	}
}

// 原子替换语义（§3.6）：rename 换入而非删改/原地改写。
//   - 观察者线程全程 stat：替换期间 path 必须始终存在（先删后改会露出空窗）；
//   - 换入前后 inode 变化（os.SameFile == false）：证明是 rename 换新文件，非原地重写。
func TestQMetaAtomicReplace(t *testing.T) {
	path := qmetaPath(t)
	if err := SaveQMeta(path, &QMeta{Total: 100, W: []int64{1}}); err != nil {
		t.Fatalf("首次 SaveQMeta: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	var missing atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(path); os.IsNotExist(err) {
				missing.Store(true)
			}
		}
	}()

	for i := 2; i <= 200; i++ {
		if err := SaveQMeta(path, &QMeta{Total: 100, W: []int64{int64(i)}}); err != nil {
			t.Fatalf("第 %d 次保存: %v", i, err)
		}
	}
	close(stop)
	<-done

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if missing.Load() {
		t.Errorf("替换期间 path 曾消失——疑似「先删旧再改名」两步实现")
	}
	if os.SameFile(before, after) {
		t.Errorf("替换后文件身份未变——疑似原地改写而非 rename 换入")
	}
	got, err := LoadQMeta(path)
	if err != nil || got.Total != 100 || len(got.W) != 1 || got.W[0] != 200 {
		t.Errorf("最终内容: got %v, err %v", got, err)
	}
}

// 坏格式一律报错（调用方据 error 丢弃元数据、全新开始，§3.6）。
// 基准合法内容: total=100, n=2, layout(100,2) = [0,49],[50,99]。
func TestLoadQMetaBadFormat(t *testing.T) {
	if _, err := LoadQMeta(filepath.Join(t.TempDir(), "不存在.qmeta")); err == nil {
		t.Errorf("文件不存在应报错")
	}

	cases := []struct{ name, content string }{
		{"空文件", ""},
		{"只有 total", "100\n"},
		{"total 非数字", "abc\n2\n0,49,10\n50,99,0\n"},
		{"total 非正数", "0\n1\n0,99,0\n"},
		{"n 非数字", "100\nx\n0,49,10\n50,99,0\n"},
		{"n=0", "100\n0\n"},
		{"n 超 MAX_SEG", "100\n16385\n"},
		{"片行数不足", "100\n2\n0,49,10\n"},
		{"片行数多余", "100\n2\n0,49,10\n50,99,0\n0,0,0\n"},
		{"片行缺字段", "100\n2\n0,49\n50,99,0\n"},
		{"片行多字段", "100\n2\n0,49,10,1\n50,99,0\n"},
		{"字段非数字", "100\n2\n0,49,x\n50,99,0\n"},
		{"S 与重推划分不符", "100\n2\n0,48,10\n50,99,0\n"},
		{"E 与重推划分不符", "100\n2\n0,49,10\n50,98,0\n"},
		{"W 为负", "100\n2\n0,49,-1\n50,99,0\n"},
		{"末行截断无换行", "100\n2\n0,49,10\n50,99"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			path := qmetaPath(t)
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if m, err := LoadQMeta(path); err == nil {
				t.Errorf("坏格式 %q 应报错, 却返回 %v", tt.content, m)
			}
		})
	}
}

// 非法输入在落盘前拦截：不产生 path，也不留 tmp。
func TestSaveQMetaValidation(t *testing.T) {
	path := qmetaPath(t)
	bad := []*QMeta{
		nil,
		{Total: 0, W: []int64{0}},
		{Total: -5, W: []int64{0}},
		{Total: 100, W: nil},
		{Total: 100, W: []int64{10, -1}},
	}
	for i, m := range bad {
		if err := SaveQMeta(path, m); err == nil {
			t.Errorf("case %d (%v): 期望报错", i, m)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("非法输入不应产生 meta 文件")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("非法输入不应留下 tmp")
	}
}

// 并发保存串行化：同一 path 多写者并发全量重写 + 并发读者。
// 每次保存写 W={x,x}（两片相等）；任何撕裂都会破坏这一不变量或触发 Load 报错。
func TestSaveQMetaConcurrent(t *testing.T) {
	path := qmetaPath(t)
	if err := SaveQMeta(path, &QMeta{Total: 100, W: []int64{0, 0}}); err != nil {
		t.Fatalf("初始 SaveQMeta: %v", err)
	}

	const workers, iters = 8, 50
	stop := make(chan struct{})
	var loadErrs atomic.Int64
	var reads atomic.Int64
	readerDone := make(chan struct{})
	go func() { // 并发读者：任一时刻读到的必须是某次完整保存的快照
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			m, err := LoadQMeta(path)
			if err != nil {
				loadErrs.Add(1)
			} else if m.W[0] != m.W[1] {
				loadErrs.Add(1) // 撕裂快照
			} else {
				reads.Add(1)
			}
		}
	}()

	var wg sync.WaitGroup
	var saveErrs atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= iters; i++ {
				if err := SaveQMeta(path, &QMeta{Total: 100, W: []int64{int64(i), int64(i)}}); err != nil {
					saveErrs.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-readerDone

	if n := saveErrs.Load(); n != 0 {
		t.Errorf("%d 次并发保存报错", n)
	}
	if n := loadErrs.Load(); n != 0 {
		t.Errorf("%d 次并发读取看到坏快照/报错", n)
	}
	if reads.Load() == 0 {
		t.Errorf("读者从未成功读到快照（测试本身失效）")
	}
	got, err := LoadQMeta(path)
	if err != nil || got.Total != 100 || got.W[0] != got.W[1] {
		t.Errorf("最终内容: got %v, err %v", got, err)
	}
}
