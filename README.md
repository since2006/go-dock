# go-dock

把 **Claude Code** 和 **Codex** 的套餐额度、token 用量和任务状态（进行中、需授权、已完成……）显示在一块 USB 小屏上，同时提供本地网页管理台。

- 屏幕：微雪 [ESP32-S3-Touch-AMOLED-2.41-B](https://www.waveshare.net/shop/ESP32-S3-Touch-AMOLED-2.41-B.htm)（600×450 AMOLED，触摸），**需要 V2 版硬件**
- 电脑端：Go 编写的单个可执行文件，支持 macOS / Windows / Linux，网页通过 `go:embed` 内嵌

```
go-dock/
├── host/        Go 电脑端：采集、状态汇总、USB 通信、网页管理台
├── firmware/    屏幕固件：ESP-IDF + LVGL，适用于 V2 板
└── docs/
    ├── protocol-v1.md   电脑端与屏幕之间的通信协议
    └── plans/           实现计划
```

## 能看到什么

| 内容 | 来源 |
|---|---|
| Claude 5 小时 / 7 天额度与重置时间 | Claude Code 状态栏数据中的官方 `rate_limits`（需安装「状态栏」集成） |
| Codex 5 小时 / 7 天额度 | Codex 会话日志中的 `rate_limits`，以及定期调用 `codex app-server` |
| 今日 / 近 7 天 token、API 等价费用 | 本地会话记录。token 数 = 输入 + 缓存写入 + 输出，不含缓存读取（每轮重复读取同一段上下文，算进去会虚高上百倍）；费用按官方价格估算，包含缓存读取。Codex 只统计 token |
| 每个会话的状态 | hooks 实时上报；没装 hooks 时读取 Claude 的会话状态文件和 Codex 的会话日志兜底（Codex 的「需授权」只有 hooks 能报，会话日志里不记录） |
| 会话名、AI 最新回复、当前动作 | 会话名取 `/rename` 改的名字，否则用自动生成的标题（Codex 还没生成时用第一次提问）；AI 最新回复取本轮最新一段回复的第一行（就是 Claude Code 里 ● 开头的那句），都从本地会话记录读取，不需要 hooks。当前动作：Claude 来自 hooks，Codex 没装 hooks 时也能从会话日志读到正在跑的命令、改的文件 |
| 子代理（子任务） | Claude Code 用 Agent 工具派出的子代理、Codex 派生的子线程，分组显示在所属会话下面：类型、任务描述、当前动作、状态、已运行时间；子代理等待授权时整个会话显示「需授权」 |

同时运行多个任务时，屏幕和网页按 **需授权 > 等待输入 > 出错 > 进行中 > 已完成** 排序，子 agent 缩进显示在所属会话下面；屏幕一页放不下时上下滑动翻页。主会话派出后台子 agent 后自己先停下时，显示「进行中 · 等待 N 个子任务完成」，而不是「已完成」。

go-dock **不读取任何登录凭据**，也不调用非公开接口。

## 安装

从 [Releases](https://github.com/since2006/go-dock/releases) 下载对应平台的程序（如 `go-dock_darwin_arm64`），放到一个固定位置：

```sh
mkdir -p ~/.local/bin
curl -L -o ~/.local/bin/go-dock https://github.com/since2006/go-dock/releases/latest/download/go-dock_darwin_arm64
chmod +x ~/.local/bin/go-dock
```

> **macOS 请用上面的 `curl` 命令下载。** 浏览器下载的文件会被 macOS 打上隔离标记，运行时提示无法验证开发者、可能不安全，并拒绝打开。如果用浏览器下载，运行 `xattr -d com.apple.quarantine <文件路径>` 去掉标记即可。之后的在线升级由程序自己下载，不会再出现这个提示。

hooks 和开机自启记录的都是程序的绝对路径，所以装好后不要再挪动它。如果挪了，到网页上重新安装集成、重新开启开机自启即可。

## 从源码编译

需要 Go 1.26 或更新版本。

```sh
cd host
go build -o bin/go-dock ./cmd/go-dock
```

macOS 本机编译会默认启用 cgo，可以按 USB VID/PID 识别屏幕。交叉编译时（`CGO_ENABLED=0`）macOS 版改为按 `/dev/cu.usbmodem*` 端口名识别，功能不受影响。

> 用 `go run` 运行时不能安装集成，也不能开启开机自启。自己编译的程序版本号为 `dev`，不能在线升级；需要时可加上 `-ldflags "-X main.version=0.1.0"`。

## 运行

```sh
go-dock                  # 启动后台服务并打开 http://127.0.0.1:47321
go-dock --no-browser     # 不打开浏览器
go-dock run --background # 后台运行：不打开浏览器、不输出到终端
```

| 页面 | 用途 |
|---|---|
| 概览 | 两家额度、今日 token、所有会话 |
| 设备预览 | 按 600×450 渲染屏幕效果，并显示实际下发给屏幕的数据；没有硬件时也能调试 |
| 集成 | 一键安装 / 卸载 hooks 和状态栏包装 |
| 插件 | MQTT broker（可以有多个）和功能页插件（目前是「灯光」）的配置，可直接开关灯 |
| 设置 | 串口、显示时长、各采集源开关等 |
| 日志 | 实时日志，可按级别过滤 |

配置和日志保存在系统配置目录下的 `go-dock/`，可用环境变量 `GO_DOCK_HOME` 改到别处：

- macOS：`~/Library/Application Support/go-dock/`
- Linux：`~/.config/go-dock/`
- Windows：`%AppData%\go-dock\`

## 开机自启

在网页「设置 → 系统」里打开「开机自动启动」，或运行 `go-dock autostart on`（`off` 关闭、`status` 查看）。打开后从**下次登录**开始生效，不会马上再启动一个 go-dock。

| 系统 | 方式 |
|---|---|
| macOS | `~/Library/LaunchAgents/io.github.since2006.go-dock.plist` |
| Linux | 有 systemd 时用用户服务 `~/.config/systemd/user/go-dock.service`，否则用 `~/.config/autostart/go-dock.desktop` |
| Windows | 注册表 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 中的 `go-dock` |

开机自启以 `run --background` 运行。在 Windows 上，登录时命令行窗口会闪一下然后关闭。

## 在线升级

go-dock 每天检查一次 GitHub Releases（可在设置里关掉），有新版本时网页顶部会出现提示。点「升级并重启」后的流程：
1. 下载当前平台的程序。
2. 用 `SHA256SUMS` 校验，并运行一次新程序确认版本号正确。
3. 原地替换后自动重启，几秒后网页重新连上。

默认只推送正式版。想更早用到新功能，可以在「设置」里打开「接收测试版（beta）」，之后测试版（如 `v0.3.0-beta.1`）和正式版里版本号更高的那个都会推送。测试版可能不稳定，关掉开关后会回到只推送正式版（已经装上的测试版不会自动降级，等下一个正式版发布时再升级过去）。

升级不会改变程序路径，所以已安装的 hooks 和开机自启不受影响。如果程序所在目录没有写权限（例如放在 `/usr/local/bin` 且属于 root），会提示手动替换。

## 屏幕固件

固件在 [firmware/](firmware/README.md)，**只支持 V2 版硬件**（V1、V2 引脚不同，固件不通用）。板子丝印有「Rev2.0」，或外壳贴有「V2」标签的就是 V2。

最简单的烧录方式是从 [Releases](https://github.com/since2006/go-dock/releases) 下载 `go-dock-firmware-v2.bin`，然后：

```sh
pip install esptool
esptool.py --chip esp32s3 write_flash 0x0 go-dock-firmware-v2.bin
```

烧录前先退出 go-dock（或在设置里取消「启用 USB 屏幕」），否则串口被占用会烧录失败。从源码编译、硬件说明见 [firmware/README.md](firmware/README.md)。

**提示音**：有任务等你授权或回答时，屏幕可以响铃提醒（默认开启，设置里可以关掉、选音色、试听）。除了「叮咚」「嘀嘀」这类，还有一组 8-bit 风格的原创音效（拾取、升级、过关、跳跃、警笛）；选「自定义」可以自己写音符，比如 `E7 100, - 40, C8 300`。板子本身没有喇叭，要在排针上接一个几块钱的无源蜂鸣器，接法见 [firmware/README.md](firmware/README.md#蜂鸣器提醒)。

这样用 USB 刷一次之后，固件就能在网页上升级：固件版本跟着 go-dock 走，go-dock 升级后，如果设备上的固件更旧，网页顶部会提示「升级固件」，点一下就通过 USB 发给设备，约 1 分钟，屏幕上显示进度。出错或中途拔线时设备继续用原来的固件。详见 [firmware/README.md](firmware/README.md#在线升级)。

## 集成

在网页「集成」页安装。每次修改前，原文件都会备份为 `<文件>.go-dock-<时间>.bak`；卸载会精确移除 go-dock 自己的条目，其他配置原样保留。

| 集成 | 修改的文件 | 作用 |
|---|---|---|
| Claude Code hooks | `~/.claude/settings.json` 的 `hooks` | 会话开始、提问、工具调用、授权请求、完成、出错、子代理开始与结束时立即上报。「需授权」能立刻显示，并带出工具名和命令；运行中的命令显示 Claude 为它写的说明（和 Claude Code 界面里命令上方那行一样） |
| Claude Code 状态栏 | `~/.claude/settings.json` 的 `statusLine` | 读取官方 5h / 7d 额度。原有状态栏命令（例如 claude-hud）会被包装起来继续调用，显示效果不变 |
| Codex hooks | `~/.codex/hooks.json` | 提问、工具调用、授权请求、完成、中断时立即上报。**安装后需在 Codex 里运行 `/hooks`，信任这些 hooks 才会生效** |

- 新安装的集成从**新开的会话**开始生效。
- 如果移动了 go-dock 程序、修改了监听端口，或者新版本增加了要监听的事件（例如子代理开始与结束），「集成」页会提示「需更新」，点「重新安装」即可。
- 遵守 `CLAUDE_CONFIG_DIR` 和 `CODEX_HOME` 环境变量，也可以在设置页里手动指定目录。

## 插件：MQTT 与灯光

屏幕上 Agent 页的右边可以放功能页，左右滑动切换。功能页由 go-dock 里的插件生成：插件在电脑上运行，通过 MQTT 控制设备；屏幕只负责显示和把点击传回来，本身不联网，也不保存任何密码。新的功能页一般不需要重新编译固件。

MQTT broker 可以配多个（比如办公室一个、家里一个、云上一个），每个一条连接，插件里的每个设备自己选用哪个；某个 broker 断开时，只有用它的设备变灰。

> 屏幕上的功能页需要新版固件（在网页上就能升级），旧固件只显示 Agent 页。网页「设备预览」也能左右切到功能页，点卡片同样会真的发命令。电脑关机或睡眠时，屏幕也就控制不了灯。

第一个插件是「灯光」：开灯、关灯、显示当前状态。在网页「插件」页里：

1. 在「MQTT broker」里添加 broker：名称、地址，有账号的话填用户名和密码，保存后状态变成「已连接」。地址可以只写 `192.168.1.10`（默认端口 1883），TLS 用 `mqtts://host:8883`。最多 8 个。
2. 添加灯：名称、命令主题，以及开灯 / 关灯时发送的内容（默认 `ON` / `OFF`），勾选「在屏幕上显示『灯光』页」后保存。最多 6 盏。有多个 broker 时，每盏灯选自己用哪个；只有一个时不用选。
3. 填了状态主题，显示的是灯实际回报的状态：点击后先显示「切换中…」，收到回报再变成「已开」「已关」，5 秒没回报显示「未响应」。不填状态主题，就按最后一次发出的命令显示。

状态是 JSON 时在「更多选项」里填状态字段（如 `state`），JSON 里的 `true` / `false` 会直接识别。填了查询主题，每次连上 broker 都会发一次查询，让灯报告当前状态，免得启动后一直显示「未知」。常见设备的填法（按各项目文档整理，还没在真实设备上验证）：

| 设备 | 命令主题 | 开 / 关时发送 | 状态主题 | 状态字段 | 查询主题和内容 |
|---|---|---|---|---|---|
| Tasmota | `cmnd/<设备>/POWER` | `ON` / `OFF` | `stat/<设备>/POWER` | 不填 | `cmnd/<设备>/POWER`，内容留空 |
| Zigbee2MQTT | `zigbee2mqtt/<名称>/set` | `{"state":"ON"}` / `{"state":"OFF"}` | `zigbee2mqtt/<名称>` | `state` | `zigbee2mqtt/<名称>/get`，内容 `{"state":""}` |

配置保存在 `config.json` 的 `mqtt.brokers` 和 `plugins.lights` 两节。灯的 `broker` 填 broker 的 `id`，留空表示第一个 broker；删除还有灯在用的 broker 时会提示先把灯改到别的 broker。

排查问题看「日志」页：发出的开关命令、查询，以及收到的状态都会记下主题和报文（JSON 按原样显示）；状态对不上时会提示「无法识别灯的状态」，并列出收到的报文和当前的状态字段、状态值。同一个状态重复上报只记在 debug 级别，免得刷屏。

### 空调（红外）

第二个插件是「空调」：用屏幕外接的 38 kHz 红外发射模块，像遥控器一样控制格力空调，屏幕上能开关、调温度（16–30°C）、换模式（制冷、制热、除湿、送风、自动）和风速。再接一个红外接收模块，有人用原装遥控器调了空调，屏幕上的设定也会跟着变。

- 红外是单向的，空调不会回报状态，页面显示的是最后一次发出或收到的设定，go-dock 重启后也记得。
- 空调关着时调温度、模式、风速只记下来，下次开机一起发出去。自动模式温度固定 25°C，除湿时风速固定为低，和原装遥控器一样。
- 在网页「插件」页打开「空调」，遥控器协议不确定就用默认的 YAW1F；接了接收模块后按一下原装遥控器，日志里会提示它是哪一种。
- 编码在 go-dock 里完成，屏幕只负责把波形发出去，以后支持别的品牌不用升级固件。

> 屏幕发红外需要新版固件（在网页上就能升级），旧固件上空调页显示「屏幕固件不支持红外」并置灰。红外模块的接法见 [firmware/README.md](firmware/README.md#红外遥控空调)。

想加别的功能页：在 `host/internal/plugins/<名字>/` 实现 `plugins.Plugin` 接口（用通用控件描述页面、处理点击），加上配置，再在 `host/internal/app` 里注册。设计说明和后续计划见 [docs/plans/0002-plugins-mqtt.md](docs/plans/0002-plugins-mqtt.md)。

## 安全

- 管理台默认只监听 `127.0.0.1`；会校验 Host 头，并拒绝跨站请求和非 JSON 的写请求。
- hooks 上报的提问和工具参数只保存在内存里，不会写进 info 级别日志。
- MQTT 密码保存在本机的 `config.json` 里（go-dock 创建的文件权限为 0600），不会写进日志。灯光插件收发的 MQTT 报文会写进 info 级别日志，方便排查。

## 开发

```sh
cd host
go vet ./...
go test -race ./...
```

发布新版本：推送形如 `v0.2.0` 的标签，`.github/workflows/release.yml` 会编译 6 个平台的程序、生成 `SHA256SUMS` 并创建 Release。在线升级依赖这些文件名，不要改动。

```sh
git tag v0.2.0 && git push origin v0.2.0
```

标签带后缀（如 `v0.3.0-beta.1`）时发布为预发布版本：只会推送给打开了「接收测试版」的用户。

固件：

```sh
. ~/esp/esp-idf/export.sh   # ESP-IDF v6.1；用 EIM 安装的是 . ~/.espressif/tools/activate_idf_v6.1.sh
cd firmware && idf.py build
```

修改通信协议时，必须同时修改 `docs/protocol-v1.md`、`host/internal/device/protocol.go` 和固件的 `firmware/main/link.c`。更多约定见 [AGENTS.md](AGENTS.md)。
