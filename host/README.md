# wserial — 无线串口主机端工具

`wserial` 是无线串口项目的主机端单二进制工具（Go，纯 Go 无 cgo）。它把
ESP32-C3 通过 WiFi 暴露的串口变成电脑上的**虚拟串口**，或者直接在终端里
收发：

```
用户程序 ──/tmp/ttyWS0(PTY)──┐
                            ├─ wserial ── TCP 3333 (WSP/1 帧协议) ── ESP32-C3 ── UART ── 目标硬件
终端 stdin/stdout ──────────┘               或 TCP 3334 (RAW 纯透传)
```

协议契约见仓库根目录 `docs/protocol.md`，权威结构体定义见
`firmware/main/include/wsp_proto.h`。本工具与固件的帧编解码逐字节一致
（CRC-16/CCITT-FALSE，小端序，测试向量见 `internal/wsp/wsp_test.go`）。

---

## 构建

需要 Go 1.25+。

```bash
cd host
go build -o wserial ./cmd/wserial        # 当前平台
go vet ./...
go test ./...
```

交叉编译（Windows 为纯 Go，无 cgo）：

```bash
GOOS=darwin  GOARCH=arm64 go build -o dist/wserial-darwin-arm64  ./cmd/wserial
GOOS=linux   GOARCH=amd64 go build -o dist/wserial-linux-amd64   ./cmd/wserial
GOOS=windows GOARCH=amd64 go build -o dist/wserial-windows.exe   ./cmd/wserial
```

---

## 子命令

```
wserial <command> [选项]

  list       通过 mDNS 列出局域网内的无线串口设备
  run        创建虚拟串口并桥接到设备
  monitor    在当前终端直接收发串口数据（不创建虚拟串口）
  version    显示版本与协议信息
  help       显示总帮助
```

所有日志都写入 **stderr**，stdout 只输出数据/表格；串口数据路径不打印任何日志。

### `wserial list`

通过 mDNS 浏览 `_wserial._tcp.local`，默认搜索 3 秒。

```bash
wserial list                  # 表格: 名称、IP、端口、MAC、波特率
wserial list --timeout 5s
wserial list --json           # JSON 数组，便于脚本处理
```

设备 mDNS 未发现时退出码为 **4**（`--json` 仍会输出 `[]`）。
MAC 来自 TXT 记录 `mac=`；固件未广播时显示 `-`。

### `wserial run`

创建虚拟串口并持续桥接，断线自动重连，虚拟串口不会消失。

```bash
wserial run                                   # mDNS 自动发现 + /tmp/ttyWS0
wserial run -H 192.168.1.50                   # 直连设备（默认端口 3333）
wserial run -H 192.168.1.50 -P 3333 -d /tmp/ttyWS1
wserial run -H 192.168.1.50 -b 9600           # 下发 UART_CFG 修改目标波特率
wserial run -H 192.168.1.50 -b 0              # 保持设备当前串口配置
wserial run -H 192.168.1.50 --raw             # RAW 模式（端口 3334）
wserial run --reset-on-start -H 192.168.1.50  # 连接后发送 120ms 复位脉冲
wserial run -v --log-level debug              # 详细日志（stderr）
```

| 选项 | 说明 |
|------|------|
| `-H, --host <host>` | 设备 IP/主机名；留空则 mDNS 自动发现 |
| `-P, --port <port>` | TCP 端口（默认 3333；`--raw` 时默认 3334） |
| `-d, --device <path>` | Unix：符号链接路径（默认 `/tmp/ttyWS0`）；Windows：com0com 端口的一端（如 `COM12`，必填） |
| `-b, --baud <n>` | 目标波特率（默认 115200；`0` = 保持设备当前值，不下发配置） |
| `--databits <5-8>` | 数据位（默认 8） |
| `--parity <none\|odd\|even>` | 校验位（默认 none） |
| `--stopbits <1\|2>` | 停止位（默认 1） |
| `--flow <none\|rtscts>` | 硬件流控（默认 none） |
| `--raw` | RAW 模式：连接 3334，纯字节透传，无握手/无帧/无心跳 |
| `--no-reconnect` | 断开后不自动重连 |
| `--reset-on-start` | 握手/配置后发送 `RESET_TARGET(action=pulse, 120ms)` |
| `--ping-interval <dur>` | WSP 心跳间隔（默认 15s；`0` = 关闭） |
| `-v, --verbose` | 等价于 `--log-level debug` |
| `--log-level <level>` | `error\|warn\|info\|debug\|trace`（默认 info） |

行为要点：

* 连接后先发 `HELLO`，5 秒内等 `HELLO_ACK`；随后按参数下发 `UART_CFG`
  并等待 `UART_CFG_ACK`。设备回 `ERROR(INVALID_ARG)` 时只告警、继续用设备原配置。
* PTY/COM 端口在整个运行期只创建一次：TCP 断开后按 **1s→2s→4s→8s→10s**
  指数退避重连，重连成功后重新握手并重发配置；已从虚拟串口读出但尚未发出的
  字节会保留到重连后再发。
* `DATA` 帧按 1024 字节分帧；双向各一个 goroutine，互不阻塞。
* 每 15s 发 `PING` 保活；收到 `ERROR(BUSY)` 或协议版本不兼容时停止重连。
* 按 **Ctrl+C / SIGTERM** 干净退出，并删除自己创建的 PTY 符号链接。
* 空闲时轮询 PTY 的 `TIOCMGET`，把用户程序设置的 DTR/RTS 变化映射为
  `LINE_CTRL` 帧（平台不支持时自动关闭该功能）。

运行成功后会打印（stderr）：

```
[info] 虚拟串口已就绪 /tmp/ttyWS0
[info] 用户程序可打开该设备收发数据；按 Ctrl+C 退出
[info] 已连接设备 ... 串口配置已生效: 115200 8N1 flow=none
```

### macOS / Linux 用法

`run` 用 `openpty(3)` 创建 PTY，把 slave 设为 raw 模式并 `chmod 0666`，
然后在 `--device` 指定路径创建指向 `/dev/ttysNNN`（Linux 为 `/dev/pts/N`）
的符号链接。之后任何串口程序打开该路径即可：

```bash
wserial run -H 192.168.1.50 &
screen /tmp/ttyWS0 115200
# 或 picocom -b 115200 /tmp/ttyWS0
# 或你自己的程序 open("/tmp/ttyWS0", O_RDWR)
```

若目标路径已存在且**不是**符号链接，工具会拒绝覆盖（避免误删真实设备）；
已存在的旧符号链接会被替换。退出时恢复删除。

### Windows 用法（com0com）

Windows 无法由普通程序创建虚拟串口，因此使用开源工具 **com0com** 建立一对
互联的 COM 端口，`wserial` 占用一端，用户程序使用另一端：

1. 安装 com0com（需管理员权限），打开 `Setup Command Prompt`；
2. 创建一对端口，例如 `COM11 <-> COM12`：
   ```
   install PortName=COM11 PortName=COM12
   ```
   （端口号以 `com0com` 实际分配为准，可在设备管理器确认）
3. 启动桥接，`--device` 指定 **wserial 自己打开的那一端**：
   ```bat
   wserial run -H 192.168.1.50 --device COM12
   ```
4. 用户程序打开 **对端**：`COM11`（波特率任意，数据由 wserial 桥接）。

`-d/--device` 在 Windows 下必填；未填写或端口打不开时会给出可操作的中文错误
（检查 com0com 是否建好端口对、端口是否被其他程序占用、是否用管理员权限）。

### `wserial monitor`

不创建虚拟串口，直接把当前终端接到设备串口，适合快速调试。

```bash
wserial monitor -H 192.168.1.50
wserial monitor -H 192.168.1.50 --hex     # 十六进制显示 RX/TX
wserial monitor --raw -H 192.168.1.50     # RAW 透传
printf 'AT\r\n' | wserial monitor -H 192.168.1.50
```

* 终端原始模式，收到什么显示什么（二进制直接输出到 stdout）；
* **Ctrl+]** 退出（管道输入时请用 Ctrl+C）；
* `--hex` 输出形如 `RX | 48 65 6c 6c 6f` / `TX | 41 54 0d 0a`；
* 支持自动重连与 `--reset-on-start`、`--baud` 等与 `run` 相同的会话选项；
* `-d/--device` 仅为 CLI 一致性保留，monitor 不使用。

### `wserial version`

```
wserial 0.1.0
WSP/1 无线串口主机端工具（darwin/arm64）
默认端口: WSP=3333 RAW=3334
构建: commit=dev date=unknown
```

可用 `-ldflags "-X main.version=... -X main.commit=... -X main.buildDate=..."` 注入。

---

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 成功（含 Ctrl+C/SIGTERM 干净退出） |
| 1 | 运行错误（连接失败、协议错误、虚拟串口创建失败等） |
| 2 | 命令行用法错误 |
| 4 | mDNS 未发现设备；或设备被占用（`ERROR(BUSY)`） |

---

## 协议实现摘要（WSP/1）

| 内容 | 位置 |
|------|------|
| 帧头/帧类型/常量 | `internal/wsp/consts.go` |
| CRC-16/CCITT-FALSE 与编码 | `internal/wsp/crc.go`、`frame.go` |
| 流式解析器（与 C 状态机逐字节一致，含重同步/错误计数） | `internal/wsp/parser.go` |
| 结构体编解码（hello/dev_info/uart_cfg/line_status/stats/error/reset） | `internal/wsp/structs.go` |
| 会话与握手 | `internal/wsp/session.go` |
| TCP、重连退避 | `internal/transport/` |
| 虚拟串口（PTY / com0com） | `internal/vport/` |
| mDNS 发现 | `internal/discovery/` |
| 桥接编排（run/monitor） | `internal/bridge/` |

测试重点：

* `internal/wsp/wsp_test.go`：`docs/protocol.md` §2.2 全部测试向量逐字节一致；
  逐字节/任意切分半包、粘包、垃圾前缀重同步、CRC 错误恢复、超长长度拒绝、
  版本不匹配、未知帧、结构体边界与非法参数拒绝；每字节都覆盖。
* `internal/wsp/session_test.go`：HELLO 握手、UART_CFG/ACK、ERROR(BUSY) 致命、
  INVALID_ARG 非致命、握手超时、握手期间收到 DATA 不丢失、DATA 1024 分帧。
* `internal/bridge/bridge_test.go`：内存虚拟串口 + 本地 mock 设备的端到端
  双向回显、UART_CFG 下发、PING、断线重连（虚拟端口不销毁）、RAW、monitor。
* `internal/discovery/discovery_test.go`：mDNS TXT 解析、表格/JSON 输出、去重排序，
  以及通过注入的假 browser 验证发现窗口会等待异步 mDNS 应答（不依赖真实组播）。
* `internal/bridge/pty_test.go`：真实 PTY 端到端（`TestRunPTYEchoAndReconnect`、
  `TestRunPTYRawEcho`）。受限沙箱（如 DSH workspace-write 禁止 `/dev/ptmx`）
  会自动 **SKIP**；在普通 macOS/Linux 终端上运行即为真实 PTY 验证：
  ```bash
  cd host && go test -v -run 'TestRunPTY' ./internal/bridge/
  ```
* 仓库 `test/integration/`（qa 维护）会驱动编译好的 `wserial` 二进制与
  `test/sim` 模拟器做端到端集成；PTY 不可用时同样自动跳过，设置
  `WSP_REQUIRE_PTY=1` 可强制失败。

---

## 已知限制

* **Windows DTR/RTS/BREAK** 不映射到 `LINE_CTRL`（Unix PTY 会在支持时轮询
  `TIOCMGET` 映射 DTR/RTS）；BREAK 暂不支持。
* macOS 的 pseudo-terminal 驱动通常不提供 modem 状态位，`TIOCMGET` 失败时
  DTR/RTS 映射会自动禁用（`--log-level debug` 可见）。
* `--raw` 模式按协议无握手/无保活，完全依赖 TCP；修改波特率/流控/线路状态
  需使用 WSP 模式。
* 设备同一时刻只允许一个串口客户端；第二个客户端会收到 `ERROR(BUSY)` 并以
  退出码 4 结束，同时清理自己的符号链接。
* mDNS TXT 记录不包含 MAC 时 `list` 的 MAC 列显示 `-`；`list` 只做发现，不校验
  设备是否可连通。
* `--baud 0` 表示保持设备当前串口配置，此时 `--databits/--parity/--stopbits/--flow`
  会被忽略（并打印告警）。
* 虚拟串口读到但尚未成功写入 TCP 的少量字节会在断线时保留并在重连后重发；
  极端情况下最后一帧可能重复（TCP 写成功但连接随即断开），串口应用应有
  帧/行协议容错。
* Windows 需用户自行安装并配置 com0com；本工具不创建端口对。
* 受限沙箱若禁止打开 `/dev/ptmx`，`run` 无法创建 PTY（会报
  `创建 PTY 失败: operation not permitted`），`monitor` 模式不受影响。
* mDNS 依赖局域网组播（224.0.0.251:5353）。若网络/沙箱屏蔽组播，`list` 会
  返回空并退出码 4；此时用 `-H/--host` 指定设备 IP 即可。

## 故障排查

| 现象 | 处理 |
|------|------|
| `未通过 mDNS 发现 _wserial._tcp 设备` | 确认设备与电脑在同一 WiFi；用 `wserial list` 或 `-H <IP>` 直连；部分网络屏蔽 mDNS 组播 |
| `连接 ... 失败: connection refused` | 设备端口被改过？用 `-P` 指定；`list --json` 可看到实际端口 |
| `设备返回 ERROR(BUSY)` | 已有其他 WSP/RAW 客户端（含 `socat`/网页终端），先断开 |
| `创建 PTY 失败: operation not permitted` | 受限沙箱/容器禁止 ptmx，请在普通终端运行 |
| 用户程序读不到数据 | 确认程序打开的是 `--device` 路径（符号链接），且配置了相同的串口参数；用 `-v` 看握手是否成功 |
| Windows 打开 COM 失败 | 确认 com0com 已安装端口对、`--device` 用的是 wserial 端、端口未被占用 |
