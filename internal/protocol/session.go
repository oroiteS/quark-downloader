// session.go 实现夸克 API 会话:DoAPI 是所有业务接口的唯一出站通道,
// 统一节流、完整头模板、退避重试、响应判定与鉴权自愈(规格 §2.3)。
package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"time"
)

// maxRetries 是 408/429/5xx/网络错误的退避重试上限(§2.3):首次请求 + ≤3 次重试。
const maxRetries = 3

// maxBackoffMs 是单次退避时长上限(§2.3):15s。
const maxBackoffMs = 15000

// BackoffDuration 返回第 attempt 次(0 起)重试前的退避时长:
// 600ms × 2.2^attempt × (0.6 + 0.8r),上限 15s(§2.3)。
// r∈[0,1) 由调用方注入,便于测试确定性地断言公式。
func BackoffDuration(attempt int, r float64) time.Duration {
	ms := 600 * math.Pow(2.2, float64(attempt)) * (0.6 + 0.8*r)
	if ms > maxBackoffMs {
		return maxBackoffMs * time.Millisecond
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// authHealBackoff 返回鉴权自愈前的退避时长:随机 2–4s(§2.3)。
func authHealBackoff() time.Duration {
	return time.Duration((2 + 2*rand.Float64()) * float64(time.Second))
}

// throttler 是节流器的最小接口,*Throttler 满足之。
// 独立成接口是为了测试注入 no-op,不改 Throttler 的公开签名。
type throttler interface {
	Wait(ctx context.Context) error
}

// apiEnvelope 是夸克 API 的通用业务响应外壳(§2.3):
// status == 200 && code == 0 为成功,data 为业务负载原文。
type apiEnvelope struct {
	Status  int             `json:"status"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// sessionBase 是换 Cookie 流程(§2.5)涉及的三个域 base。URL 拼接一律走本
// 结构而非硬编码域名,测试把字段指向 httptest 服务即可整体接管域名判断。
type sessionBase struct {
	pan     string // pan.quark.cn:account/info、list(过墙域)
	drivePC string // drive-pc.quark.cn:file/sort
	drive   string // drive.quark.cn:member
}

// defaultSessionBase 返回三个域的生产 base(§2.5 逐字)。
func defaultSessionBase() sessionBase {
	return sessionBase{
		pan:     "https://pan.quark.cn",
		drivePC: "https://drive-pc.quark.cn",
		drive:   "https://drive.quark.cn",
	}
}

// Session 是发往夸克 API 的统一会话:持有 HTTP 客户端、Cookie 整串与节流器。
// DoAPI 是所有业务接口的唯一出站通道,语义见方法注释。
type Session struct {
	client   *http.Client
	jar      *CookieJar
	throttle throttler
	authHeal func(ctx context.Context) bool
	// sleep 等待退避时长且尊重 ctx 取消;测试注入 no-op 加速。
	sleep func(ctx context.Context, d time.Duration) error
	// wall 是过 TLS 指纹墙的客户端(§2.5 步骤 1/2a 的 pan 域专用),
	// 默认 NewWallClient(),测试注入普通 client 指向 fake 服务。
	wall *http.Client
	// base 是换 Cookie 流程的域 base,见 sessionBase;测试可整体覆盖。
	base sessionBase
	// keepalivePeriod 是保活心跳周期(§2.6),默认 5min;测试注入短周期。
	keepalivePeriod time.Duration
	// quickCheckTimeout 是快速校验的单次请求超时(§2.6),默认 10s;测试注入短超时。
	quickCheckTimeout time.Duration
}

// NewSession 创建会话。httpClient 为 nil 时使用禁用系统代理的默认客户端
// (§2.3:代理会劫持请求并改变 ClientHello 指纹);jar 为 nil 时空 Cookie 串。
func NewSession(httpClient *http.Client, jar *CookieJar) *Session {
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		httpClient = &http.Client{Transport: transport}
	}
	if jar == nil {
		jar = NewCookieJar()
	}
	return &Session{
		client:            httpClient,
		jar:               jar,
		throttle:          NewThrottler(),
		sleep:             sleepContext,
		wall:              NewWallClient(),
		base:              defaultSessionBase(),
		keepalivePeriod:   5 * time.Minute,
		quickCheckTimeout: 10 * time.Second,
	}
}

// SetAuthHeal 注册鉴权自愈回调(§2.3):DoAPI 遇 401/412 时退避 2–4s 后
// 调用 fn 做快速登录校验,返回 true(Cookie 仍有效)则重试原请求一次。
// 应在发起请求前设置;与 DoAPI 并发调用不安全。
func (s *Session) SetAuthHeal(fn func(ctx context.Context) bool) {
	s.authHeal = fn
}

// DoAPI 发一次夸克 API 请求并做统一响应判定(§2.3):
// 节流 → 完整头模板构造请求 → 发送 → 合并 Set-Cookie → 按响应分类。
//
//   - 408/429/5xx/网络错误:按 BackoffDuration 指数退避重试,至多 3 次;
//   - 401/412:走鉴权自愈(见 SetAuthHeal),自愈失败报登录失效;
//   - 其余:解析 JSON 外壳,status==200 && code==0 成功返回 data 字段原文
//     (data 缺失时为 nil),否则返回映射后的 *APIError。
//
// ctx 取消会在节流等待、退避睡眠处先行返回 ctx.Err()。
func (s *Session) DoAPI(ctx context.Context, method, url string, body []byte) (json.RawMessage, error) {
	healed := false // 本次调用是否已用过一次鉴权自愈(§2.3 只自愈一次)
	for attempt := 0; ; attempt++ {
		if err := s.throttle.Wait(ctx); err != nil {
			return nil, err
		}

		var reader io.Reader
		if len(body) > 0 {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reader)
		if err != nil {
			// URL/方法非法属调用方错误,不值得重试。
			return nil, fmt.Errorf("构造请求 %s %s: %w", method, url, err)
		}
		req.Header = APIHeaders(s.jar.String(), url)
		if len(body) > 0 {
			// §2.3 头模板未含 Content-Type;§2.8 所有 POST body 均为
			// JSON,有 body 时补 application/json,与真实客户端头一致。
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := s.client.Do(req)
		if err != nil {
			if attempt >= maxRetries {
				return nil, fmt.Errorf("请求 %s %s 失败(已重试 %d 次): %w", method, url, maxRetries, err)
			}
			if serr := s.sleep(ctx, BackoffDuration(attempt, rand.Float64())); serr != nil {
				return nil, serr
			}
			continue
		}

		// 每次响应的 Set-Cookie 增量合并回 Cookie 串(§2.3 规则 2),
		// 先于状态分发——WAF 412 下发的 Cookie 也可能有用。
		s.mergeSetCookies(resp)

		status := resp.StatusCode

		// 鉴权自愈:401/412 → 退避 2–4s → 快速校验 → Cookie 有效则重试原请求一次
		// (偶发风控不该被误判为掉登录)。
		if status == http.StatusUnauthorized || status == http.StatusPreconditionFailed {
			resp.Body.Close()
			if !healed && s.authHeal != nil {
				healed = true
				if serr := s.sleep(ctx, authHealBackoff()); serr != nil {
					return nil, serr
				}
				if s.authHeal(ctx) {
					continue
				}
			}
			return nil, &APIError{
				StatusCode: status,
				Message:    "登录已失效，请重新扫码登录",
			}
		}

		// 可重试:408/429/5xx。
		if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
			resp.Body.Close()
			if attempt >= maxRetries {
				return nil, &APIError{
					StatusCode: status,
					Message:    fmt.Sprintf("服务端错误 HTTP %d(已重试 %d 次)", status, maxRetries),
				}
			}
			if serr := s.sleep(ctx, BackoffDuration(attempt, rand.Float64())); serr != nil {
				return nil, serr
			}
			continue
		}

		// 终态:读 body 做业务判定。
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("读取响应 body: %w", rerr)
		}
		return adjudicate(status, raw)
	}
}

// adjudicate 按通用响应外壳判定(§2.3):status==200 && code==0 成功,
// 返回 data 原文;否则返回映射用户可读信息的 *APIError。
func adjudicate(httpStatus int, raw []byte) (json.RawMessage, error) {
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		snippet := string(raw)
		if len(snippet) > 120 {
			snippet = snippet[:120] + "..."
		}
		return nil, &APIError{
			StatusCode: httpStatus,
			Message:    fmt.Sprintf("响应不是有效 JSON(HTTP %d): %s", httpStatus, snippet),
		}
	}
	if env.Status == 200 && env.Code == 0 {
		return env.Data, nil
	}
	return nil, newAPIError(httpStatus, env.Code, env.Message)
}

// mergeSetCookies 把响应下发的 Set-Cookie 增量合并回 Cookie 串,
// 域名取实际请求的 host(去掉端口)。
func (s *Session) mergeSetCookies(resp *http.Response) {
	values := resp.Header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	domain := ""
	if resp.Request != nil && resp.Request.URL != nil {
		domain = resp.Request.URL.Hostname()
	}
	s.jar.Merge(domain, values)
}

// sleepContext 睡 d,期间 ctx 先行取消则返回 ctx.Err()。
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
