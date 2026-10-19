# 固件构建与烧录（ESP32-C3）

本文说明如何安装 ESP-IDF v5.4.4、编译、配置与烧录 wireless-serial 固件。
主机工具见 [installation.md](installation.md)。

## 1. 前置条件

| 项目 | 要求 |
|------|------|
| 芯片 | ESP32-C3（4 MB Flash 开发板即可） |
| ESP-IDF | **v5.4.4**（工作区已内置 `.idf/esp-idf`） |
| Python | 3.9+，且需与 `.idf/tools/python_env/` 中已安装的环境版本一致 |
| 工具链 | `riscv32-esp-elf-gcc`（ESP32-C3 是 RISC-V）、cmake、ninja（install.sh 自动安装） |
| 磁盘 | 约 2 GB（工具链在 `.idf/tools`） |

检查：

```bash
cd /path/to/wireless-serial
git -C .idf/esp-idf describe --tags --always   # 期望 v5.4.4
ls .idf/tools 2>/dev/null | head              # python_env、tools、dist ...
```

## 2. 安装 ESP-IDF v5.4.4（工作区本地）

> 仓库已约定把 ESP-IDF 与工具链装在工作区 `.idf/` 下（`.gitignore` 已忽略），
> 不污染 `~/.espressif`。若你已有 ESP-IDF v5.4.4，可跳过本节并设置
> `IDF_PATH`。

```bash
cd /path/to/wireless-serial
mkdir -p .idf
cd .idf

# 1) 克隆指定版本（含子模块）
git clone -b v5.4.4 --recursive https://github.com/espressif/esp-idf.git esp-idf

# 2) 安装 esp32c3 工具链到 .idf/tools
cd esp-idf
IDF_TOOLS_PATH=/path/to/wireless-serial/.idf/tools ./install.sh esp32c3
```

安装完成后验证：

```bash
cd /path/to/wireless-serial
export IDF_TOOLS_PATH="$PWD/.idf/tools"
. .idf/esp-idf/export.sh
idf.py --version
```

`export.sh` 会输出类似 `ESP-IDF v5.4.4` 的信息。**每个新终端都需要重新
source**（构建脚本会自动处理）。

> **Python 版本必须匹配**：`install.sh` 会按当时的 `python3` 版本创建
> `.idf/tools/python_env/idf5.4_py3.XX_env`。之后如果 `python3` 升级/换源
> （例如 Homebrew 从 3.9 升到 3.14），`export.sh` 会报
> `ESP-IDF Python virtual environment ... not found`。两种处理方式：
>
> ```bash
> # 方式 1：让 PATH 指向与已安装环境匹配的 python3（示例为 macOS 系统 python 3.9）
> export PATH="/usr/bin:$PATH"
>
> # 方式 2：重新安装工具链，为新 python3 创建环境
> cd .idf/esp-idf && IDF_TOOLS_PATH="$PWD/../tools" ./install.sh esp32c3
> ```
>
> `scripts/build-firmware.sh` / `scripts/flash.sh` 在检测到该错误时会自动
> 查找 `python_env/` 下已有的环境，并设置 `IDF_PYTHON_ENV_PATH` 重试；
> 也可以自己 `export IDF_PYTHON_ENV_PATH=.idf/tools/python_env/idf5.4_py3.9_env`。
>
> **组件管理器缓存与代理**：首次构建会通过 ESP-IDF Component Manager 下载
> `espressif/mdns`。若 `$HOME` 不可写（容器/沙箱），或系统代理不可用，可能
> 报缓存目录或 `Cannot establish a connection to the component registry`：
>
> ```bash
> # HOME 不可写：把组件缓存放到工作区
> export IDF_COMPONENT_CACHE_PATH="$PWD/build/idf_component_manager"
> mkdir -p "$IDF_COMPONENT_CACHE_PATH"
>
> # 代理配置错误/不需要代理：禁用代理（仅影响当前终端）
> export NO_PROXY="*" no_proxy="*"
> ```
>
> 组件下载成功后缓存在 `firmware/managed_components/`，版本锁定在
> `firmware/dependencies.lock`；同一工作区的后续构建无需再联网。

## 3. 配置（menuconfig，可选）

固件有合理的 Kconfig 默认值，通常**不需要**修改。需要改 WiFi、UART 引脚
或端口时：

```bash
cd /path/to/wireless-serial
export IDF_TOOLS_PATH="$PWD/.idf/tools"
. .idf/esp-idf/export.sh
cd firmware
idf.py menuconfig
```

选项位于 menuconfig 顶层的 **`Wireless Serial`** 菜单（`CONFIG_WSP_*`）。
主要选项与默认值：

| 选项（完整名 = `CONFIG_WSP_` + 下表名） | 默认值 | 说明 |
|-----------------------------|--------|------|
| `FW_VERSION_MAJOR` / `MINOR` / `PATCH` | 1 / 0 / 0 | HELLO_ACK / INFO 上报的固件版本 |
| `DEVICE_NAME` | `wserial` | 设备名（≤15 字节），用于 WSP 信息、mDNS 与网页 |
| `WIFI_SSID` / `WIFI_PASSWORD` | 空 | 首次启动要加入的 STA；为空则直接进入 SoftAP 配网 |
| `AP_PASSWORD` | `wserial123` | SoftAP 密码（<8 字符会变成开放网络） |
| `AP_CHANNEL` | 1 | SoftAP 信道 |
| `AP_MAX_CLIENTS` | 4 | SoftAP 最大客户端数 |
| `WSP_PORT` | 3333 | WSP 监听端口 |
| `RAW_PORT` | 3334 | RAW 监听端口 |
| `HTTP_PORT` | 80 | HTTP 监听端口 |
| `UART_TX_GPIO` / `UART_RX_GPIO` | 4 / 5 | UART1 引脚 |
| `UART_RTS_GPIO` / `UART_CTS_GPIO` | -1 / -1 | 硬件流控引脚，-1 表示未接 |
| `DEFAULT_BAUD` | 115200 | 默认波特率（50..5000000） |
| `DEFAULT_DATA_BITS` / `DEFAULT_PARITY` / `DEFAULT_STOP_BITS` | 8 / 0 / 1 | 默认 8N1（parity：0=无、1=奇、2=偶） |
| `RESET_GPIO` | 3 | 目标 EN/RESET，低有效，由 RTS 驱动；-1 禁用 |
| `BOOT_GPIO` | 10 | 目标 BOOT/IO0，低有效，由 DTR 驱动；-1 禁用 |
| `STA_CONNECT_TIMEOUT_MS` | 15000 | 每次 STA 连接超时 |
| `STA_MAX_RETRIES` | 3 | STA 重试次数，失败后回退 SoftAP |
| `HELLO_TIMEOUT_MS` | 5000 | WSP 握手超时 |
| `IDLE_TIMEOUT_S` | 120 | WSP 会话空闲超时（客户端建议每 15s 发 PING） |
| `RX_BUFFER_SIZE` / `TX_BUFFER_SIZE` | 8192 / 8192 | UART 收发缓冲；RX 满时丢新字节并计入 `uart_overflows` |
| `STATUS_LED` | 关闭（n） | 可选状态灯 |
| `STATUS_LED_GPIO` / `STATUS_LED_ACTIVE_LOW` | 8 / 是 | 仅在启用状态灯时生效 |

> 完整定义与帮助文本见 `firmware/main/Kconfig.projbuild`；在 menuconfig 中
> 按 `/` 可搜索选项名。修改后保存会更新 `firmware/sdkconfig`（已被
> `.gitignore` 忽略）。`firmware/sdkconfig.defaults` 还固化了 Flash 大小
> （4 MB）、单应用大分区、`CONFIG_FREERTOS_HZ=1000`、WebSocket 支持、
> 任务看门狗（只告警不重启）等基础配置。

配网也可以不重新编译：设备启动后进入 SoftAP，用浏览器打开
`http://192.168.4.1` 填写 WiFi，配置写入 NVS 并持久化。

## 4. 构建

### 4.1 一键脚本（推荐）

```bash
cd /path/to/wireless-serial
make fw
# 等价于: bash scripts/build-firmware.sh
```

脚本自动完成：

1. 检查 `.idf/esp-idf/export.sh` 是否存在；
2. 设置 `IDF_TOOLS_PATH=.idf/tools` 并 source `export.sh`；
3. 首次构建自动 `idf.py set-target esp32c3`；
4. `idf.py build`；
5. 打印 app / bootloader / partition-table 路径与烧录命令。

可选参数：

```bash
bash scripts/build-firmware.sh --clean          # 先 fullclean 再构建
bash scripts/build-firmware.sh --no-set-target  # 跳过 set-target
```

### 4.2 手动构建

```bash
cd /path/to/wireless-serial
export IDF_TOOLS_PATH="$PWD/.idf/tools"
. .idf/esp-idf/export.sh

cd firmware
idf.py set-target esp32c3   # 仅首次，或切换芯片时
idf.py build
```

产物在 `firmware/build/`：

```
firmware/build/wireless_serial.bin              # 应用
firmware/build/bootloader/bootloader.bin        # 二级引导
firmware/build/partition_table/partition-table.bin
firmware/build/flash_args                       # 合并烧录参数
```

## 5. 烧录

### 5.1 通过 Makefile（参数化端口）

```bash
make flash PORT=/dev/cu.usbmodem1101            # macOS（ESP32-C3 内置 USB）
make flash PORT=/dev/ttyUSB0                    # Linux（CH340/CP210x）
make flash PORT=/dev/cu.usbmodem1101 MONITOR=1  # 烧录后打开监视器
make flash PORT=/dev/cu.usbmodem1101 BAUD=115200
```

### 5.2 通过脚本

```bash
bash scripts/flash.sh -p /dev/cu.usbmodem1101 -b 460800
bash scripts/flash.sh -p /dev/cu.usbmodem1101 -m     # 烧录后 monitor
```

### 5.3 通过 idf.py / esptool.py

```bash
cd firmware
idf.py -p /dev/cu.usbmodem1101 -b 460800 flash monitor
```

等价的原生 esptool 命令（ESP32-C3 默认 4 MB Flash）：

```bash
esptool.py --chip esp32c3 -p /dev/cu.usbmodem1101 -b 460800 \
  --before default_reset --after hard_reset write_flash \
  --flash_mode dio --flash_freq 80m --flash_size detect \
  0x0     build/bootloader/bootloader.bin \
  0x8000  build/partition_table/partition-table.bin \
  0x10000 build/wireless_serial.bin
```

> 分区偏移以 `firmware/partitions*.csv` 与 `build/flash_args` 为准；上表为
> ESP-IDF v5.4.4 默认布局。若你改过分区表，请用 `idf.py flash` 自动处理。

### 5.4 进入下载模式

多数 ESP32-C3 开发板可用 USB 直接烧录（内置 USB-JTAG/Serial，无需手动
按 BOOT）。若提示 `Failed to connect`：

1. 按住 **BOOT**（GPIO9）不放；
2. 按一下 **RST/EN**，松开；
3. 松开 BOOT，再执行烧录；
4. 烧录完成后按 RST 正常运行。

macOS 串口名形如 `/dev/cu.usbmodem1101`；Linux 为 `/dev/ttyACM0` 或
`/dev/ttyUSB0`。用 `ls /dev/cu.usb*`（macOS）、`ls /dev/ttyUSB* /dev/ttyACM*`
（Linux）确认。

## 6. 首次启动验证

```bash
cd firmware
idf.py -p /dev/cu.usbmodem1101 monitor
# 退出: Ctrl+]
```

正常日志会包含（前缀为 ESP-IDF 的 `I (时间) 标签:`）：

```
app_main: Wireless Serial v1.0.0 / ESP32-C3 (WSP/1)
app_main: device "wserial", UART1 115200 8N1 flow=0
wifi_mgr: connected to "MyWiFi" (rssi -52), IP 192.168.1.50
# 若 WiFi 失败/未配置，则改为：
# wifi_mgr: SoftAP "wserial-AB12CD" up at http://192.168.4.1/ (WPA2)
app_main: network: mode=sta ssid="MyWiFi" ip=192.168.1.50 rssi=-52
mdns_srv: wserial-AB12CD.local -> wserial._wserial._tcp port 3333
app_main: ready: name="wserial" WSP=3333 RAW=3334 HTTP=80 mDNS=_wserial._tcp
```

之后的健康日志每约 30 秒打印一次（heap、模式、IP、当前客户端、收发与
错误计数）：

```
app_main: heap=...B mode=sta ip=192.168.1.50 client=none uart_rx=0 uart_tx=0 overflow=0 crc_err=0
```

SoftAP 的 SSID 后缀与 mDNS 主机名后缀一致（都来自 WiFi STA MAC 的后
3 字节），默认密码 `wserial123`（Kconfig 可改；少于 8 字符会变成开放网络）。

验证步骤：

1. 让电脑与设备在同一 WiFi（或连接设备热点 `wserial-XXXXXX`）；
2. 浏览器打开 `http://<设备IP>/`（SoftAP 下为 `http://192.168.4.1`），
   应看到配置页与串口终端；
3. 主机端执行 `./host/wserial list` 或 `./host/wserial run`。

## 7. 清理与重建

```bash
make clean                 # 删除 firmware/build、sdkconfig、host 产物
bash scripts/build-firmware.sh --clean
```

`.idf/` 工具链不会被 `make clean` 删除。

## 8. 常见构建错误

| 现象 | 原因 | 处理 |
|------|------|------|
| `source export.sh` 报 Python/工具缺失 | 工具链未安装或 `IDF_TOOLS_PATH` 不对 | 重新执行 `./install.sh esp32c3`，确认 `.idf/tools` |
| `ERROR: ESP-IDF Python virtual environment ...py3.XX_env not found` | 当前 `python3` 版本与已安装的 IDF Python 环境不一致 | 见第 2 节「Python 版本必须匹配」：调整 PATH 或重跑 install.sh |
| `CMake Error: IDF_PATH ... not found` | 未 source export.sh | 用 `make fw`/脚本，或手动 source |
| `Target ... not supported` | sdkconfig 目标不是 esp32c3 | `idf.py set-target esp32c3`，或 `bash scripts/build-firmware.sh --clean` 重建 |
| `Failed to connect to ESP32-C3` | 未进下载模式 / 串口不对 / 被占用 | 按 5.4 节；`lsof` 检查占用；换数据线 |
| 组件管理器缓存/权限报错 | 网关/容器 `$HOME` 不可写 | `export IDF_COMPONENT_CACHE_PATH="$PWD/build/idf_component_manager"` |
| `Cannot establish a connection to the component registry` | 系统代理不可用 | `export NO_PROXY="*" no_proxy="*"`，或修正代理设置 |
| 编译内存不足 | 并行度过高 | `idf.py build -j2` 或 `IDF_EXTRA_ARGS="-j2" make fw` |
| 固件启动后反复重启 | 供电不足 / 看门狗 | 用独立 5V 供电，检查日志中的 reset reason |

## 9. OTA 升级（路线图，尚未实现）

当前固件**不支持** OTA，只能通过 USB 烧录。路线图计划：

* 双分区 OTA + HTTP 上传固件；
* 版本校验与回滚；
* 主机工具 `wserial ota` 子命令。

在实现之前，请勿在设备部署后修改分区表或依赖远程升级。

## 10. 固件源码结构

```
firmware/
├── CMakeLists.txt                 # project(wireless_serial)
├── sdkconfig.defaults             # 默认配置（构建时合并到 sdkconfig）
├── dependencies.lock              # 托管组件版本锁定（mdns 1.14.0）
└── main/
    ├── CMakeLists.txt             # 源文件、include、内嵌网页
    ├── idf_component.yml          # 托管组件 espressif/mdns
    ├── Kconfig.projbuild          # “Wireless Serial” 菜单（CONFIG_WSP_*）
    ├── app_main.c                 # 初始化、启动流程与健康监控
    ├── wifi_mgr.c                 # STA / SoftAP 回退
    ├── uart_bridge.c              # UART1 桥接与运行时配置
    ├── tcp_server.c               # WSP 3333 / RAW 3334 服务
    ├── http_server.c              # HTTP 80、/api/*、WebSocket /ws
    ├── config_store.c             # NVS 持久化
    ├── mdns_srv.c                 # mDNS 广播与 TXT 更新
    ├── status_led.c               # 状态灯（可选）
    ├── www/index.html             # 内嵌配置页与网页串口终端
    ├── include/                   # 组件头文件（wifi_mgr.h、uart_bridge.h、
    │                              #   tcp_server.h、http_server.h、config_store.h、
    │                              #   mdns_srv.h、status_led.h、wsp_proto.h）
    └── wsp_proto.c                # WSP/1 帧编解码（冻结，可移植 C99）
```

## 11. 相关文档

* [protocol.md](protocol.md) — 协议与 HTTP API
* [wiring.md](wiring.md) — 与目标硬件接线
* [installation.md](installation.md) — 主机工具安装
* [troubleshooting.md](troubleshooting.md) — 烧录/启动/联网排错
* [faq.md](faq.md) — 芯片选型与常见问题
