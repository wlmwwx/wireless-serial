#!/usr/bin/env bash
#
# test/run.sh — one-command verification of the wireless-serial test
# infrastructure (no ESP32 hardware required):
#
#   1. C protocol conformance tests   (firmware/main/wsp_proto.c + test/proto_test.c)
#   2. Go device simulator build/vet  (test/sim)
#   3. Go device simulator unit tests (test/sim, includes the independent
#      WSP/1 codec tests in test/sim/wire)
#   4. Simulator binary smoke test    (spawn real binary, random ports,
#      connect through the addrfile contract)
#
# All TCP ports are ephemeral (127.0.0.1:0) so parallel runs never collide.
# The simulator process is always killed and waited for on exit.
#
# Usage:
#   test/run.sh
#   CC=clang test/run.sh
#
# Exit code: 0 when everything passed, non-zero otherwise.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BUILD_DIR="$SCRIPT_DIR/build"
LOG_DIR="$BUILD_DIR/logs"
mkdir -p "$BUILD_DIR" "$LOG_DIR"

CC="${CC:-cc}"
CFLAGS=(-std=c99 -Wall -Wextra -Werror -O1)

# Hermetic Go build cache inside the workspace (test/build is gitignored).
# Set WSP_TEST_GOCACHE to share a cache, e.g. WSP_TEST_GOCACHE="$HOME/.cache/go-build".
export GOCACHE="${WSP_TEST_GOCACHE:-$SCRIPT_DIR/.cache/gocache}"

SIM_PID=""
ADDRFILE="$BUILD_DIR/sim-addr.json"

cleanup() {
    local rc=$?
    if [[ -n "$SIM_PID" ]] && kill -0 "$SIM_PID" 2>/dev/null; then
        kill "$SIM_PID" 2>/dev/null || true
        wait "$SIM_PID" 2>/dev/null || true
    fi
    SIM_PID=""
    exit "$rc"
}
trap cleanup EXIT INT TERM

step() {
    printf '\n==> %s\n' "$*"
}

# ---------------------------------------------------------------------------
step "[1/4] C protocol conformance tests (firmware/main/wsp_proto.c)"
"$CC" "${CFLAGS[@]}" -I"$ROOT_DIR/firmware/main/include" \
    "$SCRIPT_DIR/proto_test.c" \
    "$ROOT_DIR/firmware/main/wsp_proto.c" \
    -o "$BUILD_DIR/proto_test"
"$BUILD_DIR/proto_test"

# ---------------------------------------------------------------------------
step "[2/4] Go simulator: build + vet"
(
    cd "$SCRIPT_DIR/sim"
    go build ./...
    go vet ./...
)

# ---------------------------------------------------------------------------
step "[3/4] Go simulator: unit tests"
(
    cd "$SCRIPT_DIR/sim"
    go test ./... -count=1
)

# ---------------------------------------------------------------------------
step "[4/4] simulator binary smoke test (random ports)"
(
    cd "$SCRIPT_DIR/sim"
    go build -o "$BUILD_DIR/sim" .
    go build -o "$BUILD_DIR/sim-smoke" ./smoke
)

rm -f "$ADDRFILE"
"$BUILD_DIR/sim" \
    -wsp 127.0.0.1:0 \
    -raw 127.0.0.1:0 \
    -hw 127.0.0.1:0 \
    -admin 127.0.0.1:0 \
    -addrfile "$ADDRFILE" \
    >"$LOG_DIR/sim.out" 2>"$LOG_DIR/sim.err" &
SIM_PID=$!

ready=0
for _ in $(seq 1 200); do
    if [[ -s "$ADDRFILE" ]]; then
        ready=1
        break
    fi
    if ! kill -0 "$SIM_PID" 2>/dev/null; then
        echo "ERROR: simulator exited before becoming ready" >&2
        cat "$LOG_DIR/sim.err" >&2 || true
        exit 1
    fi
    sleep 0.05
done
if [[ "$ready" -ne 1 ]]; then
    echo "ERROR: simulator did not write $ADDRFILE within 10s" >&2
    cat "$LOG_DIR/sim.out" >&2 || true
    cat "$LOG_DIR/sim.err" >&2 || true
    exit 1
fi

"$BUILD_DIR/sim-smoke" -addrfile "$ADDRFILE"

kill "$SIM_PID" 2>/dev/null || true
wait "$SIM_PID" 2>/dev/null || true
SIM_PID=""

printf '\n============================================\n'
printf 'ALL TESTS PASSED (C conformance + simulator unit tests + binary smoke)\n'
