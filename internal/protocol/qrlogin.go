// qrlogin.go 实现扫码登录协议状态机(规格 §2.4):
// 取二维码 token → 轮询 service_ticket → 交由换 Cookie 流程(§2.5,Task 5)。
// uop 域响应外壳是 status==2000000,与通用 API 外壳(status==200&&code==0,
// §2.3)不同,故不走 DoAPI 的通用判定,改用 Session 的普通客户端
// (uop 域不过 TLS 指纹墙,§2.1)+ 完整头模板自解析外壳。
package protocol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// uopBaseURL 是 CAS 扫码登录域(§2.2)。包级变量便于测试注入 fake 服务,
// 不改 Session 公开接口。
var uopBaseURL = "https://uop.quark.cn"

const (
	// qrClientID 是扫码登录 CAS 的 client_id(§2.2 逐字):532。
	qrClientID = "532"
	// qrLoginTimeout 是扫码登录总时长上限(§2.4):300s。
	qrLoginTimeout = 300 * time.Second
	// qrScanURLTemplate 是 §2.2 扫码页 URL 模板(逐字),
	// token 与 client_id 外的参数全部原样保留。
	qrScanURLTemplate = "https://su.quark.cn/4_eMHBJ?token=%s&client_id=%s" +
		"&ssb=weblogin&uc_param_str=" +
		"&uc_biz_str=S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0"
)

// QRState 是扫码登录状态机的状态(§2.4 状态表)。
type QRState int

const (
	// QRWaiting 等待扫码(50004001);单次轮询网络失败也归入此态(静默跳过本轮)。
	QRWaiting QRState = iota
	// QRScanned 已扫码,等手机确认(其它非空 data、无 ticket)。
	QRScanned
	// QRGotTicket 已拿到 service_ticket,可进入换 Cookie(§2.5)。
	QRGotTicket
	// QRInvalid 二维码已失效(50004002),需刷新二维码。
	QRInvalid
	// QRExpired 二维码已过期(50004003)。
	QRExpired
	// QRTimeout 总时长 ≥300s。
	QRTimeout
)

// uopEnvelope 是 uop 域响应外壳(§2.4):status==2000000 为成功,
// 其余值(50004001/50004002/50004003/…)按状态机分派。
type uopEnvelope struct {
	Status  int             `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// uopMembersData 是 data 字段的形状:members 下再挂具体字段,
// 取 token 时为 token,轮询时为 service_ticket。
type uopMembersData struct {
	Members struct {
		Token         string `json:"token"`
		ServiceTicket string `json:"service_ticket"`
	} `json:"members"`
}

// RandomRequestID 返回 32 位随机 hex(CAS 请求的 request_id,§2.4),
// 每次 GET 重新生成。
func RandomRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 读系统熵源失败属运行环境级异常,无法给出退化值。
		panic("protocol: crypto/rand 不可用: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// QRLogin 是一次扫码登录会话:持有二维码 token 与开始时刻,
// Poll 按轮询推进状态机。非并发安全,由调用方串行轮询。
type QRLogin struct {
	sess  *Session
	token string
	start time.Time
}

// StartQRLogin 取二维码 token(§2.4 第 1 步):
// GET /cas/ajax/getTokenForQrcodeLogin,成功外壳 status==2000000,
// token 取自 data.members.token。
func (s *Session) StartQRLogin(ctx context.Context) (*QRLogin, error) {
	url := fmt.Sprintf("%s/cas/ajax/getTokenForQrcodeLogin?client_id=%s&v=1.2&request_id=%s",
		uopBaseURL, qrClientID, RandomRequestID())

	body, err := s.uopGet(ctx, url)
	if err != nil {
		return nil, err
	}
	var env uopEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("解析取 token 响应: %w", err)
	}
	if env.Status != 2000000 {
		return nil, fmt.Errorf("获取扫码 token 失败(status=%d): %s", env.Status, env.Message)
	}
	var m uopMembersData
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &m); err != nil {
			return nil, fmt.Errorf("解析取 token 响应 data: %w", err)
		}
	}
	// 外壳成功但缺 token 属协议异常,如实报错而非静默重试。
	if m.Members.Token == "" {
		return nil, fmt.Errorf("取 token 响应缺少 data.members.token: %s", string(env.Data))
	}
	return &QRLogin{sess: s, token: m.Members.Token, start: time.Now()}, nil
}

// Poll 执行一轮轮询(§2.4 第 2 步),返回 (状态, service_ticket, err)。
// 状态机:
//
//	ticket 非空        → QRGotTicket(携带 ticket)
//	status==50004001   → QRWaiting
//	status==50004002   → QRInvalid(终态,刷新二维码)
//	status==50004003   → QRExpired(终态)
//	其它非空 data 无 ticket → QRScanned
//	其余(空 data 等)   → QRWaiting 兜底
//	总时长 ≥300s       → QRTimeout(先于发请求判定)
//
// 单次轮询的网络失败、HTTP 非 200、坏 JSON 一律静默跳过本轮:
// 返回 (QRWaiting, "", nil),不算错误(§2.4"静默跳过本轮继续");
// 唯一例外是 ctx 已取消——返回 ctx 错误,不掩盖取消信号。
// 轮询节奏(1.2s/次)由调用方控制。
func (l *QRLogin) Poll(ctx context.Context) (QRState, string, error) {
	if time.Since(l.start) >= qrLoginTimeout {
		return QRTimeout, "", nil
	}
	url := fmt.Sprintf("%s/cas/ajax/getServiceTicketByQrcodeToken?client_id=%s&v=1.2&token=%s&request_id=%s",
		uopBaseURL, qrClientID, l.token, RandomRequestID())

	body, err := l.sess.uopGet(ctx, url)
	if err != nil {
		if ctx.Err() != nil {
			return QRWaiting, "", ctx.Err()
		}
		return QRWaiting, "", nil
	}
	var env uopEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return QRWaiting, "", nil
	}
	var m uopMembersData
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &m) // 解析失败视作无 ticket,由状态机兜底
	}
	switch {
	case m.Members.ServiceTicket != "":
		return QRGotTicket, m.Members.ServiceTicket, nil
	case env.Status == 50004001:
		return QRWaiting, "", nil
	case env.Status == 50004002:
		return QRInvalid, "", nil
	case env.Status == 50004003:
		return QRExpired, "", nil
	case dataNonEmpty(env.Data):
		return QRScanned, "", nil
	default:
		return QRWaiting, "", nil
	}
}

// ScanURL 返回 §2.2 扫码页 URL(逐字模板),供渲染成二维码或直接打开。
func (l *QRLogin) ScanURL() string {
	return fmt.Sprintf(qrScanURLTemplate, l.token, qrClientID)
}

// uopGet 发一个 uop 域 GET:节流(§2.3 任意 API 请求共用)→ 完整头模板 →
// 普通客户端直连(该域无 TLS 指纹墙)→ 合并 Set-Cookie 增量 → 读 body。
// 状态判定交还调用方(uop 外壳与通用外壳不同,见文件头注)。
func (s *Session) uopGet(ctx context.Context, url string) ([]byte, error) {
	if err := s.throttle.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求 %s: %w", url, err)
	}
	req.Header = APIHeaders(s.jar.String(), url)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	// §2.3 规则 2:uop 域响应的 Set-Cookie 增量(如 _UP_* 系列)也合并回整串。
	s.mergeSetCookies(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("uop 响应 HTTP %d: %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// dataNonEmpty 报告 uop data 字段是否非空:
// 缺失、null、空对象 {} 均视为空。
func dataNonEmpty(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null" && t != "{}"
}
