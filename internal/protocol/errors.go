// errors.go 定义夸克 API 调用的统一错误类型与用户可读信息映射(规格 §2.3)。
package protocol

import (
	"fmt"
	"strings"
)

// capacityKeywords 是"网盘容量不足"的消息特征(§2.3),小写匹配。
var capacityKeywords = []string{
	"容量",
	"capacity",
	"storage is full",
	"not enough space",
	"space limit",
}

// APIError 表示一次失败的夸克 API 调用。StatusCode 为 HTTP 状态码,
// Code 为业务响应 JSON 的 code 字段(无业务码时为 0),
// Message 为已映射的用户可读信息。
type APIError struct {
	StatusCode int
	Code       int
	Message    string
}

// Error 返回用户可读信息;状态码与业务码保留在字段上供程序判定。
func (e *APIError) Error() string {
	return e.Message
}

// newAPIError 构造已应用 §2.3 用户可读映射的错误。
func newAPIError(status, code int, message string) *APIError {
	return &APIError{
		StatusCode: status,
		Code:       code,
		Message:    mapQuarkMessage(status, code, message),
	}
}

// mapQuarkMessage 把服务端原始 message 映射为用户可读信息(§2.3):
//   - 含 "user NOT real name" → 实名提示
//   - 含容量关键词,或 code ∈ {23017, 23018} → 容量提示
//   - 其余透传原文;原文为空时给出带状态码的兜底文案
func mapQuarkMessage(status, code int, message string) string {
	if strings.Contains(strings.ToLower(message), "user not real name") {
		return "账号未实名，无法分享/转存"
	}
	lower := strings.ToLower(message)
	for _, kw := range capacityKeywords {
		if strings.Contains(lower, kw) {
			return "网盘容量不足"
		}
	}
	if code == 23017 || code == 23018 {
		return "网盘容量不足"
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Sprintf("请求失败(HTTP %d, 业务码 %d)", status, code)
	}
	return message
}
