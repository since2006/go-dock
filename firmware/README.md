# firmware（占位）

ESP32 固件还没开始写。这里先记录已经确定的信息，供后续开发使用。

## 硬件

微雪 ESP32-S3-Touch-AMOLED-2.41-B（-B 是带外壳的版本）

| 项目 | 说明 |
|---|---|
| 主控 | ESP32-S3R8，内置 8 MB PSRAM；外置 16 MB Flash |
| 屏幕 | 2.41" AMOLED 600×450，**RM690B0**，QSPI 接口 |
| 触摸 | **FT6336**，I2C，带中断脚 |
| USB | USB-C 直连 ESP32-S3 原生 USB（无 USB 转串口芯片），默认 VID:PID `303A:1001` |
| 其他 | QMI8658 六轴 IMU、PCF85063 RTC、锂电池接口与充电、TF 卡槽 |

**先确认板子版本**：这块板有 V1 / V2 两个版本，引脚定义不同，示例程序不能混用。V2 板丝印有「Rev2.0」，或外壳贴有「V2」标签。

建议框架：V2 用 ESP-IDF 5.5.x + LVGL 9；V1 用 ESP-IDF ≥ 5.1.4，或 Arduino core ≥ 3.0.7 + LVGL 8.4。

## 与电脑端通信

见 [docs/protocol-v1.md](../docs/protocol-v1.md)。要点：

- USB 串口，每行一条 JSON，单行不超过 4096 字节。
- 电脑端每次状态变化时发送完整的 `snapshot`，另外每 5 秒发一次；15 秒收不到消息就显示「主机离线」。
- 启动日志和调试输出**不要**走这个串口，需要上报的日志用协议里的 `log` 消息。
- 需要内置中文字库子集。

## 界面参考

go-dock 网页管理台的「设备预览」页会按 600×450 渲染目标布局，下面显示实际发送的 `snapshot` JSON（`GET /api/device/frame`），可以直接对照开发。
