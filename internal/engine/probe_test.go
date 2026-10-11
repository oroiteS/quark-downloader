package engine

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

const probeCookie = "__pus=uid123; _puus=token456"

// newProbeClient 走引擎客户端路径（fresh-DNS Transport），探测与下载同栈。
func newProbeClient() *http.Client {
	return newClient(4, net.DefaultResolver)
}

// reqInfo 通过 channel 传回（handler goroutine → 测试 goroutine 需要 happens-before）。
type reqInfo struct {
	rangeH, cookie, ua string
}

// 形态一：真 Range 服务器——收到 bytes=0-0 后回 206 + Content-Range，body 恰 1 字节。
func TestProbeServerSupportsRange(t *testing.T) {
	const total = int64(123456789)
	reqs := make(chan reqInfo, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs <- reqInfo{r.Header.Get("Range"), r.Header.Get("Cookie"), r.Header.Get("User-Agent")}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", total))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("x")) // 206 的 body 恰 1 字节
	}))
	defer srv.Close()

	p, err := Probe(context.Background(), newProbeClient(), srv.URL, probeCookie)
	if err != nil {
		t.Fatalf("支持 Range 的服务器不应报错: %v", err)
	}
	if !p.AcceptsRanges {
		t.Error("AcceptsRanges 应为 true（服务器回了 206）")
	}
	if p.Unknown {
		t.Error("Unknown 应为 false")
	}
	if p.Total != total {
		t.Errorf("Total = %d, 期望 %d（取自 Content-Range 尾部）", p.Total, total)
	}

	select {
	case ri := <-reqs:
		if ri.rangeH != "bytes=0-0" {
			t.Errorf("请求 Range 头 = %q, 期望 %q", ri.rangeH, "bytes=0-0")
		}
		if ri.cookie != probeCookie {
			t.Errorf("请求 Cookie = %q, 期望原样 %q（防盗链要求带 Cookie）", ri.cookie, probeCookie)
		}
		if ri.ua == "" {
			t.Error("请求缺少 User-Agent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务器未收到探测请求")
	}
}

// 形态二：忽略 Range 的服务器——回 200 + Content-Length。规格要求 200 时
// 立即关闭不读 body（大小从 Content-Length 拿）：body 给到远超任何 TCP 缓冲的
// 32MB，若实现读完了 body，handler 的 Write 会全部成功并置 done。
func TestProbeServerIgnoresRange(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 32<<20)
	var handlerDone atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if _, err := w.Write(body); err != nil {
			return // client 提前断开：证明没有读完 body
		}
		handlerDone.Store(true)
	}))
	defer srv.Close()

	p, err := Probe(context.Background(), newProbeClient(), srv.URL, probeCookie)
	if err != nil {
		t.Fatalf("不支持 Range 的服务器是合法形态，不应报错: %v", err)
	}
	if p.AcceptsRanges {
		t.Error("AcceptsRanges 应为 false（服务器回了 200）")
	}
	if p.Unknown {
		t.Error("Unknown 应为 false")
	}
	if p.Total != int64(len(body)) {
		t.Errorf("Total = %d, 期望 %d（取自 Content-Length）", p.Total, len(body))
	}

	time.Sleep(150 * time.Millisecond) // 留出「读完 32MB」的时间窗口
	if handlerDone.Load() {
		t.Error("Probe 似乎读完了 200 响应的整个 body——规格要求 200 立即关闭不读")
	}
}

// 形态三：挂连接——接受后立刻关闭，探测得到的是网络错误 → 大小未知。
func TestProbeConnectionDies(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // 接了就挂
		}
	}()

	p, err := Probe(context.Background(), newProbeClient(), "http://"+ln.Addr().String()+"/file", probeCookie)
	if !p.Unknown {
		t.Errorf("挂连接应 Unknown=true, got %+v", p)
	}
	if p.AcceptsRanges {
		t.Error("挂连接不应判定为支持 Range")
	}
	if err == nil {
		t.Error("挂连接应返回非 nil err（供上层日志/重试决策）")
	}
}

// 异常形态：403（防盗链拒等）→ 大小未知。
func TestProbeUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	p, err := Probe(context.Background(), newProbeClient(), srv.URL, probeCookie)
	if !p.Unknown {
		t.Errorf("403 应 Unknown=true, got %+v", p)
	}
	if err == nil {
		t.Error("403 应返回非 nil err")
	}
}

// 异常形态：206 但 Content-Range 畸形（范围不匹配 / 尾部非数字 / 尾部 *）。
func TestProbeMalformedContentRange(t *testing.T) {
	cases := []string{
		"bytes 5-10/99",     // 范围不是 0-0
		"bytes 0-0/notanum", // 尾部非数字
		"bytes 0-0/*",       // 支持 Range 但总大小未知
		"",                  // 头缺失
		"garbage",           // 完全畸形
	}
	for _, cr := range cases {
		cr := cr
		t.Run("Content-Range="+strconv.Quote(cr), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if cr != "" {
					w.Header().Set("Content-Range", cr)
				}
				w.WriteHeader(http.StatusPartialContent)
				w.Write([]byte("x"))
			}))
			defer srv.Close()

			p, err := Probe(context.Background(), newProbeClient(), srv.URL, probeCookie)
			if !p.Unknown {
				t.Errorf("畸形 Content-Range %q 应 Unknown=true, got %+v", cr, p)
			}
			if err == nil {
				t.Errorf("畸形 Content-Range %q 应返回非 nil err", cr)
			}
		})
	}
}

// 异常形态：200 但无 Content-Length（chunked 流）→ 大小未知。
func TestProbeChunked200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		fmt.Fprint(w, "chunk-one")
		f.Flush() // 不设 Content-Length → chunked → client 侧 ContentLength = -1
	}))
	defer srv.Close()

	p, err := Probe(context.Background(), newProbeClient(), srv.URL, probeCookie)
	if !p.Unknown {
		t.Errorf("chunked 200 应 Unknown=true, got %+v", p)
	}
	if err == nil {
		t.Error("chunked 200 应返回非 nil err")
	}
}
