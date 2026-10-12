package store

// TaskCtx(§6.2 任务续传上下文)与 tasks.json 持久化的测试。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fullTaskCtx 全字段非零样例(往返不丢字段的基准)。
func fullTaskCtx() TaskCtx {
	return TaskCtx{
		EngineID:  "eng-7",
		FileName:  "复仇者联盟.mkv",
		PwdID:     "pwd-id-1",
		Stoken:    "stoken-1",
		Passcode:  "pass1",
		FID:       "fid-1",
		Token:     "share-token-1",
		FromShare: true,
		RelDir:    "电影/漫威",
		Dest:      "/dl/电影/漫威/复仇者联盟.mkv",
		Size:      9544303183,
		CreatedAt: 1760000000,
	}
}

// TestTaskCtxCanRefresh 可重取判定矩阵(§6.2 原版 + G12 放宽)。
// 原版:fid 非空 且(非分享 或 token+pwdId+stoken 齐全);
// G12 放宽:passcode 持久化后 share 可用 pwd_id+passcode 重取 stoken,
// 放宽为 fid 非空 且(非分享 或 (pwdId+passcode 齐全 或 token+stoken 齐全))。
func TestTaskCtxCanRefresh(t *testing.T) {
	cases := []struct {
		name string
		ctx  TaskCtx
		want bool
	}{
		// —— 前置门槛:fid 为空一律不可重取(所有模式重取都要 fid)——
		{"fid 空,分享上下文全齐", TaskCtx{PwdID: "p", Stoken: "s", Token: "t", FromShare: true}, false},
		{"fid 空,非分享", TaskCtx{FromShare: false}, false},
		// —— 非分享:fid 在即可 ——
		{"非分享,仅 fid", TaskCtx{FID: "f1", FromShare: false}, true},
		{"非分享,分享字段空串占位", TaskCtx{FID: "f1", PwdID: "", Stoken: "", Token: "", FromShare: false}, true},
		// —— 分享:原版判定 token+pwdId+stoken 齐全 ——
		{"分享,原版三件套齐全", TaskCtx{FID: "f1", PwdID: "p", Stoken: "s", Token: "t", FromShare: true}, true},
		// —— 分享:G12 放宽分支 ——
		{"分享,G12 放宽 pwdId+passcode(无 token/stoken)", TaskCtx{FID: "f1", PwdID: "p", Passcode: "c", FromShare: true}, true},
		{"分享,G12 放宽 token+stoken(无 pwdId)", TaskCtx{FID: "f1", Stoken: "s", Token: "t", FromShare: true}, true},
		{"分享,四件全齐(两分支同时满足)", TaskCtx{FID: "f1", PwdID: "p", Stoken: "s", Passcode: "c", Token: "t", FromShare: true}, true},
		// —— 分享:任何一支都不满足 ——
		{"分享,仅 token", TaskCtx{FID: "f1", Token: "t", FromShare: true}, false},
		{"分享,仅 stoken", TaskCtx{FID: "f1", Stoken: "s", FromShare: true}, false},
		{"分享,仅 passcode(缺 pwdId)", TaskCtx{FID: "f1", Passcode: "c", FromShare: true}, false},
		{"分享,仅 pwdId(缺 passcode)", TaskCtx{FID: "f1", PwdID: "p", FromShare: true}, false},
		{"分享,token+pwdId 缺 stoken 无 passcode", TaskCtx{FID: "f1", PwdID: "p", Token: "t", FromShare: true}, false},
		{"分享,token+passcode 缺 stoken 缺 pwdId", TaskCtx{FID: "f1", Passcode: "c", Token: "t", FromShare: true}, false},
		{"分享,全部上下文空", TaskCtx{FID: "f1", FromShare: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ctx.CanRefresh(); got != tc.want {
				t.Errorf("CanRefresh() = %v, want %v (ctx=%+v)", got, tc.want, tc.ctx)
			}
		})
	}
}

// TestTasksRoundTrip 保存→读回全等(JSON 往返不丢字段)。
func TestTasksRoundTrip(t *testing.T) {
	p := testPaths(t.TempDir())
	ts := []TaskCtx{
		fullTaskCtx(),
		{EngineID: "eng-8", FileName: "a.zip", FID: "f2", FromShare: false, RelDir: "", Dest: "/dl/a.zip", Size: 1024, CreatedAt: 1760000001},
	}
	if err := SaveTasks(p, ts); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	got := LoadTasks(p)
	if len(got) != len(ts) {
		t.Fatalf("任务数: got %d, want %d", len(got), len(ts))
	}
	for i := range ts {
		if got[i] != ts[i] {
			t.Errorf("任务[%d]往返不等:\n got  %+v\n want %+v", i, got[i], ts[i])
		}
	}
}

// TestTasksJSONShape 落盘 JSON 字段名(小驼峰)是 webapi(Task 13)透传给
// 前端的契约,逐一在场。
func TestTasksJSONShape(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := SaveTasks(p, []TaskCtx{fullTaskCtx()}); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	b, err := os.ReadFile(p.TasksFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"engineId", "fileName", "pwdId", "stoken", "passcode",
		"fid", "token", "fromShare", "relDir", "dest", "size", "createdAt",
	} {
		if !strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("落盘 JSON 缺字段 %q: %s", key, b)
		}
	}
	if !json.Valid(b) {
		t.Errorf("落盘内容非法 JSON: %q", b)
	}
}

// TestLoadTasksMissingFile 文件缺失(首次启动)→ 空任务列表,不报错。
func TestLoadTasksMissingFile(t *testing.T) {
	p := testPaths(t.TempDir())
	if got := LoadTasks(p); len(got) != 0 {
		t.Errorf("缺失文件: got %+v, want 空", got)
	}
}

// TestLoadTasksBadFile 坏文件(非法 JSON/空文件/类型错)→ 空任务列表,
// 不挡启动(可用性优先,与 LoadSettings 同款容错)。
func TestLoadTasksBadFile(t *testing.T) {
	cases := map[string]string{
		"非 JSON":  "not json {{{",
		"空文件":     "",
		"字段类型错":   `[{"fid":123}]`,
		"截断 JSON": `[{"fid":"f1"`,
		"顶层对象":    `{"tasks":[]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := testPaths(dir)
			if err := os.WriteFile(p.TasksFile, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := LoadTasks(p); len(got) != 0 {
				t.Errorf("坏文件 %q: got %+v, want 空", content, got)
			}
		})
	}
}

// TestSaveTasksEmpty 空列表落盘为 "[]"(而非 null),读回仍空。
func TestSaveTasksEmpty(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := SaveTasks(p, nil); err != nil {
		t.Fatalf("SaveTasks(nil): %v", err)
	}
	b, err := os.ReadFile(p.TasksFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "[]" {
		t.Errorf("空列表应落盘 []: %q", b)
	}
	if got := LoadTasks(p); len(got) != 0 {
		t.Errorf("读回应空: %+v", got)
	}
}

// TestSaveTasksCreatesDataDir DataDir 不存在 → Save 负责建目录。
func TestSaveTasksCreatesDataDir(t *testing.T) {
	base := t.TempDir()
	p := testPaths(filepath.Join(base, "x", "y"))
	if err := SaveTasks(p, []TaskCtx{{FID: "f"}}); err != nil {
		t.Fatalf("SaveTasks 应自建 DataDir: %v", err)
	}
	if _, err := os.Stat(p.TasksFile); err != nil {
		t.Errorf("落盘文件缺失: %v", err)
	}
}

// TestSaveTasksAtomic 原子性可观察面:换入后不残留 .tmp;内容合法 JSON
// 且换行收尾(人可读,用户可能手编)。
func TestSaveTasksAtomic(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := SaveTasks(p, []TaskCtx{fullTaskCtx()}); err != nil {
		t.Fatalf("SaveTasks: %v", err)
	}
	if _, err := os.Stat(p.TasksFile + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("换入后不应残留 .tmp: %v", err)
	}
	b, err := os.ReadFile(p.TasksFile)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Errorf("落盘内容非法 JSON: %q", b)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Errorf("文本文件应以换行收尾: %q", b)
	}
}

// TestSaveTasksReplacesExisting 覆盖已有文件(rename 原子替换):
// 第二次 Save 后读回即新列表,旧任务不残留(取消/完成的任务从
// tasks.json 摘除靠这一语义)。
func TestSaveTasksReplacesExisting(t *testing.T) {
	p := testPaths(t.TempDir())
	v1 := []TaskCtx{{EngineID: "e1", FID: "f1", Dest: "/dl/old.mkv", CreatedAt: 1}}
	v2 := []TaskCtx{
		{EngineID: "e2", FID: "f2", Dest: "/dl/new.mkv", CreatedAt: 2},
		{EngineID: "e3", FID: "f3", Dest: "/dl/new2.mkv", CreatedAt: 3},
	}
	if err := SaveTasks(p, v1); err != nil {
		t.Fatal(err)
	}
	if err := SaveTasks(p, v2); err != nil {
		t.Fatal(err)
	}
	got := LoadTasks(p)
	if !reflect.DeepEqual(got, v2) {
		t.Errorf("覆盖写: got %+v, want %+v", got, v2)
	}
}

// TestSaveTasksOverwritesStaleTmp 上次崩溃残留的 .tmp 无害:
// O_TRUNC 重写后照常换入。
func TestSaveTasksOverwritesStaleTmp(t *testing.T) {
	p := testPaths(t.TempDir())
	if err := os.WriteFile(p.TasksFile+".tmp", []byte("残骸"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := []TaskCtx{{EngineID: "e1", FID: "f1"}}
	if err := SaveTasks(p, want); err != nil {
		t.Fatalf("残留 .tmp 不应阻碍保存: %v", err)
	}
	if got := LoadTasks(p); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if _, err := os.Stat(p.TasksFile + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("换入后 .tmp 应消失: %v", err)
	}
}
