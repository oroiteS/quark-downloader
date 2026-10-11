package protocol

import "testing"

// TestParseShare 覆盖 §2.7 分享文案解析:纯链接、整段文案、pwd= 参数、
// RE2 改写后的中文提取码双层防线(pwd1234)、口令只做标记、32 位目录 fid
// (含 -名称后缀、多 fid 取最后、pwd_id 恰 32 位不误作 fid)等真实形态。
func TestParseShare(t *testing.T) {
	const fidA = "0123456789abcdef0123456789abcdef" // 恰好 32 位
	const fidB = "fedcba9876543210fedcba9876543210"
	const fidC = "ffffffffffffffffffffffffffffffff"

	cases := []struct {
		name    string
		text    string
		want    ShareRef
		wantErr string // 非空则期待该错误信息
	}{
		{
			name: "纯链接",
			text: "https://pan.quark.cn/s/abc123",
			want: ShareRef{PwdID: "abc123"},
		},
		{
			name: "链接带查询参数",
			text: "https://pan.quark.cn/s/abc123?entry=index",
			want: ShareRef{PwdID: "abc123"},
		},
		{
			name: "整段文案中文提取码全角冒号",
			text: "来拿资源 https://pan.quark.cn/s/abc123 提取码：Xy9z",
			want: ShareRef{PwdID: "abc123", Passcode: "Xy9z"},
		},
		{
			name: "整段文案密码空格分隔",
			text: "来拿资源 https://pan.quark.cn/s/abc123 密码 Xy9z",
			want: ShareRef{PwdID: "abc123", Passcode: "Xy9z"},
		},
		{
			name: "提取码紧贴关键词无分隔符",
			text: "链接 https://pan.quark.cn/s/abc123 提取码Xy9z",
			want: ShareRef{PwdID: "abc123", Passcode: "Xy9z"},
		},
		{
			name: "繁体提取碼",
			text: "https://pan.quark.cn/s/abc123 提取碼：Xy9z",
			want: ShareRef{PwdID: "abc123", Passcode: "Xy9z"},
		},
		{
			name: "访问码",
			text: "https://pan.quark.cn/s/abc123 访问码 ab12",
			want: ShareRef{PwdID: "abc123", Passcode: "ab12"},
		},
		{
			name: "PWD 大写带冒号",
			text: "https://pan.quark.cn/s/abc123 PWD:ab12",
			want: ShareRef{PwdID: "abc123", Passcode: "ab12"},
		},
		{
			name: "pwd= 查询参数形式",
			text: "https://pan.quark.cn/s/abc123?pwd=Xy9z",
			want: ShareRef{PwdID: "abc123", Passcode: "Xy9z"},
		},
		{
			name: "pwd= 优先于中文提取码",
			text: "https://pan.quark.cn/s/abc123?pwd=first 提取码：second",
			want: ShareRef{PwdID: "abc123", Passcode: "first"},
		},
		{
			name:    "pwd1234 独立出现不误判为提取码",
			text:    "分享ID pwd1234 已发",
			wantErr: "未识别到分享链接",
		},
		{
			name: "pwd1234 伴随真链接时 Passcode 必须为空",
			text: "https://pan.quark.cn/s/abc123 分享ID pwd1234 已发",
			want: ShareRef{PwdID: "abc123"},
		},
		{
			name: "链接内 pwd_id 本身叫 pwd1234",
			text: "https://pan.quark.cn/s/pwd1234 提取码：Xy9z",
			want: ShareRef{PwdID: "pwd1234", Passcode: "Xy9z"},
		},
		{
			name: "口令独立出现",
			text: "/~469d3M9zTu~:/",
			want: ShareRef{IsKouling: true},
		},
		{
			name: "口令嵌在整段文案",
			text: "【夸克网盘】好东西 /~469d3M9zTu~ 快来",
			want: ShareRef{IsKouling: true},
		},
		{
			name: "四位最短口令",
			text: "复/~ab12~:/制",
			want: ShareRef{IsKouling: true},
		},
		{
			name: "32位 fid 带 -名称 后缀",
			text: "https://pan.quark.cn/s/ab12#/list/share/" + fidA + "-我的文件夹",
			want: ShareRef{PwdID: "ab12", DirFID: fidA},
		},
		{
			name: "fid 后直接问号参数",
			text: "https://pan.quark.cn/s/ab12#/list/share/" + fidA + "?entry=index",
			want: ShareRef{PwdID: "ab12", DirFID: fidA},
		},
		{
			name: "多个 32 位串取最后一个",
			text: "https://pan.quark.cn/s/ab12#/list/share/" + fidA + "/sub/" + fidB,
			want: ShareRef{PwdID: "ab12", DirFID: fidB},
		},
		{
			name: "最后一个 32 位串等于 pwd_id 时回退前一个",
			text: "目录 /" + fidC + "，链接 https://pan.quark.cn/s/" + fidB,
			want: ShareRef{PwdID: fidB, DirFID: fidC},
		},
		{
			name: "pwd_id 恰好 32 位不误作 fid",
			text: "https://pan.quark.cn/s/" + fidA,
			want: ShareRef{PwdID: fidA},
		},
		{
			name:    "普通文本无链接",
			text:    "今天天气不错",
			wantErr: "未识别到分享链接",
		},
		{
			name:    "空字符串",
			text:    "",
			wantErr: "未识别到分享链接",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseShareText(c.text)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseShareText(%q) 期望错误 %q,实际成功 %+v", c.text, c.wantErr, got)
				}
				if err.Error() != c.wantErr {
					t.Fatalf("ParseShareText(%q) 错误信息 = %q,期望 %q", c.text, err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseShareText(%q) 意外错误: %v", c.text, err)
			}
			if got != c.want {
				t.Fatalf("ParseShareText(%q) = %+v,期望 %+v", c.text, got, c.want)
			}
		})
	}
}
