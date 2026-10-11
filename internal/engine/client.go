// 引擎 HTTP 客户端：连接池、每次拨号刷新 DNS、禁用代理，逐字出自规格 §3.4。
package engine

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
)

// hostResolver 抽象 DNS 解析，便于测试注入计数桩（*net.Resolver 天然满足）。
type hostResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// engineTransport 在标准 Transport 上挂一个未导出可注入的 resolver 字段。
// 生产路径用 net.DefaultResolver；测试替换成桩以验证「每次拨号都重新解析」。
type engineTransport struct {
	*http.Transport
	resolver hostResolver
}

// NewEngineClient 构造下载引擎的 HTTP 客户端（§3.4）：
//
//   - 连接池上限 = max(1024, 2×maxConns)：几百路并发 + 片间换连接的余量；
//   - 每次新建连接都重新 LookupHost——不缓存解析。夸克 CDN 一域多 IP，
//     这是慢连接被抢占后重连能换到别的边缘节点的前提（§3.4/§3.7 协同）；
//   - Proxy=nil：禁用系统代理。
func NewEngineClient(maxConns int) *http.Client {
	return newClient(maxConns, net.DefaultResolver)
}

func newClient(maxConns int, r hostResolver) *http.Client {
	pool := max(1024, 2*maxConns)
	et := &engineTransport{resolver: r}
	et.Transport = &http.Transport{
		Proxy:               nil, // 显式禁用系统代理（§3.4）
		DialContext:         et.dialFreshDNS,
		MaxIdleConns:        pool, // 池总上限同步抬到同值，否则默认 100 会把空闲连接提前关掉
		MaxIdleConnsPerHost: pool,
		MaxConnsPerHost:     pool,
	}
	return &http.Client{Transport: et}
}

// dialFreshDNS 是 Transport 的拨号函数：每次拨号都重新解析域名，绝不复用上次结果。
// 解析后自行选定 IP 直拨（替代标准库的 happy-eyeballs），把节点选择权留在引擎手里。
func (t *engineTransport) dialFreshDNS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := t.resolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("feiwu: LookupHost 对 " + host + " 返回空结果")
	}
	d := &net.Dialer{}
	return d.DialContext(ctx, network, net.JoinHostPort(pickIP(ips), port))
}

// pickIP 从解析结果挑一个目标 IP：先滤出 IPv4（避免在无 IPv6 的网络环境随机
// 拨到坏地址），再随机——一域多 IP 时让连接在边缘节点间散开，抢占重连才有
// 机会换节点（§3.7「若所有连接都撞同一个 IP，抢占重连还是同节点」）。
func pickIP(ips []string) string {
	v4 := make([]string, 0, len(ips))
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			v4 = append(v4, s)
		}
	}
	if len(v4) == 0 {
		v4 = ips // 只有 IPv6 时照用全部
	}
	return v4[rand.IntN(len(v4))]
}
