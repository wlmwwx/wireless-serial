# WSP/1 — 无线串口协议规范

> 本文是固件（ESP32-C3）与主机端工具（`wserial`）之间的**唯一接口契约**。
> 两端的帧编解码实现必须与本文及 `firmware/main/include/wsp_proto.h` 完全一致。
> 协议版本：**1**（帧头高 4 位为版本号）。

---

## 1. 传输与端口

ESP32-C3 加入 WiFi 后同时监听三个端口：

| 端口 | 名称 | 协议 | 用途 |
|------|------|------|------|
| 3333 | WSP | 本规范定义的帧协议 | `wserial` 工具默认使用，支持远程配置波特率 / 流控 / DTR-RTS |
| 3334 | RAW | 纯 TCP 字节流 | 兼容 `socat`、`com0com + com2tcp/hub4com`、`nc` 等现成工具 |
| 80   | HTTP | HTTP/1.1 + WebSocket | 浏览器配置页与网页串口终端 |

* 同一时刻**只允许一个串口客户端**（WSP 或 RAW 二者互斥），因为目标硬件的 UART 是独占资源。
* 端口号可通过 Kconfig / NVS / HTTP API 修改。
* 服务发现：mDNS 服务类型 `_wserial._tcp.local`，TXT 记录包含 `proto=1`、`raw=3334`、`name=<设备名>`、`baud=<当前波特率>`。
* 所有多字节整数均为**小端序（little-endian）**。

---

## 2. 帧格式

```
 偏移  长度  字段
  0     1    magic0 = 'W' (0x57)
  1     1    magic1 = 'S' (0x53)
  2     1    ver_type：高 4 位 = 协议版本 (1)，低 4 位 = 帧类型
  3     1    flags：保留，必须为 0
  4     2    payload 长度 L，小端，范围 0..1024
  6     2    CRC-16/CCITT-FALSE，小端
  8     L    payload
```

* 帧头固定 **8 字节**，最大帧长 `8 + 1024 = 1032` 字节。
* **CRC 计算范围**：先对前 6 个字节（magic0..长度字段）计算，再对 payload 逐字节继续计算；CRC 字段自身不参与。
* 参数：poly `0x1021`，init `0xFFFF`，不反射，无最终异或（即 CCITT-FALSE）。

### 2.1 CRC 参考实现

```c
uint16_t crc16_update(uint16_t crc, const uint8_t *d, size_t n) {
    for (size_t i = 0; i < n; i++) {
        crc ^= (uint16_t)d[i] << 8;
        for (int b = 0; b < 8; b++)
            crc = (crc & 0x8000) ? (uint16_t)((crc << 1) ^ 0x1021) : (uint16_t)(crc << 1);
    }
    return crc;
}
// 帧 CRC = crc16_update(crc16_update(0xFFFF, header6, 6), payload, L);
```

### 2.2 测试向量（必须逐字节一致）

| 用途 | 十六进制字节 |
|------|--------------|
| `CRC("123456789")` | `29 B1` |
| DATA, payload=`"Hi"` | `57 53 12 00 02 00 11 0A 48 69` |
| HELLO, payload=`01 00 00 04` | `57 53 10 00 04 00 09 37 01 00 00 04` |
| STATS 请求（空 payload） | `57 53 17 00 00 00 AF 07` |
| PING, payload=`01 02 03 04` | `57 53 18 00 04 00 E0 12 01 02 03 04` |

> 校验：`57 53 17 00 00 00 AF 07` 中 `0x07AF` 是 header `57 53 17 00 00 00` 的 CRC（小端存放为 `AF 07`）。

---

## 3. 帧类型

| 值 | 名称 | 方向 | payload |
|----|------|------|---------|
| `0x0` | HELLO | C→D | `wsp_hello_t` (4 B) |
| `0x1` | HELLO_ACK | D→C | `wsp_dev_info_t` (62 B) |
| `0x2` | DATA | 双向 | 1..1024 字节原始 UART 数据 |
| `0x3` | UART_CFG | C→D | `wsp_uart_cfg_t` (8 B) |
| `0x4` | UART_CFG_ACK | D→C | `wsp_uart_cfg_t` (8 B)，设备实际生效的配置 |
| `0x5` | LINE_CTRL | C→D | 1 字节位掩码 |
| `0x6` | LINE_STATUS | D→C | `wsp_line_status_t` (2 B) |
| `0x7` | STATS | C→D 空 / D→C | `wsp_stats_t` (24 B) |
| `0x8` | PING | 双向 | 0..64 字节不透明数据 |
| `0x9` | PONG | 双向 | 原样回显 PING 的 payload |
| `0xA` | ERROR | D→C | 1 字节错误码 + UTF-8 文本 |
| `0xB` | RESET_TARGET | C→D | `wsp_reset_cmd_t` (4 B) |
| `0xC` | INFO | C→D 空 / D→C | `wsp_dev_info_t` (62 B) |

方向：C = 主机客户端，D = 设备（ESP32-C3）。

### 3.1 `wsp_hello_t`（4 字节，C→D）

| 偏移 | 类型 | 字段 | 说明 |
|------|------|------|------|
| 0 | u8 | proto_ver | 必须为 1 |
| 1 | u8 | flags | 保留，0 |
| 2 | u16 | max_payload | 客户端可接收的最大 DATA payload，通常 1024 |

### 3.2 `wsp_dev_info_t`（62 字节，D→C）

| 偏移 | 类型 | 字段 | 说明 |
|------|------|------|------|
| 0 | u8 | proto_ver | 1 |
| 1 | u8 | fw_major | 固件主版本 |
| 2 | u8 | fw_minor | 固件次版本 |
| 3 | u8 | fw_patch | 补丁号 |
| 4 | u32 | max_payload | 设备支持的最大 payload（1024） |
| 8 | u32 | baud | 当前目标 UART 波特率 |
| 12 | u8 | data_bits | 5..8 |
| 13 | u8 | parity | 0=无, 1=奇, 2=偶 |
| 14 | u8 | stop_bits | 1..2 |
| 15 | u8 | flow | 0=无, 1=RTS/CTS |
| 16 | char[16] | name | 设备名，UTF-8，不足补 `\0` |
| 32 | u8[6] | mac | WiFi MAC |
| 38 | char[8] | chip | `"esp32c3"`，补 `\0` |
| 46 | u32 | uptime_s | 运行秒数 |
| 50 | u16 | wsp_port | WSP 端口（3333） |
| 52 | u16 | raw_port | RAW 端口（3334） |
| 54 | u8[8] | reserved | 全 0 |

### 3.3 `wsp_uart_cfg_t`（8 字节）

| 偏移 | 类型 | 字段 | 取值 |
|------|------|------|------|
| 0 | u32 | baud | 50 .. 5000000 |
| 4 | u8 | data_bits | 5..8 |
| 5 | u8 | parity | 0/1/2 |
| 6 | u8 | stop_bits | 1/2 |
| 7 | u8 | flow | 0/1 |

非法值必须被拒绝：设备回 `ERROR(WSP_ERR_INVALID_ARG)`，且 UART 保持原配置不变。

### 3.4 LINE_CTRL / LINE_STATUS

`LINE_CTRL`（C→D）1 字节位掩码：

| 位 | 名称 | 含义 |
|----|------|------|
| 0 | `WSP_LINE_DTR` | 置位=断言 DTR（驱动设备 DTR GPIO 为低，等效于主机 DTR 有效） |
| 1 | `WSP_LINE_RTS` | 置位=断言 RTS（驱动 RTS GPIO） |
| 2 | `WSP_LINE_BREAK` | 置位=发送 BREAK（UART 保持低电平），清零恢复正常 |

`LINE_STATUS`（D→C）2 字节：

| 偏移 | 类型 | 字段 |
|------|------|------|
| 0 | u8 | state 位掩码：DTR=0x01, RTS=0x02, CTS=0x04, DSR=0x08, DCD=0x10, BREAK=0x20 |
| 1 | u8 | reserved |

设备在 LINE_CTRL 生效后立即回一帧 LINE_STATUS。

### 3.5 `wsp_stats_t`（24 字节，D→C）

| 偏移 | 类型 | 字段 |
|------|------|------|
| 0 | u32 | uart_rx_bytes：从目标硬件收到的字节数 |
| 4 | u32 | uart_tx_bytes：发往目标硬件的字节数 |
| 8 | u32 | frames_in：收到的合法 WSP 帧数 |
| 12 | u32 | frames_out：发出的 WSP 帧数 |
| 16 | u32 | crc_errors：CRC 错误帧数 |
| 20 | u32 | uart_overflows：因 RX 环形缓冲满而丢弃的字节数 |

### 3.6 ERROR

`code`（1 字节）+ 可选 UTF-8 文本。错误码：

| 码 | 名称 | 含义 |
|----|------|------|
| 0 | NONE | |
| 1 | BAD_VERSION | 协议版本不支持 |
| 2 | BAD_LENGTH | 长度越界 |
| 3 | BAD_CRC | CRC 校验失败 |
| 4 | NOT_HELLO | 握手前发送了其他帧 |
| 5 | UNSUPPORTED | 未知帧类型 |
| 6 | INVALID_ARG | 参数非法 |
| 7 | BUSY | 已有客户端连接 |
| 8 | INTERNAL | 内部错误 |
| 9 | UART_FAIL | UART 配置/驱动失败 |

收到 ERROR 后客户端**不应**立即断开，除非错误码为 BUSY 或协议版本不兼容。

### 3.7 RESET_TARGET（4 字节，C→D）

| 偏移 | 类型 | 字段 | 说明 |
|------|------|------|------|
| 0 | u8 | action | 0=拉低复位脚，1=释放，2=脉冲 |
| 1 | u8 | reserved | 0 |
| 2 | u16 | pulse_ms | action=2 时的高电平时间（1..5000），默认 120 |

注意：`action=2` 只驱动复位脚；如需进入下载模式，客户端应先 `LINE_CTRL` 拉低 BOOT 脚，再脉冲复位。

---

## 4. 会话流程

```
主机                                      设备
 |                                          |
 |------------ TCP connect :3333 ---------->|
 |------------ HELLO ---------------------->|   必须在 5s 内
 |<------------------- HELLO_ACK ----------|   设备信息 + 当前 UART 配置
 |------------ UART_CFG (可选) ------------>|   需要修改波特率时
 |<------------------- UART_CFG_ACK --------|
 |------------ DATA ----------------------->|   → 写入目标 UART TX
 |<------------------- DATA ----------------|   ← 目标 UART RX
 |------------ PING (空闲 15s) ------------>|
 |<------------------- PONG ----------------|
```

规则：

1. 连接建立后，客户端**必须**先发 HELLO；设备在收到 HELLO 前收到任何其他帧 → `ERROR(NOT_HELLO)` 并断开。
2. 设备 5 秒内未收到 HELLO → 直接关闭连接。
3. 握手成功后进入 **DATA 态**，可双向自由发送 DATA / UART_CFG / LINE_CTRL / STATS / PING / RESET_TARGET。
4. 设备空闲超时默认 **120 秒**（可通过 Kconfig 调整）；客户端建议每 **15 秒**发 PING，设备回 PONG。
5. 客户端断开后设备自动回到监听状态，1 秒内可接受新连接。
6. 新连接在 BUSY 时：设备发 `ERROR(BUSY)` 后关闭，或（默认配置 `latest_wins=false`）直接关闭。建议客户端显示"设备被占用"。

### 4.1 背压与丢包

* 主机→目标：DATA 帧按序写入 UART，使用阻塞写 + 超时。
* 目标→主机：UART RX 环形缓冲（默认 8 KB）→ 组帧发送。若 TCP 发送阻塞导致缓冲区满，丢弃新到的 UART 字节并累加 `uart_overflows`，同时通过 INFO/STATS 可查询。
* 主机侧若来不及消费 PTY，TCP 窗口会自然反压，不会丢数据。

---

## 5. 流控与 DTR/RTS 语义

* `flow=1`（RTS/CTS）时，设备使用硬件流控引脚；目标硬件的 CTS 未接时可能阻塞发送，建议默认 `flow=0`。
* DTR/RTS 默认**仅作为 GPI 输出**，可用于复位 Arduino/ESP 类目标：
  * 约定 `RTS → 目标 EN/RESET`（低有效）
  * 约定 `DTR → 目标 BOOT/IO0`（低有效）
  * 引脚编号在 Kconfig / NVS 中配置，默认 RESET=GPIO3、BOOT=GPIO10；未接线时无副作用。
* `LINE_STATUS` 中的 CTS/DSR/DCD 反映设备侧输入电平。
* 主机工具在 PTY 上收到用户对 DTR/RTS 的 ioctl（`TIOCM_DTR` 等）时，应映射为 LINE_CTRL 帧（Unix 下需由工具轮询/监听）。

---

## 6. RAW 兼容模式

端口 3334 为纯字节流：TCP 收到的字节原样写入目标 UART，UART 收到的字节原样写回 TCP。无握手、无帧、无 CRC。

限制：

* 无法远程修改波特率/流控/DTR-RTS（使用设备当前 NVS 配置）。
* 无保活机制；靠 TCP keepalive。
* 与 WSP 客户端互斥。

典型用法：

```bash
# macOS / Linux
socat -d -d pty,link=/tmp/ttyWS0,raw,echo=0 tcp:192.168.1.50:3334
# Windows（com0com 已建好 COM11<->COM12 对）
com2tcp \\.\COM12 192.168.1.50 3334
# 或 hub4com
hub4com --baud=115200 --route=0:1 \\.\COM12 \\.\COM11
```

> 注意：`com2tcp` 属于 com0com 发行包；新版 Windows 建议直接用 `wserial run --raw`。

---

## 7. 设备 HTTP / WebSocket 接口

用于无工具时的配置与调试（非 WSP 的一部分，但属于设备契约）。

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/` | 单页配置界面（状态、串口参数、WiFi 配网、网页串口终端） |
| GET | `/api/status` | JSON：name, chip, mac, ip, rssi, uptime, uart, stats, client_connected |
| GET | `/api/config` | JSON：当前持久化配置（不含 WiFi 密码明文，返回 `set:true/false`） |
| POST | `/api/config` | JSON 局部更新：`{"baud":9600,"parity":"even","name":"...","wifi":{"ssid":"..","pass":".."}}`，返回 200 + 新配置 |
| GET | `/api/scan` | JSON 数组：周边 WiFi 列表 `{ssid, rssi, auth}` |
| POST | `/api/reboot` | 重启设备 |
| WS | `/ws` | 二进制帧=串口数据；文本帧=JSON 控制指令 `{"cmd":"uart","baud":115200}`、`{"cmd":"line","dtr":0,"rts":0}` |

* STA 模式下页面无需认证（可选 Kconfig 开启 HTTP Basic Auth）。
* SoftAP 配网模式下 SSID 为 `wserial-XXXXXX`，默认密码 `wserial123`（Kconfig 可改），网关 `192.168.4.1`。

---

## 8. 版本演进规则

* 帧头高 4 位承载版本号；版本不匹配的设备/客户端必须回 `ERROR(BAD_VERSION)` 并断开，**不得猜测解析**。
* 新增帧类型使用低 4 位剩余值（`0xD`–`0xF`）；新增字段只能追加到结构体尾部并增大长度，旧端遇长 payload 应忽略尾部多余字节。
* 保留字段发送方必须置 0，接收方必须忽略。

---

## 9. 参考实现位置

| 内容 | 路径 |
|------|------|
| 协议常量与结构体（单一定义源） | `firmware/main/include/wsp_proto.h` |
| 帧编解码（可移植 C99，固件与测试共用） | `firmware/main/wsp_proto.c` |
| 主机端 Go 实现 | `host/internal/wsp/` |
| 一致性测试 | `test/proto_test.c`（C 编解码 ↔ 测试向量）、`host/internal/wsp/wsp_test.go`（Go 编解码） |
