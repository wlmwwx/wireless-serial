#!/usr/bin/env bash
#
# test/integration.sh — end-to-end integration tests (no real ESP32 needed).
#
#   user program (PTY or monitor pipes)
#        <-> wserial (host/)
#        <-> WSP/1 or RAW over TCP
#        <-> device simulator (test/sim)
#        <-> fake target hardware (TCP hardware-side port)
#
# The script builds wserial from host/ (task-2) and the simulator from
# test/sim/ into test/build/, then runs `go test -tags integration ./... -v`
# from test/. All TCP ports are chosen randomly by the simulator; PTYs are
# created under a per-test temporary directory.
#
# Usage:
#   test/integration.sh                 # run everything
#   test/integration.sh -run TestVirtualSerial -v
#   WSP_REQUIRE_PTY=1 test/integration.sh   # fail (not skip) if PTY is unavailable
#
# Full verification (including the PTY-based virtual serial port tests) must be
# run on a normal host terminal:
#
#   WSP_REQUIRE_PTY=1 test/integration.sh
#
# In restricted environments (e.g. a file sandbox that denies open("/dev/ptmx"))
# the virtual-serial tests are SKIPPED with an explicit message, while the
# monitor-mode tests still exercise the same WSP/RAW/UART_CFG/reconnect/BUSY
# paths through stdin/stdout pipes.
#
# Exit code: 0 when all tests pass (or are skipped by design), non-zero on
# failure or missing host build.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BUILD_DIR="$SCRIPT_DIR/build"
mkdir -p "$BUILD_DIR"

# Go caches: keep the user's normal configuration when it is writable, and
# fall back to workspace-local caches for sandboxed/CI environments (the
# project workspace is the only guaranteed write area there).
if [[ -n "${WSP_TEST_GOCACHE:-}" ]]; then
    export GOCACHE="$WSP_TEST_GOCACHE"
elif gocache_default="$(go env GOCACHE 2>/dev/null)" \
    && mkdir -p "$gocache_default" 2>/dev/null \
    && touch "$gocache_default/.wsp-write-test" 2>/dev/null; then
    rm -f "$gocache_default/.wsp-write-test"
    export GOCACHE="$gocache_default"
else
    mkdir -p "$SCRIPT_DIR/.cache/gocache"
    export GOCACHE="$SCRIPT_DIR/.cache/gocache"
fi

if [[ -n "${WSP_TEST_GOMODCACHE:-}" ]]; then
    export GOMODCACHE="$WSP_TEST_GOMODCACHE"
elif gomodcache_default="$(go env GOMODCACHE 2>/dev/null)" \
    && mkdir -p "$gomodcache_default" 2>/dev/null \
    && touch "$gomodcache_default/.wsp-write-test" 2>/dev/null; then
    rm -f "$gomodcache_default/.wsp-write-test"
    export GOMODCACHE="$gomodcache_default"
else
    mkdir -p "$SCRIPT_DIR/.cache/gomodcache"
    export GOMODCACHE="$SCRIPT_DIR/.cache/gomodcache"
fi

step() {
    printf '\n==> %s\n' "$*"
}

START_TS=$(date +%s)

step "[1/3] 构建 wserial（host/cmd/wserial -> test/build/wserial）"
(
    cd "$ROOT_DIR/host"
    go build -o "$BUILD_DIR/wserial" ./cmd/wserial
)
"$BUILD_DIR/wserial" version | head -1

step "[2/3] 构建设备模拟器（test/sim -> test/build/sim）"
(
    cd "$SCRIPT_DIR/sim"
    go build -o "$BUILD_DIR/sim" .
)

step "[3/3] 运行端到端集成测试（go test -tags integration -v）"
LOG_FILE="$SCRIPT_DIR/.cache/integration.log"
mkdir -p "$(dirname "$LOG_FILE")"
(
    cd "$SCRIPT_DIR"
    go test -tags integration ./... -v -count=1 -timeout 10m "$@" 2>&1 | tee "$LOG_FILE"
)

if grep -q -- "--- SKIP: TestVirtualSerial" "$LOG_FILE"; then
    printf '\nWARNING: 当前环境禁止创建 PTY（/dev/ptmx），虚拟串口测试被跳过；\n' >&2
    printf '         monitor 模式测试已覆盖 WSP/RAW/配置/重连/BUSY 链路。\n' >&2
    printf '         请在普通终端重新运行本脚本以执行完整 PTY 测试。\n' >&2
fi

END_TS=$(date +%s)
printf '\n============================================\n'
printf 'INTEGRATION TESTS PASSED (%ss)\n' "$((END_TS - START_TS))"
if grep -q -- "--- SKIP: TestVirtualSerial" "$LOG_FILE"; then
    printf 'NOTE: PTY 虚拟串口测试被跳过（沙箱/无 PTY 环境），见上方 WARNING。\n'
fi
