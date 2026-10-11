//go:build manual

// manual_test.go 是真机手动集成验证用例(Task 12):真实走一遍
// 扫码登录 → 换 Cookie → 快速校验 → 分享解析 → 列表 → 直链 → 导出
// 的全链路。默认不参与编译(//go:build manual),编译进来后还要求
// 环境变量 FEIWU_MANUAL=1 才真正执行——build tag + 运行时环境变量
// 双保险,防止误触真机流程(少一个都会 t.Skip)。
//
// 运行方式(需真机手机扫码,由控制器与用户安排,CI 永不运行):
//
//	FEIWU_MANUAL=1 go test -tags manual ./internal/protocol/ \
//	  -run TestManualLoginAndParse -v -count=1
//
// 可选环境变量:
//
//	FEIWU_SHARE_TEXT  真实分享文案(纯链接/整段文案);为空则登录链路
//	                  走完后跳过解析部分
//
// 注意:协议握手必须直连(规格 §2.3),NewSession/NewWallClient 均已
// 禁用代理;运行前请勿给本进程设置 HTTP(S)_PROXY 之外的系统级劫持。
package protocol

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	// manualPollInterval 是轮询节奏(规格 §2.4):1.2s/次。
	manualPollInterval = 1200 * time.Millisecond
	// manualLoginBudget 是扫码登录总时长上限(规格 §2.4):300s。
	manualLoginBudget = 300 * time.Second
	// manualQRSize 是登录二维码 PNG 边长(像素),足够手机摄像头识别。
	manualQRSize = 480
	// manualShowEntries 是分享列表最多打印的条数。
	manualShowEntries = 5
	// manualLinkFiles 是参与取直链的文件数上限(三入口 × 节流,保持请求量克制)。
	manualLinkFiles = 3
)

// TestManualLoginAndParse 覆盖 Task 3–11 全部接口的真机链路:
// StartQRLogin → QRPNG 渲染扫码 → Poll 轮询到 GotTicket →
// ExchangeCookie(断言 __puus 登录态)→ QuickCheck →
// FEIWU_SHARE_TEXT 提供时:ParseShareText → ShareToken → ShareList →
// DownloadLinks(打印打分最高直链)→ ExportLinks(motrix)。
func TestManualLoginAndParse(t *testing.T) {
	// 运行门槛第二重:build tag 之外再查环境变量,防止
	// `go test -tags manual ./...` 这类批量命令误触真机流程。
	if os.Getenv("FEIWU_MANUAL") != "1" {
		t.Skip("未设置 FEIWU_MANUAL=1，跳过真机手动用例（双保险：还需 -tags manual 编译）")
	}

	ctx := context.Background()

	// 1. 会话与二维码 token:NewSession(nil,nil) 即默认业务客户端
	//    (禁系统代理)+ 默认过墙客户端(utls Chrome 指纹)。
	sess := NewSession(nil, nil)
	qr, err := sess.StartQRLogin(ctx)
	if err != nil {
		t.Fatalf("StartQRLogin 失败: %v", err)
	}

	// 2. 二维码 PNG 落盘 + 终端打印两种扫码途径。
	pngPath := filepath.Join(os.TempDir(), "feiwu-login-qr.png")
	png, err := QRPNG(qr.ScanURL(), manualQRSize)
	if err != nil {
		t.Fatalf("生成二维码 PNG 失败: %v", err)
	}
	if err := os.WriteFile(pngPath, png, 0o644); err != nil {
		t.Fatalf("写二维码 PNG %s 失败: %v", pngPath, err)
	}
	t.Logf("请用夸克 APP 扫码登录(二选一):\n  1. 打开二维码图片: %s\n  2. 浏览器访问: %s",
		pngPath, qr.ScanURL())

	// 3. 轮询直到终态:1.2s/次,300s 上限;状态变化才打印,不刷屏。
	var ticket string
	last := QRState(-1) // 哨兵:保证首个状态必打印
	deadline := time.Now().Add(manualLoginBudget)
	for ticket == "" {
		st, tk, err := qr.Poll(ctx)
		if err != nil {
			t.Fatalf("轮询出错: %v", err)
		}
		if st != last {
			t.Logf("扫码状态: %s", manualStateName(st))
			last = st
		}
		switch st {
		case QRGotTicket:
			ticket = tk
			continue // 拿到 ticket,不再 sleep
		case QRInvalid, QRExpired, QRTimeout:
			t.Fatalf("扫码登录未完成(状态 %s),请重跑用例获取新二维码", manualStateName(st))
		}
		if time.Now().After(deadline) {
			t.Fatalf("扫码登录超过 %v 未完成", manualLoginBudget)
		}
		time.Sleep(manualPollInterval)
	}

	// 4. service_ticket 换登录 Cookie;LoginState 即 __puus 判据(§2.6)。
	info, err := sess.ExchangeCookie(ctx, ticket)
	if err != nil {
		t.Fatalf("ExchangeCookie 失败: %v", err)
	}
	if !sess.LoginState() {
		t.Fatal("ExchangeCookie 后 LoginState 仍为 false（Cookie 串缺少非空 __puus）")
	}
	t.Logf("登录成功，昵称: %q", info.Nickname)

	// 5. 快速校验登录态。
	ok, _, err := sess.QuickCheck(ctx)
	if err != nil {
		t.Fatalf("QuickCheck 出错: %v", err)
	}
	if !ok {
		t.Fatal("QuickCheck 判定登录态无效")
	}
	t.Log("QuickCheck 通过")

	// 6. 分享解析链路:文案为空则跳过(登录部分已全部通过)。
	shareText := os.Getenv("FEIWU_SHARE_TEXT")
	if shareText == "" {
		t.Skip("未提供 FEIWU_SHARE_TEXT，跳过解析部分（登录链路已全部通过）")
	}
	ref, err := ParseShareText(shareText)
	if err != nil {
		t.Fatalf("ParseShareText 失败: %v", err)
	}
	t.Logf("解析分享: pwd_id=%s 提取码=%q 起始目录=%s 口令=%v",
		ref.PwdID, ref.Passcode, ref.DirFID, ref.IsKouling)
	if ref.PwdID == "" {
		t.Fatal("分享文案只识别到夸克口令（无公开解析接口），请改用分享链接重试")
	}

	stoken, err := sess.ShareToken(ctx, ref.PwdID, ref.Passcode)
	if err != nil {
		t.Fatalf("ShareToken 失败: %v", err)
	}
	t.Logf("取得 stoken（前 12 字符）: %.12s", stoken)

	entries, err := sess.ShareList(ctx, ref.PwdID, stoken, ref.DirFID)
	if err != nil {
		t.Fatalf("ShareList 失败: %v", err)
	}
	t.Logf("分享列表共 %d 条，前 %d 条:", len(entries),
		min(len(entries), manualShowEntries))
	var files []FileEntry
	for i, e := range entries {
		if i < manualShowEntries {
			t.Logf("  [%d] %s  size=%d  dir=%v", i+1, e.FileName, e.Size, e.Dir)
		}
		if !e.Dir {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		t.Skip("分享内没有文件（全是目录），跳过直链与导出部分")
	}

	// 7. 直链:分享免转存模式(FIDsToken/PwdID/Stoken 三件套),限少量文件。
	limit := min(len(files), manualLinkFiles)
	fids := make([]string, 0, limit)
	tokens := make([]string, 0, limit)
	for _, f := range files[:limit] {
		fids = append(fids, f.FID)
		tokens = append(tokens, f.ShareFIDToken)
	}
	results, err := sess.DownloadLinks(ctx, LinkRequest{
		FIDs:      fids,
		FIDsToken: tokens,
		PwdID:     ref.PwdID,
		Stoken:    stoken,
	})
	if err != nil {
		t.Fatalf("DownloadLinks 失败: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("DownloadLinks 返回空结果（三入口均成功但 data 为空数组）")
	}
	best := 0
	for i, r := range results {
		t.Logf("直链[%d] %s  score=%d  url=%s", i+1, r.FileName, r.Score,
			manualTruncate(r.URL, 80))
		if r.Score > results[best].Score {
			best = i
		}
	}
	b := results[best]
	t.Logf("打分最高: %s  score=%d  url=%s", b.FileName, b.Score,
		manualTruncate(b.URL, 80))
	if b.URL == "" {
		t.Errorf("打分最高的直链 URL 为空（fid=%s）", b.FID)
	}
	if b.Score <= 0 {
		t.Errorf("打分最高的直链 score=%d（预期 >0，疑似占位/错误页链接）", b.Score)
	}

	// 8. Motrix 导出:Cookie 串必填(防盗链 412),取 jar 整串。
	// 完整导出文本含明文 Cookie,只写入临时文件(路径进日志),
	// 终端回显一律走 manualMaskSensitive 打码版。
	items := make([]ExportItem, 0, len(results))
	for _, r := range results {
		items = append(items, ExportItem{RelPath: r.FileName, URL: r.URL})
	}
	exported, err := ExportLinks(items, sess.jar.String(), ExportMotrix)
	if err != nil {
		t.Fatalf("ExportLinks(motrix) 失败: %v", err)
	}
	exportPath := filepath.Join(os.TempDir(), "feiwu-links-export.txt")
	if err := os.WriteFile(exportPath, []byte(exported), 0o644); err != nil {
		t.Fatalf("写导出文件 %s 失败: %v", exportPath, err)
	}
	t.Logf("Motrix 导出（%d 条）完整文本已写入: %s", len(items), exportPath)
	t.Logf("Motrix 导出（%d 条，Cookie 打码版）:\n%s", len(items),
		manualMaskSensitive(exported))
}

// manualSensitiveRe 匹配登录态核心 Cookie 的 k=v 形态(§2.2:__puus/__pus
// 是登录核心,值不可进任何日志);值终止于 Cookie 串/JSON/CSV 的常见边界。
var manualSensitiveRe = regexp.MustCompile(`(__puus|__pus)=[^;"',\s]+`)

// manualMaskSensitive 把文本中出现的 __puus/__pus 值替换为 ***,
// 其余内容(其它 Cookie 字段、URL、文件名等)原样保留。用于手动用例的
// 终端回显:完整明文只落临时文件,日志永不携带登录凭证。
func manualMaskSensitive(text string) string {
	return manualSensitiveRe.ReplaceAllString(text, "$1=***")
}

// TestManualMaskSensitive 验证打码契约:__puus/__pus 的值必被替换,
// 其余字段保留;打码幂等(已打码形态不再变化)。不依赖真机环境,
// -tags manual 编译后即可运行。
func TestManualMaskSensitive(t *testing.T) {
	cookie := "ctoken=abc; __puus=SECRETVALUE; __pus=S2; _UP_A=keep"
	want := "ctoken=abc; __puus=***; __pus=***; _UP_A=keep"
	if got := manualMaskSensitive(cookie); got != want {
		t.Errorf("Cookie 串打码 = %q, want %q", got, want)
	}

	// JSON 导出文本:cookie 字段内的敏感值打码,url 等其余字段原样。
	jsonText := `{"cookie":"__puus=SECRETVALUE; ctoken=abc","url":"https://d.example/f.mp4?Expires=1"}`
	wantJSON := `{"cookie":"__puus=***; ctoken=abc","url":"https://d.example/f.mp4?Expires=1"}`
	if got := manualMaskSensitive(jsonText); got != wantJSON {
		t.Errorf("JSON 打码 = %q, want %q", got, wantJSON)
	}

	// 幂等:二次打码不再变化,且原值绝不残留。
	if got := manualMaskSensitive(manualMaskSensitive(cookie)); strings.Contains(got, "SECRETVALUE") {
		t.Errorf("二次打码后原值仍残留: %q", got)
	}
	if strings.Contains(manualMaskSensitive(cookie+" __puus=***"), "SECRETVALUE") {
		t.Error("已打码片段不应复活原值")
	}
}

// manualStateName 返回扫码状态的日志名(带语义注记)。
func manualStateName(s QRState) string {
	switch s {
	case QRWaiting:
		return "Waiting（等待扫码）"
	case QRScanned:
		return "Scanned（已扫码，等手机确认）"
	case QRGotTicket:
		return "GotTicket（已取得 service_ticket）"
	case QRInvalid:
		return "Invalid（二维码已失效）"
	case QRExpired:
		return "Expired（二维码已过期）"
	case QRTimeout:
		return "Timeout（超 300s 上限）"
	default:
		return fmt.Sprintf("Unknown(%d)", int(s))
	}
}

// manualTruncate 截断 s 至最多 n 字节(URL 为 ASCII,字节截断安全)。
func manualTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
