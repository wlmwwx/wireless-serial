//go:build !windows

package vport

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// DefaultDevice is the stable symlink path used when -d/--device is omitted on
// Unix.
const DefaultDevice = "/tmp/ttyWS0"

// DefaultDevicePath returns the platform default virtual port path.
func DefaultDevicePath() string { return DefaultDevice }

// unixPort is a PTY pair. The slave side backs the stable symlink that user
// programs open; the master side is read/written by the bridge.
type unixPort struct {
	master *os.File
	slave  *os.File
	link   string
	target string

	warnings []string

	closeOnce sync.Once
	closeErr  error
}

// Open creates a fresh PTY, puts its slave side into raw mode, makes it world
// readable/writable and publishes it at device via a symlink. An existing
// symlink at device is replaced; a non-symlink path is an error so that a real
// device is never clobbered.
func Open(device string, _ Options) (Port, error) {
	if device == "" {
		device = DefaultDevice
	}
	if strings.HasSuffix(device, "/") {
		return nil, fmt.Errorf("虚拟串口路径 %q 不能是目录", device)
	}

	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("创建 PTY 失败: %w", err)
	}
	target := slave.Name()

	cleanup := func() {
		_ = master.Close()
		_ = slave.Close()
	}

	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		cleanup()
		return nil, fmt.Errorf("设置 PTY 原始模式失败: %w", err)
	}
	// The slave device is created mode 0620 (owner only). Make it accessible
	// to all local users, matching what `socat pty,raw,echo=0` does. This is
	// best effort: some platforms/containers restrict chmod on devfs, and the
	// owner can still open the port.
	var warnings []string
	if err := os.Chmod(target, 0o666); err != nil {
		warnings = append(warnings, fmt.Sprintf("设置 %s 权限为 0666 失败（仅影响其他用户访问）: %v", target, err))
	}

	if err := publishSymlink(device, target); err != nil {
		cleanup()
		return nil, err
	}

	return &unixPort{master: master, slave: slave, link: device, target: target, warnings: warnings}, nil
}

// Warnings returns non-fatal setup problems (e.g. chmod failed).
func (p *unixPort) Warnings() []string { return p.warnings }

// publishSymlink atomically (re)creates the stable path.
func publishSymlink(device, target string) error {
	if fi, err := os.Lstat(device); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("路径 %s 已存在且不是符号链接，拒绝覆盖（请换一个 --device）", device)
		}
		if err := os.Remove(device); err != nil {
			return fmt.Errorf("移除旧的符号链接 %s 失败: %w", device, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查路径 %s 失败: %w", device, err)
	}
	if err := os.Symlink(target, device); err != nil {
		return fmt.Errorf("创建符号链接 %s -> %s 失败: %w", device, target, err)
	}
	return nil
}

// Name returns the stable path users should open.
func (p *unixPort) Name() string { return p.link }

// Target returns the underlying /dev/ttysNNN path (for diagnostics).
func (p *unixPort) Target() string { return p.target }

// Read reads bytes the user program wrote to the virtual port.
func (p *unixPort) Read(b []byte) (int, error) { return p.master.Read(b) }

// Write delivers bytes to the user program.
func (p *unixPort) Write(b []byte) (int, error) { return p.master.Write(b) }

// Close closes both ends of the PTY and removes the symlink we created.
func (p *unixPort) Close() error {
	p.closeOnce.Do(func() {
		// Remove the link first so a concurrent process cannot open a dead
		// PTY through it.
		if err := os.Remove(p.link); err != nil && !os.IsNotExist(err) {
			p.closeErr = fmt.Errorf("移除符号链接 %s 失败: %w", p.link, err)
		}
		if err := p.master.Close(); err != nil && p.closeErr == nil {
			p.closeErr = err
		}
		if err := p.slave.Close(); err != nil && p.closeErr == nil {
			p.closeErr = err
		}
	})
	return p.closeErr
}
