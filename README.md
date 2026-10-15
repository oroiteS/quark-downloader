# 飞舞 (feiwu)

夸克网盘下载器（Go 实现）。

## 安装

Homebrew（macOS）：

```bash
brew install --cask oroiteS/tap/feiwu
```

也可从 [Releases](https://github.com/oroiteS/quark-downloader/releases) 手动下载：

- macOS：`feiwu-vX.Y.Z-macos-universal.dmg`（未签名，首次打开需右键 → 打开）
- Windows：`feiwu-windows-amd64-portable.zip`（解压即用）

## 构建与开发

前端源码在 [web/](web/)（React + Vite，pnpm 管理）：

```bash
cd web
pnpm install          # 安装依赖
pnpm dev              # 开发服务器，/api 代理到 127.0.0.1:45678 的后端
pnpm test             # 全量测试（Vitest）
pnpm build            # 先跑 typecheck（prebuild 链），产物直落 internal/webapi/web/dist
```

后端为 Go 单二进制，前端产物经 `//go:embed`（internal/webapi/static.go）打进二进制。桌面直接运行即打开 Wails 原生窗口；窗口模式构建需 `-tags production`（Wails 硬要求，缺 tag 时窗口拒绝启动：macOS 报错并以非零码退出，Windows 弹英文错误框后无窗口静默退出）：

```bash
go build -tags production -o feiwu ./cmd/feiwu
./feiwu
```

纯服务端模式（不开窗口）加 `FEIWU_NO_WINDOW=1`，配合 `pnpm dev` 在浏览器里开发前后端联调；dev 模式前后端缺省端口已对齐（45678），双端都不再需要设 `FEIWU_PORT`：

```bash
FEIWU_NO_WINDOW=1 go run ./cmd/feiwu
```

设置页的「选择目录…」按钮走 `POST /api/fs/pick-dir`：由后端调系统原生目录选择对话框（macOS 访达 / Windows FolderBrowserDialog / Linux zenity）返回绝对路径，未支持平台或对话框命令缺失返回 503 `fs_picker_unsupported`。

## CI 与自动打包

- `.github/workflows/ci.yml`：push main / PR 跑 Go 全量测试、前端测试与嵌入产物构建检查。
- `.github/workflows/release.yml`：三种入口（经 plan job 裁决）——
  ① push `v*` tag 按该 tag 打包发布；② **push main 且 ci 全绿 → 自动 bump
  patch tag（v0.1.0 起）并发布 Release**（workflow_run 联动，无需手动打 tag）；
  ③ 手动 dispatch 仅打包出 artifacts。产物：Windows portable zip
  （`feiwu-windows-amd64-portable.zip`，CGO=0 交叉编译；内含 `feiwu.exe` 与
  `feiwu.portable` 标记，数据/Cookie 全落 exe 旁 `./data/`，免安装）+
  macOS universal dmg（`feiwu-vX.Y.Z-macos-universal.dmg`，arm64+amd64 双原生
  构建 lipo 合并，内含带图标的 `feiwu.app`）。dmg 未签名未公证，首次打开需
  右键 → 打开。

运行时环境变量（均可选）：

- `FEIWU_PORT`：监听端口，缺省 `127.0.0.1:45678`（固定高位，端口占用时自动回退随机端口并在启动日志告警；显式指定被占则启动报错）。实际地址打印在启动日志。
- `FEIWU_DATA_DIR`：数据目录，优先级最高；缺省走平台默认目录（Windows 另有 portable 判据）。
- `FEIWU_NO_WINDOW=1`：无窗口模式（纯服务端），仅 `=1` 时生效；桌面默认打开 Wails 原生窗口。
- `FEIWU_OPEN_BROWSER=1`：无窗口模式下启动后自动打开浏览器（窗口模式下忽略，不跳浏览器）。
- `FEIWU_NO_BROWSER=1`：**已废弃**（保留兼容，仅无窗口模式生效）：跳过浏览器打开。

行为矩阵：

| 条件 | 行为 |
|------|------|
| 默认（桌面） | Wails 窗口，无浏览器 |
| `FEIWU_NO_WINDOW=1` | 纯服务端 + 按 `FEIWU_OPEN_BROWSER` 决定是否 open |
| 窗口初始化失败 | slog.Error + 非零退出（不 fallback 浏览器） |
| Ctrl-C / SIGTERM | 触发退出；有未完成任务时同样弹关窗确认，取消则本次退出被阻止 |

## App 图标

![飞舞 App 图标](assets/icon.png)

图标（[assets/icon.png](assets/icon.png)，1254×1254 PNG）由 **gpt-image-2** 模型生成：先以提示词 ① 生成初版，再以现有图标为参考图、用提示词 ② 将背景更换为纯白色，得到当前版本。

### 提示词 ①：初版生成

```text
设计一个正方形 1:1 的 App 图标，主角是一只圆润肥硕的卡通小猪正在开心地跳舞。

主体：小猪通体是温暖的皮肤色（肉色调 #F5C9A7），身材肥胖浑圆，挺着微微颤动的大肚子，短小的前蹄一只高举过头顶、另一只向侧后方伸展，一条后蹄点地、另一条腾空踢起，全身随舞步向一侧倾斜，大耳朵欢快地扇动，细卷的尾巴绕成一个圆圈；眯眼大笑，嘴角咧开露出两颗小龅牙，两颊有粉嫩的腮红。

构图：主体居中，占据画面约 65%，四周保留充足负空间；背景为奶油橙到浅杏色的柔和径向渐变，小猪脚下有一枚柔和的椭圆落影。

风格与材质：现代 3D 吉祥物渲染，哑光软胶 vinyl 质感，表面带细腻次表面散射，轮廓圆润无锐角；摄影棚顶部柔光箱主光 + 背后环形轮廓光，高光柔和过渡。

输出：1024×1024，高清锐利，成品级应用图标质感，适合在桌面与手机上直接使用。

避免：画面出现任何文字、字母、数字或水印；出现第二只角色、多余肢体或畸形蹄部；廉价高反光塑料感与杂乱背景元素。
```

### 提示词 ②：背景更换（当前版本）

```text
【BACKGROUND CHANGE ONLY】

以下附加的图标图片是最高优先级参考图与唯一事实来源。

保留（Preserve）：画面中央跳舞小猪的一切细节——温暖的皮肤色（#F5C9A7）、肥胖浑圆的体型与微微颤动的大肚子、一只前蹄高举过头顶另一只向侧后方伸展的舞姿、腾空踢起的后蹄、眯眼大笑的表情与两颗小龅牙、粉嫩腮红、扇动的大耳朵与卷成圆圈的尾巴、哑光软胶 3D 质感与柔和高光，以及主体在画面中的大小与居中构图——全部保持原样，不得重绘、不得改变姿势、表情与配色。

只修改（Change）：仅替换背景。将奶油橙到浅杏色的径向渐变背景替换为纯净的纯白色（#FFFFFF）背景；小猪脚下保留一枚浅灰色柔和椭圆落影，使其在白色背景上依然自然。

避免：改变小猪的任何细节、姿势或表情；画面出现文字、字母、数字或水印；加入边框、渐变、纹理、杂点或其他背景元素。
```

## 参考仓库

本项目参考仓库：<https://github.com/huaotem-bot/gezi-quark-downloader>
