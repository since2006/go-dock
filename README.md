# go-dock

把 **Claude Code** 和 **Codex** 的套餐额度、token 用量和任务状态（进行中、需授权、已完成……）显示在一块 USB 小屏上，同时提供本地网页管理台。

- 屏幕：微雪 [ESP32-S3-Touch-AMOLED-2.41-B](https://www.waveshare.net/shop/ESP32-S3-Touch-AMOLED-2.41-B.htm)（600×450 AMOLED，触摸）
- 电脑端：Go 编写的单个可执行文件，支持 macOS / Windows / Linux，网页通过 `go:embed` 内嵌

```
go-dock/
├── host/        Go 电脑端：采集、状态汇总、USB 通信、网页管理台
├── firmware/    ESP32 固件（尚未开始，见 firmware/README.md）
└── docs/
    ├── protocol-v1.md   电脑端与屏幕之间的通信协议
    └── plans/           实现计划
```

## 能看到什么

| 内容 | 来源 |
|---|---|
| Claude 5 小时 / 7 天额度与重置时间 | Claude Code 状态栏数据中的官方 `rate_limits`（需安装「状态栏」集成） |
| Codex 5 小时 / 7 天额度 | Codex 会话日志中的 `rate_limits`，以及定期调用 `codex app-server` |
| 今日 / 近 7 天 token、API 等价费用 | 本地会话记录（Claude 按官方价格估算费用；Codex 只统计 token） |
| 每个会话的状态 | hooks 实时上报；没装 hooks 时读取 Claude 的会话状态文件和 Codex 的会话日志兜底 |

同时运行多个任务时，屏幕和网页按 **需授权 > 等待输入 > 出错 > 进行中 > 已完成** 排序；屏幕每页 4 条，滑动翻页。

go-dock **不读取任何登录凭据**，也不调用非公开接口。

## 编译

需要 Go 1.26 或更新版本。

```sh
cd host
go build -o bin/go-dock ./cmd/go-dock
```

macOS 本机编译会默认启用 cgo，可以按 USB VID/PID 识别屏幕。交叉编译时（`CGO_ENABLED=0`）macOS 版改为按 `/dev/cu.usbmodem*` 端口名识别，功能不受影响。

> 请把可执行文件放在固定位置再安装集成：hooks 里记录的是它的绝对路径。用 `go run` 运行时不能安装集成。

## 运行

```sh
./bin/go-dock            # 启动后台服务并打开 http://127.0.0.1:47321
./bin/go-dock --no-browser
```

| 页面 | 用途 |
|---|---|
| 概览 | 两家额度、今日 token、所有会话 |
| 设备预览 | 按 600×450 渲染屏幕效果，并显示实际下发给屏幕的数据；没有硬件时也能调试 |
| 集成 | 一键安装 / 卸载 hooks 和状态栏包装 |
| 设置 | 串口、显示时长、各采集源开关等 |
| 日志 | 实时日志，可按级别过滤 |

配置和日志保存在系统配置目录下的 `go-dock/`，可用环境变量 `GO_DOCK_HOME` 改到别处：

- macOS：`~/Library/Application Support/go-dock/`
- Linux：`~/.config/go-dock/`
- Windows：`%AppData%\go-dock\`

## 集成

在网页「集成」页安装。每次修改前，原文件都会备份为 `<文件>.go-dock-<时间>.bak`；卸载会精确移除 go-dock 自己的条目，其他配置原样保留。

| 集成 | 修改的文件 | 作用 |
|---|---|---|
| Claude Code hooks | `~/.claude/settings.json` 的 `hooks` | 会话开始、提问、工具调用、授权请求、完成、出错时立即上报。「需授权」能立刻显示，并带出工具名和命令 |
| Claude Code 状态栏 | `~/.claude/settings.json` 的 `statusLine` | 读取官方 5h / 7d 额度。原有状态栏命令（例如 claude-hud）会被包装起来继续调用，显示效果不变 |
| Codex hooks | `~/.codex/hooks.json` | 提问、工具调用、授权请求、完成、中断时立即上报。**安装后需在 Codex 里运行 `/hooks`，信任这些 hooks 才会生效** |

- 新安装的集成从**新开的会话**开始生效。
- 如果移动了 go-dock 程序或修改了监听端口，「集成」页会提示「需更新」，点「重新安装」即可。
- 遵守 `CLAUDE_CONFIG_DIR` 和 `CODEX_HOME` 环境变量，也可以在设置页里手动指定目录。

## 安全

- 管理台默认只监听 `127.0.0.1`；会校验 Host 头，并拒绝跨站请求和非 JSON 的写请求。
- hooks 上报的提问和工具参数只保存在内存里，不会写进 info 级别日志。

## 开发

```sh
cd host
go vet ./...
go test -race ./...
```

修改通信协议时，必须同时修改 `docs/protocol-v1.md` 和 `host/internal/device/protocol.go`。更多约定见 [AGENTS.md](AGENTS.md)。
