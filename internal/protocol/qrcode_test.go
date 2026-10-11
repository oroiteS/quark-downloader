package protocol

import (
	"bytes"
	"image/png"
	"testing"
)

// pngMagic 是 PNG 文件头 8 字节魔数(\x89PNG\r\n\x1a\n)。
// QRPNG 的输出要直接塞进登录页 <img>,必须是合法 PNG 流。
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// scanURL 还原 qrScanURLTemplate(qrlogin.go)填入 token/client_id 后的
// 真实形状,长度与生产同量级——URL 长度决定 QR 版本与 PNG 字节数。
const scanURL = "https://su.quark.cn/4_eMHBJ?token=f4d9a2b3c8e71f60d5a2b9c4e8f17326&client_id=100000" +
	"&ssb=weblogin&uc_param_str=&uc_biz_str=S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0"

func TestQRPNG(t *testing.T) {
	pngBytes, err := QRPNG(scanURL, 512)
	if err != nil {
		t.Fatalf("QRPNG 报错: %v", err)
	}
	if len(pngBytes) == 0 {
		t.Fatal("QRPNG 返回空 bytes")
	}
	if !bytes.HasPrefix(pngBytes, pngMagic) {
		t.Fatalf("输出不是 PNG: 前 8 字节 = % x", pngBytes[:8])
	}
	// 解码头信息校验尺寸:pixelSize 必须原样体现在图上(登录页展示尺寸)。
	cfg, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("PNG 无法解码: %v", err)
	}
	if cfg.Width != 512 || cfg.Height != 512 {
		t.Fatalf("图尺寸 = %dx%d, 期望 512x512", cfg.Width, cfg.Height)
	}
	// 真实形状 URL(179 字节)在 512px 下实测 1408 bytes,断 >1KB。
	// 注:库用索引色,短文本可压到 1KB 以下,故该阈值绑定真实量级输入。
	if len(pngBytes) <= 1024 {
		t.Fatalf("真实形状 URL 的 512px PNG 过小: %d bytes", len(pngBytes))
	}
}

func TestQRPNGEmptyText(t *testing.T) {
	// 选定语义:透传库默认行为——空文本由 go-qrcode 报错
	// (encoder.go "no data to encode"),不产出 PNG。
	// 薄封装不另加策略,调用方拿到 err 自行处理。
	pngBytes, err := QRPNG("", 512)
	if err == nil {
		t.Fatal(`QRPNG("") 应报错, 实际成功`)
	}
	if len(pngBytes) != 0 {
		t.Fatalf(`QRPNG("") 报错时应无输出, 实际 %d bytes`, len(pngBytes))
	}
}
