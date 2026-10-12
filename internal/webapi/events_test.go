package webapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestHubBroadcastDelivers 基线语义：Subscribe 后 Broadcast 的事件可被该
// 订阅者收到（多订阅者各自独立收到）。
func TestHubBroadcastDelivers(t *testing.T) {
	h := NewHub()
	ch1, cancel1 := h.Subscribe()
	defer cancel1()
	ch2, cancel2 := h.Subscribe()
	defer cancel2()

	h.Broadcast(Event{Type: "auth"})

	for name, ch := range map[string]<-chan Event{"订阅者1": ch1, "订阅者2": ch2} {
		select {
		case e := <-ch:
			if e.Type != "auth" {
				t.Fatalf("%s 收到 Type=%q, 期望 auth", name, e.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s 未收到广播事件", name)
		}
	}
}

// TestHubSlowSubscriberDoesNotBlock 慢消费者取舍（简报逐字语义）：订阅者
// 不读、chan 打满后，Broadcast 绝不阻塞——满则丢弃该订阅者本帧，保住
// 全局广播。快订阅者不受慢订阅者影响，照常收到事件。配合 -race。
func TestHubSlowSubscriberDoesNotBlock(t *testing.T) {
	h := NewHub()
	slowCh, cancelSlow := h.Subscribe()
	defer cancelSlow()
	fastCh, cancelFast := h.Subscribe()
	defer cancelFast()

	const frames = 200 // 远超订阅者缓冲深度
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < frames; i++ {
			h.Broadcast(Event{Type: "task_progress", TaskID: "t1"})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("慢订阅者阻塞了 Broadcast（广播不应等待消费者）")
	}

	// 快订阅者应能收到事件（慢者的存在不影响快者的投递）。
	select {
	case <-fastCh:
	default:
		t.Fatal("快订阅者未收到事件（被慢订阅者拖累）")
	}

	// 慢订阅者丢弃了部分帧（收到数 < 发送数），且读光后不再有数据。
	n := 0
	for {
		select {
		case <-slowCh:
			n++
		default:
			goto drained
		}
	}
drained:
	if n >= frames {
		t.Fatalf("慢订阅者收到全部 %d 帧——与「chan 满丢帧」语义矛盾", frames)
	}
}

// TestHubCancelIdempotent cancel 幂等：重复调用不 panic；cancel 后
// Broadcast 不投递（也不向已关闭 channel 发送）。
func TestHubCancelIdempotent(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe()
	cancel()
	cancel() // 第二次必须幂等

	h.Broadcast(Event{Type: "auth"}) // 不应 panic
	select {
	case e, ok := <-ch:
		if ok && e.Type != "" {
			t.Fatal("cancel 后仍投递了事件")
		}
	default:
		// channel 已关闭且无残留事件：可接受。
	}
}

// TestHubCloseClosesAll Hub.Close（App.Close/服务停机链使用）：关闭全部
// 订阅者 channel、幂等，Close 后 Broadcast 安全。
func TestHubCloseClosesAll(t *testing.T) {
	h := NewHub()
	ch1, cancel1 := h.Subscribe()
	defer cancel1()
	ch2, cancel2 := h.Subscribe()
	defer cancel2()

	h.Close()
	h.Close() // 幂等

	for name, ch := range map[string]<-chan Event{"ch1": ch1, "ch2": ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("%s 在 Close 后仍可读出数据", name)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s 在 Close 后未关闭", name)
		}
	}
	h.Broadcast(Event{Type: "auth"}) // 不应 panic
	if got := h.subscriberCount(); got != 0 {
		t.Fatalf("Close 后订阅表非空: %d", got)
	}
}

// TestSSEStream SSE 端点全链路（httptest 真连接读流）：
//  1. Content-Type text/event-stream；
//  2. 心跳注释行（间隔注入为短值）先行到达——同时证明 handler 已订阅；
//  3. Broadcast 后读到 data: {json} 事件帧；
//  4. 客户端断开（关 body）→ hub 退订无泄漏，随后 Broadcast 安全。
func TestSSEStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	origHeartbeat := sseHeartbeat
	sseHeartbeat = 25 * time.Millisecond // 注入短心跳：先于心跳证明订阅已建立
	defer func() { sseHeartbeat = origHeartbeat }()

	app := New(Deps{})
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	ctx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q, 期望 text/event-stream", ct)
	}

	// 读 goroutine + select 总超时：任何一段 3 秒无数据都算失败（不依赖
	// resp.Body 内部类型实现 SetReadDeadline）。
	lines := make(chan string, 64)
	readErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				readErr <- err
				return
			}
			lines <- strings.TrimRight(line, "\n")
		}
	}()
	readLine := func() string {
		t.Helper()
		select {
		case line := <-lines:
			return line
		case err := <-readErr:
			t.Fatalf("读 SSE 流: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("读 SSE 流超时（3 秒无数据）")
		}
		return "" // 不可达（t.Fatal 即退出）
	}

	// 1) 等心跳注释行：handler 已进入收发循环（订阅必然完成）。
	sawHeartbeat := false
	for i := 0; i < 20 && !sawHeartbeat; i++ {
		if line := readLine(); strings.HasPrefix(line, ":") {
			sawHeartbeat = true
		}
	}
	if !sawHeartbeat {
		t.Fatal("3 秒内未读到心跳注释行")
	}

	// 2) 广播 → 读到 data: {json} 帧（与 Event JSON 序列化逐字节一致）。
	want, err := json.Marshal(Event{Type: "auth", Payload: json.RawMessage(`{"ok":true}`)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	app.hub.Broadcast(Event{Type: "auth", Payload: json.RawMessage(`{"ok":true}`)})
	sawData := false
	for i := 0; i < 20 && !sawData; i++ {
		line := readLine()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if got := strings.TrimPrefix(line, "data: "); got != string(want) {
			t.Fatalf("事件帧 = %q, 期望 %q", got, want)
		}
		sawData = true
	}
	if !sawData {
		t.Fatal("未读到 data: 事件帧")
	}

	// 3) 客户端断开 → 退订（轮询订阅表归零），随后 Broadcast 安全。
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for app.hub.subscriberCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := app.hub.subscriberCount(); got != 0 {
		t.Fatalf("客户端断开后未退订（订阅表残留 %d 条——泄漏）", got)
	}
	app.hub.Broadcast(Event{Type: "auth"}) // 退订后广播不应 panic
}
