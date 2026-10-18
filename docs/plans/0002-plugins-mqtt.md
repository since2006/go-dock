# 功能页插件与 MQTT（含办公室灯光）

## Context

屏幕的主页面是 Agent 页，左右滑动切到其他「功能页」，第三方开发者可以加自己的页面。第一个功能页是办公室灯光控制：开灯、关灯、显示当前状态。灯走 MQTT，而 MQTT 本身要做成通用底座，以后接别的物联网设备也用它。

改动之前：
- 固件只走 USB，没有 WiFi、MQTT、NVS。触摸已接入 LVGL，上下滑翻任务页，左右滑预留给功能页（协议 §7.1）。
- 设备从不回传操作，`touch` 消息只是预留。

## 决定

**1. MQTT 客户端放在电脑端（Go），在网页里配置。**
- 屏幕仍然只走 USB：不用给屏幕配网，板子上不存 WiFi 和 broker 密码，也不用在 ESP32 上处理 TLS 和断线重连（内存余量也没测过）。
- 接新设备只改电脑端，不重编固件。
- 代价：电脑关机或睡眠时屏幕不能控灯。这块屏本来就要靠电脑提供 Agent 数据，可以接受。
- **可以连多个 broker，插件按 id 引用**，和 Node-RED 里被多个节点共用的 broker 配置节点是一个思路。插件不该被迫共用用户的某一个 broker（比如灯走办公室的 Zigbee2MQTT，别的插件要连云上的服务），同一个插件里的设备也可能在不同的 broker 上。凭据只存一处，几个插件用同一个 broker 时也只建一条连接。灯光插件按每盏灯选 broker；只有一个 broker 时不用选，留空表示第一个。

**2. 插件 = 电脑端插件 + 屏幕通用页面**，做法类似 Stream Deck、openHASP。
- 插件跑在电脑端，用几种通用控件描述页面，第一版只有开关卡片 `switch`；固件只负责渲染和回传点击。
- 新插件一般不需要新固件，官方在线升级照常可用。
- 没选「fork 后自己编固件」，原因有两个：
  - fork 会和主线越走越远，合并很痛苦；
  - go-dock 会把自己版本的官方固件推给设备，自编固件会被覆盖。
- 需要自绘界面时，再开放 C 原生页面（见阶段 3），也不靠 fork。

**3. 先做电脑端和网页预览，固件等预览定稿后再跟进**（见阶段 2）。新协议消息靠能力协商：设备 hello 带 `"pages":1` 才会收到 `pages`，所以现有固件不受影响。

## 结构

```
网页「插件」页 ──PUT /api/config──► config(mqtt.brokers, plugins.lights) ──onConfigChange──┐
                                                                                         ▼
mqtt.Pool ─Client(id)─► Bus ◄── lights.Plugin ──Page()──► plugins.Host ──► device.BuildPages ──► pages（USB）
 │ 每个 broker 一个 Client          ▲ Do(item, act)                                    └──► /api/state.pages ──► 设备预览
 └──► broker A、broker B …        └── action（设备）/ POST /api/plugins/{page}/{item}（网页）
```

- **`internal/mqtt`**：`Client` 是一条 broker 连接，基于 paho.mqtt.golang（MQTT 3.1.1，纯 Go）；`Pool` 按 id 给每个配置的 broker 一个 `Client`。
  - 删掉的 broker 只是停用，`Client` 一直保留，插件拿到的引用不会失效，broker 加回来时订阅也跟着恢复。
  - 不用 paho 自带的重连：它没法及时停下，所以自己写重连循环，退避 1–30 秒。
  - 订阅登记在客户端里，每次连上都重新订阅；同一个主题可以挂多个处理函数。
  - 发布用 QoS 0，断线时立刻返回错误、不排队，避免恢复连接后补发过时的开关命令。
  - mqtt / mqtts 直连，不走 `all_proxy`，以免局域网 broker 被代理拦住。
  - 测试用 `mqtttest` 里的进程内假 broker。
- **`internal/plugins`**：`Plugin` 接口（`ID`、`Page`、`Do`）和 `Host`。`Host` 按顺序汇总各插件的页面，并按页面 id 分发点击。页面类型 `model.Page` / `PageItem` / `Action` 放在 `internal/model`，device 包只依赖 model。
- **`internal/plugins/lights`**：
  - 每盏灯：命令主题、开 / 关时发送的内容，以及可选的状态主题（JSON 时用 gjson 路径取字段）和可选的查询主题。
  - 发命令前先标成「切换中」，收到匹配的状态后清除；5 秒没回报显示「未响应」；没有状态主题时按发出的命令显示。
  - 每盏灯用自己 broker 的连接。某个 broker 断开时只禁用它上面的灯，页面 `error` 写明是哪个（如「家 未连接」，只用一个 broker 时就是「MQTT 未连接」）；连接恢复时只查询这个 broker 上的灯。
- **`internal/device`**：
  - `BuildPages` 截断文字，限制页数和控件数，丢掉 id 不合法的项。
  - 串口循环在每条 `snapshot` 后发送 `pages`：握手后和心跳时总是发，状态变化时只在内容变了才发。
  - 收到 `action` 放到 goroutine 里交给插件，最多 4 个同时处理。
- **网页**：
  - 「设备预览」可以左右切页（按钮、←/→、拖动），灯光页点卡片会真的发命令。
  - 新的「插件」页配置 broker 列表（每个显示自己的连接状态）和灯（有多个 broker 时每盏灯有下拉框）。
  - 各表单只提交自己的字段，由服务端合并。
- **配置**：`mqtt.brokers` 列表，灯的 `broker` 填 broker id（留空表示第一个）；校验会拦下引用不存在的 broker，所以删还在用的 broker 会提示先改灯。`Store.Get` 返回深拷贝，broker 和灯的列表解码时整表替换。这顺带修了 PUT 时原地改坏已存配置的问题，`pricing` 以前就有这个问题。

协议细节见 `docs/protocol-v1.md` §3.5（`pages`）、§4.4（`action`）、§7.1（功能页布局）。

## 阶段 2：固件（预览定稿后，等用户说「同步固件」）

- **`link.c`**：
  - hello 加 `"pages":1`；
  - 用双缓冲解析 `pages` 到 `dock_pages_t`，和 snapshot 的交换方式一样；
  - 新增 `link_send_action()`，id 按协议保证不需要转义；
  - USB 接收缓冲从 8 KB 提到 16 KB，因为 snapshot 和 pages 两条最长 4 KB 的行会连着到达。
- **`model.h`**：最多 4 页、每页 6 项；字符串缓冲按主机的字数上限乘以 UTF-8 最长字节数，再加结尾的 0；`on` 用 -1 / 0 / 1 表示。
- **`ui.c`**：
  - 拆成「外壳 + 页面注册表」。外壳负责顶栏、页码点、遮罩卡片、亮度和防烧屏位移；Agent 页和通用功能页都是注册进去的页面。
  - `on_gesture` 处理左右滑。
  - 卡片网格与预览的 `scrGrid` 一致，点击后先在本地显示「切换中」。
  - 屏幕变暗时的第一下点击只用来唤醒；滑动后调用 `lv_indev_wait_release()`，避免一次滑动同时被当成点击。
- **`sim.c`**：加 1 / 3 / 4 / 6 盏灯的截图场景。

## 阶段 3：路线（未排期）

- **外部插件**：用 MQTT 保留消息发布页面描述（如 `go-dock/pages/<id>`），在 `…/action` 主题收点击。任何语言都能写插件，不用改 go-dock。也可以做成本地 HTTP 接口。
- **C 原生页面**：给需要自绘的页面用，做成外部 ESP-IDF 组件（`EXTRA_COMPONENT_DIRS`），注册进阶段 2 的页面注册表。这类自编固件在 hello 里带 `variant`，go-dock 看到后不再推送官方固件，避免覆盖。
- **更多控件**：如按钮、数值、滑块，按需要再加；新 `kind` 对旧固件只是一张不能点的卡片。
- **厂商预设**：在网页里一键填好 Tasmota、Zigbee2MQTT 等设备的主题和内容。
