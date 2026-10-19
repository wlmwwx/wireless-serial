//go:build integration && windows

// Windows has no PTY: the virtual serial port is a com0com port pair instead.
// The end-to-end test therefore requires com0com to be installed and a pair
// (e.g. COM11 <-> COM12) to be created with its setupc tool; wserial opens one
// end (--device COM12) and the user/test opens the other end (COM11).
//
// This file exists so that `go test -tags integration ./...` succeeds on
// Windows with an explicit skip instead of compile errors.
package integration

import "testing"

func TestWindowsIntegrationRequiresCom0com(t *testing.T) {
	t.Skip("Windows 端到端集成测试需要 com0com：先安装 com0com 并用 setupc 创建端口对" +
		"（如 COM11<->COM12），再以 --device COM12 运行 wserial；" +
		"本机未配置 com0com 时跳过（详见 docs/installation.md）。")
}
