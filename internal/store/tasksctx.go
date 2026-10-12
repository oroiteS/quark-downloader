package store

// 任务续传上下文(§6.2)——tasks.json 的内存形态与持久化。重启后旧直链
// 必已过期,重取直链需要协议上下文;每个任务的上下文在引擎运行期间由
// 上层(webapi/装配层)持久化到这里,启动时读回交引擎自动续传。

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
)

// TaskCtx 是单个任务的续传上下文(§6.2 字段清单逐字:pwdId/stoken/
// passcode 分享模式才有;fid 所有模式;token 分享内文件的 share
// fid_token;fromShare 是否来自分享;relDir/dest/size 目标路径与大小)。
// json tag(小驼峰)是 webapi(Task 13)透传给前端的契约,不可偏移。
type TaskCtx struct {
	EngineID  string `json:"engineId"`  // 引擎侧任务标识(恢复对账用)
	FileName  string `json:"fileName"`  // 展示文件名
	PwdID     string `json:"pwdId"`     // 分享 pwd_id(分享模式)
	Stoken    string `json:"stoken"`    // 分享 stoken(分享模式)
	Passcode  string `json:"passcode"`  // 分享提取码(分享模式,G12 后持久化)
	FID       string `json:"fid"`       // 文件 fid(所有模式)
	Token     string `json:"token"`     // 分享内文件的 share fid_token(分享模式)
	FromShare bool   `json:"fromShare"` // 是否来自分享
	RelDir    string `json:"relDir"`    // 分享内相对目录(重进分享浏览位置)
	Dest      string `json:"dest"`      // 目标文件绝对路径
	Size      int64  `json:"size"`      // 文件总字节
	CreatedAt int64  `json:"createdAt"` // 任务创建时间(unix 秒)
	// Exists 提交前已检测到 dest 存在同名完整文件(size 相符,G11「已存在
	// 跳过」):未提交引擎,EngineID 为空;GET /api/tasks(Task 10)把它透出
	// 为「已存在」终态。json tag 是 webapi 透传契约。
	Exists bool `json:"exists,omitempty"`
	// Mode 落盘模式("a"/"b",§3.5):POST /api/tasks 落盘,retry 复活
	// (Task 10)与重启恢复(Task 11)读回——DTO 的 mode 字段与重走创建
	// 路径都需要,而 §6.2 原字段清单无此项(控制者裁决追加,Exists 同例
	// 的最小扩展)。空值按 "a" 缺省语义读回(旧数据兼容)。
	Mode string `json:"mode,omitempty"`
	// NeedsReparse 恢复链的如实标注(Task 11,控制者裁决 Ruling T11-R1
	// 追加,Exists/Mode 同例):CanRefresh=false(上下文不足以重取直链)
	// 或恢复期重取直链失败的条目置真——GET /api/tasks 透出
	// needs_reparse 终态,绝不静默(Review Focus #3:磁盘无完整文件而
	// 显示「完成」正是红线要防的静默失败形态)。用户 retry 复活或重新
	// 解析创建成功后随 TaskCtx 覆盖而消除。
	NeedsReparse bool `json:"needsReparse,omitempty"`
}

// CanRefresh 可重取判定。§6.2 原版逐字:fid 非空 且(非分享 或
// (token+pwdId+stoken 齐全));G12 改进:passcode 持久化后,share 可用
// pwd_id+passcode 重新走解析拿新 stoken——判定放宽为:
//
//	fid 非空 且(非分享 或 (pwdId+passcode 齐全 或 token+stoken 齐全))
//
// 即分享模式两支任一齐全即可重取(G12)。注意 §6.2 警告仍然成立:
// 免转存任务(stoken 一次性)过期后即便 CanRefresh 为真,重取也依赖
// 重新解析分享,失败提示须如实说「需重新解析分享」——那是引擎/上层
// 提示语的事,本判定只回答「上下文字面上够不够重取」。
func (t TaskCtx) CanRefresh() bool {
	if t.FID == "" {
		return false
	}
	if !t.FromShare {
		return true
	}
	return (t.PwdID != "" && t.Passcode != "") || (t.Token != "" && t.Stoken != "")
}

// LoadTasks 读 p.TasksFile(tasks.json)。容错与 LoadSettings 同款:
// 缺失(首次启动)/坏文件 → 空列表,不报错——读不出上下文只意味着
// 旧任务不自动续传,不该挡启动(可用性优先)。
func LoadTasks(p Paths) []TaskCtx {
	b, err := os.ReadFile(p.TasksFile)
	if err != nil {
		return nil // 不存在或不可读 → 无任务
	}
	var ts []TaskCtx
	if err := json.Unmarshal(b, &ts); err != nil {
		// 坏 JSON → 无任务(下次 Save 覆盖自愈)。自愈是可用性优先,但
		// 静默会让全部旧任务的续传上下文消失而无从察觉——记 Warn
		// 如实相告(T4 遗留 deferred,T11 恢复链首个消费前补)。
		slog.Warn("feiwu: tasks.json 损坏,旧任务不自动续传(下次保存覆盖自愈)",
			"file", p.TasksFile, "err", err)
		return nil
	}
	return ts
}

// tasksMu 把「写 tmp → rename 换入」绑成不可分事务(沿用 SaveSettings/
// qmeta 模式):webapi 侧任务状态变迁可能并发触发保存,同一进程内不
// 交叉写同一个 tmp。跨进程互斥不提供——单实例应用。
var tasksMu sync.Mutex

// SaveTasks 把 ts 原子写入 p.TasksFile(tmp+Sync+rename,沿用
// SaveSettings 的写入协议:数据先落盘再一步换目录项,断电时要么旧
// 文件完好、要么新内容完整)。DataDir 不存在则先建(MkdirAll)。
// 空列表落盘为 "[]"(手编友好,也避免 nil 序列化成 null)。
func SaveTasks(p Paths, ts []TaskCtx) error {
	if ts == nil {
		ts = []TaskCtx{}
	}
	b, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n') // 文本文件换行收尾(用户可能手编)

	tasksMu.Lock()
	defer tasksMu.Unlock()

	if err := os.MkdirAll(p.DataDir, 0o755); err != nil {
		return err
	}
	tmp := p.TasksFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // 数据先落盘,再换目录项
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p.TasksFile)
}
