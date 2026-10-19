#!/usr/bin/env bash
#
# flash.sh — 烧录 ESP32-C3 固件（参数化端口 / 波特率）
#
# 用法：
#   scripts/flash.sh -p /dev/cu.usbmodem1101 [-b 460800] [-m]
#   scripts/flash.sh -p COM5 -b 921600 -m
#
# 参数：
#   -p, --port PORT     烧录串口（必填，或通过环境变量 PORT 提供）
#   -b, --baud BAUD     烧录波特率，默认 460800（不稳定时可降到 115200）
#   -m, --monitor       烧录完成后立即打开 idf.py monitor（Ctrl+] 退出）
#       --build-dir DIR 固件构建目录，默认 firmware/build
#   -h, --help          显示帮助
#
# 说明：
#   * 本脚本通过 idf.py flash 调用 esptool.py，端口与波特率原样透传；
#   * 会自动 source 工作区 .idf/esp-idf/export.sh 并设置 IDF_TOOLS_PATH=.idf/tools；
#   * ESP32-C3 使用内置 USB-JTAG/Serial 时，/dev/cu.usbmodem* 即可；
#     若开发板走 CH340/CP210x，则用 /dev/cu.wchusbserial* / /dev/cu.SLAB_USBtoUART*。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IDF_DIR="${IDF_PATH:-$ROOT/.idf/esp-idf}"
export IDF_TOOLS_PATH="${IDF_TOOLS_PATH:-$ROOT/.idf/tools}"
FW_DIR="$ROOT/firmware"

PORT="${PORT:-}"
BAUD="${BAUD:-460800}"
MONITOR=0
BUILD_DIR="$FW_DIR/build"

usage() {
    # 打印文件头注释（去掉开头的 #）作为帮助信息
    awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
}

while [ $# -gt 0 ]; do
    case "$1" in
        -p|--port)     PORT="${2:-}"; shift 2 ;;
        -b|--baud)     BAUD="${2:-}"; shift 2 ;;
        -m|--monitor)  MONITOR=1; shift ;;
        --build-dir)   BUILD_DIR="${2:-}"; shift 2 ;;
        -h|--help)     usage; exit 0 ;;
        *) echo "未知参数: $1（用 -h 查看用法）" >&2; exit 2 ;;
    esac
done

msg()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[警告]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[错误]\033[0m %s\n' "$*" >&2; exit 1; }

if [ -z "$PORT" ]; then
    echo "可用串口：" >&2
    ls -1 /dev/cu.usb* /dev/cu.wchusbserial* /dev/cu.SLAB_USBtoUART* \
          /dev/ttyUSB* /dev/ttyACM* 2>/dev/null | sed 's/^/  /' >&2 || true
    die "缺少串口参数。用法: scripts/flash.sh -p /dev/cu.usbmodem1101 -b 460800"
fi
if [ ! -e "$PORT" ]; then
    die "串口不存在: ${PORT}（macOS 用 ls /dev/cu.usb*；Linux 用 ls /dev/ttyUSB* /dev/ttyACM*）"
fi
if [ ! -d "$BUILD_DIR" ] || [ -z "$(find "$BUILD_DIR" -maxdepth 1 -name '*.bin' -print -quit 2>/dev/null)" ]; then
    die "未找到固件产物（${BUILD_DIR}）。请先执行: make fw"
fi
if [ ! -f "$IDF_DIR/export.sh" ]; then
    die "未找到 ESP-IDF：$IDF_DIR/export.sh（见 docs/firmware-build.md）"
fi

msg "加载 ESP-IDF 工具链"
mkdir -p "$ROOT/build"
EXPORT_LOG="$ROOT/build/.idf-export-flash.log"

try_export() {
    set +e
    . "$IDF_DIR/export.sh" >"$EXPORT_LOG" 2>&1
    local rc=$?
    set -e
    if [ "$rc" -eq 0 ] && command -v idf.py >/dev/null 2>&1; then
        return 0
    fi
    return 1
}

if ! try_export; then
    EXISTING_ENV=""
    if [ -z "${IDF_PYTHON_ENV_PATH:-}" ] && [ -d "$IDF_TOOLS_PATH/python_env" ]; then
        EXISTING_ENV="$(find "$IDF_TOOLS_PATH/python_env" -maxdepth 1 -mindepth 1 -type d -name 'idf*_env' 2>/dev/null | head -n1)"
    fi
    if [ -n "$EXISTING_ENV" ]; then
        warn "当前 python3 ($(python3 --version 2>/dev/null || echo 未知)) 与已安装环境不匹配，改用 $EXISTING_ENV"
        export IDF_PYTHON_ENV_PATH="$EXISTING_ENV"
        try_export || true
    fi
fi
if ! command -v idf.py >/dev/null 2>&1; then
    echo "----- export.sh 输出 -----" >&2
    tail -n 20 "$EXPORT_LOG" >&2 || true
    rm -f "$EXPORT_LOG"
    die "加载 ESP-IDF 工具链失败。请确认 .idf/tools 已安装，且 python3 版本与 python_env 匹配（见 docs/firmware-build.md 第 2 节）。"
fi
rm -f "$EXPORT_LOG"

msg "烧录: port=$PORT baud=$BAUD"
cd "$FW_DIR"
if [ "$MONITOR" = "1" ]; then
    exec idf.py -p "$PORT" -b "$BAUD" flash monitor
else
    exec idf.py -p "$PORT" -b "$BAUD" flash
fi
