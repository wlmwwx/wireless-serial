//go:build integration && !windows

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// vportProc runs `wserial run` and owns the resulting virtual serial port.
type vportProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	devPath string

	file *os.File
	in   *stream // bytes coming from the device (PTY slave -> test)
	// out is the PTY slave itself; writing to it is equivalent to a user
	// program writing bytes to the serial port.

	stdout syncBuffer
	stderr syncBuffer

	exited   chan error
	exitErr  error
	stopOnce sync.Once
}

// startVirtualSerial launches `wserial run -H <host> -P <port> -d <devPath>`.
func startVirtualSerial(t *testing.T, wserialBin string, target simAddrs,
	devPath string, baud uint, useRaw bool, extra ...string) *vportProc {
	t.Helper()
	port := target.WSPPort
	if useRaw {
		port = target.RAWPort
	}
	args := []string{
		"run",
		"-H", hostOfAddr(t, target.WSP),
		"-P", fmt.Sprintf("%d", port),
		"-d", devPath,
		"-b", fmt.Sprintf("%d", baud),
		"-v",
	}
	if useRaw {
		args = append(args, "--raw")
	}
	args = append(args, extra...)

	cmd := exec.Command(wserialBin, args...)
	p := &vportProc{t: t, cmd: cmd, devPath: devPath}
	cmd.Stdout = &p.stdout
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wserial run: %v", err)
	}
	p.exited = make(chan error, 1)
	go func() { p.exited <- cmd.Wait() }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

// waitReady waits for the symlink to appear and for the PTY slave behind it to
// be openable, then puts it into raw mode and starts the RX stream.
func (p *vportProc) waitReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "virtual serial port "+p.devPath, func() (bool, error) {
		if p.file != nil {
			return true, nil
		}
		if !processAlive(p.cmd) {
			return false, fmt.Errorf("wserial exited before the virtual port was ready; stderr:\n%s",
				p.stderr.tail(8000))
		}
		fi, err := os.Lstat(p.devPath)
		if err != nil {
			return false, nil
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return false, fmt.Errorf("%s exists but is not a symlink", p.devPath)
		}
		f, err := os.OpenFile(p.devPath, os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			// The symlink can be published just before the PTY is fully usable.
			return false, nil
		}
		if err := makeRaw(int(f.Fd())); err != nil {
			_ = f.Close()
			return false, fmt.Errorf("set raw mode on %s: %w", p.devPath, err)
		}
		p.file = f
		p.in = newStream(f)
		return true, nil
	})
}

// write sends bytes as the user program would to the virtual serial port.
func (p *vportProc) write(t *testing.T, b []byte, timeout time.Duration, desc string) {
	t.Helper()
	if p.file == nil {
		t.Fatalf("%s: virtual serial port is not open", desc)
	}
	writeAll(t, p.file, b, timeout, desc)
}

// read reads exactly n bytes received through the virtual serial port.
func (p *vportProc) read(t *testing.T, n int, timeout time.Duration, desc string) []byte {
	t.Helper()
	if p.in == nil {
		t.Fatalf("%s: virtual serial port is not open", desc)
	}
	return p.in.readN(t, n, timeout, desc)
}

func (p *vportProc) alive() bool {
	select {
	case p.exitErr = <-p.exited:
		return false
	default:
		return processAlive(p.cmd)
	}
}

func (p *vportProc) failWithDiagnostics(t *testing.T, msg string) {
	t.Helper()
	t.Fatalf("%s\nwserial stderr:\n%s", msg, p.stderr.tail(8000))
}

func (p *vportProc) stop(t *testing.T) {
	p.stopOnce.Do(func() {
		if p.file != nil {
			_ = p.file.Close()
			p.file = nil
		}
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case p.exitErr = <-p.exited:
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
			select {
			case p.exitErr = <-p.exited:
			case <-time.After(2 * time.Second):
			}
			t.Errorf("wserial run did not exit within 5s of SIGTERM")
		}
		if p.exitErr != nil {
			t.Logf("wserial run exit: %v; stderr:\n%s", p.exitErr, p.stderr.tail(4000))
		}

		// wserial must remove the symlink it created (bug otherwise).
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Lstat(p.devPath); os.IsNotExist(err) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("virtual serial symlink %s still exists after wserial exited", p.devPath)
	})
}

// runWserialToExit runs a wserial invocation expected to terminate (e.g. the
// second client in the BUSY test), returning its exit code and stderr.
func runWserialToExit(t *testing.T, timeout time.Duration, wserialBin string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, wserialBin, args...)
	var stderr syncBuffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("wserial did not exit within %s: %v\nstderr:\n%s", timeout, err, stderr.tail(4000))
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("wserial failed to run: %v\nstderr:\n%s", err, stderr.tail(4000))
		}
		code = ee.ExitCode()
	}
	return code, stderr.String()
}

// ---------------------------------------------------------------------------
// virtual serial port (PTY) tests
// ---------------------------------------------------------------------------

// TestVirtualSerialWSPDataPath is the main end-to-end test: a user program
// opens the virtual serial port and exchanges text, every byte value and
// 64 KiB blocks with the fake target hardware, through the WSP frame protocol.
func TestVirtualSerialWSPDataPath(t *testing.T) {
	requirePTY(t)
	simBin, wserialBin := ensureBinaries(t)
	sp := startSim(t, simBin, nil)
	hw := dialHW(t, sp.addr)

	devPath := filepath.Join(t.TempDir(), "ttyWSIT")
	vp := startVirtualSerial(t, wserialBin, sp.addr, devPath, 115200, false)
	vp.waitReady(t, 10*time.Second)
	waitForSerialClient(t, sp.addr, "wsp", 10*time.Second)
	waitForUARTCfg(t, sp.addr, 115200, 10*time.Second)

	// The device path must be a symlink to a /dev/ttys* PTY slave.
	fi, err := os.Lstat(devPath)
	if err != nil {
		t.Fatalf("lstat %s: %v", devPath, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink (mode %v)", devPath, fi.Mode())
	}
	target, err := os.Readlink(devPath)
	if err != nil {
		t.Fatalf("readlink %s: %v", devPath, err)
	}
	if !strings.HasPrefix(target, "/dev/") {
		t.Fatalf("%s points to %q, want a /dev/ PTY slave", devPath, target)
	}

	// host -> target: text
	down := []byte("hello-hardware")
	vp.write(t, down, 5*time.Second, "virtual port (host->target text)")
	assertBytes(t, readExactConn(t, hw, len(down), 5*time.Second, "target<-host text"), down,
		"host->target text")

	// target -> host: text
	up := []byte("hello-host")
	writeConn(t, hw, up, 5*time.Second, "target->host text")
	assertBytes(t, vp.read(t, len(up), 5*time.Second, "host<-target text"), up,
		"target->host text")

	// host -> target: all byte values, 4 KiB
	binDown := allByteValues4K()
	vp.write(t, binDown, 15*time.Second, "virtual port (host->target 4 KiB)")
	assertBytes(t, readExactConn(t, hw, len(binDown), 15*time.Second, "target<-host 4 KiB"), binDown,
		"host->target 4 KiB binary (0x00..0xFF)")

	// target -> host: 4 KiB binary
	binUp := pattern(4096, 0xC0FFEE)
	writeConn(t, hw, binUp, 15*time.Second, "target->host 4 KiB")
	assertBytes(t, vp.read(t, len(binUp), 15*time.Second, "host<-target 4 KiB"), binUp,
		"target->host 4 KiB binary")

	// 64 KiB in both directions.
	bigDown := pattern(64<<10, 0x12345678)
	vp.write(t, bigDown, 30*time.Second, "virtual port (host->target 64 KiB)")
	assertBytes(t, readExactConn(t, hw, len(bigDown), 30*time.Second, "target<-host 64 KiB"), bigDown,
		"host->target 64 KiB")

	bigUp := pattern(64<<10, 0xDEADBEEF)
	writeConn(t, hw, bigUp, 30*time.Second, "target->host 64 KiB")
	assertBytes(t, vp.read(t, len(bigUp), 30*time.Second, "host<-target 64 KiB"), bigUp,
		"target->host 64 KiB")

	if !vp.alive() {
		vp.failWithDiagnostics(t, "wserial exited during the data transfer")
	}

	st, err := fetchStatus(sp.addr)
	if err != nil {
		t.Fatalf("fetch admin status: %v", err)
	}
	if st.Device.Stats.UARTTXBytes < uint32(len(bigDown)) {
		t.Fatalf("uart_tx_bytes = %d, want >= %d", st.Device.Stats.UARTTXBytes, len(bigDown))
	}
	if st.Device.Stats.UARTRXBytes < uint32(len(bigUp)) {
		t.Fatalf("uart_rx_bytes = %d, want >= %d", st.Device.Stats.UARTRXBytes, len(bigUp))
	}
}

// TestVirtualSerialUARTCfg verifies that the requested baud rate reaches the
// simulated device as UART_CFG (acknowledged, device-side value updated) while
// the virtual port stays usable.
func TestVirtualSerialUARTCfg(t *testing.T) {
	requirePTY(t)
	simBin, wserialBin := ensureBinaries(t)
	sp := startSim(t, simBin, nil)
	hw := dialHW(t, sp.addr)

	devPath := filepath.Join(t.TempDir(), "ttyWSIT-cfg")
	vp := startVirtualSerial(t, wserialBin, sp.addr, devPath, 9600, false)
	vp.waitReady(t, 10*time.Second)
	waitForSerialClient(t, sp.addr, "wsp", 10*time.Second)

	st := waitForUARTCfg(t, sp.addr, 9600, 10*time.Second)
	if !hasRecentFrame(st, 0x3) { // UART_CFG
		t.Fatalf("device did not record a UART_CFG frame: %+v", st.Device.RecentFrames)
	}
	if !strings.Contains(vp.stderr.String(), "串口配置已生效") {
		t.Logf("wserial stderr did not mention 串口配置已生效 (not fatal):\n%s", vp.stderr.tail(4000))
	}

	msg := []byte("after-uart-cfg")
	vp.write(t, msg, 5*time.Second, "virtual port after UART_CFG")
	assertBytes(t, readExactConn(t, hw, len(msg), 5*time.Second, "target<-host after UART_CFG"), msg,
		"data after UART_CFG")
}

// TestVirtualSerialRaw verifies raw passthrough mode through the virtual serial
// port (no handshake, no framing).
func TestVirtualSerialRaw(t *testing.T) {
	requirePTY(t)
	simBin, wserialBin := ensureBinaries(t)
	sp := startSim(t, simBin, nil)
	hw := dialHW(t, sp.addr)

	devPath := filepath.Join(t.TempDir(), "ttyWSIT-raw")
	vp := startVirtualSerial(t, wserialBin, sp.addr, devPath, 0, true)
	vp.waitReady(t, 10*time.Second)
	waitForSerialClient(t, sp.addr, "raw", 10*time.Second)

	down := allByteValues4K()
	vp.write(t, down, 15*time.Second, "raw virtual port (host->target)")
	assertBytes(t, readExactConn(t, hw, len(down), 15*time.Second, "raw target<-host"), down,
		"RAW host->target 4 KiB binary")

	up := pattern(8192, 0xABCDEF)
	writeConn(t, hw, up, 15*time.Second, "raw target->host")
	assertBytes(t, vp.read(t, len(up), 15*time.Second, "raw host<-target"), up,
		"RAW target->host 8 KiB binary")
}

// TestVirtualSerialReconnect kills the simulator, restarts it on the same
// ports and verifies that wserial reconnects while the same PTY stays open and
// usable (the virtual port must not disappear across WiFi link flaps).
func TestVirtualSerialReconnect(t *testing.T) {
	requirePTY(t)
	simBin, wserialBin := ensureBinaries(t)
	sp1 := startSim(t, simBin, nil)
	hw1 := dialHW(t, sp1.addr)

	devPath := filepath.Join(t.TempDir(), "ttyWSIT-reconnect")
	vp := startVirtualSerial(t, wserialBin, sp1.addr, devPath, 115200, false)
	vp.waitReady(t, 10*time.Second)
	waitForSerialClient(t, sp1.addr, "wsp", 10*time.Second)
	waitForUARTCfg(t, sp1.addr, 115200, 10*time.Second)

	linkTarget, err := os.Readlink(devPath)
	if err != nil {
		t.Fatalf("readlink %s: %v", devPath, err)
	}

	before := []byte("before-restart")
	vp.write(t, before, 5*time.Second, "virtual port before restart")
	assertBytes(t, readExactConn(t, hw1, len(before), 5*time.Second, "target<-host before restart"),
		before, "host->target before restart")

	// Kill the device and restart it on the exact same ports.
	saved := sp1.addr
	sp1.stop(t)
	_ = hw1.Close()
	sp2 := startSim(t, simBin, &saved)
	hw2 := dialHW(t, sp2.addr)

	// wserial backs off starting at 1s; give it plenty of time.
	waitForSerialClient(t, sp2.addr, "wsp", 25*time.Second)
	waitForUARTCfg(t, sp2.addr, 115200, 10*time.Second)
	if !vp.alive() {
		vp.failWithDiagnostics(t, "wserial exited instead of reconnecting")
	}

	// The same PTY must still exist and still point at the same slave.
	linkTarget2, err := os.Readlink(devPath)
	if err != nil {
		t.Fatalf("virtual serial port disappeared across reconnect: %v", err)
	}
	if linkTarget2 != linkTarget {
		t.Fatalf("virtual serial port retargeted from %q to %q across reconnect", linkTarget, linkTarget2)
	}

	after := []byte("after-reconnect")
	vp.write(t, after, 10*time.Second, "virtual port after reconnect")
	assertBytes(t, readExactConn(t, hw2, len(after), 10*time.Second, "target<-host after reconnect"),
		after, "host->target after reconnect")

	up := []byte("host-after-reconnect")
	writeConn(t, hw2, up, 10*time.Second, "target->host after reconnect")
	assertBytes(t, vp.read(t, len(up), 10*time.Second, "host<-target after reconnect"), up,
		"target->host after reconnect")
}

// TestVirtualSerialBusy verifies that a second virtual-serial client fails with
// a clear BUSY error while the first PTY session keeps working.
func TestVirtualSerialBusy(t *testing.T) {
	requirePTY(t)
	simBin, wserialBin := ensureBinaries(t)
	sp := startSim(t, simBin, nil)
	hw := dialHW(t, sp.addr)

	devPath := filepath.Join(t.TempDir(), "ttyWSIT-busy-1")
	first := startVirtualSerial(t, wserialBin, sp.addr, devPath, 115200, false)
	first.waitReady(t, 10*time.Second)
	waitForSerialClient(t, sp.addr, "wsp", 10*time.Second)

	devPath2 := filepath.Join(t.TempDir(), "ttyWSIT-busy-2")
	code, stderr := runWserialToExit(t, 20*time.Second, wserialBin,
		"run", "-H", hostOfAddr(t, sp.addr.WSP), "-P", fmt.Sprintf("%d", sp.addr.WSPPort),
		"-d", devPath2, "-b", "115200", "-v")
	if code == 0 {
		t.Fatalf("second wserial exited 0, want non-zero; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "BUSY") {
		t.Fatalf("second wserial stderr does not mention BUSY:\n%s", stderr)
	}
	// The failed client must clean up its own symlink.
	waitFor(t, 5*time.Second, "second virtual port symlink cleanup", func() (bool, error) {
		_, err := os.Lstat(devPath2)
		return os.IsNotExist(err), nil
	})

	// The first virtual port keeps working.
	if !first.alive() {
		first.failWithDiagnostics(t, "first wserial exited when a second client was rejected")
	}
	msg := []byte("busy-first-still-works")
	first.write(t, msg, 5*time.Second, "first virtual port after busy rejection")
	assertBytes(t, readExactConn(t, hw, len(msg), 5*time.Second, "target<-first after busy"), msg,
		"first virtual port still works after BUSY")
}
