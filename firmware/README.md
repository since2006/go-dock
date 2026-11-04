# go-dock 屏幕固件

运行在微雪 ESP32-S3-Touch-AMOLED-2.41 上的固件：通过 USB 接收 go-dock 发来的数据，在 600×450 的屏幕上显示 Claude / Codex 的额度和任务状态。用 ESP-IDF 6.1 + LVGL 9 编写，界面与网页「设备预览」一致（见 [docs/protocol-v1.md](../docs/protocol-v1.md) §7.1）。

## 只支持 V2 板

这块板有 V1 / V2 两个硬件版本，**引脚不同，固件不通用**。本固件只适用于 **V2**：

- 没有外壳：板子丝印有「Rev2.0」的是 V2
- 有外壳（-B 型号）：外壳贴有「V2」质检标签的是 V2

V1 和 V2 的主要区别：

| 信号 | V1 | V2 |
|---|---|---|
| 屏幕复位 | GPIO21 | 扩展芯片 TCA9554 的 EXIO0 |
| 屏幕 TE | EXIO0 | GPIO21 |
| 触摸复位 | GPIO3 | EXIO1 |
| 触摸中断 | EXIO2 | GPIO3 |
| 扩展芯片中断 | 无 | GPIO18 |

## 功能

- 顶栏：活跃任务汇总、主机连接状态、时钟（按电脑所在时区）
- 额度卡：Claude / Codex 的 5 小时、7 天额度和重置倒计时，今日 token（不含缓存读取）
- 任务列表：按「需授权 > 等待输入 > 出错 > 进行中 > 已完成」排序；会话有标题时同时显示标题和当前动作，子 agent 缩进列在所属会话下面；一页放不下就翻页，同一个会话不拆开
- 触摸：上下滑动或点右侧圆点翻任务页，左右滑动或点标题换一台电脑
- 顶栏写这台电脑的名字（go-dock 设置里的「本机名称」，默认是电脑的名称）
- 多台电脑：最多 4 个 go-dock 同时连着，每台一页，有新变化的那台轮到前面（当前页至少停 30 秒，等你授权或回答的 5 秒），没有新变化时有任务的几台每 1 分钟轮流，摸着屏幕时不自动换；标题后面每台一个圆点，别的电脑有任务等你时圆点变色（规则见 [协议文档 §7.1](../docs/protocol-v1.md)）
- 连接信息页：左右滑过最后一台电脑（或点标题）就到，显示屏幕的地址、配对码、WiFi 和已连接的电脑，不参与自动轮换，2 分钟没碰就回去；新电脑连上时自动换到它那一页
- 读数条：打开 go-dock 的「性能」插件后，用量上方一条显示电脑的 CPU、GPU、内存、风扇、温度、磁盘，一次三个，多的每 6 秒向左轮换一组
- 超过 15 秒收不到数据时显示「主机离线」；还没连上时显示「等待 go-dock」；这张卡片底部显示固件版本
- WiFi：先用 USB 连上电脑，在 go-dock 网页「设置 → 屏幕 WiFi」里点「扫描并选择」，在弹出框里选一个 WiFi、输入密码（隐藏的 WiFi 直接填名称；可选固定 IP），弹出框里会显示连上了还是连不上、连不上的原因；连上后屏幕的「连接信息」页（没连电脑时是屏幕中间的卡片）显示地址和配对码，点「使用这块屏幕」后 go-dock 就通过 WiFi 连接，可以拔掉 USB 线（屏幕仍要供电）。屏幕显示的数据只走 WiFi，USB 线只用来设置 WiFi 和升级固件。只支持 2.4 GHz，数据在局域网里明文传输
- 在 go-dock 网页上升级固件，插着 USB 线时走 USB，否则经 WiFi，经 WiFi 时要在屏幕上点「升级」确认；升级时屏幕显示进度（见下面「在线升级」）
- 亮度和防烧屏：亮度、多久没有活跃任务后变暗（默认 5 分钟）、变暗后的亮度（可以熄屏）在 go-dock 网页「设置」里改，设置保存在屏幕上，断开 go-dock 也照此执行；几台电脑以最后改的为准，没改过的那台不会覆盖；任何一台有任务需要你处理（需授权、等待输入、出错）时保持全亮，触摸后亮起 1 分钟；每分钟把画面整体挪动 2 像素
- 有任务等你授权或回答时，以及任务完成时（可选），外接的蜂鸣器响铃提醒（见下面「蜂鸣器提醒」）
- 电池供电时开机后保持供电（GPIO16）

## 编译和烧录

### 方式一：直接烧录发布的固件

[Releases](https://github.com/since2006/go-dock/releases) 里有合并好的 `go-dock-firmware-v2.bin`，从地址 `0x0` 烧录：

```sh
pip install esptool
esptool.py --chip esp32s3 write_flash 0x0 go-dock-firmware-v2.bin
```

也可以用浏览器版的 [ESP 烧录工具](https://espressif.github.io/esptool-js/)（Chrome / Edge），地址填 `0x0`。

### 方式二：从源码编译

1. 安装 ESP-IDF v6.1（只需一次；也可以用乐鑫的 [EIM 安装器](https://docs.espressif.com/projects/idf-im-ui/en/latest/)，之后用 `. ~/.espressif/tools/activate_idf_v6.1.sh` 代替下面的 `export.sh`）：

   ```sh
   mkdir -p ~/esp && cd ~/esp
   git clone -b v6.1 --recursive https://github.com/espressif/esp-idf.git
   cd esp-idf && ./install.sh esp32s3
   python3 tools/idf_tools.py install cmake ninja   # macOS 上没有 cmake/ninja 时
   ```

2. 编译并烧录：

   ```sh
   . ~/esp/esp-idf/export.sh
   cd firmware
   idf.py build
   idf.py -p /dev/cu.usbmodem1101 flash   # Linux 一般是 /dev/ttyACM0，Windows 是 COMx
   ```

**烧录前先让 go-dock 松开串口**：go-dock 运行时会一直占着这个 USB 串口，导致烧录失败。可以先退出 go-dock，或在网页「设置」里取消「启用 USB 屏幕」，烧完再打开。

烧录失败、找不到端口时，进入下载模式：按住 BOOT 键，按一下 RESET 键，先松开 RESET 再松开 BOOT。

自己编译的固件版本号是 `dev`（来自 `version.txt`，发布时 CI 会写成 tag 的版本）。

## 在线升级

固件版本跟着 go-dock 走：每次发布都同时带上 go-dock 程序和同版本的固件。go-dock 发现设备上的固件版本更旧时，网页顶部会提示「设备固件可以升级」，点「升级固件」后：

1. go-dock 从同版本的 Release 下载 `go-dock-firmware-v2-app.bin`（只含程序部分），用 `SHA256SUMS` 校验；
2. 分块发给设备：插着 USB 线时走 USB；没插线时经 WiFi，屏幕先弹出「升级固件？」，在屏幕上点「升级」才开始（点「取消」或 60 秒不点就不升级）。设备写进空闲的 OTA 分区，屏幕显示进度，大约 1–2 分钟；
3. 设备重启进入新固件（经 WiFi 时会自己重新连上），网页显示「设备固件已升级到 x」。

升级中途拔线、断电、断网或出错，设备仍运行原来的固件，可以重新升级。新固件启动后要联系上 go-dock 才算确认；如果它启动不了或 2 分钟内没联系上，bootloader 会自动回到原来的固件。

发布前想在真机上试新固件，可以把编译出的程序镜像交给 go-dock，网页上就会提示升级到这个文件里的版本：

```sh
GO_DOCK_FIRMWARE_FILE=firmware/build/go-dock-firmware.bin go-dock
```

## 蜂鸣器提醒

板子本身没有喇叭。想在有任务等你授权或回答、任务完成时听到提醒，在 34 针排针（J8）上接一个**无源**蜂鸣器：

| 蜂鸣器 | 排针 |
|---|---|
| 信号（I/O、S 或 +） | 15 脚（GPIO8） |
| GND（−） | 1 脚（GND）；18、33 脚也是 GND |
| VCC（三针模块才有） | 16、30 或 32 脚（3V3） |

- 要用**无源**蜂鸣器：固件用 PWM 发出不同频率才能放出不同的音色，有源蜂鸣器只会一个音。
- 推荐带三极管的三针无源蜂鸣器模块，高电平、低电平触发的都可以：不响的时候引脚悬空，两种模块都不通电。模块电源接 **3V3**，不要接 5V（低电平触发的模块接 5V 时，GPIO 的 3.3V 关不断它）。
- 也可以直接接一个压电式（piezo）无源蜂鸣器，声音小一些。电磁式蜂鸣器电流大，不要不经三极管直接接 GPIO。
- 排针位置如果只有焊盘，需要先焊上排针或直接焊线；带外壳的版本要打开外壳接线。
- 想换引脚，改 `main/beep.c` 里的 `PIN_BUZZER`。GPIO 1、39–42 也空着；别用屏幕、触摸、TF 卡（GPIO 2、4–6）、USB 和调试串口占用的引脚。

在 go-dock 网页「设置 → 设备」里可以关掉提醒、选择提示音、调音量，点「试听」检查接线。音量靠缩短 PWM 脉宽调小，高、低电平触发的模块都适用；低电平触发的模块调小音量时通电时间反而更长，耗电略多。提示音由 go-dock 把音符发给设备（协议 §3.6），以后加新音色不用升级固件。

## 串口和日志

- **USB-C 口**接 ESP32-S3 的原生 USB（USB-Serial/JTAG），只用来和 go-dock 通信（换行分隔的 JSON，见协议文档）。固件不会往这个口打印日志。
- **调试日志**输出到 UART0（排针 TXD / RXD，即 GPIO43 / GPIO44，115200）。
- 固件也会用协议里的 `log` 消息把关键信息发给 go-dock，能在网页「日志」页看到。

## 硬件（V2）

| 功能 | 引脚 / 地址 |
|---|---|
| 屏幕 RM690B0（QSPI） | CS GPIO9、CLK GPIO10、D0–D3 GPIO11–14；复位 EXIO0；TE GPIO21 |
| 触摸 FT6336 | I2C 0x38；中断 GPIO3；复位 EXIO1 |
| I2C 总线 | SDA GPIO47、SCL GPIO48（触摸、TCA9554 0x20、RTC PCF85063 0x51、IMU QMI8658） |
| 电源保持 | GPIO16，必须拉高，否则电池供电时松开电源键就会断电 |
| 按键 | BOOT GPIO0、PWR GPIO15（低电平有效） |
| 电池电压 | GPIO17（ADC2_CH6），分压后 ×3 |
| 蜂鸣器（外接） | GPIO8（J8 排针 15 脚），LEDC PWM |

屏幕用 `esp_lcd_sh8601` 驱动加微雪提供的初始化命令；MADCTL 设为 `0x30` 转成横屏，可见区域在显存第 16 行起，所以设置了 16 像素的偏移。刷新区域的起止坐标必须是偶数。

## 目录

```
firmware/
├── main/
│   ├── main.c        启动：USB 通信 + LVGL 任务
│   ├── display.c     V2 板的屏幕、触摸、背光
│   ├── link.c        协议：WiFi 上收 snapshot / line，USB 上设置 WiFi；两边都能升级
│   ├── ota.c         固件升级：写空闲分区、经 WiFi 时先在屏幕上确认、新固件试用
│   ├── wlan.c        WiFi：连网、保存设置和配对码
│   ├── beep.c        外接蜂鸣器：播放主机发来的提示音
│   ├── ui.c          界面：Agent 页、读数条、状态和升级卡片
│   ├── fonts.c       运行时用 TinyTTF 渲染内嵌字体
│   └── fonts/        Noto Sans SC、JetBrains Mono 子集（SIL OFL 1.1）
├── sim/              在电脑上渲染界面的模拟器
└── tools/make_fonts.py   重新生成字体子集
```

## 在电脑上预览界面

`sim/` 把 `main/ui.c` 编译成电脑上的程序，喂几组示例数据，输出 600×450 的截图，不用板子也能检查布局；还会检查读数条的轮换、升级确认卡片上的按钮点得准不准、滑动翻任务页和屏幕调暗：

```sh
cd firmware
idf.py reconfigure                       # 第一次需要，下载 LVGL
cmake -B build-sim -S sim && cmake --build build-sim
mkdir -p out && build-sim/dock_sim out    # 生成 out/*.ppm
```

## 字体

界面文字用 TinyTTF 在运行时渲染，一个字体文件覆盖所有字号：

- **Noto Sans SC 常规体**：GB2312 全部 6763 个汉字，以及常用符号（`§`、`•`、`–`、`≥`、`✓`、`①`、制表符、全角字符等）
- **Noto Sans SC 粗体**：3755 个常用字和同样的符号，缺字自动回退到常规体
- **JetBrains Mono**：数字和英文

要调整字符集，用 `tools/make_fonts.py` 重新生成（需要 `pip install fonttools`）。

## 还没在实物上验证

以下内容按微雪文档和示例代码实现，但还没有在真实的板子上验证：

- 16 像素偏移：画面四边是否都完整
- 触摸方向：横屏后的坐标转换是否正确
- 亮度：调光的具体数值是否合适
- 蜂鸣器：按原理图和 ESP-IDF 文档实现，还没接实物试过音量和音色
- 读数条：模拟器里试过轮换，真机上滑动是否流畅还要确认

发现画面错位或触摸方向不对，请反馈。
