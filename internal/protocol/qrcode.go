// qrcode.go 把登录二维码 token URL 编成 PNG(规格 §4)。
// 选型报告结论:自研编码器整章跳过,go-qrcode 覆盖 v1–40(需求 v1–20)、
// byte 模式、纠错 M、掩码择优与静默区,此处仅做薄封装。
package protocol

import qrcode "github.com/skip2/go-qrcode"

// QRPNG 把 text 编码为纠错 M 等级的二维码,返回 pixelSize×pixelSize 的
// PNG bytes,登录页 <img> 可直接使用(配 Content-Type: image/png)。
// 空文本由库报错("no data to encode")并原样透传,不生成二维码。
func QRPNG(text string, pixelSize int) ([]byte, error) {
	return qrcode.Encode(text, qrcode.Medium, pixelSize)
}
