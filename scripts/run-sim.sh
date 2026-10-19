#!/usr/bin/env bash
#
# run-sim.sh — 无需硬件的联调：启动模拟设备（test/sim）+ wserial run
#
# 模拟设备提供：
#   * WSP  :13333（默认）  —— wserial 连接
#   * RAW  :13334（默认）  —— 纯透传
#   * HW   :13335（默认）  —— “目标硬件”一侧的 TCP 端口，用 nc 可模拟串口对端
#
# 用法：
#   scripts/run-sim.sh
#   scripts/run-sim.sh --device /tmp/ttyWS1 --baud 9600
#   scripts/run-sim.sh --raw              # 走 RAW 端口 13334
#   scripts/run-sim.sh --no-wserial       # 只启动模拟设备，供手工联调
#
# 环境变量：
#   WSP_PORT（默认 13333）、RAW_PORT（13334）、HW_PORT（13335）
#   DEVICE（默认 /tmp/ttyWS0）、BAUD（默认 115200）
#   SIM_ARGS  追加给模拟设备的参数（如 --latency 5ms --drop 0.01）
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="$ROOT/build"
SIM_DIR="$ROOT/test/sim"
HOST_DIR="$ROOT/host"

WSP_PORT="${WSP_PORT:-13333}"
RAW_PORT="${RAW_PORT:-13334}"
HW_PORT="${HW_PORT:-13335}"
DEVICE="${DEVICE:-/tmp/ttyWS0}"
BAUD="${BAUD:-115200}"
RAW_MODE=0
START_WSCLI=1

usage() {
    # 打印文件头注释（去掉开头的 #）作为帮助信息
    awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --device)     DEVICE="${2:-}"; shift 2 ;;
        --baud)       BAUD="${2:-}"; shift 2 ;;
        --wsp-port)   WSP_PORT="${2:-}"; shift 2 ;;
        --raw-port)   RAW_PORT="${2:-}"; shift 2 ;;
        --hw-port)    HW_PORT="${2:-}"; shift 2 ;;
        --raw)        RAW_MODE=1; shift ;;
        --no-wserial) START_WSCLI=0; shift ;;
        -h|--help)    usage; exit 0 ;;
        *) echo "未知参数: $1（用 -h 查看用法）" >&2; exit 2 ;;
    esac
done

msg()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[错误]\033[0m %s\n' "$*" >&2; exit 1; }

mkdir -p "$BUILD_DIR"
command -v go >/dev/null 2>&1 || die "未找到 go 命令，请安装 Go（https://go.dev/dl/）"

# ---------- 1. 编译模拟设备 ----------
SIM_BIN="$BUILD_DIR/wserial-sim"
if [ -f "$SIM_DIR/go.mod" ]; then
    msg "编译模拟设备: test/sim"
    ( cd "$SIM_DIR" && go build -trimpath -buildvcs=false -o "$SIM_BIN" . )
elif [ -f "$ROOT/go.mod" ] && [ -d "$SIM_DIR" ]; then
    msg "编译模拟设备: ./test/sim"
    ( cd "$ROOT" && go build -trimpath -buildvcs=false -o "$SIM_BIN" ./test/sim )
elif [ -f "$ROOT/test/run.sh" ]; then
    die "已找到 test/run.sh，但 test/sim 尚未就绪（qa-dev 正在开发）；稍后重试或直接 bash test/run.sh。"
else
    die "未找到 test/sim；模拟设备源码尚未就绪。请先让 qa-dev 完成 task-3。"
fi

# ---------- 2. 编译主机工具 ----------
WSCLI="$BUILD_DIR/wserial"
if [ -f "$HOST_DIR/go.mod" ]; then
    msg "编译主机工具: host/cmd/wserial"
    ( cd "$HOST_DIR" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$WSCLI" ./cmd/wserial )
elif [ -x "$HOST_DIR/wserial" ]; then
    WSCLI="$HOST_DIR/wserial"
else
    die "未找到 host/go.mod，也没有已构建的 host/wserial；请先执行 make host。"
fi

# ---------- 3. 启动模拟设备 ----------
SIM_PID=""
cleanup() {
    local rc=$?
    if [ -n "$SIM_PID" ] && kill -0 "$SIM_PID" 2>/dev/null; then
        msg "停止模拟设备 (pid $SIM_PID)"
        kill "$SIM_PID" 2>/dev/null || true
        wait "$SIM_PID" 2>/dev/null || true
    fi
    exit "$rc"
}
trap cleanup EXIT INT TERM

msg "启动模拟设备: WSP=:$WSP_PORT RAW=:$RAW_PORT HW=:$HW_PORT"
# shellcheck disable=SC2086
"$SIM_BIN" -wsp ":$WSP_PORT" -raw ":$RAW_PORT" -hw ":$HW_PORT" -baud "$BAUD" ${SIM_ARGS:-} &
SIM_PID=$!

# 等待 WSP 端口就绪（最多 10 秒）
READY=0
for _ in $(seq 1 50); do
    if ! kill -0 "$SIM_PID" 2>/dev/null; then
        die "模拟设备启动失败（进程已退出）"
    fi
    if command -v nc >/dev/null 2>&1; then
        if nc -z 127.0.0.1 "$WSP_PORT" >/dev/null 2>&1; then READY=1; break; fi
    else
        sleep 0.2
        READY=1
        break
    fi
    sleep 0.2
done
[ "$READY" = "1" ] || die "等待 WSP 端口 :$WSP_PORT 超时"

echo
msg "模拟设备已就绪"
echo "  WSP 端口        : 127.0.0.1:$WSP_PORT   （wserial 连接此端口）"
echo "  RAW 端口        : 127.0.0.1:$RAW_PORT"
echo "  硬件侧 TCP 端口 : 127.0.0.1:$HW_PORT    （模拟“目标硬件”，可用下面命令收发）"
echo "    发送到硬件:  printf 'hello' | nc 127.0.0.1 $HW_PORT"
echo "    从硬件接收:  nc -l 127.0.0.1 $HW_PORT   # 需先停掉模拟设备的 hw 端口或改用其它工具"
echo

if [ "$START_WSCLI" = "0" ]; then
    msg "仅启动模拟设备（--no-wserial）。按 Ctrl+C 退出。"
    wait "$SIM_PID"
fi

# ---------- 4. 启动 wserial ----------
if [ "$RAW_MODE" = "1" ]; then
    PORT_ARG="$RAW_PORT"
    extra=(--raw)
    msg "wserial run --raw -> 127.0.0.1:${RAW_PORT}，虚拟串口 $DEVICE"
else
    PORT_ARG="$WSP_PORT"
    extra=()
    msg "wserial run -> 127.0.0.1:${WSP_PORT}，虚拟串口 ${DEVICE}，波特率 $BAUD"
fi

set +e
"$WSCLI" run --host 127.0.0.1 --port "$PORT_ARG" --device "$DEVICE" --baud "$BAUD" "${extra[@]}" -v
RC=$?
set -e
msg "wserial 退出（code=${RC}）"
