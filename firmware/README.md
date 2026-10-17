# go-dock 屏幕固件

运行在微雪 ESP32-S3-Touch-AMOLED-2.41 上的固件：通过 USB 接收 go-dock 发来的数据，在 600×450 的屏幕上显示 Claude / Codex 的额度和任务状态。用 ESP-IDF 5.5 + LVGL 9 编写，界面与网页「设备预览」一致（见 [docs/protocol-v1.md](../docs/protocol-v1.md) §7.1）。

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
- 额度卡：Claude / Codex 的 5 小时、7 天额度和重置倒计时，今日 token
- 任务列表：每页 4 条，按「需授权 > 等待输入 > 出错 > 进行中 > 已完成」排序；有子 agent 的会话显示「子任务 N」
- 触摸：上下滑动或点右侧圆点翻页
- 超过 15 秒收不到数据时显示「主机离线」；还没连上时显示「等待 go-dock」
- 防烧屏：每分钟把画面整体挪动 2 像素；5 分钟没有活跃任务就调暗，有任务需要你处理（需授权、等待输入、出错）时保持全亮，触摸后亮起 1 分钟
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

1. 安装 ESP-IDF v5.5.4（只需一次）：

   ```sh
   mkdir -p ~/esp && cd ~/esp
   git clone -b v5.5.4 --recursive https://github.com/espressif/esp-idf.git
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

屏幕用 `esp_lcd_sh8601` 驱动加微雪提供的初始化命令；MADCTL 设为 `0x30` 转成横屏，可见区域在显存第 16 行起，所以设置了 16 像素的偏移。刷新区域的起止坐标必须是偶数。

## 目录

```
firmware/
├── main/
│   ├── main.c        启动：USB 通信 + LVGL 任务
│   ├── display.c     V2 板的屏幕、触摸、背光
│   ├── link.c        USB 协议：解析 snapshot，回 hello / ack / log
│   ├── ui.c          Agent 页界面
│   ├── fonts.c       运行时用 TinyTTF 渲染内嵌字体
│   └── fonts/        Noto Sans SC、JetBrains Mono 子集（SIL OFL 1.1）
├── sim/              在电脑上渲染界面的模拟器
└── tools/make_fonts.py   重新生成字体子集
```

## 在电脑上预览界面

`sim/` 把 `main/ui.c` 编译成电脑上的程序，喂几组示例数据，输出 600×450 的截图，不用板子也能检查布局：

```sh
cd firmware
idf.py reconfigure                       # 第一次需要，下载 LVGL
cmake -B build-sim -S sim && cmake --build build-sim
mkdir -p out && build-sim/dock_sim out    # 生成 out/*.ppm
```

## 字体

界面文字用 TinyTTF 在运行时渲染，一个字体文件覆盖所有字号：

- **Noto Sans SC 常规体**：GB2312 全部 6763 个汉字
- **Noto Sans SC 粗体**：3755 个常用字，缺字自动回退到常规体
- **JetBrains Mono**：数字和英文

要调整字符集，用 `tools/make_fonts.py` 重新生成（需要 `pip install fonttools`）。

## 还没在实物上验证

以下内容按微雪文档和示例代码实现，但还没有在真实的板子上验证：

- 16 像素偏移：画面四边是否都完整
- 触摸方向：横屏后的坐标转换是否正确
- 亮度：调光的具体数值是否合适

发现画面错位或触摸方向不对，请反馈。
