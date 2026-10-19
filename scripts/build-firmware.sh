#!/usr/bin/env bash
#
# build-firmware.sh — 构建 ESP32-C3 固件（ESP-IDF v5.4.4）
#
# 特性：
#   * 自动 source 工作区内的 .idf/esp-idf/export.sh
#   * 自动设置 IDF_TOOLS_PATH=.idf/tools（工具链装在工作区内，不污染 ~/.espressif）
#   * 首次构建自动执行 idf.py set-target esp32c3
#   * 构建完成后打印 app / bootloader / partition-table 产物路径与烧录命令
#
# 用法：
#   scripts/build-firmware.sh [--clean] [--no-set-target] [-h]
#
# 环境变量：
#   IDF_TARGET        默认 esp32c3
#   IDF_PATH          可覆盖 .idf/esp-idf
#   IDF_TOOLS_PATH    可覆盖 .idf/tools
#   IDF_EXTRA_ARGS    追加给 idf.py build 的参数
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IDF_DIR="${IDF_PATH:-$ROOT/.idf/esp-idf}"
export IDF_TOOLS_PATH="${IDF_TOOLS_PATH:-$ROOT/.idf/tools}"
FW_DIR="$ROOT/firmware"
TARGET="${IDF_TARGET:-esp32c3}"
DO_CLEAN=0
DO_SET_TARGET=1

usage() {
    # 打印文件头注释（去掉开头的 #）作为帮助信息
    awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --clean)          DO_CLEAN=1 ;;
        --no-set-target)  DO_SET_TARGET=0 ;;
        -h|--help)        usage; exit 0 ;;
        *) echo "未知参数: $1（用 -h 查看用法）" >&2; exit 2 ;;
    esac
    shift
done

msg()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[警告]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[错误]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- 1. 检查 ESP-IDF ----------
if [ ! -f "$IDF_DIR/export.sh" ]; then
    die "未找到 ESP-IDF：$IDF_DIR/export.sh
请按 docs/firmware-build.md 安装 ESP-IDF v5.4.4 到 .idf/esp-idf，或设置 IDF_PATH 指向已有安装。"
fi
if [ ! -f "$FW_DIR/CMakeLists.txt" ]; then
    die "未找到 $FW_DIR/CMakeLists.txt；固件源码可能尚未就绪。"
fi

IDF_VER=""
if [ -f "$IDF_DIR/version.txt" ]; then
    IDF_VER="$(tr -d '[:space:]' < "$IDF_DIR/version.txt")"
elif command -v git >/dev/null 2>&1 && [ -d "$IDF_DIR/.git" ]; then
    IDF_VER="$(git -C "$IDF_DIR" describe --tags --always 2>/dev/null || true)"
fi
IDF_VER="${IDF_VER:-未知}"
msg "ESP-IDF ${IDF_VER}  ($IDF_DIR)"
msg "IDF_TOOLS_PATH=$IDF_TOOLS_PATH"

# 组件管理器缓存：家目录不可写时（容器/沙箱/只读 HOME）落到工作区。
if [ -z "${IDF_COMPONENT_CACHE_PATH:-}" ] && [ ! -w "${HOME:-/nonexistent}" ]; then
    export IDF_COMPONENT_CACHE_PATH="$ROOT/build/idf_component_manager"
    mkdir -p "$IDF_COMPONENT_CACHE_PATH"
    warn "HOME 不可写，组件缓存改用 ${IDF_COMPONENT_CACHE_PATH}"
fi

# ---------- 2. 加载工具链 ----------
# export.sh 既可能以非 0 退出，也可能只打印 ERROR 后返回 0，两种情况都要识别。
# 若当前 python3 与已安装的 IDF Python 环境版本不匹配，则自动改用
# IDF_TOOLS_PATH/python_env/ 下现有的环境重试（ESP-IDF 官方支持的变量）。
EXPORT_LOG="$ROOT/build/.idf-export.log"
mkdir -p "$ROOT/build"

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
    echo "--------------------------" >&2
    rm -f "$EXPORT_LOG"
    die "加载 ESP-IDF 工具链失败。
常见原因：工具链未安装，或当前 python3 版本与 .idf/tools 中的虚拟环境不匹配。
处理：在 .idf/esp-idf 下执行
    IDF_TOOLS_PATH=$IDF_TOOLS_PATH ./install.sh esp32c3
重新安装工具链（见 docs/firmware-build.md 第 2 节）。"
fi
rm -f "$EXPORT_LOG"

# ---------- 3. 目标芯片 ----------
cd "$FW_DIR"
if [ "$DO_CLEAN" = "1" ]; then
    msg "idf.py fullclean"
    idf.py fullclean
fi

if [ "$DO_SET_TARGET" = "1" ]; then
    if [ ! -f sdkconfig ] || ! grep -q "^CONFIG_IDF_TARGET=\"$TARGET\"" sdkconfig 2>/dev/null; then
        msg "idf.py set-target $TARGET"
        idf.py set-target "$TARGET"
    else
        msg "目标芯片已是 ${TARGET}，跳过 set-target"
    fi
fi

# ---------- 4. 构建 ----------
msg "idf.py build"
# shellcheck disable=SC2086
idf.py build ${IDF_EXTRA_ARGS:-}

# ---------- 5. 产物 ----------
PROJECT_NAME="$(sed -n 's/^[[:space:]]*project(\([^ )]*\).*/\1/p' "$FW_DIR/CMakeLists.txt" | head -n1)"
PROJECT_NAME="${PROJECT_NAME:-wireless_serial}"
APP_BIN="$FW_DIR/build/${PROJECT_NAME}.bin"

echo
msg "构建成功"
if [ -f "$APP_BIN" ]; then
    printf '  app             : %s\n' "$APP_BIN"
else
    warn "未找到预期产物 ${APP_BIN}（请确认 project() 名称）"
    find "$FW_DIR/build" -maxdepth 2 -name '*.bin' 2>/dev/null | sed 's/^/  找到: /' || true
fi
[ -f "$FW_DIR/build/bootloader/bootloader.bin" ] && \
    printf '  bootloader      : %s\n' "$FW_DIR/build/bootloader/bootloader.bin"
[ -f "$FW_DIR/build/partition_table/partition-table.bin" ] && \
    printf '  partition-table : %s\n' "$FW_DIR/build/partition_table/partition-table.bin"
printf '\n烧录:\n  make flash PORT=/dev/cu.usbmodemXXXX MONITOR=1\n'
printf '  # 或: scripts/flash.sh -p /dev/cu.usbmodemXXXX -b 460800\n'
printf '查看日志:\n  idf.py -p /dev/cu.usbmodemXXXX monitor\n'
