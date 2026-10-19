# 无线串口（wireless-serial）

把目标硬件的 UART 通过 WiFi 映射成电脑上的**普通串口设备**。

ESP32-C3 运行桥接固件，电脑运行单文件工具 `wserial`：Arduino IDE、
PlatformIO、PuTTY、串口助手等程序打开 `/dev/ttyWS0`（macOS/Linux）或
`COM11`（Windows）就像插了一根 USB 串口线，真正的目标硬件可以在几米到
几十米之外，无需 USB 线缆。

* 固件：ESP-IDF **v5.4.4**，目标芯片 **ESP32-C3**
* 主机工具：Go 单二进制，支持 macOS / Linux / Windows
* 协议：自研 **WSP/1**（帧协议 + CRC-16），另有 RAW 纯透传端口兼容
  `socat` / `com2tcp` 等现成工具
* 文档：中文，命令可直接复制执行

## 特性

已实现（v1）：

| 能力 | 说明 |
|------|------|
| 三个服务端口 | `3333` WSP 帧协议、`3334` RAW 纯透传、`80` HTTP 配置页 |
| 虚拟串口 | macOS/Linux 用 PTY + 符号链接；Windows 用 com0com 端口对 |
| 远程串口配置 | 运行时修改波特率/数据位/校验/停止位/流控（WSP `UART_CFG`） |
| DTR/RTS 与复位 | `LINE_CTRL` / `RESET_TARGET`，可复位 Arduino/ESP 类目标板 |
| 设备发现 | mDNS `_wserial._tcp`，`wserial list` 列出设备 |
| 自动重连 | 断线指数退避（1s→10s），重连后重新握手、虚拟串口不消失 |
| 统计与诊断 | `STATS` 帧、HTTP `/api/status`、`-v` 日志 |
| 网页串口终端 | 浏览器打开设备 IP 即可收发与配置 |
| WiFi 回退 | STA 连接失败自动进入 SoftAP 配网（`wserial-XXXXXX`） |
| RAW 兼容 | `socat`、`com2tcp`、`hub4com` 可直接连接 3334 |
| 无硬件联调 | `make sim` 启动模拟设备，完整验证主机链路 |

路线图（**尚未实现**，文档中均已标注）：TLS/wss 与客户端认证、OTA
升级、RS485 方向控制、原生 Windows 虚拟串口驱动、多客户端、`wserial ota`。

## 架构

```
       目标硬件（3.3V TTL UART）                电脑
┌───────────────────────────┐        ┌──────────────────────────────────┐
│  MCU / 开发板 / 传感器    │        │  用户程序                        │
│  TX ─────────┐            │        │  Arduino IDE / picocom / PuTTY   │
│  RX ◄────────┤            │        │        ▲                         │
│  GND ────────┤            │        │        │ /dev/ttyWS0 或 COM11    │
└──────────────┼────────────┘        │  ┌─────┴─────────────────────┐   │
               │                     │  │ wserial                   │   │
┌──────────────▼─────────────┐       │  │  ├─ 虚拟串口（PTY/COM）   │   │
│ ESP32-C3（ESP-IDF v5.4.4） │       │  │  ├─ WSP 客户端 + 自动重连 │   │
│   UART1  TX=GPIO4 RX=GPIO5 │       │  │  └─ RAW 透传              │   │
│   ┌──────────────────────┐ │       │  └───────────▲───────────────┘   │
│   │ 环形缓冲 8KB         │ │       │              │                   │
│   │ WSP/1 帧编解码 + CRC │ │       │              │ TCP over WiFi     │
│   ├──────────────────────┤ │       │              │                   │
│   │ :3333 WSP  :3334 RAW │ │◄──────┼──────────────┘                   │
│   │ :80 HTTP + WebSocket │ │       │                                  │
│   │ mDNS / NVS / 状态灯  │ │       │                                  │
│   └──────────────────────┘ │       │                                  │
└────────────────────────────┘       └──────────────────────────────────┘
```

数据通路：目标 UART ↔ 8 KB 环形缓冲 ↔ WSP DATA 帧（≤1024 B payload）↔
TCP ↔ wserial ↔ PTY/COM ↔ 用户程序。详见 [docs/architecture.md](docs/architecture.md)。

## 5 分钟快速开始

### 0. 准备

* 硬件：ESP32-C3 开发板、目标硬件、杜邦线
* 软件：Go 1.25+（`host/go.mod` 要求）、ESP-IDF v5.4.4（工作区 `.idf/` 已内置）
* 网络：2.4 GHz WiFi（没有也能用 SoftAP 配网）

### 1. 接线（3 根线起步）

```
ESP32-C3 GPIO4 (TX)  ──►  目标 RX
ESP32-C3 GPIO5 (RX)  ◄──  目标 TX
ESP32-C3 GND         ──   目标 GND
```

目标为 5V/RS232 时必须加电平转换，详见 [docs/wiring.md](docs/wiring.md)。

### 2. 构建并烧录固件

```bash
make fw                                          # 首次自动 set-target esp32c3
make flash PORT=/dev/cu.usbmodem1101 MONITOR=1   # macOS 示例，Linux 用 /dev/ttyUSB0
```

烧录后串口日志会打印设备 IP、mDNS 名与端口。WiFi 未配置时设备进入
SoftAP：SSID `wserial-XXXXXX`、密码 `wserial123`，浏览器打开
`http://192.168.4.1` 配网（写入 NVS，掉电保留）。

### 3. 构建主机工具

```bash
make host
./host/wserial version
```

### 4. 发现设备并创建虚拟串口

```bash
./host/wserial list                     # mDNS 发现
./host/wserial run                      # 默认 /tmp/ttyWS0，115200 8N1
# 或指定 IP（mDNS 不可用时）
./host/wserial run --host 192.168.1.50 -d /tmp/ttyWS0 -b 115200 -v
```

Windows 需先创建 com0com 端口对（`scripts/install-com0com.ps1`），然后：

```powershell
.\wserial.exe run --host 192.168.1.50 --device COM12 -b 115200
```

### 5. 打开串口工具

```bash
picocom -b 115200 /tmp/ttyWS0
# 或 screen /tmp/ttyWS0 115200；Windows 打开 COM11（PuTTY / Arduino IDE）
```

现在向虚拟串口写入的数据会经 WiFi 到达目标 UART，目标输出也会实时回到
串口终端。断开 WiFi 后 `wserial` 会自动重连，虚拟串口保持不变。

### 没有硬件？先跑模拟器

```bash
make sim
# 自动启动模拟设备 + wserial，另开终端: picocom -b 115200 /tmp/ttyWS0
```

## 构建目标

```bash
make            # 显示帮助
make host       # 本机编译 wserial -> host/wserial
make host-all   # 交叉编译 macOS/Linux/Windows -> host/dist/
make test       # 协议一致性测试 + Go 单测
make sim        # 模拟设备 + wserial 联调（无需硬件）
make fw         # 构建 ESP32-C3 固件
make flash PORT=/dev/cu.usbmodem1101 [MONITOR=1]
make clean      # 清理产物（保留 .idf/）
```

## 目录结构

```
wireless-serial/
├── README.md                 # 本文件
├── Makefile                  # 顶层构建入口（help/host/host-all/test/sim/fw/flash/clean）
├── LICENSE                   # MIT
├── .gitignore
├── .idf/                     # 本地 ESP-IDF v5.4.4 + 工具链（不提交）
│   ├── esp-idf/
│   └── tools/
├── docs/
│   ├── protocol.md           # WSP/1 协议规范（权威）
│   ├── architecture.md       # 系统架构与数据流
│   ├── wiring.md             # 接线、电平转换、自动下载电路
│   ├── installation.md       # 三平台安装（含 Windows com0com）
│   ├── firmware-build.md     # ESP-IDF 安装、构建、烧录
│   ├── host-usage.md         # wserial 全部子命令与典型场景
│   ├── troubleshooting.md    # 故障排查
│   └── faq.md                # 常见问题
├── firmware/                 # ESP32-C3 固件（ESP-IDF）
│   ├── CMakeLists.txt        # project(wireless_serial)
│   ├── sdkconfig.defaults    # 基础配置（Flash、看门狗、WebSocket 等）
│   └── main/
│       ├── CMakeLists.txt
│       ├── Kconfig.projbuild # “Wireless Serial” 菜单（CONFIG_WSP_*）
│       ├── idf_component.yml # espressif/mdns 托管组件
│       ├── app_main.c        # 启动流程与健康监控
│       ├── wifi_mgr.c        # STA + SoftAP 回退
│       ├── uart_bridge.c     # UART1 桥接与运行时配置
│       ├── tcp_server.c      # WSP 3333 / RAW 3334
│       ├── http_server.c     # HTTP 80 + /ws
│       ├── config_store.c    # NVS 持久化
│       ├── mdns_srv.c        # mDNS 广播
│       ├── status_led.c      # 可选状态灯
│       ├── www/index.html    # 内嵌配置页与网页串口终端
│       ├── include/          # 组件头文件（含 wsp_proto.h）
│       └── wsp_proto.c       # WSP/1 帧编解码（可移植 C99）
├── host/                     # Go 主机工具
│   ├── go.mod
│   ├── cmd/wserial/main.go   # CLI 入口
│   └── internal/
│       ├── wsp/              # 帧编解码 + 客户端状态机 + 单测
│       ├── transport/        # TCP、重连退避、RAW
│       ├── bridge/           # 虚拟串口/终端与 WSP 的双向桥接
│       ├── vport/            # PTY / COM 虚拟串口（build tags 分离）
│       ├── discovery/        # mDNS 发现
│       └── log/              # 分级日志（stderr）
├── test/                     # 测试与模拟设备（独立 Go module）
│   ├── go.mod
│   ├── proto_test.c          # WSP 协议一致性测试（C99，上千项断言）
│   ├── run.sh                # 一键测试：C 单测 + 模拟器单测 + 冒烟
│   ├── integration.sh        # 端到端集成测试入口
│   ├── integration/          # 虚拟串口 ↔ WSP ↔ 模拟硬件 的联调测试
│   └── sim/                  # 设备模拟器（WSP/RAW/硬件侧 TCP 端口）
└── scripts/
    ├── build-firmware.sh     # 加载 .idf 工具链并构建固件
    ├── flash.sh              # 参数化烧录（端口/波特率）
    ├── build-host.sh         # 三平台交叉编译
    ├── run-sim.sh            # 模拟设备 + wserial 联调
    └── install-com0com.ps1   # Windows com0com 安装与端口对创建
```

> 目录结构以实际仓库为准；本文档只描述已实现或明确标注为路线图的内容。

## 文档索引

| 文档 | 内容 |
|------|------|
| [docs/protocol.md](docs/protocol.md) | WSP/1 协议规范：帧格式、CRC、全部帧类型、HTTP API（权威） |
| [docs/architecture.md](docs/architecture.md) | 架构、数据流、并发模型、背压与性能预期 |
| [docs/wiring.md](docs/wiring.md) | 接线图、3.3V 电平、DTR/RTS→EN/IO0、RS232/RS485 |
| [docs/installation.md](docs/installation.md) | 三平台安装、PTY、Windows com0com 与驱动签名 |
| [docs/firmware-build.md](docs/firmware-build.md) | ESP-IDF 安装、menuconfig、构建、烧录、下载模式 |
| [docs/host-usage.md](docs/host-usage.md) | `wserial` 子命令/选项、Arduino/PlatformIO/PuTTY、RAW 模式 |
| [docs/troubleshooting.md](docs/troubleshooting.md) | 连不上、mDNS、PTY 无数据、乱码、驱动、端口占用 |
| [docs/faq.md](docs/faq.md) | 与蓝牙/ESP-Link/USB CDC 对比、延迟、安全、选型 |

## 常见问题入口

* 连不上设备 → [docs/troubleshooting.md](docs/troubleshooting.md#3-发现设备但连不上)
* `wserial list` 找不到 → [mDNS 排查](docs/troubleshooting.md#2-mdns-发现不到设备)
* 虚拟串口没数据 → [PTY 无数据](docs/troubleshooting.md#4-虚拟串口没有数据)
* 数据乱码 → [乱码排查](docs/troubleshooting.md#5-数据乱码)
* Windows 驱动/端口 → [com0com 问题](docs/troubleshooting.md#8-windowscom0com-驱动签名与端口问题)
* 和蓝牙串口/ESP-Link 的区别 → [docs/faq.md](docs/faq.md)

## 安全提示

* 设备与主机之间的 WSP/RAW/HTTP **没有应用层认证**（TLS 属于路线图），
  请只在可信局域网使用，**不要暴露到公网**。
* SoftAP 默认密码 `wserial123`，首次使用请通过网页配置修改。
* 目标硬件的 UART 为独占资源：同一时刻只允许一个串口客户端。

## 许可证

本项目使用 [MIT License](LICENSE)。第三方依赖（Go 模块、ESP-IDF、com0com）
遵循各自许可证。
