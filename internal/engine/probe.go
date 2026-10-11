// 能力探测（§3.2）：对直链发 GET + Range: bytes=0-0，据响应判定
// 是否支持分片以及文件总大小，供 worker 装配做「分片 or 单线程」决策。
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ProbeResult 是一次能力探测（§3.2）的结论。
type ProbeResult struct {
	AcceptsRanges bool  // 服务器支持 Range（探测得到 206）
	Total         int64 // 文件总大小（字节）；Unknown=true 时无意义
	Unknown       bool  // 大小未知（异常形态）：上层应放弃分片，走单线程
}

// Probe 按规格 §3.2 对直链探测：GET + Range: bytes=0-0，三分支——
//
//   - 206 + Content-Range: bytes 0-0/<total> → AcceptsRanges=true, Total=total
//   - 200 + Content-Length: n                → AcceptsRanges=false, Total=n
//   - 其它（意外状态码 / 头缺失或畸形 / 网络错误）→ Unknown=true
//
// err 与 Unknown 同生共死（err 非 nil ⇔ Unknown=true）：调用方可只看
// Unknown 决策单线程，err 仅供日志与重试判断，不是致命失败的信号。
//
// body 处理（§3.2）：206 的 body 恰 1 字节，读完即关；200 立即关闭不读
// ——大小取自 Content-Length，把 4GB 的 body 拉下来当探测就本末倒置了。
func Probe(ctx context.Context, c *http.Client, url, cookieStr string) (p ProbeResult, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return unknownf("构造探测请求失败: %w", err)
	}
	req.Header = DownloadHeaders(cookieStr) // 全套下载头（§3.4）：不带 Cookie 会被防盗链拒
	req.Header.Set("Range", "bytes=0-0")    // 探测专用头，与 worker 按片设置的 Range 无关

	resp, err := c.Do(req)
	if err != nil {
		return unknownf("探测请求失败: %w", err) // 挂连接 / DNS / 超时等
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// body 恰 1 字节，读完排空再交给 defer 关闭（保持连接语义干净）。
		_, _ = io.Copy(io.Discard, resp.Body)
		total, err := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if err != nil {
			return ProbeResult{AcceptsRanges: true, Unknown: true}, err
		}
		return ProbeResult{AcceptsRanges: true, Total: total}, nil
	case http.StatusOK:
		if resp.ContentLength < 0 {
			return unknownf("200 响应无 Content-Length（chunked），大小未知")
		}
		return ProbeResult{AcceptsRanges: false, Total: resp.ContentLength}, nil
	default:
		return unknownf("探测响应状态码 %d, 期望 206 或 200", resp.StatusCode)
	}
}

// parseContentRangeTotal 解析 "bytes 0-0/<total>" 尾部的 total。
// 范围必须恰为 0-0（与请求一致）；尾部为 *（大小未知）或非数字均为错。
func parseContentRangeTotal(cr string) (int64, error) {
	const prefix = "bytes 0-0/"
	if len(cr) < len(prefix) || !strings.EqualFold(cr[:len(prefix)], prefix) {
		return 0, fmt.Errorf("Content-Range %q 不符合 bytes 0-0/<total>", cr)
	}
	tail := cr[len(prefix):]
	if tail == "*" {
		return 0, errors.New("Content-Range 尾部为 *：支持 Range 但总大小未知")
	}
	total, err := strconv.ParseInt(tail, 10, 64)
	if err != nil || total <= 0 {
		return 0, fmt.Errorf("Content-Range %q 的 total %q 不是正整数", cr, tail)
	}
	return total, nil
}

// unknownf 构造「大小未知」结果：Unknown=true 且 err 携带原因。
func unknownf(format string, args ...any) (ProbeResult, error) {
	return ProbeResult{Unknown: true}, fmt.Errorf(format, args...)
}
