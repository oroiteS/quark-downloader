// api.go 实现规格 §2.8 的业务接口:分享凭证 stoken、分享文件列表、
// 我的网盘列表、容量与账号信息。全部经 DoAPI 出站(节流/退避/自愈/判定
// 复用 §2.3 会话层);分页一律 50 条/页,循环到"本页 0 条"或
// metadata._total 已取满。③④⑥⑧按设计非目标不在本层实现。
package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// pageSize 是 §2.8 规定的固定分页大小:50 条/页。
const pageSize = 50

// FileEntry 是分享/网盘列表的单个条目(§2.8 ②⑤ 的公共取值):
// fid/file_name/size/dir 四字段 + 分享侧专属的 share_fid_token。
// 网盘列表(file/sort)响应无 share_fid_token,解析后为零值。
// FID 与 ShareFIDToken 供 Task 9 直链 LinkRequest 共用。
type FileEntry struct {
	FID           string `json:"fid"`
	FileName      string `json:"file_name"`
	Size          int64  `json:"size"`
	Dir           bool   `json:"dir"`
	ShareFIDToken string `json:"share_fid_token"`
}

// listResponse 是 sharepage/detail 与 file/sort 的公共响应形状:
// data.list[] 为本页条目,metadata._total 为全集总数,用于分页终止判断。
type listResponse struct {
	List     []FileEntry `json:"list"`
	Metadata struct {
		Total int `json:"_total"`
	} `json:"metadata"`
}

// ShareToken 换取分享凭证(§2.8 ①):POST share/sharepage/token,
// body {"pwd_id","passcode"}(无码分享 passcode 传空串),取值 data.stoken。
func (s *Session) ShareToken(ctx context.Context, pwdID, passcode string) (string, error) {
	body, err := json.Marshal(struct {
		PwdID    string `json:"pwd_id"`
		Passcode string `json:"passcode"`
	}{pwdID, passcode})
	if err != nil {
		return "", fmt.Errorf("构造 stoken 请求 body: %w", err)
	}
	data, err := s.DoAPI(ctx, http.MethodPost,
		s.base.drivePC+"/1/clouddrive/share/sharepage/token?pr=ucpro&fr=pc", body)
	if err != nil {
		return "", err
	}
	var payload struct {
		Stoken string `json:"stoken"`
	}
	if err := unmarshalData(data, &payload); err != nil {
		return "", fmt.Errorf("解析 stoken 响应: %w", err)
	}
	return payload.Stoken, nil
}

// ShareList 列出分享目录下的全部文件(§2.8 ②,分页循环):
// pdirFID 为目录 fid,根目录传 "0" 或空串(归一化为 "0")。
// 逐页 GET sharepage/detail,直到本页 0 条或 metadata._total 取满。
func (s *Session) ShareList(ctx context.Context, pwdID, stoken, pdirFID string) ([]FileEntry, error) {
	pdirFID = rootFID(pdirFID)
	return pageAll(ctx, func(page int) ([]FileEntry, int, error) {
		// URL 模板逐字对齐 §2.8 ②;动态值(pr/stoken/pdirFID)做 query 转义。
		u := fmt.Sprintf(
			"%s/1/clouddrive/share/sharepage/detail?pr=ucpro&fr=pc&pwd_id=%s&stoken=%s&pdir_fid=%s"+
				"&force=0&_page=%d&_size=%d&_fetch_banner=0&_fetch_share=0&_fetch_total=1"+
				"&_sort=file_type:asc,updated_at:desc&ver=2&fetch_share_full_path=0",
			s.base.drivePC, url.QueryEscape(pwdID), url.QueryEscape(stoken),
			url.QueryEscape(pdirFID), page, pageSize)
		data, err := s.DoAPI(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, 0, err
		}
		var lr listResponse
		if err := unmarshalData(data, &lr); err != nil {
			return nil, 0, fmt.Errorf("解析分享列表响应: %w", err)
		}
		return lr.List, lr.Metadata.Total, nil
	})
}

// DriveList 列出我的网盘目录下的全部文件(§2.8 ⑤,分页循环):
// pdirFID 为目录 fid,根目录传 "0" 或空串(归一化为 "0")。
// 逐页 GET file/sort,终止规则与 ShareList 相同;响应无 share_fid_token。
func (s *Session) DriveList(ctx context.Context, pdirFID string) ([]FileEntry, error) {
	pdirFID = rootFID(pdirFID)
	return pageAll(ctx, func(page int) ([]FileEntry, int, error) {
		// URL 模板逐字对齐 §2.8 ⑤。
		u := fmt.Sprintf(
			"%s/1/clouddrive/file/sort?pr=ucpro&fr=pc&uc_param_str=&pdir_fid=%s"+
				"&_page=%d&_size=%d&_fetch_total=1&_fetch_sub_dirs=0"+
				"&_sort=file_type:asc,updated_at:desc&_fetch_full_path=0"+
				"&fetch_all_file=1&fetch_risk_file_name=1",
			s.base.drivePC, url.QueryEscape(pdirFID), page, pageSize)
		data, err := s.DoAPI(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, 0, err
		}
		var lr listResponse
		if err := unmarshalData(data, &lr); err != nil {
			return nil, 0, fmt.Errorf("解析网盘列表响应: %w", err)
		}
		return lr.List, lr.Metadata.Total, nil
	})
}

// Capacity 查询网盘容量(§2.8 ⑨):GET member,总容量与已用容量
// 均为普通盘与保险箱(secret)两项合计。
func (s *Session) Capacity(ctx context.Context) (total, used int64, err error) {
	data, err := s.DoAPI(ctx, http.MethodGet,
		s.base.drivePC+"/1/clouddrive/member?pr=ucpro&fr=pc", nil)
	if err != nil {
		return 0, 0, err
	}
	var payload struct {
		TotalCapacity       int64 `json:"total_capacity"`
		SecretTotalCapacity int64 `json:"secret_total_capacity"`
		UseCapacity         int64 `json:"use_capacity"`
		SecretUseCapacity   int64 `json:"secret_use_capacity"`
	}
	if err := unmarshalData(data, &payload); err != nil {
		return 0, 0, fmt.Errorf("解析容量响应: %w", err)
	}
	return payload.TotalCapacity + payload.SecretTotalCapacity,
		payload.UseCapacity + payload.SecretUseCapacity, nil
}

// Account 返回当前账号的昵称与头像(保活/头像展示区共用):
// 走 QuickCheck 的成功路径——account/info HTTP 200 时 data 即账号信息;
// QuickCheck 判定登录失效(401/412)时返回用户可读错误。
func (s *Session) Account(ctx context.Context) (AccountInfo, error) {
	ok, info, _ := s.QuickCheck(ctx) // QuickCheck 的 error 恒为 nil,见其文档
	if !ok {
		return AccountInfo{}, &APIError{
			StatusCode: http.StatusUnauthorized,
			Message:    "登录已失效，请重新扫码登录",
		}
	}
	return info, nil
}

// pageAll 是 §2.8 的通用分页循环:fetchPage(page) 请求第 page 页并返回
// (本页条目, metadata._total);终止条件——本页 0 条,或 total>0 且
// 累计条数已达 total。任何一页出错整体失败,不返回半截结果。
func pageAll(ctx context.Context, fetchPage func(page int) ([]FileEntry, int, error)) ([]FileEntry, error) {
	var all []FileEntry
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, total, err := fetchPage(page)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			break // 本页 0 条:全集已取完(§2.8)。
		}
		all = append(all, entries...)
		if total > 0 && len(all) >= total {
			break // metadata._total 已取满(§2.8)。
		}
	}
	return all, nil
}

// rootFID 归一化目录 fid:空串视为根目录 "0"(§2.8 ②⑤ 均以 0 为根)。
func rootFID(fid string) string {
	if fid == "" {
		return "0"
	}
	return fid
}

// unmarshalData 把 DoAPI 返回的 data 原文解析进 v;data 缺失(空)时
// 报错而不是静默零值,便于及早暴露响应形状异常。
func unmarshalData(data []byte, v any) error {
	if len(data) == 0 {
		return fmt.Errorf("响应缺少 data 字段")
	}
	if err := json.Unmarshal(data, v); err != nil {
		snippet := string(data)
		if len(snippet) > 120 {
			snippet = snippet[:120] + "..."
		}
		return fmt.Errorf("data 不是预期的 JSON 形状: %s", snippet)
	}
	return nil
}
