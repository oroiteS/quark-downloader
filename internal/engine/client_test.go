package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"quark-downloader/internal/protocol"
)

// 形状测试：Transport 各项配置逐项断言（§3.4）。
// 连接池上限 = max(1024, 2×maxConns)，MaxIdleConnsPerHost 与 MaxConnsPerHost 同值。
func TestEngineClientShape(t *testing.T) {
	tests := []struct {
		maxConns int
		want     int
	}{
		{0, 1024},   // max(1024, 0)
		{16, 1024},  // 2×16=32 < 1024，取下限
		{512, 1024}, // 2×512 恰等于 1024
		{600, 1200}, // 2×600=1200 首次超过 1024
		{1024, 2048},
	}
	for _, tt := range tests {
		c := NewEngineClient(tt.maxConns)
		if c == nil {
			t.Fatalf("maxConns=%d: NewEngineClient 返回 nil", tt.maxConns)
		}
		et, ok := c.Transport.(*engineTransport)
		if !ok {
			t.Fatalf("maxConns=%d: Transport 类型 %T, 应为 *engineTransport", tt.maxConns, c.Transport)
		}
		if et.Transport == nil {
			t.Fatalf("maxConns=%d: 内嵌 *http.Transport 为 nil", tt.maxConns)
		}
		tr := et.Transport
		if tr.Proxy != nil {
			t.Errorf("maxConns=%d: Proxy 应为 nil（§3.4 禁用系统代理）", tt.maxConns)
		}
		if tr.DialContext == nil {
			t.Errorf("maxConns=%d: DialContext 为 nil, 应为自定义 DNS 刷新拨号", tt.maxConns)
		}
		if got := tr.MaxConnsPerHost; got != tt.want {
			t.Errorf("maxConns=%d: MaxConnsPerHost=%d, 期望 %d", tt.maxConns, got, tt.want)
		}
		if got := tr.MaxIdleConnsPerHost; got != tt.want {
			t.Errorf("maxConns=%d: MaxIdleConnsPerHost=%d, 期望 %d", tt.maxConns, got, tt.want)
		}
		if got := tr.MaxIdleConns; got < tt.want {
			t.Errorf("maxConns=%d: MaxIdleConns=%d < %d（池总上限低于分主机上限会让空闲连接被提前关掉）",
				tt.maxConns, got, tt.want)
		}
	}
}

// countingResolver 是记录 LookupHost 调用次数的桩（未导出，仅包内测试用）。
type countingResolver struct {
	calls atomic.Int64
	addrs []string // 为空时原样返回查询名
}

func (r *countingResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	r.calls.Add(1)
	if r.addrs != nil {
		return r.addrs, nil
	}
	return []string{host}, nil
}

// 每次新建连接都必须重新解析 DNS（§3.4）：同一地址多次独立拨号，解析次数 ≥ 连接数。
// 不缓存解析是慢连接抢占能换到节点的前提（夸克 CDN 一域多 IP）。
func TestEngineClientDialResolvesEveryConn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	cr := &countingResolver{addrs: []string{"127.0.0.1"}}
	c := newClient(4, cr)

	const conns = 3
	for i := 0; i < conns; i++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Close = true // 每请求关连接 → 强制下一次重新拨号
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("第 %d 个请求失败: %v", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "ok" {
			t.Fatalf("第 %d 个响应体 %q", i+1, body)
		}
	}
	if got := cr.calls.Load(); got < conns {
		t.Errorf("LookupHost 调用 %d 次 < 连接数 %d：拨号未做到每次重新解析", got, conns)
	}
}

// 端到端：NewEngineClient（默认 net.DefaultResolver 路径）+ DownloadHeaders
// 打到本地回环服务，验证拨号链路与头的透传。
func TestEngineClientRoundTrip(t *testing.T) {
	type seen struct{ ua, cookie, site, accept string }
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{
			r.Header.Get("User-Agent"), r.Header.Get("Cookie"),
			r.Header.Get("Sec-Fetch-Site"), r.Header.Get("Accept"),
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := NewEngineClient(8)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = DownloadHeaders("__puus=s1; _puus=s2")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("status=%d body=%q, 期望 200/ok", resp.StatusCode, body)
	}

	s := <-got
	if s.ua != protocol.UserAgent {
		t.Errorf("服务端收到 UA=%q, 期望 %q", s.ua, protocol.UserAgent)
	}
	if s.cookie != "__puus=s1; _puus=s2" {
		t.Errorf("服务端收到 Cookie=%q, 期望原样整串", s.cookie)
	}
	if s.site != "cross-site" {
		t.Errorf("服务端收到 Sec-Fetch-Site=%q, 期望 cross-site", s.site)
	}
	if s.accept != "*/*" {
		t.Errorf("服务端收到 Accept=%q, 期望 */*", s.accept)
	}
}
