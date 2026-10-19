# 故障排查

按「现象 → 可能原因 → 排查命令 → 处理」组织。先做第 1 节的快速自检，再跳到
对应章节。

## 1. 快速自检

```bash
# 1) 主机工具是否可运行
wserial version

# 2) 设备是否被发现（WSP 设备广播 _wserial._tcp）
wserial list --timeout 5s

# 3) 设备端口是否可达（把 IP 换成实际值）
nc -vz 192.168.1.50 3333      # WSP
nc -vz 192.168.1.50 3334      # RAW
nc -vz 192.168.1.50 80        # HTTP

# 4) 网页状态（最直观：RSSI、UART 配置、stats、客户端占用）
curl -s http://192.168.1.50/api/status | python3 -m json.tool

# 5) 虚拟串口是否创建
ls -l /tmp/ttyWS0
```

一条命令汇总：

```bash
wserial run -H 192.168.1.50 -d /tmp/ttyWS0 -b 115200 -v
# 观察 stderr：发现/连接/握手/重连日志会指出失败阶段
```

## 2. mDNS 发现不到设备

**现象**：`wserial list` 空；`wserial run` 报未发现设备（退出码 4）。

**原因**：设备与电脑不在同一网段；路由器禁止组播；mDNS 被防火墙拦截；
设备未连上 WiFi（仍在 SoftAP）；设备名冲突。

**排查**：

```bash
# 设备实际 IP：从串口日志、路由器 DHCP 列表、或 HTTP 配置页获取
wserial list --timeout 10s

# 直接指定 IP，绕开 mDNS
wserial run --host 192.168.1.50 -v
wserial list --json

# 试试 mDNS 名
ping -c 1 wserial-ab12cd.local
dns-sd -B _wserial._tcp          # macOS，能列出服务
avahi-browse -rt _wserial._tcp   # Linux（需 avahi-utils）
```

**处理**：

1. 确认电脑与设备连同一个 AP/网段（SoftAP 模式下电脑要连 `wserial-XXXXXX`）。
2. 防火墙放行 UDP 5353（mDNS）与出站 TCP 3333/3334。
3. 直接 `--host <IP>` 即可工作，mDNS 只是便利功能。
4. 路由器若开启「AP 隔离/客户端隔离」，同网段设备互不可见，需关闭。

## 3. 发现设备但连不上

**现象**：`list` 能看到设备，`run` 连接超时/被拒绝/连上即断。

**排查**：

```bash
nc -vz 192.168.1.50 3333
curl -s http://192.168.1.50/api/status    # client_connected 是否为 true
wserial run -H 192.168.1.50 -v
```

**可能原因与处理**：

| 原因 | 判断 | 处理 |
|------|------|------|
| 已有客户端占用（BUSY） | `client_connected: true`；日志含 `BUSY` | 关闭其它 wserial/网页终端/socat；等待空闲超时（默认 120s） |
| 防火墙拦截入站 | 电脑/路由器防火墙 | 放行 TCP 3333/3334；跨网段时检查路由 |
| 连到了错误端口 | `--port` 指向 RAW 却按 WSP 握手 | WSP 用 3333；纯透传用 3334 + `--raw` |
| 设备刚重启 | 几秒内连接 | 等待启动完成；wserial 会自动重连 |
| IP 变化（DHCP） | 路由器重新分配 | 用 mDNS 名或设置静态租约；`--host wserial-XXXXXX.local` |
| 设备进入 SoftAP | 设备不在原 WiFi | 电脑连设备热点；打开 `http://192.168.4.1` 重新配网 |

## 4. 虚拟串口没有数据

**现象**：`/tmp/ttyWS0`（或 COM11）存在，但串口程序读不到任何数据，或写入
目标无反应。

**排查顺序**：

```bash
# 1) wserial 是否仍连接（-v 日志；或 /api/status 的 client_connected）
wserial run -H 192.168.1.50 -v

# 2) PTY 是否存在、权限是否可读写
ls -l /tmp/ttyWS0
# 期望: lrwxr-xr-x ... /tmp/ttyWS0 -> /dev/ttysNNN
# 若属主是 root：说明用 sudo 启动过，见下文

# 3) 设备侧统计（判断数据是否到达设备/是否被丢）
curl -s http://192.168.1.50/api/status

# 4) 换一个串口程序验证（排除程序自身问题）
picocom -b 115200 /tmp/ttyWS0
```

**常见原因**：

* **打开错了端口**（Windows）：用户程序应打开 COM11（com0com 对的一侧），
  wserial 打开 COM12（另一侧）；两侧都用同一侧会互相等待。
* **权限**：`/tmp/ttyWS0` 被 root 拥有（用 sudo 启动过 wserial）→
  停掉后用普通用户重启，执行 `rm -f /tmp/ttyWS0` 再试。
* **串口程序没打开 DTR/流控**：某些程序打开时自动拉 RTS/DTR，若接线把
  RTS 接 EN，可能让目标一直复位；检查接线或关闭程序的硬件流控。
* **目标波特率不匹配**：见第 5、6 节。
* **单向无数据**：检查 TX/RX 是否交叉接反、GND 是否共地。
* **PTY 数据被缓冲**：`picocom` 默认行缓冲显示，输入无回显时按回车；
  用 `--imap` 或换 `screen` 验证。

Windows 另见第 8 节（com0com 端口状态）。

## 5. 数据乱码

**现象**：收到可读但乱码的字符，或大量 `0x00`/随机字节。

**原因与处理**：

| 可能原因 | 验证 | 处理 |
|----------|------|------|
| 波特率不一致 | 目标手册/代码中的波特率 | `wserial run -b 9600` 或先用 `-b 0` 保持设备当前值 |
| 数据位/校验/停止位不一致 | 目标为 7E1、8N1 等 | `--databits 7 --parity even --stopbits 1` |
| 电平不匹配 | 目标 5V/RS232 直连 | 见 [wiring.md](wiring.md)，加电平转换 |
| 未共地 | 只接了 TX/RX | 连接 GND |
| 目标 UART 输出被其它设备干扰 | 断开目标只接 USB | 检查供电/接线/地环路 |
| 波特率过高导致误码 | 降速测试 | 降到 115200/57600 验证链路 |
| 目标输出的是二进制协议 | 不是文本 | 用十六进制显示（`wserial monitor` 或串口工具的 hex 模式） |

快速判断：把目标波特率在 9600/19200/38400/57600/115200 间切换测试；
`/api/status` 中的 `uart` 字段应与 `--baud` 等参数一致。

## 6. 波特率/串口参数改不了

**现象**：传 `-b 9600` 后设备仍按 115200 工作，或出现 `INVALID_ARG`。

**原因**：

* RAW 模式（3334）不能远程改参数；
* 参数超出设备支持范围（`baud` 50..5000000，`data_bits` 5..8，`parity`
  0/1/2，`stop_bits` 1/2，`flow` 0/1）；
* 目标硬件自身固定波特率，改了设备侧也没用（需同步改目标）。

**处理**：

```bash
# 用 WSP 模式设置参数（不要加 --raw）
wserial run -H 192.168.1.50 -b 9600 --databits 8 --parity none --stopbits 1 -v

# 用 -b 0 保持设备当前值
wserial run -H 192.168.1.50 -b 0 -v

# 查看设备当前生效参数
curl -s http://192.168.1.50/api/status | python3 -m json.tool
```

注意：`UART_CFG` 被设备拒绝时会回 `ERROR(INVALID_ARG)` 且保持原配置；
检查 `-v` 日志中的错误码。

## 7. 自动重连不工作 / 频繁断开

**现象**：WiFi 抖动后虚拟串口长时间无数据；或反复连接断开。

**排查**：

```bash
wserial run -H 192.168.1.50 --ping-interval 10s -v   # 观察重连日志
ping -c 20 192.168.1.50                              # 看丢包/延迟
curl -s http://192.168.1.50/api/status | grep rssi
```

**处理**：

* 信号弱（RSSI < -75 dBm）：靠近路由器/更换信道；避免 2.4 GHz 拥塞。
* 设备空闲超时（默认 120s）不会触发链路断开，但保活失败会触发重连；
  不要把 `--ping-interval` 设得大于设备超时。
* 目标板掉电/复位会中断 UART 数据但 TCP 仍可能保持；检查目标供电。
* 使用 `--no-reconnect` 时断开即退出，属预期。
* 需要断线期间不丢数据？不可能：断链瞬间在途数据会丢（见
  [architecture.md](architecture.md) 第 6 节）。

## 8. Windows：com0com 驱动签名与端口问题

### 8.1 设备管理器有黄色感叹号

```powershell
# 查看 com0com 端口状态
Get-PnpDevice -Class Ports | Format-Table -AutoSize
& "${env:ProgramFiles(x86)}\com0com\setupc.exe" list
```

**处理**：

1. 安装 3.0.0.0 的 **signed** 包（`scripts/install-com0com.ps1` 默认）；
2. 卸载旧版 → 重启 → 重新安装；
3. 仍不行时按 [installation.md](installation.md#63-驱动签名问题) 临时禁用
   驱动签名强制（仅测试机）。

### 8.2 端口不存在 / 打开失败

* 设备管理器中确认 COM11/COM12 都在；不在则重新创建端口对；
* COM 口号可能被其它软件改动：`wserial list` 与 `setupc list` 对照；
* 端口被占用：关闭占用程序（串口助手、Arduino IDE、另一个 wserial）：

```powershell
# 查看哪些进程打开了 COM 口（需 Sysinternals handle 或 PowerShell 枚举）
Get-Process | Where-Object { $_.Name -match "arduino|putty|wserial" }
```

* 用 `wserial run --device COM12 -v`：若报「无法打开」，先确认另一侧
  COM11 未被 wserial 自己占用。

### 8.3 用户程序与 wserial 抢同一端口

两者必须各占一侧：wserial → COM12，用户程序 → COM11（或反过来）。
如果用户程序打开 COM12，会和 wserial 冲突。

### 8.4 管理员权限

安装/创建端口对需要管理员权限；**运行 wserial 不需要**。不要用管理员
PowerShell 长期跑 wserial，避免端口权限异常。

## 9. 权限问题（macOS / Linux）

**现象**：`/dev/ttyWS0` 无法打开；`picocom` 报 `Permission denied`；
真实 USB 串口打不开。

```bash
ls -l /tmp/ttyWS0
ls -l /dev/ttyUSB0 /dev/ttyACM0 2>/dev/null
id | tr ',' '\n' | grep -E 'dialout|uucp'
```

**处理**：

```bash
# 1) PTY 归 root（曾用 sudo 启动）：用普通用户重启
sudo rm -f /tmp/ttyWS0
wserial run -d /tmp/ttyWS0

# 2) Linux 真实串口：加入串口组后重新登录
sudo usermod -aG dialout "$USER"   # Debian/Ubuntu
# 或 sudo usermod -aG uucp "$USER" # Arch/Fedora
groups   # 确认已生效

# 3) 谁占用了串口
lsof /dev/ttyUSB0
```

macOS 的 PTY 由内核创建，普通用户即可读写，一般无权限问题。

## 10. 端口被占用

```bash
# macOS / Linux
lsof -nP -iTCP:3333 -iTCP:3334
lsof /tmp/ttyWS0

# Windows
netstat -ano | findstr :3333
```

* 同一设备的串口客户端互斥：关闭多余 wserial / 网页终端 / socat。
* 本机 3333 被其它程序占用时，改用设备实际 WSP 端口（Kconfig/HTTP 可改）。
* 虚拟串口路径冲突：换 `-d /tmp/ttyWS1`。
* 残留符号链接：`rm -f /tmp/ttyWS0` 后重启。

## 11. 网页配置页打不开

```bash
curl -v http://192.168.1.50/
curl -s http://192.168.1.50/api/status
```

* SoftAP 模式下地址是 `http://192.168.4.1`，电脑需连 `wserial-XXXXXX`；
* 浏览器缓存/HTTPS 强制跳转：用无痕窗口或 `http://` 显式访问；
* 设备 IP 已变：看串口日志或路由器列表；
* 设备重启中：等待 5–10 秒；
* 与 WSP 客户端互斥：网页串口终端打开时 wserial 会连不上，反之亦然。

## 12. 目标板一直复位 / 无法进入下载模式

**现象**：目标板反复重启，或无法通过 DTR/RTS 自动下载。

* 检查 DTR/RTS 映射极性：约定 RTS→EN、DTR→BOOT，均**低有效**；
* 若目标板自带 USB 下载电路，可能与外接线冲突，断开外接 EN/IO0 再试；
* 关掉串口程序的硬件流控（避免其拉 RTS）；
* 只用 `--reset-on-start` 复位、不接 BOOT 时，目标不会进入下载模式
  （这是预期：`RESET_TARGET` 只驱动复位脚）；
* ESP32-C3 自身烧录与目标板复位是两回事，见
  [firmware-build.md](firmware-build.md) 第 5.4 节。

## 13. 固件烧录/启动问题

| 现象 | 处理 |
|------|------|
| `Failed to connect to ESP32-C3` | 按 BOOT+RST 进下载模式；确认串口名；换数据线；降低烧录波特率到 115200 |
| `serial port is busy` | 关闭 `idf.py monitor`、串口助手、wserial 等占用程序 |
| 烧录成功但无日志 | 按 RST；确认波特率与 monitor 一致；检查 USB 线是否仅充电线 |
| 反复重启 | 供电不足/看门狗；用独立 5V 电源，查看 reset reason 日志 |
| 连不上 WiFi | 检查 SSID/密码（2.4 GHz only）；看日志；用 SoftAP 配网 |
| 忘记 WiFi 配置 | 串口日志提示 SoftAP 回退；或擦除 NVS：`idf.py -p PORT erase-flash` 后重烧 |

## 14. 收集诊断信息（提 issue 时附上）

```bash
# 主机侧
wserial version
wserial run -H <IP> -v 2> wserial.log
uname -a
ls -l /tmp/ttyWS0

# 设备侧
curl -s http://<IP>/api/status > status.json
curl -s http://<IP>/api/config > config.json

# 固件
cat firmware/build/flash_args 2>/dev/null
idf.py --version
```

请注明：平台（macOS/Linux/Windows 版本）、连接方式（WSP/RAW/网页）、
目标板型号与波特率、是否有中间网络设备（路由器/AP 隔离/VPN）。

## 15. 相关文档

* [installation.md](installation.md) — 安装、权限、com0com
* [host-usage.md](host-usage.md) — 选项与典型场景
* [wiring.md](wiring.md) — 接线与电平
* [firmware-build.md](firmware-build.md) — 烧录与启动
* [faq.md](faq.md) — 设计边界与常见疑问
