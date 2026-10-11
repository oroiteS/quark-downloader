// wallclient.go 实现过 TLS 指纹墙的 HTTP 客户端(规格 §2.1/§2.5,选型报告 §8.4):
// pan.quark.cn 前置 ALB 检查 ClientHello 指纹,Go 原生栈指纹有被识别风险,
// 统一走 utls 的 Chrome 指纹 + ALPN 强制 http/1.1,对 *.quark.cn 全域可用。
package protocol

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// 过墙拨号的超时:TCP 连接 10s(spike 同款),TLS 握手 15s(简报指定)。
const (
	wallDialTimeout      = 10 * time.Second
	wallHandshakeTimeout = 15 * time.Second
)

// NewWallClient 返回过 TLS 指纹墙的 HTTP 客户端,Task 5 将其注入 Session:
//
//   - Chrome ClientHello 指纹(经 wallDialTLS 拨号);
//   - ALPN 强制 http/1.1(预设原生协商 h2,服务器会回 HTTP/2 帧,§8.4 注记 3);
//   - 禁用一切代理:Proxy==nil 即不用 ProxyFromEnvironment,
//     HTTP_PROXY/HTTPS_PROXY 环境变量对本客户端无效(代理会改变指纹,§2.3);
//   - 总超时 30s(拨号 10s + 握手 15s 后仍留有余量给 API 往返)。
func NewWallClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// 关键:https 请求的 TLS 拨号全部走 utls Chrome 指纹。
			DialTLSContext: wallDialTLS,
			// 显式置 nil,表明绝不经任何代理直连。
			Proxy: nil,
			// 会话内多次 API 调用复用连接,保活与真实客户端一致。
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 30 * time.Second,
	}
}

// wallDialTLS 完成 TCP + TLS 拨号(逻辑抽自 spike 实测代码 test/spike_test.go):
// UTLSIdToSpec(HelloChrome_Auto) 生成完整 Chrome ClientHello 规格,再把其中
// ALPNExtension 替换为仅 http/1.1——预设自带的 ALPN 扩展会覆盖
// Config.NextProtos,不替换会被服务器协商成 h2(§8.4 注记 1/2)。
// uconn 上的握手受 15s 超时约束。
func wallDialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("过墙拨号地址 %q 缺端口: %w", addr, err)
	}

	dialer := &net.Dialer{Timeout: wallDialTimeout}
	raw, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("生成 Chrome ClientHello 规格: %w", err)
	}
	for i, ext := range spec.Extensions {
		if _, ok := ext.(*utls.ALPNExtension); ok {
			spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}}
			break
		}
	}
	// NextProtos 与替换后的 ALPN 扩展一致,仅当预设未来不再含
	// ALPNExtension 时兜底,不改变现有行为。
	uconn := utls.UClient(raw, &utls.Config{ServerName: host, NextProtos: []string{"http/1.1"}}, utls.HelloCustom)
	if err := uconn.ApplyPreset(&spec); err != nil {
		raw.Close()
		return nil, fmt.Errorf("应用 Chrome 指纹规格: %w", err)
	}

	hsCtx, cancel := context.WithTimeout(ctx, wallHandshakeTimeout)
	defer cancel()
	if err := uconn.HandshakeContext(hsCtx); err != nil {
		_ = uconn.Close() // 会连带关闭底层 raw 连接
		return nil, fmt.Errorf("utls 握手 %s: %w", addr, err)
	}
	return uconn, nil
}
