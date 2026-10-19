# test/ — 无硬件验证设施

本目录提供**不依赖真实 ESP32** 的验证设施，用于验证 WSP/1 协议实现与
主机工具 `wserial`：

| 路径 | 内容 |
|------|------|
| `proto_test.c` | 纯 C99 协议一致性测试，直接链接 `firmware/main/wsp_proto.c`（固件真实编解码代码） |
| `sim/` | 独立 Go module 实现的“设备模拟器”（ESP32-C3 侧），无需 ESP-IDF |
| `sim/wire/` | 模拟器自带的 WSP/1 编解码包（与 `host/` 完全独立，可被其他测试 import） |
| `sim/smoke/` | 冒烟测试客户端：读取 addrfile，走一遍握手 / 双向数据 / PING / STATS |
| `run.sh` | 一条命令跑完 C 测试 + Go 测试 + 模拟器二进制冒烟测试 |
| `integration/` | 端到端集成测试（wserial ↔ sim ↔ 假目标硬件；`-tags integration`） |
| `integration.sh` | 一键构建 wserial + sim 并运行端到端集成测试 |

## 快速运行

```bash
test/run.sh              # 全部通过时退出码 0
CC=clang test/run.sh     # 指定编译器
```

`run.sh` 依次执行：

1. `cc -std=c99 -Wall -Wextra -Werror` 编译并运行 `proto_test.c`；
2. `cd test/sim && go build ./... && go vet ./...`；
3. `cd test/sim && go test ./... -count=1`；
4. 以随机端口启动真实模拟器二进制，通过 `-addrfile` 发现端口后运行冒烟客户端；
   结束时 **kill + wait**，不留后台进程。

Go 构建缓存默认放在 `test/build/gocache`（可用环境变量 `WSP_TEST_GOCACHE` 覆盖，
例如 `WSP_TEST_GOCACHE="$HOME/.cache/go-build" test/run.sh`），
所有产物都在被 `.gitignore` 忽略的 `test/build/` 下。

也可以单独运行：

```bash
cc -std=c99 -Wall -Wextra -Werror -Ifirmware/main/include \
   test/proto_test.c firmware/main/wsp_proto.c -o /tmp/proto_test && /tmp/proto_test

cd test/sim && go test ./... -count=1 -v
```

## C 一致性测试覆盖（`proto_test.c`）

- CRC-16/CCITT-FALSE 参考向量 `CRC("123456789") = 0x29B1`
- `docs/protocol.md` §2.2 全部 4 组帧向量（DATA / HELLO / STATS / PING）
  逐字节编码一致 + 流式解析还原
- encode→decode 往返：payload 长度 0/1/2/3/7/64/255/1023/1024；
  1025 越界被拒绝；输出缓冲边界（恰好 / 差 1 字节）
- 流式解析：逐字节喂入；7 字节分块（跨越 header/CRC/payload 边界）；
  粘包（一包多帧）；半包（任意边界分两次 feed）；解析器重新初始化
- 垃圾前缀重同步、帧间垃圾、`W X`、`W W S` 重启路径；`stat_resync` 计数
- CRC 错误：丢弃错误帧、返回 `WSP_E_CRC`、继续解析下一帧；`stat_bad_crc` 计数
- 版本错误（ver=0/2）：返回 `WSP_E_VERSION`、不猜测解析、`stat_bad_version` 计数
- 长度字段 > 1024（1025 / 0xFFFF）：`WSP_E_LENGTH` 后恢复解析
- NULL 参数、NULL 回调、保留 flags 忽略、200 组随机长度随机分块往返
- 退出码 0/1，输出 `[PASS]/[FAIL]` 分节摘要与 `RESULT: PASS|FAIL`

## 设备模拟器（`test/sim`）

```bash
cd test/sim
go run . -wsp 127.0.0.1:0 -raw 127.0.0.1:0 -hw 127.0.0.1:0 \
         -admin 127.0.0.1:0 -addrfile /tmp/sim.json -log
```

标准输出第一行是机器可读的就绪行（端口均为实际随机端口）：

```
SIM_READY {"wsp":"127.0.0.1:51234","raw":"127.0.0.1:51235","hw":"127.0.0.1:51236","admin":"127.0.0.1:51237","wsp_port":51234,"raw_port":51235,"hw_port":51236,"admin_port":51237,"pid":12345}
```

### 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-wsp` | `127.0.0.1:0` | WSP/1 帧协议监听地址（等价设备 TCP 3333） |
| `-raw` | `127.0.0.1:0` | RAW 透传监听地址（等价设备 TCP 3334） |
| `-hw` | `127.0.0.1:0` | “目标硬件侧”监听地址（测试进程扮演目标 MCU） |
| `-admin` | `127.0.0.1:0` | 可选 HTTP 状态端点（空字符串 `""` 关闭） |
| `-addrfile` | 空 | 把实际监听地址**原子写入** JSON 文件（供集成测试轮询） |
| `-baud` | `115200` | 初始目标 UART 波特率（8N1，flow=0） |
| `-name` | `wserial-sim` | 设备名（≤16 字节，写入 HELLO_ACK） |
| `-latency` | `0s` | 目标→主机方向每块数据额外延迟（延迟注入） |
| `-drop` | `0` | 目标→主机方向每块数据的丢弃概率 0..1（丢包注入） |
| `-log` | `false` | 把每一帧收发摘要打到 stderr |
| `-hello-timeout` | `5s` | HELLO 等待时间，超时关闭连接 |
| `-idle` | `120s` | 会话空闲超时 |
| `-version` | | 打印版本 |

### 端口与地址约定（集成测试契约）

- 三个业务端口都支持 `host:port`；端口写 `0` 时由内核分配，实际值写入
  `-addrfile`，并同时打印在 `SIM_READY` 行。
- **addrfile JSON 结构**（`simAddrs`，额外字段 `pid` 用于清理）：

```json
{
  "wsp": "127.0.0.1:51234",
  "raw": "127.0.0.1:51235",
  "hw": "127.0.0.1:51236",
  "admin": "127.0.0.1:51237",
  "wsp_port": 51234,
  "raw_port": 51235,
  "hw_port": 51236,
  "admin_port": 51237,
  "pid": 12345
}
```

- 集成测试推荐流程：构建模拟器二进制 → 用 `-wsp 127.0.0.1:0 ... -addrfile /tmp/xxx.json`
  启动 → 轮询 addrfile 出现（或读 stdout 的 `SIM_READY`）→ 从 JSON 取端口 →
  跑测试 → 结束时 `SIGTERM` + `wait`。

### “硬件侧”端口（`-hw`）语义

测试进程连接到该端口后**扮演目标硬件**：

- 测试进程**写入**的字节 = 目标硬件通过 UART 发出的数据 → 模拟器转发给串口客户端
  （WSP 客户端收到 DATA 帧；RAW 客户端收到原始字节）；
- 测试进程**读出**的字节 = 串口客户端通过 UART 发往目标硬件的数据
  （RAW 模式为原始字节）。
- 同一时间只保留一个硬件侧连接（后连的替换先连的）；硬件侧未连接时，
  客户端写入的字节最多缓冲 1 MiB，连接后自动 flush，超出部分计入
  `uart_overflows`。

### WSP 行为

- 连接后 5 秒内必须发 HELLO（`01 00 00 04`），否则关闭连接；
  握手前发其他帧回 `ERROR(NOT_HELLO)` 并断开。
- 握手 → `HELLO_ACK`（62 字节 `wsp_dev_info_t`，含实际 `wsp_port`/`raw_port`）。
- DATA 双向；`UART_CFG` 校验（baud 50..5000000、5..8/0..2/1..2/0..1），
  非法回 `ERROR(INVALID_ARG)` 且保持原配置，合法回 `UART_CFG_ACK`。
- `LINE_CTRL` → 立即回 `LINE_STATUS`（DTR/RTS/BREAK 位）。
- `STATS` → 24 字节计数；`PING`/`PONG` 原样回显（0..64 字节）；
  `INFO` → 62 字节设备信息；`RESET_TARGET` 校验并记录；未知类型 → `ERROR(UNSUPPORTED)`。
- 版本错误 → `ERROR(BAD_VERSION)` 并断开；CRC 错误 → `ERROR(BAD_CRC)` 后继续。
- **单客户端**：已有 WSP/RAW 客户端时，新的 WSP 连接收到 `ERROR(BUSY)` 后关闭；
  RAW 连接直接关闭（RAW 无帧协议，不能回错误帧）。客户端断开后立即可重连。

### 状态查询端点（`-admin`）

`GET /status` 返回 JSON（供集成测试断言设备侧状态，例如确认 `UART_CFG` 生效）：

```json
{
  "addrs": { "wsp": "...", "raw": "...", "hw": "...", "admin": "...", "...": 0, "pid": 0 },
  "device": {
    "device_name": "wserial-sim",
    "baud": 9600, "data_bits": 8, "parity": 1, "stop_bits": 1, "flow": 0,
    "line_state": 0,
    "serial_client": "wsp",
    "hw_connected": true,
    "uart_cfg_applied": 1,
    "last_reset": { "action": 2, "pulse_ms": 120, "count": 1 },
    "stats": {
      "uart_rx_bytes": 5, "uart_tx_bytes": 10,
      "frames_in": 4, "frames_out": 3,
      "crc_errors": 0, "uart_overflows": 0
    },
    "recent_frames": [ { "type": 3, "name": "UART_CFG", "len": 8 } ]
  }
}
```

`GET /healthz` 返回 `ok`。

## 模拟器的 Go 单元测试（`test/sim/*_test.go`）

- 编解码（`sim/wire`）：协议向量、分块读取（1/2/3/5/7/8/9/13 字节）、
  垃圾/`WWS` 重同步、BAD_VERSION/BAD_LENGTH/BAD_CRC 错误分类、截断帧、Chunk 分片
- 模拟器：HELLO/HELLO_ACK/INFO/PING/PONG、双向 DATA（含 1024 字节边界、
  64 KiB 大块分帧）、UART_CFG 合法/非法/边界/长度错误、LINE_CTRL 位映射、
  STATS 计数、RESET_TARGET、未知帧、NOT_HELLO、HELLO 超时/版本错误、
  BAD_VERSION 断连、BAD_CRC 恢复、BUSY（WSP/RAW 互斥）、RAW 透传、
  latency/drop 注入、addrfile/READY 契约、连接复用与清理

## 已知限制

- 不实现设备 HTTP 配置页 / WebSocket / mDNS（见 `docs/protocol.md` §7）；
  这些不在 WSP 一致性范围内，集成测试用 `-admin /status` 代替设备侧观测。
- 不是真实 UART：波特率仅记录在设备状态中，不模拟电气时序。
- `-latency`/`-drop` 以“硬件侧一次 read 的块”为单位注入，不是逐字节模型。
- 硬件侧只支持一个连接（后连接替换先连接）；串口客户端严格互斥（WSP 或 RAW）。

## 端到端集成测试（`test/integration/` + `integration.sh`）

验证完整链路：**用户程序（虚拟串口 / 终端）↔ wserial（host/）↔ WSP/RAW over TCP ↔
设备模拟器 ↔ 假目标硬件（TCP 硬件侧端口）**，全程无真实 ESP32。

```bash
test/integration.sh                    # 构建 wserial/sim + 跑全部集成测试
test/integration.sh -run TestVirtualSerial -v   # 只跑虚拟串口相关
cd test && go test -tags integration ./... -v    # 已构建过二进制时可直接跑
```

`integration.sh` 会：
1. `cd host && go build -o test/build/wserial ./cmd/wserial`（task-2 产物，不修改 host/）；
2. `cd test/sim && go build -o test/build/sim .`；
3. `cd test && go test -tags integration ./... -v -count=1 -timeout 10m`。

Go 缓存策略：脚本优先使用用户默认 `GOCACHE`/`GOMODCACHE`（可写时）；若默认缓存不可写
（沙箱、只读 `$HOME`、CI 等），自动回退到 `test/.cache/gocache` 与
`test/.cache/gomodcache`，并同样用于 `wserial` 的构建；也可显式指定
`WSP_TEST_GOCACHE` / `WSP_TEST_GOMODCACHE`。普通宿主机上完整验证（含 PTY 虚拟串口）：

```bash
WSP_REQUIRE_PTY=1 test/integration.sh
```

`WSP_REQUIRE_PTY=1` 会让 PTY 不可用从 SKIP 变成硬失败；不带该变量时，PTY 不可用的环境
会跳过 5 个 `TestVirtualSerial*`，只跑 monitor 模式测试（见下）。

### 验证结果（真实 PTY，macOS 宿主机）

已在 macOS arm64 宿主机上（允许 `/dev/ptmx`）用完整权限执行
`WSP_REQUIRE_PTY=1 test/integration.sh`，**连续 3 次全部通过，exit=0**：

- 每次 **10 个测试全部 PASS（无 SKIP）**，耗时 3–6s；示例输出
  `ok wirelessserial/test/integration 3.027s` → `INTEGRATION TESTS PASSED`
- 5 个 monitor 测试 + 5 个虚拟串口（PTY）测试均真实执行：
  `TestVirtualSerialWSPDataPath`（文本、0x00..0xFF 全字节 4 KiB、64 KiB 双向）、
  `TestVirtualSerialUARTCfg`（设备侧 baud=9600）、`TestVirtualSerialRaw`、
  `TestVirtualSerialReconnect`（杀 sim 同端口重启后自动重连、PTY 保持可用）、
  `TestVirtualSerialBusy`（第二个客户端 BUSY 且首会话存活）
- host 包内的真实 PTY 测试（`host/internal/bridge`）同时通过：
  `TestRunPTYEchoAndReconnect`、`TestRunPTYRawEcho`

因此本目录的端到端验证**不依赖真实 ESP32**，且虚拟串口路径已在 macOS 上实测。

### 测试场景（`test/integration/`）

| 测试 | 内容 |
|------|------|
| `TestVirtualSerialWSPDataPath` | PTY 虚拟串口：文本、0x00..0xFF 全部字节（4 KiB）、4 KiB/64 KiB 双向；符号链接指向 `/dev/` PTY |
| `TestVirtualSerialUARTCfg` | `-b 9600` → 设备收到 UART_CFG、回 ACK、设备侧 baud=9600（用 sim `/status` 断言），数据仍可用 |
| `TestVirtualSerialRaw` | `--raw` 模式：4 KiB/8 KiB 二进制双向透传 |
| `TestVirtualSerialReconnect` | 杀掉并重启 sim（同端口），wserial 自动重连；虚拟串口符号链接与打开的 fd 保持不变，双向数据恢复 |
| `TestVirtualSerialBusy` | 第二个 wserial 得到 `ERROR(BUSY)` + 非 0 退出码，第一个会话不受影响；失败者的符号链接被清理 |
| `TestMonitorWSPDataPath` | 同样的数据覆盖（文本/4 KiB/64 KiB）走 `wserial monitor`（stdin/stdout，无需 PTY） |
| `TestMonitorUARTCfg` | monitor 模式下 UART_CFG 生效断言 |
| `TestMonitorReconnect` | monitor 模式断线重连 |
| `TestMonitorRaw` | monitor 模式 RAW 透传 |
| `TestMonitorBusy` | monitor 模式 BUSY 错误与首会话存活 |

所有 TCP 端口由模拟器随机分配（重连测试复用原端口）；虚拟串口路径在 `t.TempDir()`
下，测试结束（含失败）会杀掉所有进程、关闭 PTY 并检查符号链接已清理。

### 受限环境（沙箱）中的 PTY 说明

虚拟串口测试需要在 `/dev/ptmx` 上创建 PTY。普通 macOS/Linux 终端可正常创建，已按上述
方式实测通过；若运行环境禁止（例如文件写沙箱拒绝 `open("/dev/ptmx")`，返回 EPERM），
5 个 `TestVirtualSerial*` 会 **SKIP** 并在输出中说明原因，此时 monitor 模式测试仍会完整
执行同一套 WSP/RAW/配置/重连/BUSY 逻辑（已连续 3 次通过）。受限环境验收用默认方式即可
（PTY 测试自动 SKIP）；需要完整覆盖时，在普通终端执行
`WSP_REQUIRE_PTY=1 test/integration.sh`。

Windows 无 PTY：`integration_windows_test.go` 以 build tag 跳过并注明需要 com0com
端口对（如 COM11↔COM12，`--device COM12`）。
