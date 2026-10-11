// authcheck.go 实现登录态校验与保活心跳(规格 §2.6):
// LoginState 以 __puus 判定登录;QuickCheck 用 account/info 快速校验,
// 失败再试 file/sort 业务域兜底,除 401/412 外的一切失败保守视为有效;
// StartKeepalive 每 5min(首次相位随机 50%–100%)心跳一次 file/sort,
// 连续 2 次失败判定登录过期并通知。
package protocol

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"time"
)

// LoginState 报告当前是否处于登录态(§2.6):Cookie 串含 __puus。
// _UP_* 前缀 Cookie 不可作判据(规格明示);登出类 Set-Cookie 把 __puus
// 置空后同样不算,由 CookieJar.Has 的"存在且值非空"语义保证。
func (s *Session) LoginState() bool {
	return s.jar.Has("__puus")
}

// QuickCheck 快速校验登录态(§2.6):短超时(默认 10s,见 quickCheckTimeout)
// GET {pan}/account/info?fr=pc&platform=pc。pan 域前置 TLS 指纹墙,
// 请求经 Session.wall 过墙客户端发出;头模板与 Set-Cookie 合并复用
// exchangeRequest,与换 Cookie 流程同源。
//
//   - HTTP 401/412 → (false, 零值, nil):登录已失效;
//   - 其它失败(网络错误、超时、5xx 等,即拿不到 401/412/200 确定性
//     判定)→ 按规格再用业务客户端(s.client)GET file/sort 兜底
//     (受同一超时 ctx 约束):兜底 401/412 → (false, 零值, nil);
//     其余一切结果(成功或失败)→ (true, 零值, nil) 保守视为仍有效,
//     避免网络抖动误杀登录态(规格明示);
//   - HTTP 200 → (true, 账号信息, nil):昵称/头像取自响应 data。
//
// error 恒为 nil,保留在签名中是为接口稳定。
func (s *Session) QuickCheck(ctx context.Context) (bool, AccountInfo, error) {
	var info AccountInfo
	timeout := s.quickCheckTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	// WithTimeout 取父 ctx 与 timeout 中较早的截止时刻;
	// wall 域主探测与业务域兜底共用,总耗时被截在超时内。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	status, raw, err := s.exchangeRequest(ctx, s.wall,
		s.base.pan+"/account/info?fr=pc&platform=pc", "")
	if err != nil || (status != http.StatusOK &&
		status != http.StatusUnauthorized && status != http.StatusPreconditionFailed) {
		// §2.6:wall 域失败(网络错误或拿不到 401/412/200 判定)
		// 再试 file/sort 业务接口。
		return s.probeFileSort(ctx), info, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusPreconditionFailed {
		return false, info, nil
	}
	// 成功取昵称/头像(§2.6):外壳 data 存在时尽力解析,
	// 形状不符保持零值,不阻断校验结论。
	var env apiEnvelope
	if json.Unmarshal(raw, &env) == nil && len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &info)
	}
	return true, info, nil
}

// probeFileSort 是 §2.6 的业务域兜底:wall 域 account/info 失败后,
// 经业务客户端(drive-pc 域无指纹墙)GET file/sort——URL 与保活心跳
// (keepalivePing)同形态,query 必带 pr=ucpro&fr=pc(§2.8)。
// 兜底 401/412 → false(登录已失效);其余一切结果(成功、网络失败、
// 超时、其它状态码)→ 保守 true。成功也拿不到账号信息(file/sort
// 响应无账号数据),故不返回 info。
func (s *Session) probeFileSort(ctx context.Context) bool {
	status, _, err := s.exchangeRequest(ctx, s.client,
		s.base.drivePC+"/1/clouddrive/file/sort?pr=ucpro&fr=pc&uc_param_str=&pdir_fid=0&_page=1&_size=1", "")
	if err != nil {
		return true // 网络失败:保守视为仍有效
	}
	return status != http.StatusUnauthorized && status != http.StatusPreconditionFailed
}

// StartKeepalive 启动保活心跳 goroutine(§2.6):首次延迟周期×(0.5~1.0)
// ——随机相位,避免固定节奏;之后每周期 GET 一次
// file/sort(形态见 keepalivePing)。连续 2 次失败(单次失败可能只是
// 网络抖动)才判定登录过期:调用 onExpire 一次后心跳停止;
// 任一次成功即清零失败计数。ctx 取消即停;onExpire 在心跳 goroutine
// 中被调用,应为非阻塞回调。周期默认 5min(keepalivePeriod),测试注入
// 短周期;每次调用启动一个独立心跳,调用方负责不重复启动。
func (s *Session) StartKeepalive(ctx context.Context, onExpire func()) {
	if onExpire == nil {
		onExpire = func() {}
	}
	period := s.keepalivePeriod
	if period <= 0 {
		period = 5 * time.Minute
	}
	go func() {
		// 首次相位随机 50%–100%(§2.6)。
		first := time.Duration(float64(period) * (0.5 + 0.5*rand.Float64()))
		timer := time.NewTimer(first)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		ticker := time.NewTicker(period)
		defer ticker.Stop()
		const maxStrikes = 2 // 连续 2 次失败才判定过期(§2.6)
		strikes := 0
		for {
			if s.keepalivePing(ctx) {
				strikes = 0
			} else {
				// ctx 已取消时不计失败:取消后的 ping 必然失败(请求
				// 被 context 打断),若记作第二振会把主动停止心跳伪装成
				// 登录过期、误触 onExpire。先查取消再计数。
				if ctx.Err() != nil {
					return
				}
				strikes++
				if strikes >= maxStrikes {
					onExpire()
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// keepalivePing 发一次保活心跳(§2.6):经业务客户端(drive-pc 域无指纹墙)
// GET file/sort(pdir_fid=0&_page=1&_size=1,query 必带 pr=ucpro&fr=pc,
// §2.8),HTTP 200 即成功;Set-Cookie 由 exchangeRequest 顺带合并回整串
// ——心跳同时是 Cookie 保鲜手段。
func (s *Session) keepalivePing(ctx context.Context) bool {
	status, _, err := s.exchangeRequest(ctx, s.client,
		s.base.drivePC+"/1/clouddrive/file/sort?pr=ucpro&fr=pc&uc_param_str=&pdir_fid=0&_page=1&_size=1", "")
	return err == nil && status == http.StatusOK
}
