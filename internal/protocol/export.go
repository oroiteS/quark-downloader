// export.go 把直链结果拼装为 5 种可直接使用的导出格式
// (裸链/带名/aria2/curl/Motrix),供 Web 层"生成直链"对话框消费。
// 裸直链有防盗链,必须配套 Cookie(必填)/Referer(建议)/UA(同族一致),
// 因此 aria2/curl/motrix 模板一律内嵌这三者。纯字符串拼装,零依赖。
package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// ExportFormat 标识直链导出格式。
type ExportFormat string

// 五种导出格式常量。
const (
	ExportPlain  ExportFormat = "plain"  // 每行一条 URL
	ExportNamed  ExportFormat = "named"  // 每行 RelPath + tab + URL
	ExportAria2  ExportFormat = "aria2"  // aria2c 命令行(逐条、空行分隔)
	ExportCurl   ExportFormat = "curl"   // curl 命令行(逐条、空行分隔)
	ExportMotrix ExportFormat = "motrix" // Motrix 表单分段粘贴
)

// ExportItem 是单个待导出文件:RelPath 为相对保存路径(可含相对目录,
// 分隔符统一 /),URL 为直链。与 LinkResult 解耦,由调用方负责构造。
type ExportItem struct {
	RelPath string
	URL     string
}

// quarkReferer 是直链下载必带的 Referer(防盗链校验来源页)。
const quarkReferer = "https://pan.quark.cn/"

// ExportLinks 把 items 按 f 格式拼成导出文本并整串返回。
// cookieStr 为完整 Cookie 串(防盗链必填)。aria2/curl 多文件命令间
// 以空行分隔、每条前置 mkdir -p(RelPath 无目录时省略),-o 一律用
// 完整 RelPath(含相对目录,与 mkdir 的目录一致,确保落盘位置正确);
// motrix 每文件五段标注、文件间 ────── 分隔线。items 为空或格式
// 未知时返回错误。
func ExportLinks(items []ExportItem, cookieStr string, f ExportFormat) (string, error) {
	if len(items) == 0 {
		return "", errors.New("没有可导出的直链")
	}
	switch f {
	case ExportPlain:
		lines := make([]string, len(items))
		for i, it := range items {
			lines[i] = it.URL
		}
		return strings.Join(lines, "\n"), nil
	case ExportNamed:
		lines := make([]string, len(items))
		for i, it := range items {
			lines[i] = it.RelPath + "\t" + it.URL
		}
		return strings.Join(lines, "\n"), nil
	case ExportAria2:
		cmds := make([]string, len(items))
		for i, it := range items {
			cmds[i] = mkdirPrefix(it.RelPath) + fmt.Sprintf(
				`aria2c -c -x 16 -s 16 %q -o %q -U %q --header="Cookie: %s" --header="Referer: %s"`,
				it.URL, it.RelPath, UserAgent, cookieStr, quarkReferer)
		}
		return strings.Join(cmds, "\n\n"), nil
	case ExportCurl:
		cmds := make([]string, len(items))
		for i, it := range items {
			cmds[i] = mkdirPrefix(it.RelPath) + fmt.Sprintf(
				`curl -L -C - %q -o %q -A %q -b %q -e %q`,
				it.URL, it.RelPath, UserAgent, cookieStr, quarkReferer)
		}
		return strings.Join(cmds, "\n\n"), nil
	case ExportMotrix:
		blocks := make([]string, len(items))
		for i, it := range items {
			blocks[i] = fmt.Sprintf(
				"【文件名】%s\n【链接】%s\n【Referer】%s\n【Cookie】%s ★必填，不填会 412\n【User-Agent】%s 可留空",
				it.RelPath, it.URL, quarkReferer, cookieStr, UserAgent)
		}
		return strings.Join(blocks, "\n──────\n"), nil
	default:
		return "", fmt.Errorf("未知的导出格式: %s", string(f))
	}
}

// mkdirPrefix 生成 aria2/curl 命令的前置段:RelPath 含目录时返回
// `mkdir -p "<dir>" && `;无目录(RelPath 不含 /)时返回空串。
func mkdirPrefix(relPath string) string {
	if i := strings.LastIndex(relPath, "/"); i > 0 {
		return fmt.Sprintf("mkdir -p %q && ", relPath[:i])
	}
	return ""
}
