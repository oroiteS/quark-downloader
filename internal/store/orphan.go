package store

// 分片孤儿清扫(§6.4)。取消/换目录/强杀会留下孤儿分片(目录默认隐藏,
// 「磁盘越来越少」很难排查);启动期/定期扫一遍下载根,把 mtime 超过
// 1 天未触碰的断点残骸删掉。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sweepAge 孤儿判定阈值(§6.4 逐字「mtime 超过 1 天」):严格大于 24h
// 才删——正在下载的分片 mtime 持续刷新,永不被误删;恰 24h 整保留。
const sweepAge = 24 * time.Hour

// SweepOrphans 扫描 destDir(下载根,含子目录——任务可能落在
// relDir 深处)下的断点残骸并删除 mtime 超过 1 天的:
//
//   - <目标文件>.qparts/ 目录下的分片 p*(引擎布局 p%06d)与完成标记
//     *.done(p%06d.done,见 internal/engine/mergeb.go 的
//     partPath/partDonePath);
//   - 游离 .qmeta(目标文件同级的 Dest+".qmeta",见
//     internal/engine/task.go 的 qmetaPath)。
//
// 规格原文还点名了 .part*/meta.json——那是旧稿泛称,引擎落地后实际
// 形态即上述 p*/.done/.qmeta,按实际形态清扫。副作用(有意为之,
// §6.4 逐字):中断超 1 天的任务无法续传;完成任务留下的 .qmeta 同样
// 超期即删——完成凭证只服务对账窗口,不承诺永久留存。
//
// 目标文件本身与一切非分片形状的文件绝不触碰。个别条目被占用/无
// 权限(Windows 打开中的分片、只读目录)→ 跳过该条继续清扫,错误聚
// 合进返回值,绝不因单点失败中断整轮。destDir 不存在(首次启动下载
// 根还没建)→ 无孤儿可扫,安静返回 nil。
func SweepOrphans(destDir string, now time.Time) error {
	if _, err := os.Stat(destDir); err != nil {
		if os.IsNotExist(err) {
			return nil // 下载根还没建:无孤儿
		}
		return fmt.Errorf("store: 扫描孤儿,stat %s 失败: %w", destDir, err)
	}
	cutoff := now.Add(-sweepAge)
	var errs []error
	err := filepath.WalkDir(destDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 目录进入失败(无权限等):聚合后跳过该子树,其余照扫。
			errs = append(errs, fmt.Errorf("store: 扫描 %s 失败: %w", path, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil // 只对普通文件动手;目录与符号链接不碰
		}
		if !isSweepTarget(path) {
			return nil // 非分片形状(目标文件、无关文件)绝不动
		}
		fi, err := d.Info()
		if err != nil {
			errs = append(errs, fmt.Errorf("store: stat %s 失败: %w", path, err))
			return nil
		}
		if !fi.ModTime().Before(cutoff) {
			return nil // 24h 内触碰过:可能仍在下载,保留
		}
		if err := os.Remove(path); err != nil {
			// 占用/无权限 → 跳过(§6.4 逐字),聚合不中断。
			errs = append(errs, fmt.Errorf("store: 删孤儿 %s 失败: %w", path, err))
			return nil
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err) // WalkDir 自身失败(理论上回调已消化)
	}
	return errors.Join(errs...)
}

// isSweepTarget 判断 path 是否孤儿清扫的目标形状:
//   - 父目录名以 .qparts 结尾(<目标文件>.qparts/)且文件名以 p 开头
//     (分片 p%06d)或以 .done 结尾(完成标记 p%06d.done);
//   - 或文件名以 .qmeta 结尾(游离续传元数据,任意层级——它落在目标
//     文件同级,而目标可能在下载根的任意子目录)。
func isSweepTarget(path string) bool {
	name := filepath.Base(path)
	if strings.HasSuffix(name, ".qmeta") {
		return true
	}
	if strings.HasSuffix(filepath.Base(filepath.Dir(path)), ".qparts") {
		return strings.HasPrefix(name, "p") || strings.HasSuffix(name, ".done")
	}
	return false
}
