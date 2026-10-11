// links.go 实现规格 §2.8⑦ 取下载直链与 §2.9 直链质量打分:
// 同一批 fid 依次经三入口各请求一次 download 接口,单入口失败不影响
// 整体、全失败才报错;每组按 §2.9 打分(负分条按 0 计入求和),
// usable(至少一条 URL 长度 >20)优先、总分最高的一组返回。
// 全部经 DoAPI 出站,复用 §2.3 会话层的节流/退避/自愈/判定。
package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// 三入口 query 逐字对齐 §2.8⑦:同一 URL 模板追加不同入口参数,
// 服务端按入口下发不同 CDN 节点与签名的直链。
// 注意入口 3 含两个 fr(先 pc 后 android),为规格原文形态,
// 故 query 用字面量拼接而非 url.Values(后者会重排/去重键)。
const (
	entryQueryPC      = "pr=ucpro&fr=pc&uc_param_str="            // 入口1:PC 常规
	entryQueryFT      = "pr=ucpro&fr=pc&uc_param_str=&entry=ft"   // 入口2:网页入口
	entryQueryAndroid = "pr=ucpro&fr=pc&uc_param_str=&fr=android" // 入口3:移动入口
)

// downloadPath 是取直链接口的固定路径(§2.8⑦)。
const downloadPath = "/1/clouddrive/file/download"

// pcEntryOrder 是我的网盘模式的三入口请求顺序(§2.8⑦):PC → ft → android。
var pcEntryOrder = []string{entryQueryPC, entryQueryFT, entryQueryAndroid}

// shareEntryOrder 是分享免转存模式的入口顺序(§2.8⑦"entry=ft 优先"):
// entry=ft 提到最前,其余保持 PC → android。
var shareEntryOrder = []string{entryQueryFT, entryQueryPC, entryQueryAndroid}

// LinkRequest 是取直链的入参:我的网盘模式只填 FIDs;
// 分享免转存模式另填 FIDsToken/PwdID/Stoken(后三项非空即视为免转存)。
type LinkRequest struct {
	FIDs      []string
	FIDsToken []string
	PwdID     string
	Stoken    string
}

// LinkResult 是单条直链结果:fid/文件名/大小 + 直链与其 §2.9 打分。
// Score 保留原始分(可为负),负分按 0 计是分组求和处的规则。
type LinkResult struct {
	FID      string
	FileName string
	Size     int64
	URL      string
	Score    int
}

// downloadBody 是 download 接口请求体(§2.8⑦):我的网盘只有 fids,
// 分享免转存追加 fids_token/pwd_id/stoken(免转存模式三者非空,
// omitempty 恰好让我的网盘 body 只有 {"fids":[...]} 一项)。
type downloadBody struct {
	FIDs      []string `json:"fids"`
	FIDsToken []string `json:"fids_token,omitempty"`
	PwdID     string   `json:"pwd_id,omitempty"`
	Stoken    string   `json:"stoken,omitempty"`
}

// isShareMode 判定分享免转存模式:FIDsToken/PwdID/Stoken 任一非空
// (规格约定免转存三者均非空;任一非空即按免转存对待,最不易误判)。
func (r LinkRequest) isShareMode() bool {
	return len(r.FIDsToken) > 0 || r.PwdID != "" || r.Stoken != ""
}

// DownloadLinks 取一批 fid 的下载直链(§2.8⑦ 三入口择优):
// 依次用三组入口 query 各 POST 一次 download 接口(顺序请求,
// DoAPI 自带节流,规格无并发要求);单入口请求或解析失败只记录
// 不中断,三入口全失败才报错(错误合并各入口原因)。
// 每入口的 data 归一为数组/{list}/{files}/单对象四种形态,逐项取
// {fid, download_url, file_name, size} 并按 §2.9 打分;usable
// (至少一条 URL 长度 >20)的组优先,其中组总分(负分条按 0 计)
// 最高者返回;若无 usable 组则回退总分最高的成功组,由上层按
// Score 决定是否走 §2.10 刷新。
func (s *Session) DownloadLinks(ctx context.Context, req LinkRequest) ([]LinkResult, error) {
	body, err := json.Marshal(downloadBody{
		FIDs:      req.FIDs,
		FIDsToken: req.FIDsToken,
		PwdID:     req.PwdID,
		Stoken:    req.Stoken,
	})
	if err != nil {
		return nil, fmt.Errorf("构造直链请求 body: %w", err)
	}

	order := pcEntryOrder
	if req.isShareMode() {
		order = shareEntryOrder // §2.8⑦:免转存 entry=ft 优先
	}

	var groups [][]LinkResult
	var errs []error
	for _, entryQuery := range order {
		results, err := s.fetchLinks(ctx, entryQuery, body)
		if err != nil {
			errs = append(errs, err)
			continue // §2.8⑦:单入口失败不影响整体
		}
		groups = append(groups, results)
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("三入口取直链全部失败: %w", errors.Join(errs...))
	}
	return bestLinkGroup(groups), nil
}

// fetchLinks 经单个入口请求直链并解析打分。
func (s *Session) fetchLinks(ctx context.Context, entryQuery string, body []byte) ([]LinkResult, error) {
	data, err := s.DoAPI(ctx, http.MethodPost, s.base.drivePC+downloadPath+"?"+entryQuery, body)
	if err != nil {
		return nil, err
	}
	items, err := parseLinkData(data)
	if err != nil {
		return nil, fmt.Errorf("解析直链响应(入口 %s): %w", entryQuery, err)
	}
	results := make([]LinkResult, 0, len(items))
	for _, it := range items {
		results = append(results, LinkResult{
			FID:      it.FID,
			FileName: it.FileName,
			Size:     it.Size,
			URL:      it.DownloadURL,
			Score:    ScoreLink(it.DownloadURL),
		})
	}
	return results, nil
}

// linkItem 是 download 接口 data 内单条直链的原始形状(§2.8⑦)。
type linkItem struct {
	FID         string `json:"fid"`
	DownloadURL string `json:"download_url"`
	FileName    string `json:"file_name"`
	Size        int64  `json:"size"`
}

// parseLinkData 归一 §2.8⑦ 的四种 data 形态:数组 / {list:[...]} /
// {files:[...]} / 单对象。data 缺失或形状不符时报错(该入口计为失败)。
func parseLinkData(data []byte) ([]linkItem, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("响应缺少 data 字段")
	}
	if trimmed[0] == '[' { // 形态1:数组
		var items []linkItem
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, shapeErr(trimmed, err)
		}
		return items, nil
	}
	var obj struct {
		List  []linkItem `json:"list"`  // 形态2:{list:[...]}
		Files []linkItem `json:"files"` // 形态3:{files:[...]}
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, shapeErr(trimmed, err)
	}
	if len(obj.List) > 0 {
		return obj.List, nil
	}
	if len(obj.Files) > 0 {
		return obj.Files, nil
	}
	// 形态4:单对象——整体视为单条直链,有 fid 或 download_url 才采纳。
	var single linkItem
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return nil, shapeErr(trimmed, err)
	}
	if single.FID != "" || single.DownloadURL != "" {
		return []linkItem{single}, nil
	}
	return nil, shapeErr(trimmed, nil)
}

// shapeErr 统一报"data 不是预期的直链形状",附 120 字符片段定位。
func shapeErr(data []byte, cause error) error {
	snippet := string(data)
	if len(snippet) > 120 {
		snippet = snippet[:120] + "..."
	}
	if cause != nil {
		return fmt.Errorf("data 不是预期的直链形状(%v): %s", cause, snippet)
	}
	return fmt.Errorf("data 不是预期的直链形状: %s", snippet)
}

// ScoreLink 对单条 download_url 做纯本地启发式打分(§2.9 逐字):
//
//	+100 https:// 开头;+30 含 auth_key=;+15 含 Expires=;+15 含 Signature=
//	+10 含 .quark.cn;+6 含 .uc.cn;+5 含 cdn
//	−200 含 error/denied/forbidden(占位或错误页);−20 长度 <60(跳转壳)
func ScoreLink(u string) int {
	score := 0
	if strings.HasPrefix(u, "https://") {
		score += 100
	}
	if strings.Contains(u, "auth_key=") {
		score += 30
	}
	if strings.Contains(u, "Expires=") {
		score += 15
	}
	if strings.Contains(u, "Signature=") {
		score += 15
	}
	if strings.Contains(u, ".quark.cn") {
		score += 10
	}
	if strings.Contains(u, ".uc.cn") {
		score += 6
	}
	if strings.Contains(u, "cdn") {
		score += 5
	}
	if strings.Contains(u, "error") || strings.Contains(u, "denied") || strings.Contains(u, "forbidden") {
		score -= 200
	}
	if len(u) < 60 {
		score -= 20
	}
	return score
}

// bestLinkGroup 在成功组间择优(§2.9):usable(至少一条 URL 长度 >20)
// 的组优先于不可用组;同类内比较组总分——各条 Score 求和、负分条按 0
// 计。同分保持先到(即靠前的入口),保证结果稳定。
func bestLinkGroup(groups [][]LinkResult) []LinkResult {
	best := groups[0]
	bestTotal, bestUsable := groupScore(best)
	for _, g := range groups[1:] {
		total, usable := groupScore(g)
		if (!bestUsable && usable) || (bestUsable == usable && total > bestTotal) {
			best, bestTotal, bestUsable = g, total, usable
		}
	}
	return best
}

// groupScore 返回组的总分(负分条按 0 计,§2.9)与 usable 判定
// (至少一条 URL 长度 >20)。
func groupScore(group []LinkResult) (total int, usable bool) {
	for _, r := range group {
		if r.Score > 0 {
			total += r.Score
		}
		if len(r.URL) > 20 {
			usable = true
		}
	}
	return total, usable
}
