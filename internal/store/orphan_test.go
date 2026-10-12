package store

// SweepOrphans(§6.4 分片孤儿清扫)的测试:新鲜保留/过期删/子目录递归/
// 错误聚合不中断。全部离线——mtime 用 os.Chtimes 伪造,时间基准注入。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// sweepNow 固定时间基准(注入 now,离线可复现)。
var sweepNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// sweepOld mtime 早于 now 25h(> 24h 阈值,该删);
// sweepFresh mtime 早于 now 1h(分片仍在被写/刚触碰,该留)。
var (
	sweepOld   = sweepNow.Add(-25 * time.Hour)
	sweepFresh = sweepNow.Add(-1 * time.Hour)
)

// sweepTouch 建文件并伪造 mtime。
func sweepTouch(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func sweepMustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s 应存在: %v", path, err)
	}
}

func sweepMustGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s 应已删除(stat err=%v)", path, err)
	}
}

// TestSweepOrphansExpiredRemoved 过期删/新鲜保留/子目录递归/目标文件不动/
// 非分片形状不动,一个完整布局一次断言。文件形态对齐引擎断点布局
// (internal/engine/mergeb.go):<dest>.qparts/ 下 p%06d 与 p%06d.done,
// .qmeta 落在目标文件同级(Dest+".qmeta")。
func TestSweepOrphansExpiredRemoved(t *testing.T) {
	dest := t.TempDir()
	parts := filepath.Join(dest, "movie.mkv.qparts")
	deepParts := filepath.Join(dest, "电影", "深目录", "deep.mkv.qparts")
	for _, d := range []string{parts, deepParts} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 过期(>24h)→ 删。
	sweepTouch(t, filepath.Join(dest, "movie.mkv.qmeta"), sweepOld)             // 游离 .qmeta(目标同级)
	sweepTouch(t, filepath.Join(parts, "p000000"), sweepOld)                    // 过期分片
	sweepTouch(t, filepath.Join(parts, "p000001.done"), sweepOld)               // 过期完成标记
	sweepTouch(t, filepath.Join(deepParts, "p000000"), sweepOld)                // 递归子目录里的过期分片
	sweepTouch(t, filepath.Join(deepParts, "p000000.done"), sweepOld)           // 递归子目录里的过期标记
	sweepTouch(t, filepath.Join(dest, "电影", "深目录", "deep.mkv.qmeta"), sweepOld) // 递归子目录里的游离 .qmeta

	// 新鲜(≤24h,正在下载 mtime 持续刷新)→ 保留。
	sweepTouch(t, filepath.Join(dest, "fresh.mkv.qmeta"), sweepFresh)
	sweepTouch(t, filepath.Join(parts, "p000001"), sweepFresh)
	sweepTouch(t, filepath.Join(parts, "p000002.done"), sweepFresh)
	sweepTouch(t, filepath.Join(deepParts, "p000003"), sweepFresh)

	// 非清扫目标 → 一律不动(孤儿清扫只认分片形状,绝不碰用户文件)。
	sweepTouch(t, filepath.Join(dest, "movie.mkv"), sweepOld)             // 目标文件本身(mtime 再老也不删)
	sweepTouch(t, filepath.Join(dest, "无关文件.txt"), sweepOld)              // 普通文件
	sweepTouch(t, filepath.Join(parts, "notes.txt"), sweepOld)            // .qparts 里非 p*/.done 形状
	sweepTouch(t, filepath.Join(dest, "电影", "深目录", "deep.mkv"), sweepOld) // 子目录里的目标文件

	if err := SweepOrphans(dest, sweepNow); err != nil {
		t.Fatalf("SweepOrphans 全部可删时不应报错: %v", err)
	}

	for _, g := range []string{
		filepath.Join(dest, "movie.mkv.qmeta"),
		filepath.Join(parts, "p000000"),
		filepath.Join(parts, "p000001.done"),
		filepath.Join(deepParts, "p000000"),
		filepath.Join(deepParts, "p000000.done"),
		filepath.Join(dest, "电影", "深目录", "deep.mkv.qmeta"),
	} {
		sweepMustGone(t, g)
	}
	for _, k := range []string{
		filepath.Join(dest, "fresh.mkv.qmeta"),
		filepath.Join(parts, "p000001"),
		filepath.Join(parts, "p000002.done"),
		filepath.Join(deepParts, "p000003"),
		filepath.Join(dest, "movie.mkv"),
		filepath.Join(dest, "无关文件.txt"),
		filepath.Join(parts, "notes.txt"),
		filepath.Join(dest, "电影", "深目录", "deep.mkv"),
	} {
		sweepMustExist(t, k)
	}
}

// TestSweepOrphansBoundaryNewExactly24h mtime 恰好 24h(未「超过」)→
// 保留;阈值是严格大于 24h。
func TestSweepOrphansBoundaryNewExactly24h(t *testing.T) {
	dest := t.TempDir()
	parts := filepath.Join(dest, "a.mkv.qparts")
	if err := os.MkdirAll(parts, 0o755); err != nil {
		t.Fatal(err)
	}
	sweepTouch(t, filepath.Join(parts, "p000000"), sweepNow.Add(-24*time.Hour))
	sweepTouch(t, filepath.Join(dest, "a.mkv.qmeta"), sweepNow.Add(-24*time.Hour))
	if err := SweepOrphans(dest, sweepNow); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	sweepMustExist(t, filepath.Join(parts, "p000000"))
	sweepMustExist(t, filepath.Join(dest, "a.mkv.qmeta"))
}

// TestSweepOrphansMissingDir destDir 不存在(首次启动 DestDir 还没建)→
// 无孤儿可扫,安静返回 nil,不报错。
func TestSweepOrphansMissingDir(t *testing.T) {
	if err := SweepOrphans(filepath.Join(t.TempDir(), "不存在"), sweepNow); err != nil {
		t.Errorf("目录不存在应返回 nil: %v", err)
	}
}

// TestSweepOrphansEmptyDir destDir 存在但为空 → nil。
func TestSweepOrphansEmptyDir(t *testing.T) {
	if err := SweepOrphans(t.TempDir(), sweepNow); err != nil {
		t.Errorf("空目录应返回 nil: %v", err)
	}
}

// TestSweepOrphansRemovalErrorAggregates 删除失败(占用/无权限)→ 跳过该
// 条、继续清扫其余、错误聚合返回非 nil,绝不中断整个清扫。
//
// 可移植性取舍:用「只读目录(0o500,owner 无写位)中的过期分片」制造
// EACCES——unlink 权限取决于父目录写位,三平台语义一致;但 root
// (CI 容器常见)与 Windows 的 chmod 语义弱,直接跳过,本用例不背
// 平台差异的锅。macOS 上 chmod 0000 目录对 root 仍可读同属此类,
// 故一律只以非 root 的 POSIX 为准。
func TestSweepOrphansRemovalErrorAggregates(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("chmod 权限位对 Windows/root 不构成约束,跳过")
	}
	dest := t.TempDir()
	locked := filepath.Join(dest, "locked.mkv.qparts") // 只读目录:删除必失败
	ok := filepath.Join(dest, "ok.mkv.qparts")         // 正常目录:照常清扫
	for _, d := range []string{locked, ok} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sweepTouch(t, filepath.Join(locked, "p000000"), sweepOld)
	sweepTouch(t, filepath.Join(locked, "p000000.done"), sweepOld)
	sweepTouch(t, filepath.Join(ok, "p000000"), sweepOld)
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755) // 还原权限,让 T.TempDir 清理干净

	err := SweepOrphans(dest, sweepNow)
	if err == nil {
		t.Error("只读目录中的过期分片删不掉,应聚合返回 error")
	}
	// 不中断:正常目录里的过期分片照删。
	sweepMustGone(t, filepath.Join(ok, "p000000"))
	// 跳过:删不掉的原样保留。
	sweepMustExist(t, filepath.Join(locked, "p000000"))
	sweepMustExist(t, filepath.Join(locked, "p000000.done"))
}
