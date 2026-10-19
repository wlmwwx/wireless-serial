# wireless-serial — 顶层构建入口
#
# 常用目标：
#   make            # 等同 make help
#   make host       # 本机构建 wserial（host/wserial）
#   make host-all   # 交叉编译三平台到 host/dist/
#   make test       # 协议单测 + Go 单测（有 test/run.sh 时优先用它）
#   make sim        # 启动模拟设备并联调 wserial（不接硬件）
#   make fw         # 构建 ESP32-C3 固件（自动加载 .idf 工具链）
#   make flash PORT=/dev/cu.usbmodemXXXX   # 烧录固件
#   make clean      # 清理构建产物（不会删除 .idf 工具链）
#
# 可用变量（均可命令行覆盖）：
#   PORT=/dev/cu.usbmodemXXXX  烧录串口
#   BAUD=460800                烧录波特率
#   MONITOR=1                  make flash 后自动打开串口监视器

SHELL := /bin/bash
.DEFAULT_GOAL := help

ROOT       := $(CURDIR)
HOST_DIR   := $(ROOT)/host
FW_DIR     := $(ROOT)/firmware
BUILD_DIR  := $(ROOT)/build
DIST_DIR   := $(HOST_DIR)/dist
GO         ?= go
CC         ?= cc
PORT       ?=
BAUD       ?= 460800

.PHONY: help host host-all test sim fw flash clean deps doctor

help: ## 显示本帮助
	@printf '%s\n' \
	  'wireless-serial 构建入口' \
	  '' \
	  '用法: make <目标> [变量=值]' \
	  '' \
	  '目标:' \
	  '  help       显示本帮助（默认目标）' \
	  '  host       本机构建主机工具 -> host/wserial' \
	  '  host-all   交叉编译 macOS/Linux/Windows -> host/dist/' \
	  '  test       协议单测 + Go 单测（优先使用 test/run.sh）' \
	  '  sim        启动模拟设备 + wserial 联调（Ctrl+C 退出）' \
	  '  fw         构建 ESP32-C3 固件（自动 source .idf/esp-idf/export.sh）' \
	  '  flash      烧录固件，需指定 PORT；MONITOR=1 烧录后打开监视器' \
	  '  clean      删除构建产物（保留 .idf/ 与源码）' \
	  '  deps       打印依赖检查结果（go / cc / ESP-IDF）' \
	  '' \
	  '变量:' \
	  '  PORT=/dev/cu.usbmodemXXXX  烧录串口（make flash 必填）' \
	  '  BAUD=460800                烧录波特率（默认 460800）' \
	  '  MONITOR=1                  make flash 后自动 idf.py monitor' \
	  '' \
	  '示例:' \
	  '  make host && ./host/wserial list' \
	  '  make host-all' \
	  '  make test' \
	  '  make fw' \
	  '  make flash PORT=/dev/cu.usbmodem1101 MONITOR=1' \
	  '  make sim'

host: ## 本机构建主机工具
	@if [ ! -f "$(HOST_DIR)/go.mod" ]; then \
		echo "error: 未找到 $(HOST_DIR)/go.mod；host/ 源码可能尚未就绪" >&2; exit 1; \
	fi
	@mkdir -p "$(HOST_DIR)"
	@cd "$(HOST_DIR)" && CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -o wserial ./cmd/wserial
	@echo "==> 已生成 $(HOST_DIR)/wserial"
	@echo "    下一步: $(HOST_DIR)/wserial list"

host-all: ## 三平台交叉编译到 host/dist/
	@bash "$(ROOT)/scripts/build-host.sh"

test: ## 协议单测 + Go 单测
	@set -e; \
	if [ -f "$(ROOT)/test/run.sh" ]; then \
		echo "==> bash test/run.sh"; \
		cd "$(ROOT)" && bash test/run.sh; \
	else \
		echo "==> test/run.sh 不存在，执行基础测试"; \
		mkdir -p "$(BUILD_DIR)"; \
		if [ -f "$(ROOT)/test/proto_test.c" ]; then \
			$(CC) -std=c99 -Wall -Wextra -Werror -I"$(FW_DIR)/main/include" \
				"$(ROOT)/test/proto_test.c" "$(FW_DIR)/main/wsp_proto.c" \
				-o "$(BUILD_DIR)/proto_test"; \
			"$(BUILD_DIR)/proto_test"; \
		else \
			echo "  跳过 C 协议单测：test/proto_test.c 不存在"; \
		fi; \
		if [ -f "$(HOST_DIR)/go.mod" ]; then \
			cd "$(HOST_DIR)" && $(GO) test ./...; \
		else \
			echo "  跳过 Go 单测：host/go.mod 不存在"; \
		fi; \
	fi

sim: ## 启动模拟设备 + wserial 联调
	@bash "$(ROOT)/scripts/run-sim.sh"

fw: ## 构建 ESP32-C3 固件
	@bash "$(ROOT)/scripts/build-firmware.sh"

flash: ## 烧录固件（需 PORT）
	@if [ -z "$(PORT)" ]; then \
		echo "error: 请指定串口，例如: make flash PORT=/dev/cu.usbmodem1101" >&2; \
		echo "提示: macOS 用 ls /dev/cu.usb*；Linux 用 ls /dev/ttyUSB* /dev/ttyACM*" >&2; \
		exit 1; \
	fi
	@bash "$(ROOT)/scripts/flash.sh" -p "$(PORT)" -b "$(BAUD)" $(if $(filter 1,$(MONITOR)),-m,)

clean: ## 清理构建产物
	@rm -rf "$(BUILD_DIR)" "$(DIST_DIR)" "$(HOST_DIR)/wserial" "$(HOST_DIR)/wserial.exe"
	@rm -rf "$(FW_DIR)/build" "$(FW_DIR)/sdkconfig" "$(FW_DIR)/sdkconfig.old"
	@echo "==> 已清理 build/、host/dist/、host/wserial、firmware/build/、firmware/sdkconfig*"
	@echo "    .idf/ 工具链与源码未删除"

deps: ## 打印依赖检查结果
	@echo "==> 依赖检查"
	@printf '  go      : '; if command -v $(GO) >/dev/null 2>&1; then $(GO) version; else echo "缺失（https://go.dev/dl/）"; fi
	@printf '  python3 : '; if command -v python3 >/dev/null 2>&1; then python3 --version; else echo "缺失（ESP-IDF 需要）"; fi
	@printf '  cc      : '; if command -v $(CC) >/dev/null 2>&1; then $(CC) --version 2>/dev/null | head -1; else echo "缺失（安装 Xcode CLT / build-essential）"; fi
	@printf '  ESP-IDF : '; if [ -f "$(ROOT)/.idf/esp-idf/export.sh" ]; then \
		v=$$(git -C "$(ROOT)/.idf/esp-idf" describe --tags --always 2>/dev/null || echo "未知版本"); \
		echo "$(ROOT)/.idf/esp-idf ($$v)"; \
	else echo "缺失（见 docs/firmware-build.md）"; fi

doctor: deps ## deps 的别名
