// api_test.go 验证 §2.8 业务接口:stoken/分享列表分页/网盘列表分页/
// 容量求和/账号信息。全部走 httptest fake,不连真实服务;
// 分页 fake 按请求的 _page 参数回放固定页,覆盖 50 条/页循环规则。
package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// writeListPage 向 fake 响应写一页列表数据(§2.8 ②⑤ 的响应形状):
// total 写入 metadata._total,条目编号 offset..offset+count-1;
// withToken 控制是否携带 share_fid_token(仅分享侧有)。
// 每 5 条一个目录(dir=true),大小随编号变化,便于断言字段映射。
func writeListPage(w http.ResponseWriter, total, count, offset int, withToken bool) {
	items := make([]string, 0, count)
	for i := offset; i < offset+count; i++ {
		token := ""
		if withToken {
			token = fmt.Sprintf(`,"share_fid_token":"tok-%d"`, i)
		}
		items = append(items, fmt.Sprintf(
			`{"fid":"fid-%03d","file_name":"f-%03d.bin","size":%d,"dir":%v%s}`,
			i, i, int64(i)*1024, i%5 == 0, token))
	}
	fmt.Fprintf(w, `{"status":200,"code":0,"data":{"list":[%s],"metadata":{"_total":%d}}}`,
		strings.Join(items, ","), total)
}

// TestShareToken 验证 §2.8 ①:POST share/sharepage/token,body 字段精确
// (无码传空串)、query 必带 pr=ucpro&fr=pc,取值 data.stoken。
func TestShareToken(t *testing.T) {
	var gotMethod, gotPath, gotBody, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"stoken":"st-abc123"}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	tok, err := s.ShareToken(context.Background(), "shpwd1", "fx8k")
	if err != nil {
		t.Fatalf("ShareToken: %v", err)
	}
	if tok != "st-abc123" {
		t.Errorf("stoken = %q, want %q", tok, "st-abc123")
	}
	if gotMethod != http.MethodPost || gotPath != "/1/clouddrive/share/sharepage/token" {
		t.Errorf("请求 = %s %s, want POST /1/clouddrive/share/sharepage/token", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "pr=ucpro") || !strings.Contains(gotQuery, "fr=pc") {
		t.Errorf("query = %q, 应含 pr=ucpro&fr=pc", gotQuery)
	}
	if want := `{"pwd_id":"shpwd1","passcode":"fx8k"}`; gotBody != want {
		t.Errorf("body = %q, want %q", gotBody, want)
	}

	// 无码分享:passcode 传空串,body 仍显式携带空值字段。
	tok, err = s.ShareToken(context.Background(), "shpwd2", "")
	if err != nil {
		t.Fatalf("ShareToken 无码: %v", err)
	}
	if tok != "st-abc123" {
		t.Errorf("无码 stoken = %q, want st-abc123", tok)
	}
	if want := `{"pwd_id":"shpwd2","passcode":""}`; gotBody != want {
		t.Errorf("无码 body = %q, want %q", gotBody, want)
	}
}

// TestShareListPaging 验证 §2.8 ②:第 1 页 50 条+_total=60、第 2 页
// 10 条 → 共 60 条且恰好请求 2 页;整条 RawQuery 与 §2.8 ② 模板逐字
// 比对(全部固定参数与顺序逐字锁定);fid/file_name/size/dir/share_fid_token
// 五字段映射到 FileEntry。
func TestShareListPaging(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/share/sharepage/detail" {
			t.Errorf("path = %q, want /1/clouddrive/share/sharepage/detail", r.URL.Path)
			return
		}
		n.Add(1)
		q := r.URL.Query()
		// 整条 RawQuery 逐字比对(§2.8 ② URL 模板,含参数顺序):
		// 固定参数(force/_fetch_banner/_fetch_share/_fetch_total/
		// _sort/ver/fetch_share_full_path 等)全串锁定;动态值
		// pwd_id/stoken/pdir_fid 按实现同款 url.QueryEscape 构造期望
		// (本用例取值无特殊字符,转义为恒等);_page 由下方 switch
		// 限定恰为 1/2。
		wantQuery := "pr=ucpro&fr=pc&pwd_id=" + url.QueryEscape("shpwd1") +
			"&stoken=" + url.QueryEscape("st-xyz") +
			"&pdir_fid=" + url.QueryEscape("0") +
			"&force=0&_page=" + q.Get("_page") + "&_size=50" +
			"&_fetch_banner=0&_fetch_share=0&_fetch_total=1" +
			"&_sort=file_type:asc,updated_at:desc&ver=2&fetch_share_full_path=0"
		if r.URL.RawQuery != wantQuery {
			t.Errorf("RawQuery 逐字比对失败:\n got  %q\n want %q", r.URL.RawQuery, wantQuery)
		}
		switch q.Get("_page") {
		case "1":
			writeListPage(w, 60, 50, 0, true)
		case "2":
			writeListPage(w, 60, 10, 50, true)
		default:
			t.Errorf("_page = %q, 不应请求第 3 页及以后", q.Get("_page"))
		}
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	entries, err := s.ShareList(context.Background(), "shpwd1", "st-xyz", "")
	if err != nil {
		t.Fatalf("ShareList: %v", err)
	}
	if len(entries) != 60 {
		t.Fatalf("条目数 = %d, want 60(50+10 两页)", len(entries))
	}
	if n.Load() != 2 {
		t.Errorf("请求数 = %d, want 2(_total=60 取满即停)", n.Load())
	}

	// 字段映射:第 0 条是目录(0%5==0)且带 token;第 1 条是文件;末条来自第 2 页。
	e0, e1, last := entries[0], entries[1], entries[59]
	if e0.FID != "fid-000" || e0.FileName != "f-000.bin" || e0.Size != 0 ||
		!e0.Dir || e0.ShareFIDToken != "tok-0" {
		t.Errorf("entries[0] = %+v, want fid-000/f-000.bin/0/true/tok-0", e0)
	}
	if e1.Dir || e1.ShareFIDToken != "tok-1" || e1.Size != 1024 {
		t.Errorf("entries[1] = %+v, want dir=false token=tok-1 size=1024", e1)
	}
	if last.FID != "fid-059" || last.Size != 59*1024 {
		t.Errorf("entries[59] = %+v, want fid-059 来自第 2 页", last)
	}
}

// TestShareListEmptyPageStops 验证"本页 0 条"停止规则:_total 缺失(为 0)
// 时不能靠累计取满判断,第 2 页返回空列表必须终止循环。
func TestShareListEmptyPageStops(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			writeListPage(w, 0, 50, 0, true) // _total=0:服务端未给总数
			return
		}
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"list":[],"metadata":{"_total":0}}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	entries, err := s.ShareList(context.Background(), "p", "t", "0")
	if err != nil {
		t.Fatalf("ShareList: %v", err)
	}
	if len(entries) != 50 {
		t.Fatalf("条目数 = %d, want 50", len(entries))
	}
	if n.Load() != 2 {
		t.Errorf("请求数 = %d, want 2(空页即停)", n.Load())
	}
}

// TestDriveListPaging 验证 §2.8 ⑤:file/sort 同分页规则(50+10=60,
// 恰好 2 次请求);整条 RawQuery 与 §2.8 ⑤ 模板逐字比对(全部固定
// 参数与顺序逐字锁定,uc_param_str 必须为空值);dir 映射正确;
// 响应无 share_fid_token 字段时保持零值。
func TestDriveListPaging(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/file/sort" {
			t.Errorf("path = %q, want /1/clouddrive/file/sort", r.URL.Path)
			return
		}
		n.Add(1)
		q := r.URL.Query()
		// 整条 RawQuery 逐字比对(§2.8 ⑤ URL 模板,含参数顺序):
		// 固定参数(uc_param_str 空值/_fetch_sub_dirs/_fetch_total/
		// _sort/_fetch_full_path/fetch_all_file/fetch_risk_file_name)
		// 全串锁定——uc_param_str 必须以空值出现,缺失同样判失败;
		// 动态值 pdir_fid 按实现同款 url.QueryEscape 构造期望(本用例
		// 取值无特殊字符,转义为恒等);_page 由下方 switch 限定恰为 1/2。
		wantQuery := "pr=ucpro&fr=pc&uc_param_str=&pdir_fid=" + url.QueryEscape("dirfid9") +
			"&_page=" + q.Get("_page") +
			"&_size=50&_fetch_total=1&_fetch_sub_dirs=0" +
			"&_sort=file_type:asc,updated_at:desc&_fetch_full_path=0" +
			"&fetch_all_file=1&fetch_risk_file_name=1"
		if r.URL.RawQuery != wantQuery {
			t.Errorf("RawQuery 逐字比对失败:\n got  %q\n want %q", r.URL.RawQuery, wantQuery)
		}
		switch q.Get("_page") {
		case "1":
			writeListPage(w, 60, 50, 0, false)
		case "2":
			writeListPage(w, 60, 10, 50, false)
		default:
			t.Errorf("_page = %q, 不应请求第 3 页及以后", q.Get("_page"))
		}
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	entries, err := s.DriveList(context.Background(), "dirfid9")
	if err != nil {
		t.Fatalf("DriveList: %v", err)
	}
	if len(entries) != 60 {
		t.Fatalf("条目数 = %d, want 60", len(entries))
	}
	if n.Load() != 2 {
		t.Errorf("请求数 = %d, want 2", n.Load())
	}
	// file/sort 响应无 share_fid_token(§2.8 ⑤ 取值清单),零值即正确。
	e0, e1 := entries[0], entries[1]
	if e0.FID != "fid-000" || !e0.Dir || e0.ShareFIDToken != "" {
		t.Errorf("entries[0] = %+v, want fid-000 dir=true token 空值", e0)
	}
	if e1.Dir || e1.Size != 1024 || e1.FileName != "f-001.bin" {
		t.Errorf("entries[1] = %+v, want dir=false size=1024", e1)
	}
}

// TestDriveListRootDefault 验证 pdirFID 空串归一化为根目录 "0"(§2.8 ⑤)。
func TestDriveListRootDefault(t *testing.T) {
	var gotPdir string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPdir = r.URL.Query().Get("pdir_fid")
		writeListPage(w, 2, 2, 0, false)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	entries, err := s.DriveList(context.Background(), "")
	if err != nil {
		t.Fatalf("DriveList: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("条目数 = %d, want 2", len(entries))
	}
	if gotPdir != "0" {
		t.Errorf("pdir_fid = %q, want 0(空串归一化为根)", gotPdir)
	}
}

// TestCapacity 验证 §2.8 ⑨:member 接口,总容量/已用容量均含 secret 合计。
func TestCapacity(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"total_capacity":1000,`+
			`"secret_total_capacity":234,"use_capacity":300,"secret_use_capacity":66}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	total, used, err := s.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if gotPath != "/1/clouddrive/member" {
		t.Errorf("path = %q, want /1/clouddrive/member", gotPath)
	}
	if total != 1234 {
		t.Errorf("total = %d, want 1234(1000+234 secret 合计)", total)
	}
	if used != 366 {
		t.Errorf("used = %d, want 366(300+66 secret 合计)", used)
	}
}

// TestAccount 验证账号信息接口:走 QuickCheck 成功路径取昵称/头像;
// 登录失效(401)时返回用户可读错误。
func TestAccount(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"nickname":"飞舞","avatarUri":"https://img/a.png"}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{pan: srv.URL, drivePC: srv.URL, drive: srv.URL}
	s.wall = srv.Client()

	info, err := s.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if info.Nickname != "飞舞" || info.AvatarURI != "https://img/a.png" {
		t.Errorf("info = %+v, want 昵称=飞舞 头像=https://img/a.png", info)
	}
	if n.Load() != 1 {
		t.Errorf("请求数 = %d, want 1(account/info 成功即返回)", n.Load())
	}

	// 登录失效:account/info 返回 401 → Account 报登录失效,不再兜底。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv2.Close()

	s2 := newTestSession(srv2.Client(), nil)
	s2.base = sessionBase{pan: srv2.URL, drivePC: srv2.URL, drive: srv2.URL}
	s2.wall = srv2.Client()

	_, err = s2.Account(context.Background())
	if err == nil {
		t.Fatal("登录失效时 Account 应返回错误")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "登录") {
		t.Fatalf("错误应为含登录提示的 *APIError,实际 %T: %v", err, err)
	}
}
