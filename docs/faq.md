# 常见问题（FAQ）

## 1. 与蓝牙串口（HC-05/06、ESP32 BT SPP）有什么区别？

| | wireless-serial（WiFi） | 蓝牙串口 |
|---|---|---|
| 覆盖范围 | 几十米（WiFi 覆盖范围） | 约 10 米（Class 2） |
| 带宽 | 约 0.5–2 Mbps（局域网实测范围） | 经典蓝牙 SPP 通常 100–200 KB/s 量级，实际更低 |
| 配对 | 入网一次即固定，支持 mDNS 发现 | 每次配对/绑定，多设备管理麻烦 |
| 多平台 | macOS/Linux 原生 PTY，Windows 需 com0com | 各平台驱动差异大 |
| 组网 | 走现有 AP，可跨房间/楼层 | 点对点，距离受限 |
| 配置 | 网页 + WSP 远程改串口参数 | 一般只能改模块 AT 参数 |

结论：需要稳定、较远距离、较高吞吐的调试链路，选 WiFi；近距离、低功耗、
无需网络的场景，蓝牙更省电。

## 2. 与 ESP-Link 有什么区别？

ESP-Link（jeelabs/esp-link）是 ESP8266/ESP32 上的成熟固件，同样提供 WiFi
串口和网页配置。区别：

* 本项目定义了**独立的开源二进制协议 WSP/1**（帧格式、CRC、HELLO 握手、
  UART_CFG/LINE_CTRL/STATS 等），有 C 与 Go 两份实现和一致性测试；
* RAW 模式兼容 socat/com2tcp/hub4com 等现成工具，便于迁移；
* 主机工具自带 mDNS 发现、PTY/COM 虚拟串口与自动重连；
* 面向 ESP32-C3（RISC-V），构建脚本与 ESP-IDF v5.4.4 配套。

如果你只需要现成方案，ESP-Link 也可用；需要协议可扩展、双端可控、中文
文档与脚本化构建时，本项目更合适。

## 3. 和 ESP32 自带的 USB CDC（USB Serial/JTAG）比呢？

ESP32-C3 自带 USB Serial/JTAG，插 USB 即出现 `/dev/cu.usbmodem*` 或
`COMx`，**零配置、延迟最低**。但：

* 受 USB 线缆长度限制（通常 <2–3 米），无法离开桌面；
* 目标板可能被设备自身占用（例如 USB 用于供电/调试），并且一个 USB
  接口通常只有一个串口；
* 无法跨房间、跨设备共享。

wireless-serial 解决的是「目标设备离电脑较远 / 需要电气隔离 / 多台电脑
轮流接入」的场景。两者可并存：短距离调试用 USB CDC，部署/远程用 WiFi。

## 4. 延迟和吞吐大概多少？

经验范围（ESP32-C3 + 2.4 GHz WiFi + 局域网）：

* 额外延迟：约 2–15 ms（一次主机↔设备往返）；
* 持续吞吐：约 60–250 KB/s（0.5–2 Mbps），随信号质量、信道拥塞、
  主机性能变化；
* 串口侧上限：UART 最高 5 Mbps，通常瓶颈在 WiFi 与目标板。

实测方法：用 `wserial run` + 目标板回环（TX 接 RX），在主机侧发已知大小
数据并计时；或使用 `test/sim` 模拟器做基准。不要把 WiFi 串口用于硬实时
控制，它适合调试、日志、命令通道。

## 5. 支持多客户端吗？

**不支持**（WSP/RAW/网页终端互斥）。目标硬件的 UART 是独占资源，多个
客户端同时读写会导致会话交错、数据无法预期。

需要多消费者时：

* 用一台电脑上的 `wserial run` 建虚拟串口，再由本地程序（如串口多路
  复用工具）分发；
* 或在目标侧自行实现多路复用协议；
* 或多客户端广播属于路线图（**尚未实现**）。

## 6. 安全吗？会不会被公网访问？

* 设备走 WPA2 加密的 WiFi，链路层有加密；
* WSP/RAW/HTTP **没有应用层认证**（protocol.md 提到的可选 HTTP Basic Auth
  与 TLS/wss 均属路线图，当前固件未实现），因此：
  * 只在可信局域网内使用；
  * **不要把 3333/3334/80 端口映射到公网**；
  * 与不可信设备共享网络时，单独划 VLAN/访客网络；
* SoftAP 默认密码 `wserial123` 务必在首次使用时通过网页或 Kconfig 修改；
* 设备不主动连接外网，不依赖云服务。

## 7. ESP32-C3 够用吗？S3/C6 呢？

C3 足够本项目的核心功能：单核 RISC-V 160 MHz、WiFi 2.4 GHz、400 KB SRAM，
运行 UART 桥接 + TCP + HTTP + mDNS 余量充足。

* **S3**：双核、更多 GPIO/PSRAM，适合同时跑摄像头、USB Host 等重负载；
  本固件以 C3 为主，代码层面可移植，但需自行改 target 与引脚。
* **C6**：WiFi 6 + 802.15.4（Thread/Zigbee），功耗与并发更好，适合未来
  多协议网关；当前构建脚本固定 `esp32c3`，移植需改 `set-target` 与部分
  驱动（路线图性质）。
* **ESP32 经典款**：双核 + 蓝牙经典，若需要 BT SPP 可选。

## 8. 能通过它给目标板烧录固件吗？

不能。无线串口只透传 UART 数据，不转发目标板的 USB 下载时序；主机端看到
的是虚拟串口，目标板看到的是普通 UART 字节。部分目标板支持 UART 下载
（如 Arduino 的 STK500、ESP 的串口下载），理论上需要主机工具额外实现
「烧录协议 + 复位/BOOT 时序」，这属于**路线图**，当前未实现。

ESP32-C3 自身的固件烧录请走 USB（`idf.py flash`），见
[firmware-build.md](firmware-build.md)。

## 9. 支持 5V / RS232 / RS485 吗？

ESP32-C3 GPIO 是 3.3V 电平：

* **5V TTL**：需电平转换（TXB0108/TXS0108E 或电阻分压），不能直连；
* **RS232（±12V）**：必须用 MAX3232 等收发器；
* **RS485**：需 RS485 收发器；当前固件不提供 DE/RE 自动方向控制，可用
  自动换向芯片（MAX13487 等）或外部电路，属于路线图。

详见 [wiring.md](wiring.md)。

## 10. 需要联网/路由器吗？

* 正常模式：需要 2.4 GHz WiFi AP（设备与电脑同一网段）；不需要 Internet。
* 无可用 WiFi 时：设备回退 SoftAP（`wserial-XXXXXX` / `wserial123`），
  电脑直连设备热点即可使用。
* 完全离线的点对点也可以：电脑连设备热点，`wserial run --host 192.168.4.1`。

## 11. Windows 一定要装 com0com 吗？

当前版本是。Windows 没有 Unix PTY 的等价机制来「创建任意虚拟串口」，
因此用 com0com 建立虚拟端口对：用户程序开一侧，wserial 开另一侧。
安装脚本：`scripts/install-com0com.ps1`。原生 Windows 虚拟串口驱动属于
**路线图**。

## 12. 为什么默认虚拟串口是 `/tmp/ttyWS0`？

约定俗成、便于脚本引用。可以任意指定：

```bash
wserial run -d /tmp/ttyWS1
# 或
wserial run -d ~/ttyWS0
```

路径须可写；wserial 会创建指向 PTY 从端的符号链接。

## 13. 配置会丢吗？怎么恢复出厂？

* WiFi/串口/端口等配置写入 NVS，掉电保留。
* 恢复出厂：网页配置页重置，或擦除 NVS：

```bash
cd firmware
idf.py -p /dev/cu.usbmodem1101 erase-flash
idf.py -p /dev/cu.usbmodem1101 flash
```

* 修改 Kconfig 默认值后需重新构建烧录；NVS 中已有配置优先于 Kconfig
  默认值（以固件实现为准）。

## 14. 为什么串口程序打开时目标板会复位？

许多串口程序（Arduino IDE、部分终端）打开端口时会拉 DTR/RTS。若接线把
RTS 映射到目标 EN、DTR 映射到 BOOT，就会触发复位或进入下载模式。这不是
故障：

* 不需要复位控制时，断开 EN/BOOT 接线，或在 menuconfig 中关闭 DTR/RTS
  映射；
* 需要稳定复位行为时，用 `wserial run --reset-on-start`，而不是依赖
  串口程序的时序；
* 某些程序（如 minicom）可配置是否拉 DTR/RTS。

## 15. 项目许可证是什么？

MIT License，见根目录 [LICENSE](../LICENSE)。可商用、可修改，需保留版权
声明。第三方依赖（Go 模块、ESP-IDF）遵循各自许可证。

## 16. 还有问题？

1. 先看 [troubleshooting.md](troubleshooting.md)；
2. 收集 `wserial run -v` 日志、`/api/status` JSON、平台与目标板信息；
3. 检查 [protocol.md](protocol.md) 的协议行为是否符合预期；
4. 反馈时附上复现步骤与日志。
