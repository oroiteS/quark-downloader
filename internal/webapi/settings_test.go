// settings_test.go GET|PUT /api/settings 的行为契约（Task 13）：往返、
// 非法 theme/档位 400、destDir 热应用同步 manager（Ruling T13-R1）、
// threads/max 即时生效于后续预算（Ruling T13-R2 的 webapi 侧）、并发
// GET/PUT -race 冒烟。
package webapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"quark-downloader/internal/protocol"
	"quark-downloader/internal/store"
)

// settingsTestPaths 带可落盘 SettingsFile 的 Paths（PUT 持久化断言用）。
func settingsTestPaths(t *testing.T) store.Paths {
	t.Helper()
	dir := t.TempDir()
	return store.Paths{
		DataDir:      dir,
		TasksFile:    filepath.Join(dir, "tasks.json"),
		SettingsFile: filepath.Join(dir, "settings.json"),
	}
}

// TestSettingsGetPutRoundtrip GET 现值 → PUT 新值（200 回显）→ GET 往返；
// 新值落盘（LoadSettings 复读）。
func TestSettingsGetPutRoundtrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	paths := settingsTestPaths(t)
	fm := &fakeManager{}
	app := newTasksTestApp(t, &fakeUpstream{}, fm, store.Settings{
		DestDir: "/tmp/old", Threads: 32, MaxConcurrentTasks: 3, ThemeLock: "",
	}, paths)
	r := app.Router()

	w := doReq(t, r, http.MethodGet, "/api/settings", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d, 期望 200", w.Code)
	}
	var got store.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析 GET 响应: %v", err)
	}
	if got.DestDir != "/tmp/old" {
		t.Fatalf("GET destDir = %q, 期望初始值", got.DestDir)
	}

	put := store.Settings{
		DestDir: "/tmp/new", Threads: 16, MaxConcurrentTasks: 5, ThemeLock: "dark",
	}
	w = doReq(t, r, http.MethodPut, "/api/settings", put)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	w = doReq(t, r, http.MethodGet, "/api/settings", nil)
	var got2 store.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &got2); err != nil {
		t.Fatalf("复读 GET: %v", err)
	}
	if got2 != put {
		t.Fatalf("往返 = %+v, 期望 %+v", got2, put)
	}
	// 落盘可复读（重启语义）。
	if disk := store.LoadSettings(paths); disk != put {
		t.Fatalf("磁盘 = %+v, 期望 %+v", disk, put)
	}
}

// TestSettingsPutInvalidTheme 非法 theme → 400 invalid_settings，原值不动。
func TestSettingsPutInvalidTheme(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, &fakeUpstream{}, &fakeManager{}, store.Settings{
		DestDir: "/tmp/old", Threads: 32, MaxConcurrentTasks: 3,
	}, settingsTestPaths(t))
	r := app.Router()

	w := doReq(t, r, http.MethodPut, "/api/settings", map[string]any{
		"destDir": "/tmp/x", "threads": 8, "maxConcurrentTasks": 2, "themeLock": "blue",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400（body=%s）", w.Code, w.Body)
	}
	var eb struct {
		Error struct{ Code string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &eb)
	if eb.Error.Code != "invalid_settings" {
		t.Fatalf("code = %q, 期望 invalid_settings", eb.Error.Code)
	}
	w = doReq(t, r, http.MethodGet, "/api/settings", nil)
	var got store.Settings
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.DestDir != "/tmp/old" {
		t.Fatalf("非法 PUT 后 destDir = %q, 期望原值不动", got.DestDir)
	}
}

// TestSettingsPutInvalidRange threads 越界 / max 越界 / 空 destDir → 400。
func TestSettingsPutInvalidRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, &fakeUpstream{}, &fakeManager{}, store.Settings{
		DestDir: "/tmp/old", Threads: 32, MaxConcurrentTasks: 3,
	}, settingsTestPaths(t))
	r := app.Router()

	for _, body := range []map[string]any{
		{"destDir": "/tmp/x", "threads": 0, "maxConcurrentTasks": 2},
		{"destDir": "/tmp/x", "threads": 999, "maxConcurrentTasks": 2},
		{"destDir": "/tmp/x", "threads": 8, "maxConcurrentTasks": 99},
		{"destDir": "", "threads": 8, "maxConcurrentTasks": 2},
	} {
		w := doReq(t, r, http.MethodPut, "/api/settings", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body=%v 状态码 = %d, 期望 400", body, w.Code)
		}
	}
}

// TestSettingsPutHotApplyDestDir PUT 成功后 manager.SetDestDir 被同步调用
// （Ruling T13-R1：webapi absDest 与引擎 Join(cfg.DestDir) 两侧一致）。
func TestSettingsPutHotApplyDestDir(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fm := &fakeManager{}
	app := newTasksTestApp(t, &fakeUpstream{}, fm, store.Settings{
		DestDir: "/tmp/old", Threads: 32, MaxConcurrentTasks: 3,
	}, settingsTestPaths(t))

	w := doReq(t, app.Router(), http.MethodPut, "/api/settings", map[string]any{
		"destDir": "/tmp/hot", "threads": 8, "maxConcurrentTasks": 2, "themeLock": "",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d, 期望 200", w.Code)
	}
	fm.mu.Lock()
	calls := append([]string(nil), fm.setDestDirs...)
	fm.mu.Unlock()
	if len(calls) != 1 || calls[0] != "/tmp/hot" {
		t.Fatalf("SetDestDir 调用 = %v, 期望 [/tmp/hot]", calls)
	}
}

// TestSettingsThreadsAffectBudget PUT threads/max 后, 后续 POST /api/tasks
// 的预算档位即时用新值（Ruling T13-R2 的 webapi 侧：cap 读 settings）。
func TestSettingsThreadsAffectBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	up := &fakeUpstream{
		links: map[string]string{"fa": "https://dl.example.com/fa"},
		sort: map[string][]protocol.FileEntry{
			"d0": { // fids 传目录 fid（POST /api/tasks 现契约：递归展开）
				{FID: "fa", FileName: "a.bin", Size: 1000},
			},
		},
	}
	fm := &fakeManager{submitF: func(call fakeSubmitCall) (string, error) {
		return "t1", nil
	}}
	app := newTasksTestApp(t, up, fm, store.Settings{
		DestDir: t.TempDir(), Threads: 32, MaxConcurrentTasks: 3,
	}, settingsTestPaths(t))
	r := app.Router()

	w := doReq(t, r, http.MethodPut, "/api/settings", map[string]any{
		"destDir": app.settings.DestDir, "threads": 4, "maxConcurrentTasks": 2, "themeLock": "",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d, 期望 200", w.Code)
	}
	w = doReq(t, r, http.MethodPost, "/api/tasks", map[string]any{
		"source": "drive", "fids": []string{"d0"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/tasks 状态码 = %d, 期望 200（body=%s）", w.Code, w.Body)
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.submits) == 0 {
		t.Fatal("fake manager 未收到 Submit")
	}
	if th := fm.submits[0].Threads; th != 4 {
		t.Fatalf("Submit Threads = %d, 期望热应用后的 4", th)
	}
}

// TestSettingsConcurrentGetPut 并发 GET/PUT -race 冒烟（settingsSnapshot
// 读写锁纪律）。
func TestSettingsConcurrentGetPut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app := newTasksTestApp(t, &fakeUpstream{}, &fakeManager{}, store.Settings{
		DestDir: "/tmp/old", Threads: 32, MaxConcurrentTasks: 3,
	}, settingsTestPaths(t))
	r := app.Router()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			doReq(t, r, http.MethodGet, "/api/settings", nil)
		}()
		go func(i int) {
			defer wg.Done()
			doReq(t, r, http.MethodPut, "/api/settings", map[string]any{
				"destDir": "/tmp/n", "threads": 8, "maxConcurrentTasks": 2, "themeLock": "light",
			})
		}(i)
	}
	wg.Wait()
}
