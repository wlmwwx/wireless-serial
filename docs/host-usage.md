# wserial 使用手册

`wserial` 是 wireless-serial 的主机端工具：在电脑上创建虚拟串口，把
ESP32-C3 背后的目标硬件 UART 映射成普通串口设备。

```
wserial list                  # mDNS 发现设备
wserial run                   # 创建虚拟串口并桥接（默认）
wserial monitor               # 当前终端直接收发（不建虚拟串口）
wserial version               # 版本信息
wserial help                  # 总帮助
```

每个子命令都支持 `--help`，本文与 `--help` 不一致时以程序输出为准。

## 1. 退出码

| 退出码 | 含义 |
|--------|------|
| 0 | 成功 |
| 1 | 运行错误（连接失败、协议错误、PTY/COM 失败等） |
| 2 | 命令行用法错误（未知参数、非法取值；Windows 未指定 `--device` 也属此类） |
| 4 | 未发现设备（mDNS 无结果）或设备被占用（`BUSY`） |

## 2. 设备发现：`wserial list`

```bash
wserial list
wserial list --timeout 5s
wserial list --json
```

| 选项 | 默认 | 说明 |
|------|------|------|
| `--timeout` | `3s` | mDNS 搜索时长（必须大于 0） |
| `--json` | 关闭 | 输出 JSON 数组，便于脚本处理 |

输出列为**名称、IP、端口、MAC、波特率**（具体字段以实际版本为准）。没有
发现设备时向 stderr 打印提示并返回退出码 4；`--json` 下仍会输出 `[]`。
排查见 [troubleshooting.md](troubleshooting.md#2-mdns-发现不到设备)。

## 3. 创建虚拟串口：`wserial run`

```bash
# 最常用：自动发现 + 默认 /tmp/ttyWS0 + 115200
wserial run

# 指定 IP、虚拟串口路径与波特率
wserial run --host 192.168.1.50 --device /tmp/ttyWS1 --baud 9600 -v

# Windows：wserial 打开 com0com 对的一侧，用户程序打开另一侧
wserial run --host 192.168.1.50 --device COM12 -b 115200

# RAW 透传模式（端口 3334，不做握手与远程配置）
wserial run --host 192.168.1.50 --raw

# 连接后复位目标板（配合 wiring.md 的 EN/RESET 接线）
wserial run --host 192.168.1.50 --reset-on-start

# 不自动重连：断开即退出
wserial run --no-reconnect
```

### 3.1 `run` 选项

| 选项 | 简写 | 默认 | 说明 |
|------|------|------|------|
| `--host` | `-H` | 空（mDNS 自动发现） | 设备 IP 或主机名（如 `wserial-XXXXXX.local`） |
| `--port` | `-P` | `3333`（`--raw` 时 `3334`） | WSP/RAW 端口；显式指定时 `--raw` 也使用该值 |
| `--device` | `-d` | Unix：`/tmp/ttyWS0`；Windows：**必填** | 虚拟串口路径（Unix）或 COM 口（Windows） |
| `--baud` | `-b` | `115200` | 目标 UART 波特率；`0` 表示保持设备当前配置 |
| `--databits` | — | `8` | 数据位 5–8 |
| `--parity` | — | `none` | 校验：`none` / `odd` / `even` |
| `--stopbits` | — | `1` | 停止位 1 或 2 |
| `--flow` | — | `none` | 流控：`none` / `rtscts` |
| `--raw` | — | 关闭 | 走 RAW 端口纯透传，不握手、不改配置、无心跳 |
| `--no-reconnect` | — | 关闭 | 断线后不自动重连，直接退出 |
| `--reset-on-start` | — | 关闭 | 连接后向目标发 120ms 复位脉冲（仅 WSP 模式） |
| `--ping-interval` | — | `15s` | WSP 保活 PING 间隔；`0` 关闭 PING |
| `--verbose` | `-v` | 关闭 | 等价于 `--log-level debug` |
| `--log-level` | — | `info` | 日志级别 `error`/`warn`/`info`/`debug`/`trace`（只写 stderr） |

### 3.2 行为说明

1. **发现**：未指定 `--host` 时用 mDNS 查找 `_wserial._tcp`（默认 3s）；
   找不到会打印发现的设备列表与提示并退出（退出码 4）。可用 `--host`
   跳过发现。
2. **握手**：WSP 模式下发送 HELLO，读取 HELLO_ACK（设备名、固件版本、
   当前串口参数）；随后按参数发送 UART_CFG 并等待 ACK。
3. **虚拟串口**：
   * macOS/Linux：`openpty` 创建 PTY，把 `--device` 做成指向从端的符号
     链接（权限 0666，可被所有本地用户打开）；若目标路径已存在且不是
     符号链接则拒绝覆盖；退出时清理符号链接。
   * Windows：打开 `--device` 指定的 COM 口（com0com 对的 wserial 侧）；
     虚拟串口由 com0com 提供，wserial 退出后端口释放。未指定
     `--device` 时直接报用法错误（退出码 2）。
4. **数据桥接**：PTY/COM ↔ WSP DATA，按 ≤1024 字节分帧；读写两个方向独立，
   任一方向阻塞不影响另一方向。RAW 模式则为纯字节流透传。
5. **modem 控制线**：串口程序对本地 PTY/COM 的 DTR/RTS 设置会被轮询并映射
   为 `LINE_CTRL` 帧，用于驱动目标板的 EN/BOOT（Windows 由 COM 驱动映射）；
   若平台读不到 modem 位（如部分 macOS PTY），该功能自动禁用。
6. **自动重连**：默认开启，指数退避 `1s → 2s → 4s → 8s → 10s` 封顶。
   重连成功后重新握手、重发 UART_CFG、重新执行 `--reset-on-start`；
   **虚拟串口节点保持不变**，用户程序无需重开。
7. **保活**：每 `--ping-interval` 发 PING；设备回 PONG。超时进入重连；
   `--ping-interval 0` 关闭（RAW 模式固定无心跳）。
8. **退出**：`Ctrl+C`（SIGINT）或 SIGTERM 正常清理；Unix 下删除符号链接。
9. **日志**：数据面不打印日志；`-v` 日志只写 stderr，不污染串口数据。

### 3.3 参数生效规则

* `--baud 0`：不发送 UART_CFG，沿用设备当前配置；此时若还指定了
  `--databits`/`--parity`/`--stopbits`/`--flow`，程序会警告这些参数被忽略。
* RAW 模式不握手、不下发 UART_CFG：`--baud`、`--databits`、`--parity`、
  `--stopbits`、`--flow`、`--reset-on-start` 都不生效。需要先改参数时，
  用 WSP 模式设置一次（写入设备 NVS），再切 RAW。
* 非法取值（端口越界、`--databits 9`、`--parity mark`、`--ping-interval`
  为负等）在本地即报错退出（退出码 2），不会下发设备。

## 4. 终端直连：`wserial monitor`

不创建虚拟串口，直接把串口数据打印到当前终端，适合快速验证：

```bash
wserial monitor --host 192.168.1.50
wserial monitor -H 192.168.1.50 --hex       # 十六进制显示（RX/TX 前缀）
wserial monitor --raw -H 192.168.1.50       # RAW 模式
```

* `--hex`：以十六进制显示收发数据（带 RX/TX 前缀）。
* `-d/--device`：仅为 CLI 一致性保留，`monitor` 不使用虚拟串口；指定时
  会提示被忽略。
* 退出：`Ctrl+]`（终端原始模式）；`Ctrl+C` 亦可。标准输入不是终端
  （管道）时 `Ctrl+]` 不可用，按 `Ctrl+C` 退出。
* 与 `run` 共用连接/握手/心跳/重连选项，但不会创建 PTY/COM。

> 同一设备同时只允许一个串口客户端；`monitor` 运行期间不要再用 `run`
> 或网页终端连接同一设备。

## 5. 典型场景

### 5.1 picocom / minicom / screen（macOS / Linux）

```bash
# 终端 1：启动桥接
wserial run -H 192.168.1.50 -d /tmp/ttyWS0 -b 115200

# 终端 2：打开虚拟串口
picocom -b 115200 /tmp/ttyWS0
# Ctrl+A Ctrl+X 退出 picocom

# 或 minicom
minicom -D /tmp/ttyWS0 -b 115200

# 或 screen
screen /tmp/ttyWS0 115200
# Ctrl+A K 退出
```

也可以直接用文件 I/O 简单测试：

```bash
printf 'hello\n' > /tmp/ttyWS0
cat /tmp/ttyWS0
```

### 5.2 Arduino IDE

1. 先运行 `wserial run -H <IP> -d /tmp/ttyWS0 -b 115200`；
2. Arduino IDE → 工具 → 端口：选择 `/dev/ttyWS0`；
3. 打开串口监视器，波特率与目标一致。

> Arduino IDE 上传（烧录）目标板**不能**通过本工具完成：无线串口只透传
> 数据，不转发目标板的 USB 下载信号。请用目标板自身 USB 或外部编程器。
> 若需要打开监视器时自动复位目标，可加 `--reset-on-start`，并在
> [wiring.md](wiring.md) 中接好 DTR/RTS → EN/IO0。

### 5.3 PlatformIO

```ini
; platformio.ini
[env:myboard]
; 上传仍然走目标板本地 USB；这里只把监视器指向虚拟串口
monitor_port = /tmp/ttyWS0
monitor_speed = 115200
```

Windows 下把 `/tmp/ttyWS0` 换成 `COM11`（com0com 对中用户程序侧）。

### 5.4 PuTTY / 串口助手（Windows）

1. 用 `scripts/install-com0com.ps1` 创建 `COM11 <-> COM12`；
2. `wserial run --host <IP> --device COM12 -b 115200`；
3. PuTTY → Serial → `COM11`，115200 8N1；
4. 打开 PuTTY 后即可收发。

### 5.5 网页串口终端（无工具）

浏览器打开 `http://<设备IP>/`（SoftAP 下 `http://192.168.4.1`），使用
内置终端收发数据、修改串口参数。注意：网页终端与 `wserial` **不能同时**
连接（单客户端）。

### 5.6 无硬件联调

```bash
make sim
# 或: bash scripts/run-sim.sh
# 模拟器 + wserial 自动启动，虚拟串口 /tmp/ttyWS0
# 另开终端: picocom -b 115200 /tmp/ttyWS0
```

更多模拟器参数（延迟/丢包注入）见 `scripts/run-sim.sh` 的帮助。

## 6. RAW 兼容模式

RAW 端口（默认 3334）是纯 TCP 字节流：无握手、无帧、无 CRC，适合现成
工具。**RAW 与 WSP 互斥**，且无法远程修改波特率/流控/DTR-RTS，也没有
心跳机制。

### 6.1 wserial 自身

```bash
wserial run --host 192.168.1.50 --raw            # 默认连接 3334
wserial run --host 192.168.1.50 --port 3334 --raw
wserial monitor --host 192.168.1.50 --raw
```

### 6.2 macOS / Linux：socat

```bash
socat -d -d pty,link=/tmp/ttyWS0,raw,echo=0 tcp:192.168.1.50:3334
# 然后 picocom -b 115200 /tmp/ttyWS0
```

### 6.3 Windows：com2tcp / hub4com

com0com 发行包自带 `com2tcp`：

```powershell
# COM12 是 wserial 通常使用的一侧；这里直接由 com2tcp 占用 COM12
com2tcp \\.\COM12 192.168.1.50 3334
# 用户程序打开 COM11 即可
```

或使用 hub4com：

```powershell
hub4com --baud=115200 --route=0:1 \\.\COM12 \\.\COM11
```

> 新版 Windows 更推荐 `wserial run --raw`，可自动重连并给出更清晰的错误。

### 6.4 快速测试（nc）

```bash
# 向目标 UART 发送字节
printf 'hello\n' | nc 192.168.1.50 3334
```

## 7. 自动重连与设备占用的行为

* 连接断开（WiFi 抖动、设备重启、目标掉电）后，wserial 默认重连：
  1s、2s、4s、8s、10s、10s… 直到成功或用户 Ctrl+C。
* 重连成功后重新握手并重发 UART_CFG；PTY 节点/COM 口保持不变。
* 设备同一时刻只接受一个客户端：
  * 若被其它 wserial、网页终端或 `socat` 占用，会收到 `BUSY` 或直接断开，
    `wserial` 以退出码 4 结束并提示「设备被占用」；
  * 处理：关闭其它客户端，或等待设备空闲超时（默认 120s）。
* 使用 `--no-reconnect` 时，断开即退出，适合脚本/一次性采集。

## 8. 日志与诊断

```bash
wserial run -v                          # 连接、握手、重连、错误详情
wserial run --log-level debug           # 更详细
wserial run --log-level trace           # 最详细（帧级）
wserial list --json > devices.json      # 脚本化发现
```

常见检查：

```bash
# 设备端口是否可达（WSP/RAW）
nc -vz 192.168.1.50 3333
nc -vz 192.168.1.50 3334

# HTTP 状态
curl http://192.168.1.50/api/status

# 虚拟串口是否存在
ls -l /tmp/ttyWS0
```

## 9. 已知限制

* 单客户端：WSP、RAW、网页终端互斥（设计约束）。
* RAW 模式无保活、无统计、不能远程改串口参数。
* 不支持把无线串口当作目标板的烧录通道。
* Windows 依赖 com0com，需自行安装并注意驱动签名。
* 无传输加密与客户端认证（局域网内使用；不要暴露到公网）。
* `monitor` 模式不做行编辑/历史，只做原始终端透传。

路线图（尚未实现）：TLS/wss、`wserial ota`、原生 Windows 驱动、多客户端、
RS485 方向控制。详见 [faq.md](faq.md) 与 [architecture.md](architecture.md)。

## 10. 相关文档

* [protocol.md](protocol.md) — WSP 协议与 HTTP API
* [installation.md](installation.md) — 三平台安装与 com0com
* [troubleshooting.md](troubleshooting.md) — 排错清单
* [wiring.md](wiring.md) — 引脚与电平
* [faq.md](faq.md) — 常见问题
