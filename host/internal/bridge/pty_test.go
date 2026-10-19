//go:build !windows

package bridge

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	wlog "github.com/wireless-serial/wserial/internal/log"
)

// ptySupported reports whether this host/environment allows creating PTYs.
// Restricted sandboxes (e.g. DSH workspace-write) deny /dev/ptmx and make
// these tests skip instead of fail.
func ptySupported() bool {
	master, slave, err := pty.Open()
	if err != nil {
		return false
	}
	_ = master.Close()
	_ = slave.Close()
	return true
}

func skipWithoutPTY(t *testing.T) {
	t.Helper()
	if !ptySupported() {
		t.Skip("当前环境不允许创建 PTY（/dev/ptmx 不可用，例如受限沙箱）；请在不限制设备节点的宿主 macOS/Linux 上运行本测试")
	}
}

func openSlave(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	return f
}

func readExactFile(t *testing.T, f *os.File, n int, timeout time.Duration) []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, n)
		total := 0
		for total < n {
			m, err := f.Read(buf[total:])
			total += m
			if err != nil {
				errCh <- err
				return
			}
		}
		ch <- buf
	}()
	select {
	case b := <-ch:
		return b
	case err := <-errCh:
		t.Fatalf("读取虚拟串口失败: %v", err)
	case <-time.After(timeout):
		t.Fatalf("读取虚拟串口超时（%d 字节）", n)
	}
	return nil
}

// TestRunPTYEchoAndReconnect is the real end-to-end test: it creates a PTY,
// exposes it through a symlink, bridges it to a mock WSP device over TCP and
// verifies both the data path and the reconnect behaviour.
func TestRunPTYEchoAndReconnect(t *testing.T) {
	skipWithoutPTY(t)
	srv := newMockWSP(t)
	device := filepath.Join(t.TempDir(), "ttyWS0")

	var logBuf bytes.Buffer
	cfg := testConfig(srv.port())
	cfg.Device = device
	cfg.Logger = wlog.New(&logBuf, wlog.LevelDebug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, cfg) }()

	waitFor(t, 5*time.Second, "符号链接出现", func() bool {
		fi, err := os.Lstat(device)
		return err == nil && fi.Mode()&os.ModeSymlink != 0
	})

	slave := openSlave(t, device)
	defer slave.Close()

	const first = "hello-pty-wsp"
	if _, err := slave.Write([]byte(first)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(readExactFile(t, slave, len(first), 5*time.Second)); got != first {
		t.Fatalf("echo = %q, want %q", got, first)
	}

	waitFor(t, 3*time.Second, "UART_CFG 下发", func() bool {
		_, _, cfg := srv.status()
		return cfg != nil && cfg.Baud == 115200
	})

	// Kill the TCP link: the PTY and its symlink must stay alive.
	srv.closeConns()
	waitFor(t, 8*time.Second, "重连", func() bool {
		accepted, _, _ := srv.status()
		return accepted >= 2
	})
	if _, err := os.Lstat(device); err != nil {
		t.Fatalf("重连期间虚拟串口消失: %v", err)
	}

	const second = "after-reconnect"
	if _, err := slave.Write([]byte(second)); err != nil {
		t.Fatalf("write after reconnect: %v", err)
	}
	if got := string(readExactFile(t, slave, len(second), 5*time.Second)); got != second {
		t.Fatalf("echo after reconnect = %q, want %q", got, second)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v\n日志:\n%s", err, logBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run 未退出\n日志:\n%s", logBuf.String())
	}

	if _, err := os.Lstat(device); !os.IsNotExist(err) {
		t.Fatalf("退出后符号链接未被清理: %v", err)
	}
}

// TestRunPTYRawEcho covers the RAW (port 3334) byte-stream path over a real PTY.
func TestRunPTYRawEcho(t *testing.T) {
	skipWithoutPTY(t)
	echo := newRawEchoServer(t)
	device := filepath.Join(t.TempDir(), "ttyWSRaw")

	var logBuf bytes.Buffer
	cfg := testConfig(echo.port())
	cfg.Raw = true
	cfg.Device = device
	cfg.Logger = wlog.New(&logBuf, wlog.LevelDebug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, cfg) }()

	waitFor(t, 5*time.Second, "符号链接出现", func() bool {
		_, err := os.Lstat(device)
		return err == nil
	})
	slave := openSlave(t, device)
	defer slave.Close()

	payload := []byte("raw-pty-0123456789")
	if _, err := slave.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readExactFile(t, slave, len(payload), 5*time.Second); !bytes.Equal(got, payload) {
		t.Fatalf("raw echo = %q, want %q", got, payload)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run(raw) returned %v\n日志:\n%s", err, logBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run(raw) 未退出\n日志:\n%s", logBuf.String())
	}
}
