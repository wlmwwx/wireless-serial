#!/usr/bin/env bash
#
# build-host.sh — 交叉编译主机工具 wserial 到 host/dist/
#
# 用法：
#   scripts/build-host.sh                # 默认矩阵
#   scripts/build-host.sh darwin/arm64 linux/amd64   # 只构建指定平台
#   OUT=host/dist scripts/build-host.sh  # 自定义输出目录
#
# 默认矩阵（共 5 个产物）：
#   darwin/arm64   macOS Apple Silicon
#   darwin/amd64   macOS Intel
#   linux/amd64    Linux x86_64
#   linux/arm64    Linux ARM64（树莓派等）
#   windows/amd64  Windows x64（.exe）
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST_DIR="$ROOT/host"
OUT_DIR="${OUT:-$ROOT/host/dist}"
GO="${GO:-go}"
DEFAULT_PLATFORMS=(
    "darwin/arm64"
    "darwin/amd64"
    "linux/amd64"
    "linux/arm64"
    "windows/amd64"
)

msg()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[错误]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    # 打印文件头注释（去掉开头的 #）作为帮助信息
    awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
}

if [ "$#" -gt 0 ] && { [ "$1" = "-h" ] || [ "$1" = "--help" ]; }; then
    usage
    exit 0
fi

if [ ! -f "$HOST_DIR/go.mod" ]; then
    die "未找到 $HOST_DIR/go.mod；主机端源码可能尚未就绪。"
fi
command -v "$GO" >/dev/null 2>&1 || die "未找到 go 命令，请安装 Go（https://go.dev/dl/）"

PLATFORMS=("$@")
if [ ${#PLATFORMS[@]} -eq 0 ]; then
    PLATFORMS=("${DEFAULT_PLATFORMS[@]}")
fi

mkdir -p "$OUT_DIR"
msg "输出目录: $OUT_DIR"
msg "平台数量: ${#PLATFORMS[@]}"

cd "$HOST_DIR"
FAILED=0
PRODUCED=()
for PLATFORM in "${PLATFORMS[@]}"; do
    OS="${PLATFORM%%/*}"
    ARCH="${PLATFORM##*/}"
    if [ -z "$OS" ] || [ -z "$ARCH" ] || [ "$OS" = "$PLATFORM" ]; then
        echo "跳过非法平台描述: ${PLATFORM}（应形如 darwin/arm64）" >&2
        FAILED=1
        continue
    fi
    OUT_NAME="wserial-${OS}-${ARCH}"
    [ "$OS" = "windows" ] && OUT_NAME="${OUT_NAME}.exe"
    printf '  %-16s -> %s\n' "$PLATFORM" "$OUT_NAME"
    if CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" \
        "$GO" build -trimpath -buildvcs=false -ldflags '-s -w' \
        -o "$OUT_DIR/$OUT_NAME" ./cmd/wserial; then
        PRODUCED+=("$OUT_NAME")
    else
        echo "  [失败] $PLATFORM" >&2
        FAILED=1
    fi
done

if [ ${#PRODUCED[@]} -gt 0 ]; then
    (
        cd "$OUT_DIR"
        if command -v shasum >/dev/null 2>&1; then
            shasum -a 256 "${PRODUCED[@]}" > checksums.sha256
        elif command -v sha256sum >/dev/null 2>&1; then
            sha256sum "${PRODUCED[@]}" > checksums.sha256
        fi
    )
    msg "产物列表:"
    ls -lh "$OUT_DIR" | sed 's/^/  /'
    [ -f "$OUT_DIR/checksums.sha256" ] && echo "  校验和: $OUT_DIR/checksums.sha256"
fi

if [ "$FAILED" = "1" ]; then
    die "部分平台构建失败"
fi
msg "全部平台构建成功"
