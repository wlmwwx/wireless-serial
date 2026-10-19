package bridge

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	wlog "github.com/wireless-serial/wserial/internal/log"
	"github.com/wireless-serial/wserial/internal/vport"
	"github.com/wireless-serial/wserial/internal/wsp"
)

// ---------------------------------------------------------------------------
// Mock device
// ---------------------------------------------------------------------------

type mockWSP struct {
	t  *testing.T
	ln net.Listener

	mu       sync.Mutex
	conns    []net.Conn
	accepted int
	cfg      *wsp.UartCfg
	pings    int
}

func newMockWSP(t *testing.T) *mockWSP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := &mockWSP{t: t, ln: ln}
	t.Cleanup(func() {
		_ = ln.Close()
		m.closeConns()
	})
	go m.serve()
	return m
}

func (m *mockWSP) port() int { return m.ln.Addr().(*net.TCPAddr).Port }

func (m *mockWSP) serve() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		m.conns = append(m.conns, conn)
		m.accepted++
		m.mu.Unlock()
		go m.handle(conn)
	}
}

func (m *mockWSP) handle(conn net.Conn) {
	defer conn.Close()
	parser := wsp.NewParser()
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			parser.Feed(buf[:n], func(t wsp.FrameType, flags uint8, payload []byte) {
				m.onFrame(conn, t, payload)
			})
		}
		if err != nil {
			return
		}
	}
}

func (m *mockWSP) onFrame(conn net.Conn, t wsp.FrameType, payload []byte) {
	write := func(typ wsp.FrameType, p []byte) {
		b, err := wsp.Encode(typ, 0, p)
		if err != nil {
			m.t.Errorf("mock encode: %v", err)
			return
		}
		if _, err := conn.Write(b); err != nil {
			return
		}
	}
	switch t {
	case wsp.TypeHello:
		write(wsp.TypeHelloAck, deviceInfoPayload())
	case wsp.TypeUartCfg:
		if cfg, err := wsp.ParseUartCfg(payload); err == nil {
			m.mu.Lock()
			m.cfg = &cfg
			m.mu.Unlock()
			write(wsp.TypeUartCfgAck, payload)
		}
	case wsp.TypeData:
		// Echo the payload back: this is the fake target UART.
		write(wsp.TypeData, payload)
	case wsp.TypePing:
		m.mu.Lock()
		m.pings++
		m.mu.Unlock()
		write(wsp.TypePong, payload)
	case wsp.TypeLineCtrl:
		write(wsp.TypeLineStatus, []byte{payload[0], 0})
	case wsp.TypeStats:
		write(wsp.TypeStats, make([]byte, 24))
	}
}

func (m *mockWSP) status() (accepted, pings int, cfg *wsp.UartCfg) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg != nil {
		c := *m.cfg
		cfg = &c
	}
	return m.accepted, m.pings, cfg
}

func (m *mockWSP) closeConns() {
	m.mu.Lock()
	conns := append([]net.Conn(nil), m.conns...)
	m.conns = nil
	m.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// deviceInfoPayload builds a valid 62-byte wsp_dev_info_t.
func deviceInfoPayload() []byte {
	p := make([]byte, 62)
	p[0] = 1
	p[1], p[2], p[3] = 0, 1, 0
	putU32(p[4:], 1024)
	putU32(p[8:], 115200)
	p[12], p[13], p[14], p[15] = 8, 0, 1, 0
	copy(p[16:32], "mock-device")
	copy(p[32:38], []byte{1, 2, 3, 4, 5, 6})
	copy(p[38:46], "esp32c3")
	putU32(p[46:], 42)
	putU16(p[50:], 3333)
	putU16(p[52:], 3334)
	return p
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func putU16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

// rawEchoServer accepts connections and echoes every byte.
type rawEchoServer struct {
	ln net.Listener
}

func newRawEchoServer(t *testing.T) *rawEchoServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &rawEchoServer{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return s
}

func (s *rawEchoServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// ---------------------------------------------------------------------------
// Fake virtual port (used where PTY creation is not permitted)
// ---------------------------------------------------------------------------

type fakePort struct {
	name  string
	local net.Conn
}

func (p *fakePort) Read(b []byte) (int, error)  { return p.local.Read(b) }
func (p *fakePort) Write(b []byte) (int, error) { return p.local.Write(b) }
func (p *fakePort) Close() error                { return p.local.Close() }
func (p *fakePort) Name() string                { return p.name }

// newFakePortPair returns the bridge-side Port and the user-side connection.
func newFakePortPair(name string) (vport.Port, net.Conn) {
	a, b := net.Pipe()
	return &fakePort{name: name, local: a}, b
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func testConfig(port int) Config {
	return Config{
		Host:           "127.0.0.1",
		Port:           port,
		PortSet:        true,
		Device:         "fake-port",
		Baud:           115200,
		DataBits:       8,
		StopBits:       1,
		PingInterval:   150 * time.Millisecond,
		ConnectTimeout: 2 * time.Second,
		BackoffMin:     50 * time.Millisecond,
		BackoffMax:     300 * time.Millisecond,
	}
}

// withFakePort installs an in-memory port and returns the user side.
func withFakePort(t *testing.T, cfg *Config, name string) net.Conn {
	t.Helper()
	port, user := newFakePortPair(name)
	cfg.openPort = func(string, vport.Options) (vport.Port, error) { return port, nil }
	return user
}

// readExact reads n bytes from c or fails after the timeout.
func readExact(t *testing.T, c net.Conn, n int, timeout time.Duration) []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, n)
		total := 0
		for total < n {
			m, err := c.Read(buf[total:])
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

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRunEchoAndReconnect(t *testing.T) {
	srv := newMockWSP(t)

	var logBuf bytes.Buffer
	cfg := testConfig(srv.port())
	cfg.Logger = wlog.New(&logBuf, wlog.LevelDebug)
	user := withFakePort(t, &cfg, "fake-ttyWS0")
	defer user.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, cfg) }()

	// User -> bridge -> WSP DATA -> mock echoes -> bridge -> user.
	const first = "hello-wsp"
	if _, err := user.Write([]byte(first)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(readExact(t, user, len(first), 5*time.Second)); got != first {
		t.Fatalf("echo = %q, want %q", got, first)
	}

	// The requested UART configuration must have been pushed.
	waitFor(t, 3*time.Second, "UART_CFG 下发", func() bool {
		_, _, cfg := srv.status()
		return cfg != nil && cfg.Baud == 115200 && cfg.DataBits == 8 && cfg.StopBits == 1
	})
	// Keepalive PINGs must be flowing.
	waitFor(t, 3*time.Second, "PING 心跳", func() bool {
		_, pings, _ := srv.status()
		return pings > 0
	})

	// Drop the TCP connection: the virtual port must survive and reconnect.
	srv.closeConns()
	waitFor(t, 8*time.Second, "重连", func() bool {
		accepted, _, _ := srv.status()
		return accepted >= 2
	})

	const second = "after-reconnect"
	if _, err := user.Write([]byte(second)); err != nil {
		t.Fatalf("write after reconnect: %v", err)
	}
	if got := string(readExact(t, user, len(second), 5*time.Second)); got != second {
		t.Fatalf("echo after reconnect = %q, want %q", got, second)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v\n日志:\n%s", err, logBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run 未在 5s 内退出\n日志:\n%s", logBuf.String())
	}
}

func TestRunRawEcho(t *testing.T) {
	echo := newRawEchoServer(t)

	var logBuf bytes.Buffer
	cfg := testConfig(echo.port())
	cfg.Raw = true
	cfg.Logger = wlog.New(&logBuf, wlog.LevelDebug)
	user := withFakePort(t, &cfg, "fake-ttyWSRaw")
	defer user.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, cfg) }()

	payload := []byte("raw-mode-0123456789")
	if _, err := user.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readExact(t, user, len(payload), 5*time.Second); !bytes.Equal(got, payload) {
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

func TestRunNoReconnectFails(t *testing.T) {
	// Reserve a port then close it: connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	var logBuf bytes.Buffer
	cfg := testConfig(port)
	cfg.NoReconnect = true
	cfg.ConnectTimeout = 500 * time.Millisecond
	cfg.Logger = wlog.New(&logBuf, wlog.LevelDebug)
	user := withFakePort(t, &cfg, "fake-no-reconnect")
	defer user.Close()

	err = Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run(--no-reconnect) succeeded against a closed port")
	}
}

func TestRunKeepsVirtualPortAcrossReconnects(t *testing.T) {
	srv := newMockWSP(t)

	var logBuf bytes.Buffer
	cfg := testConfig(srv.port())
	cfg.Logger = wlog.New(&logBuf, wlog.LevelDebug)
	user := withFakePort(t, &cfg, "fake-stable-port")
	defer user.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, cfg) }()

	// Wait for first connection, then close it twice in a row.
	waitFor(t, 5*time.Second, "首次连接", func() bool {
		accepted, _, _ := srv.status()
		return accepted >= 1
	})
	for i := 2; i <= 3; i++ {
		srv.closeConns()
		waitFor(t, 8*time.Second, "重连", func() bool {
			accepted, _, _ := srv.status()
			return accepted >= i
		})
		// The local port object must not be recreated or closed: Run owns it
		// for its whole lifetime, so writes still reach the device.
		msg := []byte("tick")
		if _, err := user.Write(msg); err != nil {
			t.Fatalf("write after reconnect %d: %v", i, err)
		}
		if got := readExact(t, user, len(msg), 5*time.Second); !bytes.Equal(got, msg) {
			t.Fatalf("echo after reconnect %d = %q", i, got)
		}
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
}

// ---------------------------------------------------------------------------
// Monitor
// ---------------------------------------------------------------------------

// syncBuffer is a concurrency-safe bytes.Buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestMonitorEcho(t *testing.T) {
	srv := newMockWSP(t)
	out := &syncBuffer{}

	var logBuf bytes.Buffer
	cfg := testConfig(srv.port())
	cfg.Logger = wlog.New(&logBuf, wlog.LevelDebug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monErr := make(chan error, 1)
	go func() {
		monErr <- Monitor(ctx, MonitorConfig{
			Config: cfg,
			In:     bytes.NewReader([]byte("mon-hello")),
			Out:    out,
		})
	}()

	waitFor(t, 5*time.Second, "monitor 回显", func() bool {
		return out.String() == "mon-hello"
	})

	cancel()
	select {
	case err := <-monErr:
		if err != nil {
			t.Fatalf("Monitor returned %v\n日志:\n%s", err, logBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Monitor 未退出\n日志:\n%s", logBuf.String())
	}
}

func TestMonitorHexOutput(t *testing.T) {
	srv := newMockWSP(t)
	out := &syncBuffer{}

	var logBuf bytes.Buffer
	cfg := testConfig(srv.port())
	cfg.Logger = wlog.New(&logBuf, wlog.LevelDebug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	monErr := make(chan error, 1)
	go func() {
		monErr <- Monitor(ctx, MonitorConfig{
			Config: cfg,
			Hex:    true,
			In:     bytes.NewReader([]byte("Hi")),
			Out:    out,
		})
	}()

	waitFor(t, 5*time.Second, "十六进制回显", func() bool {
		s := out.String()
		return bytes.Contains([]byte(s), []byte("TX | 48 69")) &&
			bytes.Contains([]byte(s), []byte("RX | 48 69"))
	})

	cancel()
	select {
	case <-monErr:
	case <-time.After(5 * time.Second):
		t.Fatalf("Monitor 未退出\n日志:\n%s", logBuf.String())
	}
}
