// wallclient_test.go 过墙客户端的单元测试(不连真实域名):
// 形状测试验证 NewWallClient 的结构约束(自研 Transport、无代理、30s 超时),
// 本地握手测试在 127.0.0.1 上用自签证书的 TLS 服务端验证 wallDialTLS
// 真实发出的 ClientHello 中 ALPN 已强制 http/1.1。
// 真实域名的握手验证在 Task 12 真机阶段执行。
package protocol

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWallClientShape 断言过墙客户端的结构约束(简报 Step 1):
// Transport 非 nil 且为自研 *http.Transport、Proxy==nil(即不用
// ProxyFromEnvironment,环境变量代理不得劫持请求改变 ClientHello 指纹)、
// DialTLSContext 注入 utls 拨号、总超时 30s。
func TestWallClientShape(t *testing.T) {
	client := NewWallClient()
	if client == nil {
		t.Fatal("NewWallClient 返回 nil")
	}
	if client.Transport == nil {
		t.Fatal("Transport 不应为 nil")
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 应为 *http.Transport,实际 %T", client.Transport)
	}
	if tr.Proxy != nil {
		t.Errorf("Proxy 必须为 nil(绝不走 ProxyFromEnvironment,代理会改变 ClientHello 指纹),实际类型 %T", tr.Proxy)
	}
	if tr.DialTLSContext == nil {
		t.Error("DialTLSContext 应注入 utls Chrome 指纹拨号")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 应为 false(ALPN 已强制 http/1.1,不协商 h2)")
	}
	if client.Timeout != 30*time.Second {
		t.Errorf("Timeout 应为 30s,实际 %v", client.Timeout)
	}
}

// TestWallClientDialTLSLocalHandshake 在 127.0.0.1 上用自签证书起一个
// crypto/tls 服务端,验证 wallDialTLS 真实发出的 ClientHello:
//
//   - 服务端在 GetConfigForClient 回调里看到的 ALPN 只含 http/1.1
//     (证明对 spec 中 ALPNExtension 的替换真实生效,§8.4 注记 2);
//   - 客户端收到服务端证书并做校验、因自签而失败——证明 Chrome 预设
//     成功应用,握手推进到了证书校验阶段(若误开 InsecureSkipVerify
//     反而会握手成功,同样能被本测试抓住)。
//
// 全程只连本机回环地址,不触碰任何真实域名。
func TestWallClientDialTLSLocalHandshake(t *testing.T) {
	cert := selfSignCert(t)

	var mu sync.Mutex
	var alpn []string
	serverCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			alpn = hello.SupportedProtos
			mu.Unlock()
			return nil, nil
		},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("起本地 TLS 监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.Handshake()
				}
			}(c)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, dialErr := wallDialTLS(ctx, "tcp", ln.Addr().String())
	if conn != nil {
		conn.Close()
	}
	if dialErr == nil {
		t.Fatal("自签证书理应校验失败,握手却成功了(是否误开了 InsecureSkipVerify?)")
	}
	if !strings.Contains(dialErr.Error(), "certificate") && !strings.Contains(dialErr.Error(), "x509") {
		t.Errorf("握手错误应为证书校验类,实际: %v", dialErr)
	}

	mu.Lock()
	got := alpn
	mu.Unlock()
	if len(got) != 1 || got[0] != "http/1.1" {
		t.Errorf("服务端看到的 ALPN 应只有 [http/1.1],实际 %v(握手错误: %v)", got, dialErr)
	}
}

// selfSignCert 生成一把仅用于本测试的 ECDSA 自签证书。
func selfSignCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成测试私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wallclient-local-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成自签证书失败: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
