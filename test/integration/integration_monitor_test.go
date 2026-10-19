//go:build integration && !windows

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// monitorProc runs `wserial monitor` with stdin/stdout pipes. This is the same
// bridge/WSP/RAW code path as `wserial run`, without requiring a PTY, so it
// also works inside restricted environments where /dev/ptmx is unavailable.
type monitorProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *stream
	stderr syncBuffer

	exited   chan error
	exitErr  error
	stopOnce sync.Once
}

func startMonitor(t *testing.T, wserialBin string, target simAddrs, useRaw bool, baud uint, extra ...string) *monitorProc {
	t.Helper()
	port := target.WSPPort
	if useRaw {
		port = target.RAWPort
	}
	args := []string{
		"monitor",
		"-H", hostOfAddr(t, target.WSP),
		"-P", fmt.Sprintf("%d", port),
		"-b", fmt.Sprintf("%d", baud),
		"-v",
	}
	if useRaw {
		args = append(args, "--raw")
	}
	args = append(args, extra...)

	cmd := exec.Command(wserialBin, args...)
	p := &monitorProc{t: t, cmd: cmd}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("monitor stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("monitor stdout pipe: %v", err)
	}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wserial monitor: %v", err)
	}
	p.stdin = stdin
	p.stdout = newStream(stdout)
	p.exited = make(chan error, 1)
	go func() { p.exited <- cmd.Wait() }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *monitorProc) stop(t *testing.T) {
	p.stopOnce.Do(func() {
		_ = p.stdin.Close()
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case p.exitErr = <-p.exited:
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
			select {
			case p.exitErr = <-p.exited:
			case <-time.After(2 * time.Second):
			}
			t.Errorf("wserial monitor did not exit within 5s of SIGTERM")
		}
	})
}

func (p *monitorProc) alive() bool {
	select {
	case p.exitErr = <-p.exited:
		return false
	default:
		return processAlive(p.cmd)
	}
}

func (p *monitorProc) failWithDiagnostics(t *testing.T, msg string) {
	t.Helper()
	t.Fatalf("%s\nmonitor stderr:\n%s", msg, p.stderr.tail(8000))
}

// writeInput sends bytes to the monitor's stdin.
func (p *monitorProc) writeInput(t *testing.T, b []byte, timeout time.Duration, desc string) {
	t.Helper()
	writeAll(t, p.stdin, b, timeout, desc)
}

// readOutput reads exactly n bytes received from the device.
func (p *monitorProc) readOutput(t *testing.T, n int, timeout time.Duration, desc string) []byte {
	t.Helper()
	return p.stdout.readN(t, n, timeout, desc)
}

// ---------------------------------------------------------------------------
// monitor-mode tests (no PTY required)
// ---------------------------------------------------------------------------

// TestMonitorWSPDataPath drives the full WSP data path through `wserial monitor`
// (stdin/stdout pipes instead of a PTY): text, all byte values and 64 KiB
// chunks in both directions.
func TestMonitorWSPDataPath(t *testing.T) {
	simBin, wserialBin := ensureBinaries(t)
	sp := startSim(t, simBin, nil)
	hw := dialHW(t, sp.addr)
	m := startMonitor(t, wserialBin, sp.addr, false, 115200)
	waitForSerialClient(t, sp.addr, "wsp", 10*time.Second)
	waitForUARTCfg(t, sp.addr, 115200, 10*time.Second)

	// host -> target: text
	down := []byte("hello-hardware")
	m.writeInput(t, down, 5*time.Second, "monitor stdin (host->target text)")
	assertBytes(t, readExactConn(t, hw, len(down), 5*time.Second, "target<-host text"), down,
		"host->target text")

	// target -> host: text
	up := []byte("hello-host")
	writeConn(t, hw, up, 5*time.Second, "target->host text")
	assertBytes(t, m.readOutput(t, len(up), 5*time.Second, "host<-target text"), up,
		"target->host text")

	// host -> target: all byte values, 4 KiB
	binDown := allByteValues4K()
	m.writeInput(t, binDown, 15*time.Second, "monitor stdin (host->target 4 KiB)")
	assertBytes(t, readExactConn(t, hw, len(binDown), 15*time.Second, "target<-host 4 KiB"), binDown,
		"host->target 4 KiB binary (0x00..0xFF)")

	// target -> host: 4 KiB pseudo-random
	binUp := pattern(4096, 0xC0FFEE)
	writeConn(t, hw, binUp, 15*time.Second, "target->host 4 KiB")
	assertBytes(t, m.readOutput(t, len(binUp), 15*time.Second, "host<-target 4 KiB"), binUp,
		"target->host 4 KiB binary")

	// host -> target: 64 KiB
	bigDown := pattern(64<<10, 0x12345678)
	m.writeInput(t, bigDown, 30*time.Second, "monitor stdin (host->target 64 KiB)")
	assertBytes(t, readExactConn(t, hw, len(bigDown), 30*time.Second, "target<-host 64 KiB"), bigDown,
		"host->target 64 KiB")

	// target -> host: 64 KiB
	bigUp := pattern(64<<10, 0xDEADBEEF)
	writeConn(t, hw, bigUp, 30*time.Second, "target->host 64 KiB")
	assertBytes(t, m.readOutput(t, len(bigUp), 30*time.Second, "host<-target 64 KiB"), bigUp,
		"target->host 64 KiB")

	if !m.alive() {
		m.failWithDiagnostics(t, "wserial monitor exited during the data transfer")
	}

	// Device-side counters must reflect the transferred bytes.
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

// TestMonitorUARTCfg verifies that `-b 9600` reaches the device as UART_CFG,
// is acknowledged, changes the device-side baud and keeps the data path alive.
func TestMonitorUARTCfg(t *testing.T) {
	simBin, wserialBin := ensureBinaries(t)
	sp := startSim(t, simBin, nil)
	hw := dialHW(t, sp.addr)
	m := startMonitor(t, wserialBin, sp.addr, false, 9600)
	waitForSerialClient(t, sp.addr, "wsp", 10*time.Second)

	st := waitForUARTCfg(t, sp.addr, 9600, 10*time.Second)
	if st.Device.DataBits != 8 || st.Device.Parity != 0 || st.Device.StopBits != 1 || st.Device.Flow != 0 {
		t.Fatalf("device format = %d/%d/%d/%d, want 8/0/1/0",
			st.Device.DataBits, st.Device.Parity, st.Device.StopBits, st.Device.Flow)
	}
	if !hasRecentFrame(st, 0x3) { // UART_CFG
		t.Fatalf("device did not record a UART_CFG frame: %+v", st.Device.RecentFrames)
	}
	if !strings.Contains(m.stderr.String(), "串口配置已生效") {
		t.Logf("wserial stderr did not mention 串口配置已生效 (not fatal):\n%s", m.stderr.tail(4000))
	}

	// The data path still works with the new configuration.
	msg := []byte("after-uart-cfg")
	m.writeInput(t, msg, 5*time.Second, "monitor stdin after UART_CFG")
	assertBytes(t, readExactConn(t, hw, len(msg), 5*time.Second, "target<-host after UART_CFG"), msg,
		"data after UART_CFG")
}

// TestMonitorReconnect kills the simulator, restarts it on the same ports and
// verifies that wserial reconnects automatically and the data path recovers
// while the monitor process keeps running.
func TestMonitorReconnect(t *testing.T) {
	simBin, wserialBin := ensureBinaries(t)
	sp1 := startSim(t, simBin, nil)
	hw1 := dialHW(t, sp1.addr)
	m := startMonitor(t, wserialBin, sp1.addr, false, 115200)
	waitForSerialClient(t, sp1.addr, "wsp", 10*time.Second)

	// Sanity check before the restart.
	before := []byte("before-restart")
	m.writeInput(t, before, 5*time.Second, "monitor stdin before restart")
	assertBytes(t, readExactConn(t, hw1, len(before), 5*time.Second, "target<-host before restart"),
		before, "host->target before restart")

	// Stop the simulator and bring a new instance up on the same ports.
	saved := sp1.addr
	sp1.stop(t)
	_ = hw1.Close()
	sp2 := startSim(t, simBin, &saved)
	hw2 := dialHW(t, sp2.addr)

	// wserial uses exponential backoff starting at 1s.
	waitForSerialClient(t, sp2.addr, "wsp", 25*time.Second)
	waitForUARTCfg(t, sp2.addr, 115200, 10*time.Second)
	if !m.alive() {
		m.failWithDiagnostics(t, "wserial monitor exited instead of reconnecting")
	}

	// Data flows again in both directions.
	after := []byte("after-reconnect")
	m.writeInput(t, after, 10*time.Second, "monitor stdin after reconnect")
	assertBytes(t, readExactConn(t, hw2, len(after), 10*time.Second, "target<-host after reconnect"),
		after, "host->target after reconnect")

	up := []byte("host-after-reconnect")
	writeConn(t, hw2, up, 10*time.Second, "target->host after reconnect")
	assertBytes(t, m.readOutput(t, len(up), 10*time.Second, "host<-target after reconnect"), up,
		"target->host after reconnect")
}

// TestMonitorRaw verifies the RAW (no framing, no handshake) passthrough mode.
func TestMonitorRaw(t *testing.T) {
	simBin, wserialBin := ensureBinaries(t)
	sp := startSim(t, simBin, nil)
	hw := dialHW(t, sp.addr)
	m := startMonitor(t, wserialBin, sp.addr, true, 0)
	waitForSerialClient(t, sp.addr, "raw", 10*time.Second)

	down := []byte("raw-hello-hardware")
	m.writeInput(t, down, 5*time.Second, "raw monitor stdin")
	assertBytes(t, readExactConn(t, hw, len(down), 5*time.Second, "raw target<-host"), down,
		"RAW host->target text")

	up := []byte("raw-hello-host")
	writeConn(t, hw, up, 5*time.Second, "raw target->host")
	assertBytes(t, m.readOutput(t, len(up), 5*time.Second, "raw host<-target"), up,
		"RAW target->host text")

	bin := allByteValues4K()
	m.writeInput(t, bin, 15*time.Second, "raw monitor stdin binary")
	assertBytes(t, readExactConn(t, hw, len(bin), 15*time.Second, "raw target<-host binary"), bin,
		"RAW host->target 4 KiB binary")

	upBin := pattern(8192, 0xABCDEF)
	writeConn(t, hw, upBin, 15*time.Second, "raw target->host binary")
	assertBytes(t, m.readOutput(t, len(upBin), 15*time.Second, "raw host<-target binary"), upBin,
		"RAW target->host 8 KiB binary")
}

// TestMonitorBusy verifies that a second client gets a clear ERROR(BUSY) and a
// non-zero exit code while the first session keeps working.
func TestMonitorBusy(t *testing.T) {
	simBin, wserialBin := ensureBinaries(t)
	sp := startSim(t, simBin, nil)
	hw := dialHW(t, sp.addr)
	first := startMonitor(t, wserialBin, sp.addr, false, 115200)
	waitForSerialClient(t, sp.addr, "wsp", 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	second := exec.CommandContext(ctx, wserialBin,
		"monitor", "-H", hostOfAddr(t, sp.addr.WSP),
		"-P", fmt.Sprintf("%d", sp.addr.WSPPort), "-v")
	var stderr syncBuffer
	second.Stderr = &stderr
	second.Stdout = io.Discard
	err := second.Run()
	if ctx.Err() != nil {
		t.Fatalf("second wserial did not exit while the device was busy: %v\nstderr:\n%s", err, stderr.tail(4000))
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("second wserial exit = %v, want a non-zero exit code; stderr:\n%s", err, stderr.tail(4000))
	}
	if !strings.Contains(stderr.String(), "BUSY") {
		t.Fatalf("second wserial stderr does not mention BUSY:\n%s", stderr.tail(4000))
	}

	// The first session must be unaffected.
	if !first.alive() {
		first.failWithDiagnostics(t, "first wserial monitor exited when a second client was rejected")
	}
	msg := []byte("busy-first-still-works")
	first.writeInput(t, msg, 5*time.Second, "first monitor stdin after busy rejection")
	assertBytes(t, readExactConn(t, hw, len(msg), 5*time.Second, "target<-first after busy"), msg,
		"first session still works after BUSY")
}
