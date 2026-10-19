# 安装指南（macOS / Linux / Windows）

本文覆盖三种平台上主机工具 `wserial` 的安装、虚拟串口创建与权限配置。
固件构建与烧录见 [firmware-build.md](firmware-build.md)。

## 1. 前置条件

| 平台 | 需要 | 说明 |
|------|------|------|
| 通用 | Go 1.25+ | 源码构建或使用交叉编译产物；以 `host/go.mod` 的 `go` 指令为准 |
| macOS | 无需内核扩展 | 使用系统 PTY，无需安装驱动 |
| Linux | 无（PTY）；可选 `dialout/uucp` 组 | 打开**真实** USB 串口时才需要串口组权限 |
| Windows | com0com 3.0（已签名版） | 提供虚拟 COM 端口对；安装脚本见第 6 节 |

检查环境：

```bash
go version          # go1.25 或更新（以 host/go.mod 为准）
uname -a            # macOS / Linux
```

> 只有 PTY 方案（macOS/Linux）不需要 root 权限；不要把 `wserial` 以 sudo
> 运行，否则 PTY 符号链接可能归 root 所有，普通程序无法打开。

## 2. 从源码构建

```bash
cd /path/to/wireless-serial

# 方式 A：Makefile（推荐）
make host                     # 产物：host/wserial
./host/wserial version

# 方式 B：直接 go build
cd host
go build -o wserial ./cmd/wserial
./wserial version
```

运行测试（可选）：

```bash
make test                     # 协议单测 + Go 单测
```

### 2.1 交叉编译三平台

一条命令生成全部产物到 `host/dist/`：

```bash
make host-all
ls -lh host/dist/
# wserial-darwin-arm64      macOS Apple Silicon
# wserial-darwin-amd64      macOS Intel
# wserial-linux-amd64       Linux x86_64
# wserial-linux-arm64       Linux ARM64
# wserial-windows-amd64.exe Windows x64
# checksums.sha256          校验和
```

只构建指定平台：

```bash
bash scripts/build-host.sh darwin/arm64 linux/amd64
```

### 2.2 放入 PATH

```bash
# macOS / Linux
sudo install -m 0755 host/wserial /usr/local/bin/wserial
# 或用户目录（无需 sudo）
mkdir -p ~/.local/bin && install -m 0755 host/wserial ~/.local/bin/wserial
# 确保 ~/.local/bin 在 PATH 中
```

Windows（PowerShell）：

```powershell
New-Item -ItemType Directory -Force "$env:LOCALAPPDATA\wserial" | Out-Null
Copy-Item .\host\dist\wserial-windows-amd64.exe "$env:LOCALAPPDATA\wserial\wserial.exe"
# 之后把 %LOCALAPPDATA%\wserial 加入 PATH，或用完整路径运行
```

也可以直接使用 `host/wserial`、`host/dist/...`，不必安装到 PATH。

## 3. macOS 安装与使用

macOS 使用系统 PTY，无需驱动或内核扩展。

```bash
# 1) 发现设备（mDNS，需与 ESP32-C3 在同一局域网）
./host/wserial list

# 2) 创建虚拟串口并桥接（默认 /tmp/ttyWS0）
./host/wserial run

# 3) 另一个终端打开串口
picocom -b 115200 /tmp/ttyWS0
# 或 screen /tmp/ttyWS0 115200
```

说明：

* 默认设备节点 `/tmp/ttyWS0`，可用 `-d /tmp/ttyWS1` 指定其它路径。
* `wserial` 会创建符号链接（chmod 0666），退出时清理；异常退出后如残留，
  直接 `rm /tmp/ttyWS0` 即可。
* 若 `wserial list` 找不到设备，可直接指定 IP：
  `./host/wserial run --host 192.168.1.50`（IP 见路由器或设备串口日志）。
* macOS 首次运行时可能弹出「允许传入网络连接」的防火墙提示，允许即可；
  mDNS 使用 UDP 5353。

## 4. Linux 安装与使用

Linux 同样使用系统 PTY：

```bash
./host/wserial list
./host/wserial run -d /tmp/ttyWS0
picocom -b 115200 /tmp/ttyWS0
```

### 4.1 串口组权限（打开真实 USB 串口时）

PTY 不需要；如果你还要用 `picocom`/`minicom` 打开 USB 转串口设备，需加入
串口组（不同发行版组名不同）：

```bash
# Debian/Ubuntu
sudo usermod -aG dialout "$USER"

# Arch / Fedora 常用 uucp
sudo usermod -aG uucp "$USER"

# 重新登录（或 newgrp dialout）后生效
groups
```

排查权限问题：

```bash
ls -l /dev/ttyUSB0          # 属主/组是否为 dialout/uucp
sudo lsof /dev/ttyUSB0      # 是否被其它程序占用
```

### 4.2 防火墙（如启用 ufw/firewalld）

mDNS 需要 UDP 5353；wserial 主动连接设备的 TCP 3333/3334，出站通常默认
放行，无需额外配置。跨网段 mDNS 不可用时，用 `--host <IP>` 直连。

## 5. Windows 概览

Windows 没有像 Unix PTY 那样「创建任意虚拟串口节点」的通用机制，因此
`wserial` 采用 **com0com** 方案：

```
用户程序 ◄──► COM11  ══(com0com 虚拟端口对)══  COM12  ◄──► wserial ◄──► WiFi ──► ESP32-C3
```

* com0com 创建一对背靠背的虚拟串口，例如 COM11 ↔ COM12。
* **wserial 打开其中一侧（如 COM12）**，并把它桥接到 TCP。
* **用户程序打开另一侧（如 COM11）**，即可像普通串口一样收发。

> 端口号以实际创建结果为准：若 COM11/COM12 已被占用，可改用 COM20/COM21
> 等。下文命令中的 COM11/COM12 按需替换。

## 6. Windows：安装 com0com 并创建端口对

### 6.1 一键脚本（推荐）

以**管理员身份**打开 PowerShell（开始菜单搜索 PowerShell → 右键 →
以管理员身份运行）：

```powershell
cd C:\path\to\wireless-serial
powershell -ExecutionPolicy Bypass -File .\scripts\install-com0com.ps1
```

脚本会：检查管理员权限 → 下载 com0com 3.0.0.0 已签名包 → 静默安装 →
用 `setupc` 创建 `COM11 <-> COM12` → 打印后续步骤。

常用参数：

```powershell
# 自定义端口号
powershell -ExecutionPolicy Bypass -File .\scripts\install-com0com.ps1 -PortA COM20 -PortB COM21

# 已安装 com0com，只创建端口对
powershell -ExecutionPolicy Bypass -File .\scripts\install-com0com.ps1 -SkipInstall

# 只查看现有端口对
powershell -ExecutionPolicy Bypass -File .\scripts\install-com0com.ps1 -ListOnly
```

### 6.2 手动安装与 setupc 命令

1. 从 <https://sourceforge.net/projects/com0com/files/com0com/> 下载
   **3.0.0.0 的 fre-signed** 包（x64 对应 `com0com-3.0.0.0-x64-fre-signed.zip`）。
2. 解压后以管理员身份运行 `setup.exe`，按默认选项完成安装。
3. 以管理员身份打开 PowerShell，执行：

```powershell
# 查看已安装的端口对
& "${env:ProgramFiles(x86)}\com0com\setupc.exe" list

# 创建一对虚拟串口 COM11 <-> COM12
& "${env:ProgramFiles(x86)}\com0com\setupc.exe" install PortName=COM11 PortName=COM12
```

> 若安装到了 `C:\Program Files\com0com`，把 `${env:ProgramFiles(x86)}` 换成
> `${env:ProgramFiles}`。

4. 打开「设备管理器 → 端口 (COM 和 LPT)」，应能看到
   `com0com - serial port emulator (COM11)` 与 `(COM12)`。

### 6.3 驱动签名问题

Windows 10/11 对内核驱动强制签名：

* **优先使用 3.0.0.0 的 signed 包**（脚本默认下载它）。
* 旧版 2.x 未签名驱动可能报「Windows 无法验证此设备所需的驱动程序的数字
  签名」，设备管理器出现黄色感叹号，COM 口不可用。
* 若已装 signed 版仍失败：
  1. 卸载旧版 com0com 并重启；
  2. 重新以管理员安装 signed 包；
  3. 仍不行时，可临时禁用驱动签名强制（仅测试机）：
     设置 → 系统 → 恢复 → 高级启动 → 立即重新启动 → 疑难解答 → 高级选项 →
     启动设置 → 重启 → 按 `7` 禁用驱动程序强制签名。该设置只对本次启动有效。
* 不建议使用 `bcdedit /set testsigning on` 长期关闭签名，除非你清楚风险。

### 6.4 Windows 防火墙

* wserial 主动连接设备（出站），Windows 默认放行。
* `wserial list` 的 mDNS 使用 UDP 5353（组播），首次运行可能弹出防火墙
  提示，选择「允许访问」；若仍发现不到设备，用 `--host <IP>` 直连。
* 若设备与电脑不在同一网段/同一 AP（例如设备在 SoftAP 模式），先连接到
  设备热点 `wserial-XXXXXX`（密码默认 `wserial123`）。

### 6.5 运行

```powershell
# 发现设备
.\wserial.exe list

# 桥接：wserial 打开 COM12，用户程序打开 COM11
.\wserial.exe run --host 192.168.1.50 --device COM12 -b 115200 -v

# 之后在 Arduino IDE / PuTTY / 串口助手中打开 COM11，参数 115200 8N1
```

注意：

* `--device` 在 Windows 上**必须显式指定**（默认 `/tmp/ttyWS0` 是 Unix 路径）。
* 两侧端口不要都用同一个；用户程序与 wserial 各占一侧。
* 关闭 wserial 后 COM12 释放，端口对本身保留，下次可直接复用。
* Arduino IDE 的串口监视器会独占 COM11；调试时先关闭再让其它程序打开。

### 6.6 卸载/删除端口对

```powershell
# 查看端口对编号
& "${env:ProgramFiles(x86)}\com0com\setupc.exe" list

# 删除指定端口对（编号形如 CNCA0/CNCB0，以实际输出为准）
& "${env:ProgramFiles(x86)}\com0com\setupc.exe" remove 0

# 完全卸载：设置 → 应用 → com0com → 卸载
```

## 7. 首次运行验证

### 7.1 无硬件（模拟设备）

```bash
make sim
# 等价于: bash scripts/run-sim.sh
```

模拟器在本地提供 WSP/RAW/硬件侧端口，`wserial run` 会创建
`/tmp/ttyWS0`；另开终端用 `picocom -b 115200 /tmp/ttyWS0` 即可收发。

### 7.2 有硬件

```bash
./host/wserial list                 # 应列出 wserial-XXXXXX
./host/wserial run -d /tmp/ttyWS0 -v
# 另开终端
picocom -b 115200 /tmp/ttyWS0
```

在串口里输入字符，目标硬件应能收到；目标硬件的输出应出现在串口终端。

## 8. 卸载

```bash
# macOS / Linux
rm -f ~/.local/bin/wserial /usr/local/bin/wserial
rm -f /tmp/ttyWS0
rm -rf build host/dist host/wserial

# Windows
# 1. 删除 wserial.exe（PATH 中对应目录）
# 2. 设置 → 应用 → com0com → 卸载
# 3. 删除 C:\path\to\wireless-serial\host\dist（如使用预编译产物）
```

## 9. 路线图

* 原生 Windows 虚拟串口驱动（免 com0com）——**尚未实现**；
* 一键安装包（Homebrew / Winget / .deb）——**尚未实现**；
* Linux udev 规则与桌面快捷方式——**尚未实现**。

## 10. 相关文档

* [host-usage.md](host-usage.md) — 子命令与选项、典型场景
* [firmware-build.md](firmware-build.md) — 固件安装、构建、烧录
* [troubleshooting.md](troubleshooting.md) — 端口占用、权限、驱动问题
* [README.md](../README.md) — 5 分钟快速开始
