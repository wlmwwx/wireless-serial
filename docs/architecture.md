# 系统架构

本文说明 wireless-serial 的整体结构、数据通路、并发模型与性能边界。协议细节以
[protocol.md](protocol.md)（WSP/1 规范）为唯一权威，本文不重复定义帧格式。

## 1. 总览

```
┌───────────────────────────── 目标硬件 ─────────────────────────────┐
│  MCU / 开发板 / 传感器（3.3V TTL UART，可选 EN/IO0 复位与下载线）  │
└───────────────┬────────────────────────────────────────────────────┘
                │  UART TX/RX (+ RTS/CTS、EN、IO0 可选)
┌───────────────▼──────────────── ESP32-C3 ──────────────────────────┐
│  uart_bridge：UART1 + FreeRTOS stream buffer（RX 8 KB / TX 8 KB）  │
│       ▲   │                                                         │
│       │   ▼                                                         │
│  ┌──────────────────────────────┐   ┌───────────────────────────┐  │
│  │ WSP 服务 :3333               │   │ RAW 服务 :3334            │  │
│  │ wsp_parser / wsp_encode      │   │ 纯字节透传               │  │
│  │ 单客户端，握手 + 帧协议      │   │ 与 WSP 互斥               │  │
│  └──────────────┬───────────────┘   └──────────┬────────────────┘  │
│                 │        WiFi STA / SoftAP    │                    │
│  ┌──────────────▼──────────────────────────────▼────────────────┐  │
│  │ HTTP :80 配置页 + /api/* + WebSocket /ws 终端                 │  │
│  │ mDNS _wserial._tcp    NVS config_store    status_led          │  │
│  └──────────────────────────────────────────────────────────────┘  │
└───────────────────────────────┬─────────────────────────────────────┘
                                │ 802.11 b/g/n（WPA2）
┌───────────────────────────────▼─────────────────────────────────────┐
│ 电脑（macOS / Linux / Windows）                                      │
│  wserial                                                             │
│   ├─ discovery：mDNS 发现 _wserial._tcp                              │
│   ├─ transport：TCP 连接 / 重连退避 / RAW 模式                       │
│   ├─ wsp：帧编解码、握手、UART_CFG、PING/PONG、STATS                 │
│   └─ vport：Unix PTY（openpty + /tmp/ttyWS0 符号链接）               │
│              或 Windows COM 口（com0com 对端）                       │
│                          ▲                                           │
│                          │ /dev/ttyWS0 或 COM11                     │
│               用户串口程序：Arduino IDE / PlatformIO / PuTTY ...      │
└─────────────────────────────────────────────────────────────────────┘
```

关键设计取舍：

* **透明桥接优先**：主机侧创建的是标准串口设备节点，用户程序不感知 WiFi、
  TCP 或 WSP 帧的存在。
* **协议最小化**：8 字节帧头 + 最多 1024 字节 payload，CRC-16/CCITT-FALSE
  校验，足以覆盖 UART 配置、流控与统计，不引入复杂会话状态。
* **单客户端**：目标硬件 UART 是独占资源，同一时刻只允许一个 WSP 或 RAW
  客户端，避免会话交错。

## 2. 组件与职责

### 2.1 固件（`firmware/`，ESP-IDF v5.4.4）

| 组件 | 文件（以固件实现为准） | 职责 |
|------|------------------------|------|
| 启动与主流程 | `firmware/main/app_main.c` | 初始化 NVS/WiFi/UART/服务器，打印 IP 与端口 |
| WiFi 管理 | `firmware/main/wifi_mgr.c` | STA 连接（超时重试）、SoftAP 回退、重新配网 |
| UART 桥 | `firmware/main/uart_bridge.c` | UART1 驱动、运行时改参数、BREAK、RX/TX 环形缓冲 |
| TCP 服务 | `firmware/main/tcp_server.c` | 3333 WSP 会话状态机、3334 RAW、单客户端互斥 |
| HTTP 服务 | `firmware/main/http_server.c` | 配置页、`/api/status`、`/api/config`、`/api/scan`、`/api/reboot`、`/ws` |
| 配置存储 | `firmware/main/config_store.c` | NVS 持久化（设备名、串口参数、WiFi、端口） |
| mDNS | `firmware/main/mdns_srv.c` | 广播 `_wserial._tcp` 与 TXT 记录 |
| 状态灯 | `firmware/main/status_led.c` | 可选运行状态指示（默认关闭） |

协议编解码是**可移植 C99**：`firmware/main/include/wsp_proto.h` +
`firmware/main/wsp_proto.c`。它不依赖 ESP-IDF，因此同一份代码被固件和
`test/proto_test.c` 共同编译，保证两端行为一致。

### 2.2 主机工具（`host/`，Go 单二进制）

| 包 | 职责 |
|----|------|
| `host/cmd/wserial` | CLI 入口：`list` / `run` / `monitor` / `version` |
| `host/internal/wsp` | 帧编解码、客户端状态机、协议单测 |
| `host/internal/transport` | TCP 连接、自动重连（指数退避 1s→10s）、RAW 模式 |
| `host/internal/vport` | Unix PTY 与 Windows COM 口的虚拟串口实现（build tags 分离） |
| `host/internal/discovery` | mDNS 发现 `_wserial._tcp` |
| `host/internal/log` | 分级日志（只写 stderr，不污染串口数据） |

## 3. 数据通路

以「主机程序写入虚拟串口 → 目标硬件收到」为例：

```
用户程序 write(/dev/ttyWS0)
  → PTY 主端可读事件
  → wserial 读缓冲（每次 ≤1024 字节）
  → WSP DATA 帧（8 字节头 + payload + CRC）
  → TCP :3333（Nagle 关闭）
  → 固件 tcp_server 解析、校验 CRC
  → 写入 UART1 TX（阻塞写 + 超时）
  → 目标硬件 RX
```

反向（目标硬件 → 用户程序）：

```
目标硬件 TX
  → UART1 RX 中断/事件
  → RX stream buffer（8 KB）
  → 组 WSP DATA 帧（每帧 ≤1024 字节）
  → TCP 发送
  → wserial 解析 → 写入 PTY 主端
  → 用户程序 read(/dev/ttyWS0)
```

RAW 模式走 :3334，去掉 WSP 帧与握手，字节序完全不变，适合
`socat`、`com2tcp` 等现成工具；代价是无法远程修改串口参数，也没有
保活与统计。

### 3.1 帧内边界

* 一帧 DATA 的 payload 为 1..1024 字节。wserial 会按对端 HELLO 中的
  `max_payload` 与本地缓冲取较小值分片。
* 串口字节流会被任意切分：一个 UART 字节块可能跨多帧，一帧也可能包含
  多个逻辑「消息」。WSP 不提供消息边界，桥接层不得假设边界存在。
* 半包/粘包由流式解析器处理；CRC 错误或版本不匹配的帧被丢弃并重新同步，
  不影响后续帧。

## 4. 并发模型

### 4.1 固件侧（FreeRTOS）

* UART 接收事件 → RX stream buffer；发送走 UART 驱动的 TX 缓冲。
* WSP 与 RAW 各有一个监听 socket，但共享**同一把 UART 占用锁**：只有
  一个客户端能进入「已连接」状态。
* 一个客户端会话由一个 TCP 任务驱动：`recv → 解析 → 分发`，DATA 写入
  UART；UART RX 数据由发送路径组帧写回 socket。接收与发送方向互不阻塞
  （TCP 发送使用 `send` + 超时，不会无限期卡住 UART 接收任务）。
* HTTP 服务独立任务，WebSocket 串口终端与 WSP/RAW 客户端同样受单客户端
  约束。
* 空闲客户端在超时（默认 120 秒，Kconfig 可调）后自动断开，恢复监听。

### 4.2 主机侧（goroutine）

* **连接管理**：维护 TCP 连接，断线后指数退避重连（1s → 10s 封顶），
  重连成功后重新 HELLO 握手并重发 `UART_CFG`；虚拟串口节点保持存在。
* **上行**：PTY/COM → TCP（WSP DATA 或 RAW 字节）。
* **下行**：TCP → PTY/COM。
* **保活**：每 `--ping-interval`（默认 15s）发 PING，超时未收到 PONG
  时主动断开并进入重连流程。
* **清理**：收到 SIGINT/SIGTERM 时关闭 PTY、删除符号链接、关闭 TCP。
* 数据面不做日志输出；`-v` 日志只写 stderr。

## 5. 单客户端模型

* 同一时刻最多一个串口客户端（WSP / RAW / 网页 WebSocket 三者互斥，共享
  同一把 UART 占用锁）。设备**不会**抢占已有会话，第二个连接按类型处理：
  * WSP：回一帧 `ERROR(BUSY)`（错误码 7）后关闭；
  * RAW：直接关闭，不发送任何数据；
  * 网页 WebSocket：回文本 JSON `{"ok":false,"error":"serial port busy"}`
    并拒绝连接。
  要接入新客户端，需先等旧客户端断开（或空闲超时 120s）。
* 主机工具收到 `ERROR(BUSY)`（错误码 7）时应给出「设备被占用」的明确
  提示，并可提示用户检查是否有另一个 wserial、网页终端或 `socat` 正在
  连接。
* 断线后设备回到监听状态，通常 1 秒内可接受新连接；wserial 默认自动
  重连，不需要重新打开用户串口程序。

## 6. 背压与丢包策略

| 方向 | 策略 | 满时行为 |
|------|------|----------|
| 主机 → 目标 | DATA 帧按序写入 UART，阻塞写 + 超时 | 不丢帧；TCP 窗口反压到主机 |
| 目标 → 主机 | UART RX → 8 KB 环形/流缓冲 → 组帧发送 | 缓冲满时丢弃新字节，累计 `uart_overflows` |
| 主机消费慢 | 用户程序未及时读 PTY | TCP 窗口自然反压，数据不丢 |
| TCP 断链 | 自动重连 | 断链瞬间在途数据可能丢失，属预期行为 |

可通过 STATS 帧或 `/api/status` 查看 `uart_overflows`、`crc_errors` 等
计数，判断是否需要降低波特率或提高消费速度。

## 7. 性能预期

以下是**经验范围**，随 WiFi 环境、目标板、串口波特率与主机性能波动，
不是硬性指标；建议用自己的场景实测（见第 8 节方法）：

| 指标 | 范围 | 说明 |
|------|------|------|
| 有效串口波特率 | 最高 5 Mbps（UART 上限） | 受 WiFi 实际吞吐限制，见下一行 |
| 端到端吞吐 | 约 0.5–2 Mbps（60–250 KB/s） | 局域网、ESP32-C3 2.4 GHz、WPA2；随环境显著变化 |
| 额外延迟 | 约 2–15 ms（局域网） | 主机 ↔ 设备一次往返；受 WiFi 省电、重传影响 |
| 帧开销 | 每 ≤1024 字节 + 8 字节头 + CRC | 大块传输开销 <1% |
| 典型内存占用 | 固件 RX/TX 各 8 KB 缓冲 + 解析器约 1 KB | 单客户端模型 |

调优建议：

* 串口波特率高（≥921600）时，确保 WiFi 信号良好、关闭主机省电；
  必要时降低波特率或使用 RAW 模式减少 CPU 开销。
* 批量传输大文件时，用户程序应使用较大读缓冲；PTY 侧写入过小会放大
  系统调用开销。
* 对延迟敏感的场景避免开启硬件流控而目标未接 CTS（会造成阻塞）。

## 8. 观测与诊断

* **固件**：启动日志打印 IP、设备名、WSP/RAW/HTTP 端口；`/api/status`
  返回 RSSI、uptime、UART 配置、统计与客户端连接状态。
* **主机**：`wserial run -v` 输出连接到哪个 IP、握手信息、重连事件；
  错误信息包含可操作建议。
* **统计**：WSP STATS 帧（`wserial` 在 verbose 下可打印）与 HTTP
  `/api/status` 的 `stats` 字段同源。
* **无硬件**：`test/sim` 模拟设备可注入延迟/丢包，用于验证重连与背压
  行为（见 [host-usage.md](host-usage.md) 与项目 `test/` 目录）。

## 9. 已实现 / 路线图

已在 v1 范围内实现（以代码落盘为准）：

* WSP/1 全部 13 种帧（HELLO/HELLO_ACK、DATA、UART_CFG/ACK、LINE_CTRL/
  LINE_STATUS、STATS、PING/PONG、ERROR、RESET_TARGET、INFO）；
* TCP 3333 / 3334 / HTTP 80 + WebSocket 串口终端；
* WiFi STA + SoftAP 回退、mDNS、NVS 配置持久化；
* 主机端 `list` / `run` / `monitor` / `version`，Unix PTY 与 Windows
  com0com 虚拟串口，自动重连，RAW 模式。

路线图（**尚未实现**，文档出现处均标注）：

* 传输加密（TLS/wss）、WSP/HTTP 认证（如 HTTP Basic Auth）；
* 多客户端广播/会话复用（当前单客户端是设计约束）；
* 目标 UART 的 RS485 自动收发方向控制（DE/RE 引脚）；
* OTA 固件升级；
* 主机端原生 Windows 虚拟串口驱动（当前依赖 com0com）；
* WebSocket 端的多客户端与二进制大包优化。

## 10. 相关文档

* [protocol.md](protocol.md) — WSP/1 协议规范（权威）
* [wiring.md](wiring.md) — 接线与电平转换
* [installation.md](installation.md) — 三平台安装
* [firmware-build.md](firmware-build.md) — 固件构建与烧录
* [host-usage.md](host-usage.md) — wserial 用法与场景
* [troubleshooting.md](troubleshooting.md) — 排错
* [faq.md](faq.md) — 常见问题
