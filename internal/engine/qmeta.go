// .qmeta 续传元数据读写（§3.6）。监视线程（Task 7）每 1s 调 SaveQMeta
// 全量重写一次；恢复对账（Task 6/12）调 LoadQMeta 读回。S/E 不持久化在
// QMeta 里——片的划分只由 (total, n) 决定，layout 可唯一重推。
package engine

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// QMeta 是 .qmeta 的内存形态：文件总字节与每片已写字节。
// W 不设上界——对账以磁盘为准（§3.6「一律以磁盘为准，不信 W 记账」）。
type QMeta struct {
	Total int64
	W     []int64
}

// qmetaMu 把「写 tmp → rename 换入」两步绑成一个不可分事务：同一进程内
// 并发 SaveQMeta 不会交叉写同一个 tmp 文件。调用方（单一监视线程，§3.6）
// 本身也保证串行，这里是双保险。跨进程互斥不提供——规格架构每任务单写者。
var qmetaMu sync.Mutex

// SaveQMeta 把 m 原子写入 path。写入协议（§3.6 逐字）：
//
//	写 <metaPath>.tmp → 若 metaPath 已存在用「原子替换」语义一步换入 → 否则 rename
//	绝不能「先删旧再改名」——两步间断电 = 元数据全丢 = 任务从零开始。
//
// 实现上两种情况统一一次 os.Rename：Go 的 os.Rename 在 POSIX 上就是原子
// rename(2)（目标存在直接替换）；在 Windows 上内部走 MoveFileEx(
// MOVEFILE_REPLACE_EXISTING)（即规格所说 ReplaceFile 一族语义），目标存
// 在同样一步覆盖——因此不需要分支，更不需要先 os.Remove（那是事故注释
// 明令禁止的两步断电窗口）。换入前先 f.Sync：断电时要么旧文件完好、
// 要么新文件数据完整落盘，不会出现目录项先换而 tmp 内容未刷盘的空壳。
// 残留的旧 tmp 无害：O_TRUNC 重写后照常换入。
func SaveQMeta(path string, m *QMeta) error {
	if m == nil {
		return fmt.Errorf("qmeta: m 为 nil")
	}
	if m.Total <= 0 {
		return fmt.Errorf("qmeta: total=%d 非法（须 >0）", m.Total)
	}
	n := len(m.W)
	if n < 1 || int64(n) > segCountMax {
		return fmt.Errorf("qmeta: 片数=%d 越界 [1,%d]", n, segCountMax)
	}
	for i, w := range m.W {
		if w < 0 {
			return fmt.Errorf("qmeta: W[%d]=%d 非法（须 ≥0）", i, w)
		}
	}

	segs := layout(m.Total, n)
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n%d\n", m.Total, n) // 行1 total；行2 n
	for i, s := range segs { // 行3..n+2: 每片 S,E,W
		fmt.Fprintf(&b, "%d,%d,%d\n", s.Start, s.End, m.W[i])
	}

	qmetaMu.Lock()
	defer qmetaMu.Unlock()

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // 数据先落盘，再换目录项
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadQMeta 从 path 读回 QMeta。文本格式（§3.6 逐字）：行1 total；行2 n；
// 行3..n+2 每片 S,E,W。逐行校验，任何坏格式（行数、数字、S,E 与
// layout(total, n) 重推划分不符——分片划分变了、以及 total/n 越界）都返回
// error，由调用方丢弃元数据全新开始（§3.6 对账：「若 plan 缺失/total 不符/
// 分片划分不符 → 丢弃」）。文件不存在的 os 错误原样透传。
func LoadQMeta(path string) (*QMeta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // 末行换行产生的空尾
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("qmeta: 至少 2 行（total/n），实得 %d 行", len(lines))
	}
	total, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("qmeta: 行1 total %q: %w", lines[0], err)
	}
	if total <= 0 {
		return nil, fmt.Errorf("qmeta: total=%d 非法（须 >0）", total)
	}
	n64, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("qmeta: 行2 n %q: %w", lines[1], err)
	}
	if n64 < 1 || n64 > segCountMax {
		return nil, fmt.Errorf("qmeta: n=%d 越界 [1,%d]", n64, segCountMax)
	}
	n := int(n64)
	if len(lines) != n+2 {
		return nil, fmt.Errorf("qmeta: 实得 %d 行，片数 n=%d 要求 %d 行", len(lines), n, n+2)
	}

	segs := layout(total, n)
	m := &QMeta{Total: total, W: make([]int64, n)}
	for i := 0; i < n; i++ {
		line := i + 2
		fields := strings.Split(lines[line], ",")
		if len(fields) != 3 {
			return nil, fmt.Errorf("qmeta: 行%d %q 不是 S,E,W 三段", line+1, lines[line])
		}
		s, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("qmeta: 行%d S %q: %w", line+1, fields[0], err)
		}
		e, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("qmeta: 行%d E %q: %w", line+1, fields[1], err)
		}
		w, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("qmeta: 行%d W %q: %w", line+1, fields[2], err)
		}
		if s != segs[i].Start || e != segs[i].End {
			return nil, fmt.Errorf("qmeta: 行%d [%d,%d] 与 total=%d,n=%d 重推划分 [%d,%d] 不符（分片划分不符 → 丢弃）",
				line+1, s, e, total, n, segs[i].Start, segs[i].End)
		}
		if w < 0 {
			return nil, fmt.Errorf("qmeta: 行%d W=%d 非法（须 ≥0）", line+1, w)
		}
		m.W[i] = w
	}
	return m, nil
}
