// export_test.go 验证直链 5 格式导出(plain/named/aria2/curl/motrix):
// golden 全串断言——小 fixture 的期望输出逐字核对模板拼接;
// 覆盖含子目录与不含子目录两种 RelPath、多文件分隔/空行、
// 未知格式与空 items 的报错语义。纯字符串拼装,不连网。
package protocol

import (
	"fmt"
	"strings"
	"testing"
)

// exportFixture 两个文件:一个含子目录(RelPath 带 /),一个不含。
var exportFixture = []ExportItem{
	{RelPath: "电影/流浪地球2.mkv", URL: "https://dl-1.quark.cn/d/abc.mkv?auth_key=AK1"},
	{RelPath: "字幕.srt", URL: "https://dl-1.quark.cn/d/def.srt?auth_key=AK2"},
}

const exportCookie = "__pus=aa; __puus=bb"

// TestExportPlainNamed golden 断言 plain 与 named 两种格式:
// plain 每行一条 URL;named 每行 RelPath+tab+URL,相对路径原样保留。
func TestExportPlainNamed(t *testing.T) {
	got, err := ExportLinks(exportFixture, exportCookie, ExportPlain)
	if err != nil {
		t.Fatalf("plain 导出失败: %v", err)
	}
	want := strings.Join([]string{
		"https://dl-1.quark.cn/d/abc.mkv?auth_key=AK1",
		"https://dl-1.quark.cn/d/def.srt?auth_key=AK2",
	}, "\n")
	if got != want {
		t.Errorf("plain 输出不符:\n got: %q\nwant: %q", got, want)
	}

	got, err = ExportLinks(exportFixture, exportCookie, ExportNamed)
	if err != nil {
		t.Fatalf("named 导出失败: %v", err)
	}
	want = strings.Join([]string{
		"电影/流浪地球2.mkv\thttps://dl-1.quark.cn/d/abc.mkv?auth_key=AK1",
		"字幕.srt\thttps://dl-1.quark.cn/d/def.srt?auth_key=AK2",
	}, "\n")
	if got != want {
		t.Errorf("named 输出不符:\n got: %q\nwant: %q", got, want)
	}
}

// TestExportAria2Curl golden 断言 aria2 与 curl 命令行:
// 有目录的文件前置 mkdir -p "电影" &&,且 -o 用完整 RelPath
// (含相对目录,如 "电影/流浪地球2.mkv"),确保落盘进 mkdir 的目录;
// aria2c 带 -c -x 16 -s 16、--header="Cookie: <整串>" 与
// --header="Referer: ...";curl 带 -L -C -、-b <cookie>、-e <referer>;
// UA 用包内常量;多文件命令之间空行分隔;RelPath 分隔符统一 /。
func TestExportAria2Curl(t *testing.T) {
	got, err := ExportLinks(exportFixture, exportCookie, ExportAria2)
	if err != nil {
		t.Fatalf("aria2 导出失败: %v", err)
	}
	cmd1 := fmt.Sprintf(`mkdir -p "电影" && aria2c -c -x 16 -s 16 "https://dl-1.quark.cn/d/abc.mkv?auth_key=AK1" -o "电影/流浪地球2.mkv" -U %q --header="Cookie: __pus=aa; __puus=bb" --header="Referer: https://pan.quark.cn/"`, UserAgent)
	cmd2 := fmt.Sprintf(`aria2c -c -x 16 -s 16 "https://dl-1.quark.cn/d/def.srt?auth_key=AK2" -o "字幕.srt" -U %q --header="Cookie: __pus=aa; __puus=bb" --header="Referer: https://pan.quark.cn/"`, UserAgent)
	want := cmd1 + "\n\n" + cmd2
	if got != want {
		t.Errorf("aria2 输出不符:\n got: %q\nwant: %q", got, want)
	}

	got, err = ExportLinks(exportFixture, exportCookie, ExportCurl)
	if err != nil {
		t.Fatalf("curl 导出失败: %v", err)
	}
	cmd1 = fmt.Sprintf(`mkdir -p "电影" && curl -L -C - "https://dl-1.quark.cn/d/abc.mkv?auth_key=AK1" -o "电影/流浪地球2.mkv" -A %q -b "__pus=aa; __puus=bb" -e "https://pan.quark.cn/"`, UserAgent)
	cmd2 = fmt.Sprintf(`curl -L -C - "https://dl-1.quark.cn/d/def.srt?auth_key=AK2" -o "字幕.srt" -A %q -b "__pus=aa; __puus=bb" -e "https://pan.quark.cn/"`, UserAgent)
	want = cmd1 + "\n\n" + cmd2
	if got != want {
		t.Errorf("curl 输出不符:\n got: %q\nwant: %q", got, want)
	}
}

// TestExportMotrix golden 断言 Motrix 分段格式:
// 每文件五段标签【文件名】【链接】【Referer】【Cookie】【User-Agent】齐全;
// 【文件名】为裸文件名(RelPath 最后一个 / 之后的部分,不含相对目录);
// Cookie 值后带 ★必填标注;文件间 ────── 分隔线,数量=文件数-1。
func TestExportMotrix(t *testing.T) {
	got, err := ExportLinks(exportFixture, exportCookie, ExportMotrix)
	if err != nil {
		t.Fatalf("motrix 导出失败: %v", err)
	}
	block1 := `【文件名】流浪地球2.mkv
【链接】https://dl-1.quark.cn/d/abc.mkv?auth_key=AK1
【Referer】https://pan.quark.cn/
【Cookie】__pus=aa; __puus=bb ★必填，不填会 412
【User-Agent】` + UserAgent + ` 可留空`
	block2 := `【文件名】字幕.srt
【链接】https://dl-1.quark.cn/d/def.srt?auth_key=AK2
【Referer】https://pan.quark.cn/
【Cookie】__pus=aa; __puus=bb ★必填，不填会 412
【User-Agent】` + UserAgent + ` 可留空`
	want := block1 + "\n──────\n" + block2
	if got != want {
		t.Errorf("motrix 输出不符:\n got: %q\nwant: %q", got, want)
	}
	if n := strings.Count(got, "──────"); n != len(exportFixture)-1 {
		t.Errorf("分隔线数量 = %d, want %d", n, len(exportFixture)-1)
	}
}

// TestExportNoDirNoMkdir:RelPath 不含斜杠(无子目录)时,
// aria2/curl 输出不得出现 mkdir 段与 && 连接符。
func TestExportNoDirNoMkdir(t *testing.T) {
	items := []ExportItem{{RelPath: "a.mkv", URL: "https://dl-1.quark.cn/d/a.mkv?auth_key=AK"}}
	for _, f := range []ExportFormat{ExportAria2, ExportCurl} {
		got, err := ExportLinks(items, exportCookie, f)
		if err != nil {
			t.Fatalf("格式 %s 导出失败: %v", f, err)
		}
		if strings.Contains(got, "mkdir") || strings.Contains(got, "&&") {
			t.Errorf("格式 %s 无目录却含 mkdir/&&:\n%s", f, got)
		}
		if !strings.Contains(got, `-o "a.mkv"`) {
			t.Errorf("格式 %s 缺 -o \"a.mkv\" 段:\n%s", f, got)
		}
	}
}

// TestExportErrors:空 items 与未知格式均报错且返回空串。
// 语义选择:空列表导出无意义,显式报错让调用方(Web 层)暴露
// "未选中任何文件却触发生成"的缺陷,而不是静默给出空文件。
func TestExportErrors(t *testing.T) {
	if got, err := ExportLinks(nil, exportCookie, ExportPlain); err == nil || got != "" {
		t.Errorf("空 items 应报错并返回空串, got %q, err %v", got, err)
	}
	if got, err := ExportLinks([]ExportItem{}, exportCookie, ExportPlain); err == nil || got != "" {
		t.Errorf("空切片应报错并返回空串, got %q, err %v", got, err)
	}
	if got, err := ExportLinks(exportFixture, exportCookie, ExportFormat("xls")); err == nil || got != "" {
		t.Errorf("未知格式应报错并返回空串, got %q, err %v", got, err)
	}
}
