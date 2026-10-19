//go:build integration && !windows

// Package integration contains end-to-end tests for the wireless-serial stack:
//
//	wserial PTY/stdin <-> WSP or RAW over TCP <-> device simulator <-> fake target hardware
//
// Nothing here needs a real ESP32.  Every TCP port is allocated randomly by the
// simulator (or reused explicitly for the reconnect tests), so parallel test
// runs never collide.
//
// The suite auto-detects whether the environment allows creating PTYs (the
// virtual-serial tests are skipped where /dev/ptmx is unavailable, e.g. inside
// a restricted sandbox — set WSP_REQUIRE_PTY=1 to make that a hard failure).
// The monitor-mode tests never need a PTY and exercise the same
// TCP/WSP/UART_CFG/reconnect/RAW/busy code paths through stdin/stdout pipes.
package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// binaries
// ---------------------------------------------------------------------------

var (
	binOnce       sync.Once
	binErr        error
	simBinary     string
	wserialBinary string
)

// repoRoot walks up from the test's working directory until it finds the
// repository root (identified by firmware/main/wsp_proto.c).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "firmware", "main", "wsp_proto.c")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("cannot locate repository root from %q", dir)
	return ""
}

func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// goEnvValue returns a Go environment variable.
func goEnvValue(key string) string {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// dirWritable reports whether dir exists (or can be created) and is writable.
func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".wsp-write-*")
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	return true
}

// goBuildEnv returns the process environment for `go build`, pointing
// GOCACHE/GOMODCACHE at test/.cache/ when the user's default caches are not
// writable (e.g. restricted sandboxes or read-only CI home directories).
func goBuildEnv(root string) []string {
	env := os.Environ()
	cacheRoot := filepath.Join(root, "test", ".cache")
	if !dirWritable(goEnvValue("GOCACHE")) {
		env = appendEnv(env, "GOCACHE", filepath.Join(cacheRoot, "gocache"))
	}
	if !dirWritable(goEnvValue("GOMODCACHE")) {
		env = appendEnv(env, "GOMODCACHE", filepath.Join(cacheRoot, "gomodcache"))
	}
	return env
}

func appendEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}

// ensureBinaries returns the simulator and wserial binaries, building them
// into test/build/ when they do not exist yet (integration.sh pre-builds them).
func ensureBinaries(t *testing.T) (sim, wserial string) {
	t.Helper()
	binOnce.Do(func() {
		root := repoRoot(t)
		binDir := filepath.Join(root, "test", "build")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			binErr = fmt.Errorf("create %s: %w", binDir, err)
			return
		}
		simBinary = envOr("WSP_SIM_BIN", filepath.Join(binDir, "sim"))
		wserialBinary = envOr("WSP_WSERIAL_BIN", filepath.Join(binDir, "wserial"))

		env := goBuildEnv(root)
		if !isExecutable(simBinary) {
			cmd := exec.Command("go", "build", "-C", filepath.Join(root, "test", "sim"),
				"-o", simBinary, ".")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				binErr = fmt.Errorf("build simulator (%s): %v\n%s", simBinary, err, out)
				return
			}
		}
		if !isExecutable(wserialBinary) {
			cmd := exec.Command("go", "build", "-C", filepath.Join(root, "host"),
				"-o", wserialBinary, "./cmd/wserial")
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				binErr = fmt.Errorf("build wserial (%s): %v\n"+
					"hint: run test/integration.sh (it sets workspace-local GOCACHE/GOMODCACHE)\n%s",
					wserialBinary, err, out)
				return
			}
		}
	})
	if binErr != nil {
		t.Fatalf("%v", binErr)
	}
	return simBinary, wserialBinary
}

// ---------------------------------------------------------------------------
// small concurrency-safe buffers / event streams
// ---------------------------------------------------------------------------

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// tail returns at most the last n bytes of the buffer.
func (b *syncBuffer) tail(n int) string {
	s := b.String()
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}

// stream buffers everything read from r in the background so tests can consume
// it with deadlines (pipes and PTY files do not support robust read deadlines
// on all platforms).
type stream struct {
	mu   sync.Mutex
	buf  []byte
	err  error
	done bool
}

func newStream(r io.Reader) *stream {
	s := &stream{}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			s.mu.Lock()
			if n > 0 {
				s.buf = append(s.buf, buf[:n]...)
			}
			if err != nil {
				s.err = err
				s.done = true
			}
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

// readN waits up to timeout for n buffered bytes.
func (s *stream) readN(t *testing.T, n int, timeout time.Duration, desc string) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		if len(s.buf) >= n {
			out := make([]byte, n)
			copy(out, s.buf[:n])
			s.buf = s.buf[n:]
			s.mu.Unlock()
			return out
		}
		if s.done && len(s.buf) < n {
			got := len(s.buf)
			err := s.err
			s.mu.Unlock()
			t.Fatalf("%s: stream closed after %d/%d bytes (err=%v)", desc, got, n, err)
		}
		have := len(s.buf)
		s.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("%s: timeout after %s waiting for %d bytes (buffered %d)", desc, timeout, n, have)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// polling helpers
// ---------------------------------------------------------------------------

func waitFor(t *testing.T, timeout time.Duration, desc string, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := fn()
		if ok {
			return
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				t.Fatalf("timeout after %s waiting for %s (last error: %v)", timeout, desc, lastErr)
			}
			t.Fatalf("timeout after %s waiting for %s", timeout, desc)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// simulator process
// ---------------------------------------------------------------------------

type simAddrs struct {
	WSP       string `json:"wsp"`
	RAW       string `json:"raw"`
	HW        string `json:"hw"`
	Admin     string `json:"admin"`
	WSPPort   int    `json:"wsp_port"`
	RAWPort   int    `json:"raw_port"`
	HWPort    int    `json:"hw_port"`
	AdminPort int    `json:"admin_port"`
	PID       int    `json:"pid"`
}

type simProc struct {
	t        *testing.T
	cmd      *exec.Cmd
	stdout   syncBuffer
	stderr   syncBuffer
	addrFile string
	addr     simAddrs
	stopped  bool
}

// startSim launches the simulator.  When reuse is non-nil its addresses are
// used verbatim (needed by the reconnect test to bind the same ports again).
func startSim(t *testing.T, simBinary string, reuse *simAddrs) *simProc {
	t.Helper()
	dir := t.TempDir()
	addrFile := filepath.Join(dir, "sim.json")

	wsp, raw, hw, admin := "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
	if reuse != nil {
		wsp, raw, hw, admin = reuse.WSP, reuse.RAW, reuse.HW, reuse.Admin
	}

	cmd := exec.Command(simBinary,
		"-wsp", wsp, "-raw", raw, "-hw", hw, "-admin", admin,
		"-addrfile", addrFile, "-log")
	p := &simProc{t: t, cmd: cmd, addrFile: addrFile}
	cmd.Stdout = &p.stdout
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start simulator: %v", err)
	}
	t.Cleanup(func() { p.stop(t) })

	waitFor(t, 10*time.Second, "simulator addrfile", func() (bool, error) {
		rawFile, err := os.ReadFile(addrFile)
		if err != nil {
			if p.exited() {
				return false, fmt.Errorf("simulator exited early; stderr:\n%s", p.stderr.tail(4000))
			}
			return false, nil
		}
		var a simAddrs
		if err := json.Unmarshal(rawFile, &a); err != nil {
			return false, nil // partially written (unlikely: file is atomic)
		}
		if a.WSP == "" || a.RAW == "" || a.HW == "" || a.Admin == "" {
			return false, nil
		}
		p.addr = a
		return true, nil
	})
	if !p.exited() {
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("simulator stderr:\n%s", p.stderr.tail(8000))
			}
		})
	}
	return p
}

func (p *simProc) exited() bool {
	if p.cmd == nil || p.cmd.ProcessState != nil {
		return true
	}
	// Signal 0 only probes liveness; it does not consume the Wait result.
	return p.cmd.Process.Signal(syscall.Signal(0)) != nil
}

func (p *simProc) stop(t *testing.T) {
	if p.stopped || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	p.stopped = true
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Logf("simulator exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
		t.Errorf("simulator did not exit within 5s of SIGTERM")
	}
}

// ---------------------------------------------------------------------------
// admin status endpoint
// ---------------------------------------------------------------------------

type adminDevice struct {
	DeviceName     string `json:"device_name"`
	Baud           uint32 `json:"baud"`
	DataBits       byte   `json:"data_bits"`
	Parity         byte   `json:"parity"`
	StopBits       byte   `json:"stop_bits"`
	Flow           byte   `json:"flow"`
	LineState      byte   `json:"line_state"`
	SerialClient   string `json:"serial_client"`
	HWConnected    bool   `json:"hw_connected"`
	UptimeS        uint32 `json:"uptime_s"`
	UARTCfgApplied uint32 `json:"uart_cfg_applied"`
	LastReset      struct {
		Action  byte   `json:"action"`
		PulseMS uint16 `json:"pulse_ms"`
		Count   uint32 `json:"count"`
	} `json:"last_reset"`
	Stats struct {
		UARTRXBytes   uint32 `json:"uart_rx_bytes"`
		UARTTXBytes   uint32 `json:"uart_tx_bytes"`
		FramesIn      uint32 `json:"frames_in"`
		FramesOut     uint32 `json:"frames_out"`
		CRCErrors     uint32 `json:"crc_errors"`
		UARTOverflows uint32 `json:"uart_overflows"`
	} `json:"stats"`
	RecentFrames []struct {
		Type byte   `json:"type"`
		Name string `json:"name"`
		Len  int    `json:"len"`
	} `json:"recent_frames"`
}

type adminStatus struct {
	Addrs  simAddrs    `json:"addrs"`
	Device adminDevice `json:"device"`
}

var adminHTTPClient = &http.Client{Timeout: 3 * time.Second}

func fetchStatus(addrs simAddrs) (adminStatus, error) {
	var st adminStatus
	if addrs.Admin == "" {
		return st, errors.New("admin address is empty")
	}
	resp, err := adminHTTPClient.Get("http://" + addrs.Admin + "/status")
	if err != nil {
		return st, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("GET /status: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return st, err
	}
	return st, nil
}

func waitForSerialClient(t *testing.T, addrs simAddrs, want string, timeout time.Duration) adminStatus {
	t.Helper()
	var last adminStatus
	waitFor(t, timeout, "device to report serial_client="+want, func() (bool, error) {
		st, err := fetchStatus(addrs)
		if err != nil {
			return false, err
		}
		last = st
		return st.Device.SerialClient == want, nil
	})
	return last
}

func waitForUARTCfg(t *testing.T, addrs simAddrs, baud uint32, timeout time.Duration) adminStatus {
	t.Helper()
	var last adminStatus
	waitFor(t, timeout, fmt.Sprintf("UART_CFG baud=%d applied", baud), func() (bool, error) {
		st, err := fetchStatus(addrs)
		if err != nil {
			return false, err
		}
		last = st
		return st.Device.UARTCfgApplied > 0 && st.Device.Baud == baud, nil
	})
	return last
}

// hasRecentFrame reports whether the device recorded a frame of the given type.
func hasRecentFrame(st adminStatus, typ byte) bool {
	for _, f := range st.Device.RecentFrames {
		if f.Type == typ {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// hardware-side connection
// ---------------------------------------------------------------------------

func dialHW(t *testing.T, addrs simAddrs) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addrs.HW, 5*time.Second)
	if err != nil {
		t.Fatalf("dial simulator hw side %s: %v", addrs.HW, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readExactConn(t *testing.T, conn net.Conn, n int, timeout time.Duration, desc string) []byte {
	t.Helper()
	buf := make([]byte, n)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("%s: read %d bytes from hardware side: %v", desc, n, err)
	}
	return buf
}

func writeConn(t *testing.T, conn net.Conn, p []byte, timeout time.Duration, desc string) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(p); err != nil {
		t.Fatalf("%s: write %d bytes to hardware side: %v", desc, len(p), err)
	}
}

// ---------------------------------------------------------------------------
// generic writers
// ---------------------------------------------------------------------------

// writeAll writes p completely, bounding the call with a timeout (writes to a
// PTY or pipe can block when the backend stops draining).
func writeAll(t *testing.T, w io.Writer, p []byte, timeout time.Duration, desc string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		for len(p) > 0 {
			n, err := w.Write(p)
			if n > 0 {
				p = p[n:]
			}
			if err != nil {
				done <- err
				return
			}
			if n == 0 {
				done <- io.ErrShortWrite
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: write %d bytes: %v", desc, len(p), err)
		}
	case <-time.After(timeout):
		t.Fatalf("%s: timeout after %s writing %d bytes", desc, timeout, len(p))
	}
}

// ---------------------------------------------------------------------------
// payload helpers
// ---------------------------------------------------------------------------

func pattern(n int, seed uint32) []byte {
	b := make([]byte, n)
	x := seed | 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

// allByteValues4K returns 4096 bytes containing every byte value (0x00..0xFF)
// exactly 16 times, which catches text/encoding assumptions in the data path.
func allByteValues4K() []byte {
	b := make([]byte, 0, 4096)
	for i := 0; i < 16; i++ {
		for v := 0; v < 256; v++ {
			b = append(b, byte(v))
		}
	}
	return b
}

func assertBytes(t *testing.T, got, want []byte, desc string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length = %d, want %d (crc got=%08x want=%08x)",
			desc, len(got), len(want), crc32.ChecksumIEEE(got), crc32.ChecksumIEEE(want))
	}
	if !bytes.Equal(got, want) {
		first := -1
		for i := range got {
			if got[i] != want[i] {
				first = i
				break
			}
		}
		t.Fatalf("%s: payload mismatch at byte %d (crc got=%08x want=%08x)",
			desc, first, crc32.ChecksumIEEE(got), crc32.ChecksumIEEE(want))
	}
}

// ---------------------------------------------------------------------------
// PTY availability
// ---------------------------------------------------------------------------

func ptyProbe() error {
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

// requirePTY skips (or fails, with WSP_REQUIRE_PTY=1) when the environment
// forbids creating PTYs, e.g. inside a file sandbox.
func requirePTY(t *testing.T) {
	t.Helper()
	if err := ptyProbe(); err != nil {
		if os.Getenv("WSP_REQUIRE_PTY") == "1" {
			t.Fatalf("PTY creation is required (WSP_REQUIRE_PTY=1) but unavailable: %v", err)
		}
		t.Skipf("跳过虚拟串口（PTY）测试：当前环境禁止创建 PTY（/dev/ptmx: %v）。"+
			"请在普通终端运行 test/integration.sh；设置 WSP_REQUIRE_PTY=1 可强制失败而非跳过", err)
	}
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

func portOfAddr(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split address %q: %v", addr, err)
	}
	return port
}

func hostOfAddr(t *testing.T, addr string) string {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split address %q: %v", addr, err)
	}
	return host
}

// processAlive reports whether a started command is still running without
// consuming its Wait result.
func processAlive(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	return cmd.Process.Signal(syscall.Signal(0)) == nil
}
