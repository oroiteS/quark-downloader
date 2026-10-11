// links_test.go 验证 §2.8⑦ 取下载直链(三入口择优)与 §2.9 直链质量
// 打分:打分分支逐条核对;四种 data 形态解析;单入口失败不影响整体、
// 全失败才报错;usable 优先 + 组总分择优;免转存模式 body 与入口顺序。
// 全部走 httptest fake,不连真实服务。
package protocol

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 三入口 query 逐字(§2.8⑦);测试用它们精确区分 fake 收到了哪个入口。
const (
	qEntryPC      = "pr=ucpro&fr=pc&uc_param_str="
	qEntryFT      = "pr=ucpro&fr=pc&uc_param_str=&entry=ft"
	qEntryAndroid = "pr=ucpro&fr=pc&uc_param_str=&fr=android"
)

// TestScoreLink 逐字核对 §2.9 打分规则的每个分支与叠加:
// https 开头 +100;auth_key= +30 / Expires= +15 / Signature= +15;
// .quark.cn +10 / .uc.cn +6 / cdn +5;error|denied|forbidden −200;长度<60 −20。
func TestScoreLink(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want int
	}{
		{
			// https+auth_key+Expires+Signature+.quark.cn+cdn,80 字符 ≥60:
			// 100+30+15+15+10+5 = 175。
			name: "完整CDN签名链",
			url:  "https://dl-cdn-1.quark.cn/d?auth_key=AK123&Expires=1700000000&Signature=S%2B1%3D",
			want: 175,
		},
		{
			// 仅 https,62 字符 ≥60:100。
			name: "https裸长链",
			url:  "https://example.com/some/really/long/path/to/reach/sixty/chars",
			want: 100,
		},
		{
			// https 但 15 字符 <60(跳转壳):100-20 = 80。
			name: "https短壳链",
			url:  "https://ex.cn/a",
			want: 80,
		},
		{
			// http 短链:-20。
			name: "http短链",
			url:  "http://x/1",
			want: -20,
		},
		{
			// https+.uc.cn+cdn,64 字符 ≥60:100+6+5 = 111。
			name: "uc.cn域CDN链",
			url:  "https://dl-cdn-1.uc.cn/aaaaaaaaaa/bbbbbbbbbb/cccccccccc/dddddddd",
			want: 111,
		},
		{
			// https+.quark.cn 但含 error 且 26 字符 <60:100+10-200-20 = -110。
			name: "error占位链",
			url:  "https://e.quark.cn/error/x",
			want: -110,
		},
		{
			// http 长链含 forbidden:仅 -200。
			name: "forbidden长链",
			url:  "http://forbidden.example.com/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			want: -200,
		},
		{
			// https+.quark.cn+cdn 且 77 字符,但含 denied:100+10+5-200 = -85。
			name: "denied长链",
			url:  "https://dl-cdn-1.quark.cn/denied/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			want: -85,
		},
		{
			// 空串:长度 0 <60 → -20。
			name: "空链",
			url:  "",
			want: -20,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScoreLink(tc.url); got != tc.want {
				t.Errorf("ScoreLink(%q) = %d, want %d", tc.url, got, tc.want)
			}
		})
	}
}

// linkShapeServer 起一个固定回放某 data 形态的 fake download 服务,
// 并记录收到的 body;三入口请求都会得到同一形态。
func linkShapeServer(t *testing.T, dataShape string) (*httptest.Server, *atomic.Int64, *string) {
	t.Helper()
	var n atomic.Int64
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/1/clouddrive/file/download" {
			t.Errorf("path = %q, want /1/clouddrive/file/download", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		switch r.URL.RawQuery {
		case qEntryPC, qEntryFT, qEntryAndroid:
		default:
			t.Errorf("query = %q, want 三入口之一", r.URL.RawQuery)
		}
		n.Add(1)
		fmt.Fprintf(w, `{"status":200,"code":0,"data":%s}`, dataShape)
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &body
}

// TestDownloadLinksShapes 验证 §2.8⑦ 的四种 data 形态归一:
// 数组 / {list:[]} / {files:[]} / 单对象,逐项取
// {fid, download_url, file_name, size};我的网盘 body 恰为 {"fids":[...]}。
func TestDownloadLinksShapes(t *testing.T) {
	// 80 字符的高分链(175 分,见 TestScoreLink 完整CDN签名链)。
	const hiURL = "https://dl-cdn-1.quark.cn/d?auth_key=AK123&Expires=1700000000&Signature=S%2B1%3D"
	// 15 字符 https 短壳(80 分)。
	const loURL = "https://ex.cn/a"

	shapes := []struct {
		name string
		data string
		want []LinkResult
	}{
		{
			name: "数组形态",
			data: `[{"fid":"f1","download_url":"` + hiURL + `","file_name":"a.mp4","size":1024}]`,
			want: []LinkResult{{FID: "f1", FileName: "a.mp4", Size: 1024, URL: hiURL, Score: 175}},
		},
		{
			name: "list形态",
			data: `{"list":[` +
				`{"fid":"f2","download_url":"` + loURL + `","file_name":"b.mp4","size":2048},` +
				`{"fid":"f3","download_url":"` + hiURL + `","file_name":"c.mp4","size":4096}]}`,
			want: []LinkResult{
				{FID: "f2", FileName: "b.mp4", Size: 2048, URL: loURL, Score: 80},
				{FID: "f3", FileName: "c.mp4", Size: 4096, URL: hiURL, Score: 175},
			},
		},
		{
			name: "files形态",
			data: `{"files":[{"fid":"f4","download_url":"` + hiURL + `","file_name":"d.mp4","size":8}]}`,
			want: []LinkResult{{FID: "f4", FileName: "d.mp4", Size: 8, URL: hiURL, Score: 175}},
		},
		{
			name: "单对象形态",
			data: `{"fid":"f5","download_url":"` + hiURL + `","file_name":"e.mp4","size":16}`,
			want: []LinkResult{{FID: "f5", FileName: "e.mp4", Size: 16, URL: hiURL, Score: 175}},
		},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			srv, n, body := linkShapeServer(t, tc.data)
			s := newTestSession(srv.Client(), nil)
			s.base = sessionBase{drivePC: srv.URL}

			got, err := s.DownloadLinks(context.Background(), LinkRequest{FIDs: []string{"f1"}})
			if err != nil {
				t.Fatalf("DownloadLinks: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("结果 = %+v, want %d 条 %+v", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("结果[%d] = %+v, want %+v", i, got[i], w)
				}
			}
			if want := `{"fids":["f1"]}`; *body != want {
				t.Errorf("我的网盘 body = %q, want %q", *body, want)
			}
			if n.Load() != 3 {
				t.Errorf("请求数 = %d, want 3(三入口各一次)", n.Load())
			}
		})
	}
}

// TestDownloadLinksEntryFallback 验证单入口失败不影响整体、
// 三入口全失败才报错:入口1(PC)恒 500,DoAPI 重试 3 次共 4 次后放弃,
// 入口2/3 正常 → 用其余入口结果;全 500 → 报错且三入口都被尝试。
func TestDownloadLinksEntryFallback(t *testing.T) {
	// 场景A:仅入口1 失败,入口2/3 返回同一条低分链 → 成功取到结果。
	const okURL = "https://ex.cn/a"
	var nPC, nOK atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RawQuery {
		case qEntryPC:
			nPC.Add(1)
			w.WriteHeader(http.StatusInternalServerError) // 重试 3 次后放弃
		default:
			nOK.Add(1)
			fmt.Fprint(w, `{"status":200,"code":0,"data":{"fid":"f1","download_url":"`+okURL+`","file_name":"a.mp4","size":1}}`)
		}
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	got, err := s.DownloadLinks(context.Background(), LinkRequest{FIDs: []string{"f1"}})
	if err != nil {
		t.Fatalf("单入口失败不应影响整体: %v", err)
	}
	if len(got) != 1 || got[0].URL != okURL {
		t.Errorf("结果 = %+v, want 用入口2/3 的 %q", got, okURL)
	}
	if nPC.Load() != 4 {
		t.Errorf("入口1 请求数 = %d, want 4(1 次 + 3 次退避重试)", nPC.Load())
	}
	if nOK.Load() != 2 {
		t.Errorf("入口2/3 请求数 = %d, want 2", nOK.Load())
	}

	// 场景B:三入口全 500 → 报错,且 3×4 = 12 次请求都已尝试。
	var nAll atomic.Int64
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nAll.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()

	s2 := newTestSession(srv2.Client(), nil)
	s2.base = sessionBase{drivePC: srv2.URL}

	_, err = s2.DownloadLinks(context.Background(), LinkRequest{FIDs: []string{"f1"}})
	if err == nil {
		t.Fatal("三入口全失败应报错")
	}
	if !strings.Contains(err.Error(), "直链") {
		t.Errorf("错误信息 = %q, 应含直链上下文", err.Error())
	}
	if nAll.Load() != 12 {
		t.Errorf("总请求数 = %d, want 12(三入口各 1+3 次)", nAll.Load())
	}
}

// TestDownloadLinksBestGroup 验证 §2.9 组间择优:
// usable(至少一条 URL 长度 >20)优先于组总分;同为 usable 时组总分
// (负分条按 0 计)最高者胜;返回组的每条 Score 保留原始打分。
func TestDownloadLinksBestGroup(t *testing.T) {
	// 入口2:16 字符 https+.uc.cn 链,单条 106 分但组不可用(URL ≤20);
	// 入口3:60 字符裸 https 长链,单条 100 分且组可用 → 必须选入口3,
	// 证明 usable 判定优先于组总分(106 > 100 也不选不可用组)。
	const unusableURL = "https://a.uc.cn/x" // 16 字符,100+6=106
	const plainURL = "https://plain.example.com/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var u string
		switch r.URL.RawQuery {
		case qEntryPC:
			u = "http://x/1" // -20,可用组里总分最低
		case qEntryFT:
			u = unusableURL
		case qEntryAndroid:
			u = plainURL
		default:
			t.Errorf("query = %q, want 三入口之一", r.URL.RawQuery)
			return
		}
		fmt.Fprintf(w, `{"status":200,"code":0,"data":[{"fid":"f1","download_url":"%s","file_name":"a.mp4","size":1}]}`, u)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	got, err := s.DownloadLinks(context.Background(), LinkRequest{FIDs: []string{"f1"}})
	if err != nil {
		t.Fatalf("DownloadLinks: %v", err)
	}
	if len(got) != 1 || got[0].URL != plainURL {
		t.Fatalf("结果 URL = %+v, want 可用组 %q(usable 优先于 106 分不可用组)", got, plainURL)
	}
	if got[0].Score != 100 {
		t.Errorf("Score = %d, want 100(原始打分保留)", got[0].Score)
	}

	// 高分可用组胜低分可用组:入口3 返回 175 分签名链,其余入口低分。
	const hiURL = "https://dl-cdn-1.quark.cn/d?auth_key=AK123&Expires=1700000000&Signature=S%2B1%3D"
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := "https://ex.cn/a" // 80 分
		if r.URL.RawQuery == qEntryAndroid {
			u = hiURL // 175 分
		}
		fmt.Fprintf(w, `{"status":200,"code":0,"data":[{"fid":"f1","download_url":"%s","file_name":"a.mp4","size":1}]}`, u)
	}))
	defer srv2.Close()

	s2 := newTestSession(srv2.Client(), nil)
	s2.base = sessionBase{drivePC: srv2.URL}

	got2, err := s2.DownloadLinks(context.Background(), LinkRequest{FIDs: []string{"f1"}})
	if err != nil {
		t.Fatalf("DownloadLinks(高分择优): %v", err)
	}
	if len(got2) != 1 || got2[0].URL != hiURL || got2[0].Score != 175 {
		t.Errorf("结果 = %+v, want 总分最高的入口3 签名链(175)", got2)
	}
}

// TestDownloadLinksShareMode 验证分享免转存模式(§2.8⑦):
// body 携带 fids_token/pwd_id/stoken;入口顺序 entry=ft 优先;
// 返回链路打分与择优同我的网盘模式。
func TestDownloadLinksShareMode(t *testing.T) {
	var order []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		order = append(order, r.URL.RawQuery)
		bodies = append(bodies, string(raw))
		fmt.Fprint(w, `{"status":200,"code":0,"data":[{"fid":"sf1","download_url":"https://ex.cn/a","file_name":"s.mp4","size":7}]}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	got, err := s.DownloadLinks(context.Background(), LinkRequest{
		FIDs:      []string{"sf1"},
		FIDsToken: []string{"tok1"},
		PwdID:     "pwd1",
		Stoken:    "st1",
	})
	if err != nil {
		t.Fatalf("DownloadLinks(免转存): %v", err)
	}
	if len(got) != 1 || got[0].FID != "sf1" {
		t.Fatalf("结果 = %+v, want sf1 一条", got)
	}
	wantOrder := []string{qEntryFT, qEntryPC, qEntryAndroid}
	if len(order) != 3 {
		t.Fatalf("请求数 = %d, want 3", len(order))
	}
	for i, q := range wantOrder {
		if order[i] != q {
			t.Errorf("入口顺序[%d] = %q, want %q(entry=ft 优先)", i, order[i], q)
		}
	}
	const wantBody = `{"fids":["sf1"],"fids_token":["tok1"],"pwd_id":"pwd1","stoken":"st1"}`
	for i, b := range bodies {
		if b != wantBody {
			t.Errorf("body[%d] = %q, want %q", i, b, wantBody)
		}
	}
}

// TestDownloadLinksAllUnusable 验证边界:三入口都成功但都无可用链
// (URL 均 ≤20)时不报错,仍返回一组结果(Score 保留原始负分,
// 供上层按分值判断是否走刷新)。
func TestDownloadLinksAllUnusable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":200,"code":0,"data":{"fid":"f1","download_url":"http://e","file_name":"a","size":1}}`)
	}))
	defer srv.Close()

	s := newTestSession(srv.Client(), nil)
	s.base = sessionBase{drivePC: srv.URL}

	got, err := s.DownloadLinks(context.Background(), LinkRequest{FIDs: []string{"f1"}})
	if err != nil {
		t.Fatalf("全不可用但入口成功,不应报错: %v", err)
	}
	if len(got) != 1 || got[0].URL != "http://e" || got[0].Score != -20 {
		t.Errorf("结果 = %+v, want http://e 一条, Score=-20(7字符<60)", got)
	}
}
