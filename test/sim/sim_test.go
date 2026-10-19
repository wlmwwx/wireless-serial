package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wirelessserial/sim/wire"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testSimConfig() simConfig {
	return simConfig{
		WSPAddr:      "127.0.0.1:0",
		RAWAddr:      "127.0.0.1:0",
		HWAddr:       "127.0.0.1:0",
		AdminAddr:    "127.0.0.1:0",
		Baud:         115200,
		Name:         "wserial-sim",
		HelloTimeout: 2 * time.Second,
		IdleTimeout:  5 * time.Second,
	}
}

func startTestSim(t *testing.T, mutate func(*simConfig)) (*simServer, simAddrs) {
	t.Helper()
	cfg := testSimConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := startSim(cfg)
	if err != nil {
		t.Fatalf("startSim: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s, s.Addrs()
}

type wspClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func dialWSP(t *testing.T, addr string) *wspClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial wsp %s: %v", addr, err)
	}
	return &wspClient{t: t, conn: conn, br: bufio.NewReader(conn)}
}

func (c *wspClient) close() {
	_ = c.conn.Close()
}

func (c *wspClient) send(typ byte, payload []byte) {
	c.t.Helper()
	f := wire.EncodeFrame(typ, 0, payload)
	if f == nil {
		c.t.Fatalf("cannot encode %s frame (%d payload bytes)", wire.TypeName(typ), len(payload))
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.conn.Write(f); err != nil {
		c.t.Fatalf("write %s: %v", wire.TypeName(typ), err)
	}
}

func (c *wspClient) sendRaw(b []byte) {
	c.t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("write raw: %v", err)
	}
}

func (c *wspClient) recv(timeout time.Duration) (wire.Frame, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	return wire.ReadFrame(c.br)
}

func (c *wspClient) mustRecv(typ byte, timeout time.Duration) wire.Frame {
	c.t.Helper()
	fr, err := c.recv(timeout)
	if err != nil {
		c.t.Fatalf("recv %s: %v", wire.TypeName(typ), err)
	}
	if fr.Type != typ {
		c.t.Fatalf("recv %s, want %s (payload %X)", wire.TypeName(fr.Type), wire.TypeName(typ), fr.Payload)
	}
	return fr
}

func (c *wspClient) handshake() {
	c.t.Helper()
	c.send(wire.TypeHello, []byte{wire.Version, 0, 0, 4})
	fr := c.mustRecv(wire.TypeHelloAck, 2*time.Second)
	if len(fr.Payload) != 62 {
		c.t.Fatalf("HELLO_ACK payload = %d bytes, want 62", len(fr.Payload))
	}
}

// expectErrorFrame reads one frame and asserts it is ERROR with code.
func (c *wspClient) expectError(code byte) {
	c.t.Helper()
	fr := c.mustRecv(wire.TypeError, 2*time.Second)
	if len(fr.Payload) < 1 {
		c.t.Fatalf("ERROR frame has empty payload")
	}
	if fr.Payload[0] != code {
		c.t.Fatalf("ERROR code = %s (%d), want %s (%d), msg=%q",
			wire.ErrName(fr.Payload[0]), fr.Payload[0], wire.ErrName(code), code, fr.Payload[1:])
	}
}

// expectClosed asserts the server closes the connection (no more frames).
func (c *wspClient) expectClosed() {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := wire.ReadFrame(c.br)
	if err == nil {
		c.t.Fatal("expected the server to close the connection, got another frame")
	}
	var we *wire.WireError
	if errors.As(err, &we) {
		c.t.Fatalf("expected connection close, got wire error: %v", err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		c.t.Fatalf("expected connection close, got read timeout")
	}
}

func dialHW(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial hw %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readN(t *testing.T, conn net.Conn, n int, timeout time.Duration) []byte {
	t.Helper()
	buf := make([]byte, n)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes from %s: %v", n, conn.RemoteAddr(), err)
	}
	return buf
}

// readDataFrames reads DATA frames until total bytes have been received.
func readDataFrames(t *testing.T, c *wspClient, total int, timeout time.Duration) []byte {
	t.Helper()
	out := make([]byte, 0, total)
	deadline := time.Now().Add(timeout)
	for len(out) < total {
		remain := time.Until(deadline)
		if remain <= 0 {
			t.Fatalf("timeout waiting for %d bytes of DATA (got %d)", total, len(out))
		}
		fr, err := c.recv(remain)
		if err != nil {
			t.Fatalf("recv DATA after %d/%d bytes: %v", len(out), total, err)
		}
		if fr.Type != wire.TypeData {
			t.Fatalf("expected DATA, got %s", wire.TypeName(fr.Type))
		}
		out = append(out, fr.Payload...)
	}
	return out
}

func cfgBytes(baud uint32, dataBits, parity, stopBits, flow byte) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:4], baud)
	b[4], b[5], b[6], b[7] = dataBits, parity, stopBits, flow
	return b
}

func decodeStats(t *testing.T, fr wire.Frame) devStats {
	t.Helper()
	if fr.Type != wire.TypeStats {
		t.Fatalf("expected STATS, got %s", wire.TypeName(fr.Type))
	}
	if len(fr.Payload) != 24 {
		t.Fatalf("STATS payload = %d bytes, want 24", len(fr.Payload))
	}
	b := fr.Payload
	return devStats{
		uartRX:       binary.LittleEndian.Uint32(b[0:4]),
		uartTX:       binary.LittleEndian.Uint32(b[4:8]),
		framesIn:     binary.LittleEndian.Uint32(b[8:12]),
		framesOut:    binary.LittleEndian.Uint32(b[12:16]),
		crcErrors:    binary.LittleEndian.Uint32(b[16:20]),
		uartOverflow: binary.LittleEndian.Uint32(b[20:24]),
	}
}

type adminResponse struct {
	Addrs  simAddrs     `json:"addrs"`
	Device deviceStatus `json:"device"`
}

func getAdminStatus(t *testing.T, addrs simAddrs) adminResponse {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addrs.Admin + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /status: status %d", resp.StatusCode)
	}
	var out adminResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	return out
}

// reconnectWSP retries dial+handshake until the previous serial client has
// been fully released (the protocol allows a new client within 1 second).
func reconnectWSP(t *testing.T, addr string) *wspClient {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c := dialWSP(t, addr)
		c.send(wire.TypeHello, []byte{wire.Version, 0, 0, 4})
		fr, err := c.recv(500 * time.Millisecond)
		if err == nil && fr.Type == wire.TypeHelloAck {
			return c
		}
		c.close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("device did not accept a new WSP client after the previous one closed")
	return nil
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	v := uint32(seed)<<24 | 0x123456
	for i := range b {
		v = v*1664525 + 1013904223
		b[i] = byte(v >> 24)
	}
	return b
}

// ---------------------------------------------------------------------------
// handshake / device info
// ---------------------------------------------------------------------------

func TestHandshakeInfoAndPing(t *testing.T) {
	_, addrs := startTestSim(t, func(c *simConfig) { c.Baud = 9600 })
	c := dialWSP(t, addrs.WSP)
	defer c.close()

	c.send(wire.TypeHello, []byte{wire.Version, 0, 0, 4})
	fr := c.mustRecv(wire.TypeHelloAck, 2*time.Second)
	p := fr.Payload
	if p[0] != wire.Version {
		t.Fatalf("proto_ver = %d, want %d", p[0], wire.Version)
	}
	if got := binary.LittleEndian.Uint32(p[4:8]); got != wire.MaxPayload {
		t.Fatalf("max_payload = %d, want %d", got, wire.MaxPayload)
	}
	if got := binary.LittleEndian.Uint32(p[8:12]); got != 9600 {
		t.Fatalf("initial baud = %d, want 9600", got)
	}
	if p[12] != 8 || p[13] != 0 || p[14] != 1 || p[15] != 0 {
		t.Fatalf("initial format = %d%c%d flow=%d, want 8N1 flow=0", p[12], "NOE"[p[13]], p[14], p[15])
	}
	if got := string(bytes.TrimRight(p[38:46], "\x00")); got != "esp32c3" {
		t.Fatalf("chip = %q, want esp32c3", got)
	}
	if got := binary.LittleEndian.Uint16(p[50:52]); int(got) != addrs.WSPPort {
		t.Fatalf("wsp_port = %d, want %d", got, addrs.WSPPort)
	}
	if got := binary.LittleEndian.Uint16(p[52:54]); int(got) != addrs.RAWPort {
		t.Fatalf("raw_port = %d, want %d", got, addrs.RAWPort)
	}

	// INFO returns the same device info layout.
	c.send(wire.TypeInfo, nil)
	if got := c.mustRecv(wire.TypeInfo, 2*time.Second); len(got.Payload) != 62 {
		t.Fatalf("INFO payload = %d bytes, want 62", len(got.Payload))
	}

	// PING echo, including the empty and 64-byte boundaries.
	for _, payload := range [][]byte{{}, []byte{1, 2, 3, 4}, pattern(64, 0x5A)} {
		c.send(wire.TypePing, payload)
		pong := c.mustRecv(wire.TypePong, 2*time.Second)
		if !bytes.Equal(pong.Payload, payload) {
			t.Fatalf("PONG payload = %X, want %X", pong.Payload, payload)
		}
	}

	// HELLO after the handshake is answered again (lenient re-handshake).
	c.send(wire.TypeHello, []byte{wire.Version, 0, 0, 4})
	c.mustRecv(wire.TypeHelloAck, 2*time.Second)
}

// ---------------------------------------------------------------------------
// UART data path
// ---------------------------------------------------------------------------

func TestDataBidirectional(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	hw := dialHW(t, addrs.HW)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	// host -> target
	msg := []byte("hello-hardware")
	c.send(wire.TypeData, msg)
	if got := readN(t, hw, len(msg), 2*time.Second); !bytes.Equal(got, msg) {
		t.Fatalf("target received %q, want %q", got, msg)
	}

	// binary safe host -> target, exactly one full 1024-byte DATA payload.
	bin := make([]byte, wire.MaxPayload)
	for i := range bin {
		bin[i] = byte(i)
	}
	c.send(wire.TypeData, bin)
	if got := readN(t, hw, len(bin), 2*time.Second); !bytes.Equal(got, bin) {
		t.Fatalf("target received binary payload mismatch")
	}

	// target -> host
	back := []byte("hello-host")
	if _, err := hw.Write(back); err != nil {
		t.Fatalf("hw write: %v", err)
	}
	fr := c.mustRecv(wire.TypeData, 2*time.Second)
	if !bytes.Equal(fr.Payload, back) {
		t.Fatalf("host received %q, want %q", fr.Payload, back)
	}

	// target -> host 64 KiB, split into 1024-byte DATA frames by the simulator.
	big := pattern(64<<10, 0xA5)
	if _, err := hw.Write(big); err != nil {
		t.Fatalf("hw big write: %v", err)
	}
	if got := readDataFrames(t, c, len(big), 10*time.Second); !bytes.Equal(got, big) {
		t.Fatalf("64 KiB target->host payload mismatch")
	}

	// host -> target 64 KiB.
	for _, chunk := range wire.Chunk(big) {
		c.send(wire.TypeData, chunk)
	}
	if got := readN(t, hw, len(big), 10*time.Second); !bytes.Equal(got, big) {
		t.Fatalf("64 KiB host->target payload mismatch")
	}
}

func TestDataBeforeHWAttachedIsBuffered(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	// The target hardware side connects after the client already wrote data:
	// the simulator buffers bytes until the hardware peer shows up.
	c.send(wire.TypeData, []byte("early-bytes"))
	hw := dialHW(t, addrs.HW)
	if got := readN(t, hw, len("early-bytes"), 2*time.Second); string(got) != "early-bytes" {
		t.Fatalf("buffered bytes = %q", got)
	}
}

// ---------------------------------------------------------------------------
// UART_CFG
// ---------------------------------------------------------------------------

func TestUARTCfg(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	valid := cfgBytes(9600, 7, 2, 2, 1)
	c.send(wire.TypeUARTCfg, valid)
	ack := c.mustRecv(wire.TypeUARTCfgAck, 2*time.Second)
	if !bytes.Equal(ack.Payload, valid) {
		t.Fatalf("UART_CFG_ACK = %X, want %X", ack.Payload, valid)
	}

	// Device info must now report the effective configuration.
	c.send(wire.TypeInfo, nil)
	info := c.mustRecv(wire.TypeInfo, 2*time.Second).Payload
	if got := binary.LittleEndian.Uint32(info[8:12]); got != 9600 {
		t.Fatalf("INFO baud = %d, want 9600", got)
	}
	if info[12] != 7 || info[13] != 2 || info[14] != 2 || info[15] != 1 {
		t.Fatalf("INFO format = %d/%d/%d/%d, want 7/2/2/1", info[12], info[13], info[14], info[15])
	}

	// Invalid configurations are rejected, keep the old config and the
	// connection stays usable.
	invalid := [][]byte{
		cfgBytes(0, 8, 0, 1, 0),
		cfgBytes(5000001, 8, 0, 1, 0),
		cfgBytes(9600, 9, 0, 1, 0),
		cfgBytes(9600, 4, 0, 1, 0),
		cfgBytes(9600, 8, 3, 1, 0),
		cfgBytes(9600, 8, 0, 0, 0),
		cfgBytes(9600, 8, 0, 3, 0),
		cfgBytes(9600, 8, 0, 1, 2),
	}
	for i, cfg := range invalid {
		c.send(wire.TypeUARTCfg, cfg)
		fr := c.mustRecv(wire.TypeError, 2*time.Second)
		if fr.Payload[0] != wire.ErrInvalidArg {
			t.Fatalf("invalid cfg #%d: ERROR %s, want INVALID_ARG", i, wire.ErrName(fr.Payload[0]))
		}
	}
	c.send(wire.TypeInfo, nil)
	info = c.mustRecv(wire.TypeInfo, 2*time.Second).Payload
	if got := binary.LittleEndian.Uint32(info[8:12]); got != 9600 {
		t.Fatalf("baud after rejected configs = %d, want 9600", got)
	}

	// Wrong payload length.
	c.send(wire.TypeUARTCfg, cfgBytes(115200, 8, 0, 1, 0)[:7])
	if fr := c.mustRecv(wire.TypeError, 2*time.Second); fr.Payload[0] != wire.ErrBadLength {
		t.Fatalf("short UART_CFG: ERROR %s, want BAD_LENGTH", wire.ErrName(fr.Payload[0]))
	}
	// Connection still alive.
	c.send(wire.TypePing, []byte{9})
	c.mustRecv(wire.TypePong, 2*time.Second)

	// Boundary values are accepted.
	for _, baud := range []uint32{50, 5000000} {
		cfg := cfgBytes(baud, 8, 0, 1, 0)
		c.send(wire.TypeUARTCfg, cfg)
		ack = c.mustRecv(wire.TypeUARTCfgAck, 2*time.Second)
		if !bytes.Equal(ack.Payload, cfg) {
			t.Fatalf("baud %d ACK mismatch: %X", baud, ack.Payload)
		}
	}
}

// ---------------------------------------------------------------------------
// LINE_CTRL / LINE_STATUS
// ---------------------------------------------------------------------------

func TestLineControlStatus(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	cases := []struct{ in, want byte }{
		{0x00, 0x00},
		{0x01, wire.StatusDTR},
		{0x02, wire.StatusRTS},
		{0x04, wire.StatusBREAK},
		{0x07, wire.StatusDTR | wire.StatusRTS | wire.StatusBREAK},
		{0x08, 0x00}, // reserved bit ignored
	}
	for _, tc := range cases {
		c.send(wire.TypeLineCtrl, []byte{tc.in})
		fr := c.mustRecv(wire.TypeLineStatus, 2*time.Second)
		if len(fr.Payload) != 2 {
			t.Fatalf("LINE_STATUS payload = %d bytes, want 2", len(fr.Payload))
		}
		if fr.Payload[0] != tc.want {
			t.Fatalf("LINE_CTRL 0x%02X -> state 0x%02X, want 0x%02X", tc.in, fr.Payload[0], tc.want)
		}
		if fr.Payload[1] != 0 {
			t.Fatalf("LINE_STATUS reserved byte = %d, want 0", fr.Payload[1])
		}
	}

	c.send(wire.TypeLineCtrl, []byte{0x01, 0x02})
	if fr := c.mustRecv(wire.TypeError, 2*time.Second); fr.Payload[0] != wire.ErrBadLength {
		t.Fatalf("bad LINE_CTRL: ERROR %s, want BAD_LENGTH", wire.ErrName(fr.Payload[0]))
	}
}

// ---------------------------------------------------------------------------
// STATS / unsupported frames
// ---------------------------------------------------------------------------

func TestStatsAndErrors(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	hw := dialHW(t, addrs.HW)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	down := []byte("0123456789")
	c.send(wire.TypeData, down)
	readN(t, hw, len(down), 2*time.Second)

	up := []byte("abcde")
	if _, err := hw.Write(up); err != nil {
		t.Fatalf("hw write: %v", err)
	}
	if fr := c.mustRecv(wire.TypeData, 2*time.Second); !bytes.Equal(fr.Payload, up) {
		t.Fatalf("upstream payload = %q, want %q", fr.Payload, up)
	}

	c.send(wire.TypeStats, nil)
	st := decodeStats(t, c.mustRecv(wire.TypeStats, 2*time.Second))
	if st.uartTX != uint32(len(down)) {
		t.Fatalf("uart_tx_bytes = %d, want %d", st.uartTX, len(down))
	}
	if st.uartRX != uint32(len(up)) {
		t.Fatalf("uart_rx_bytes = %d, want %d", st.uartRX, len(up))
	}
	if st.crcErrors != 0 {
		t.Fatalf("crc_errors = %d, want 0", st.crcErrors)
	}

	// Unknown frame type -> ERROR(UNSUPPORTED), connection remains open.
	c.send(0xD, nil)
	if fr := c.mustRecv(wire.TypeError, 2*time.Second); fr.Payload[0] != wire.ErrUnsupported {
		t.Fatalf("unknown type: ERROR %s, want UNSUPPORTED", wire.ErrName(fr.Payload[0]))
	}
	c.send(wire.TypePing, []byte{7})
	c.mustRecv(wire.TypePong, 2*time.Second)

	// PING payload > 64 -> INVALID_ARG; empty DATA -> INVALID_ARG.
	c.send(wire.TypePing, pattern(65, 1))
	if fr := c.mustRecv(wire.TypeError, 2*time.Second); fr.Payload[0] != wire.ErrInvalidArg {
		t.Fatalf("long PING: ERROR %s, want INVALID_ARG", wire.ErrName(fr.Payload[0]))
	}
	c.send(wire.TypeData, nil)
	if fr := c.mustRecv(wire.TypeError, 2*time.Second); fr.Payload[0] != wire.ErrInvalidArg {
		t.Fatalf("empty DATA: ERROR %s, want INVALID_ARG", wire.ErrName(fr.Payload[0]))
	}
}

// ---------------------------------------------------------------------------
// RESET_TARGET / INFO / admin endpoint
// ---------------------------------------------------------------------------

func TestResetTargetAndAdminStatus(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	cfg := cfgBytes(9600, 8, 1, 1, 0)
	c.send(wire.TypeUARTCfg, cfg)
	c.mustRecv(wire.TypeUARTCfgAck, 2*time.Second)

	c.send(wire.TypeResetTarget, []byte{2, 0, 120, 0})
	// PING is processed after RESET_TARGET, proving ordering.
	c.send(wire.TypePing, []byte{0x42})
	c.mustRecv(wire.TypePong, 2*time.Second)

	status := getAdminStatus(t, addrs)
	if status.Device.Baud != 9600 {
		t.Fatalf("admin baud = %d, want 9600", status.Device.Baud)
	}
	if status.Device.DataBits != 8 || status.Device.Parity != 1 || status.Device.StopBits != 1 || status.Device.Flow != 0 {
		t.Fatalf("admin format = %d/%d/%d/%d", status.Device.DataBits, status.Device.Parity,
			status.Device.StopBits, status.Device.Flow)
	}
	if status.Device.UARTCfgApplied != 1 {
		t.Fatalf("uart_cfg_applied = %d, want 1", status.Device.UARTCfgApplied)
	}
	if status.Device.LastReset.Action != 2 || status.Device.LastReset.PulseMS != 120 ||
		status.Device.LastReset.Count != 1 {
		t.Fatalf("last_reset = %+v, want action=2 pulse=120 count=1", status.Device.LastReset)
	}
	if status.Device.SerialClient != "wsp" {
		t.Fatalf("serial_client = %q, want wsp", status.Device.SerialClient)
	}
	if status.Addrs.WSPPort != addrs.WSPPort || status.Addrs.HWPort != addrs.HWPort {
		t.Fatalf("admin addrs = %+v, want %+v", status.Addrs, addrs)
	}
	foundCfg := false
	for _, f := range status.Device.RecentFrames {
		if f.Type == wire.TypeUARTCfg {
			foundCfg = true
		}
	}
	if !foundCfg {
		t.Fatalf("recent_frames does not contain UART_CFG: %+v", status.Device.RecentFrames)
	}

	// Invalid reset commands.
	c.send(wire.TypeResetTarget, []byte{9, 0, 120, 0})
	if fr := c.mustRecv(wire.TypeError, 2*time.Second); fr.Payload[0] != wire.ErrInvalidArg {
		t.Fatalf("bad reset action: ERROR %s, want INVALID_ARG", wire.ErrName(fr.Payload[0]))
	}
	c.send(wire.TypeResetTarget, []byte{2, 0, 0, 0})
	if fr := c.mustRecv(wire.TypeError, 2*time.Second); fr.Payload[0] != wire.ErrInvalidArg {
		t.Fatalf("bad reset pulse: ERROR %s, want INVALID_ARG", wire.ErrName(fr.Payload[0]))
	}
	c.send(wire.TypeResetTarget, []byte{0, 0, 0})
	if fr := c.mustRecv(wire.TypeError, 2*time.Second); fr.Payload[0] != wire.ErrBadLength {
		t.Fatalf("short reset: ERROR %s, want BAD_LENGTH", wire.ErrName(fr.Payload[0]))
	}

	// admin health endpoint.
	resp, err := http.Get("http://" + addrs.Admin + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// handshake error paths
// ---------------------------------------------------------------------------

func TestNotHelloClosesConnection(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c := dialWSP(t, addrs.WSP)
	defer c.close()

	c.send(wire.TypeData, []byte("x"))
	c.expectError(wire.ErrNotHello)
	c.expectClosed()
}

func TestHelloTimeout(t *testing.T) {
	_, addrs := startTestSim(t, func(c *simConfig) { c.HelloTimeout = 150 * time.Millisecond })
	c := dialWSP(t, addrs.WSP)
	defer c.close()

	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := c.conn.Read(buf); err == nil {
		t.Fatal("expected the server to close the connection after the HELLO timeout")
	}
}

func TestHelloProtoVersionMismatch(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c := dialWSP(t, addrs.WSP)
	defer c.close()

	c.send(wire.TypeHello, []byte{2, 0, 0, 4})
	c.expectError(wire.ErrBadVersion)
	c.expectClosed()
}

func TestBadVersionFrame(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	bad := wire.EncodeFrame(wire.TypePing, 0, []byte{1})
	bad[2] = (2 << 4) | (bad[2] & 0x0F)
	c.sendRaw(bad)
	c.expectError(wire.ErrBadVersion)
	c.expectClosed()
}

func TestBadCrcRecovers(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	bad := wire.EncodeFrame(wire.TypePing, 0, []byte{1, 2, 3})
	bad[len(bad)-1] ^= 0xFF
	c.sendRaw(bad)
	c.expectError(wire.ErrBadCRC)

	// The session survives and the CRC error is counted.
	c.send(wire.TypePing, []byte{0xAA})
	c.mustRecv(wire.TypePong, 2*time.Second)
	c.send(wire.TypeStats, nil)
	st := decodeStats(t, c.mustRecv(wire.TypeStats, 2*time.Second))
	if st.crcErrors != 1 {
		t.Fatalf("crc_errors = %d, want 1", st.crcErrors)
	}
}

// ---------------------------------------------------------------------------
// single-client exclusivity
// ---------------------------------------------------------------------------

func TestBusyWSP(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	c1 := dialWSP(t, addrs.WSP)
	defer c1.close()
	c1.handshake()

	// Second WSP client: ERROR(BUSY) then close.
	c2 := dialWSP(t, addrs.WSP)
	defer c2.close()
	c2.expectError(wire.ErrBusy)
	c2.expectClosed()

	// RAW client while WSP is active: RAW has no framing, so the simulator
	// just closes the connection.
	raw, err := net.DialTimeout("tcp", addrs.RAW, 2*time.Second)
	if err != nil {
		t.Fatalf("dial raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := raw.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("expected RAW connection to be closed while WSP client is active (n=%d, err=%v)", n, err)
	}

	// After the first client leaves, a new one is accepted (within 1 s).
	c1.close()
	c3 := reconnectWSP(t, addrs.WSP)
	defer c3.close()
}

func TestBusyWSPWhileRawActive(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	raw, err := net.DialTimeout("tcp", addrs.RAW, 2*time.Second)
	if err != nil {
		t.Fatalf("dial raw: %v", err)
	}
	defer func() { _ = raw.Close() }()

	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.expectError(wire.ErrBusy)
	c.expectClosed()
}

// ---------------------------------------------------------------------------
// RAW mode
// ---------------------------------------------------------------------------

func TestRawPassthrough(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	hw := dialHW(t, addrs.HW)
	raw, err := net.DialTimeout("tcp", addrs.RAW, 2*time.Second)
	if err != nil {
		t.Fatalf("dial raw: %v", err)
	}
	defer func() { _ = raw.Close() }()

	// raw -> target
	msg := []byte("raw-to-hw")
	if _, err := raw.Write(msg); err != nil {
		t.Fatalf("raw write: %v", err)
	}
	if got := readN(t, hw, len(msg), 2*time.Second); !bytes.Equal(got, msg) {
		t.Fatalf("target received %q, want %q", got, msg)
	}

	// target -> raw
	back := []byte("hw-to-raw")
	if _, err := hw.Write(back); err != nil {
		t.Fatalf("hw write: %v", err)
	}
	if got := readN(t, raw, len(back), 2*time.Second); !bytes.Equal(got, back) {
		t.Fatalf("raw received %q, want %q", got, back)
	}

	// binary safety, both directions
	bin := pattern(4096, 0x3C)
	if _, err := raw.Write(bin); err != nil {
		t.Fatalf("raw binary write: %v", err)
	}
	if got := readN(t, hw, len(bin), 5*time.Second); !bytes.Equal(got, bin) {
		t.Fatal("RAW host->target binary mismatch")
	}
	if _, err := hw.Write(bin); err != nil {
		t.Fatalf("hw binary write: %v", err)
	}
	if got := readN(t, raw, len(bin), 5*time.Second); !bytes.Equal(got, bin) {
		t.Fatal("RAW target->host binary mismatch")
	}
}

// ---------------------------------------------------------------------------
// latency / drop injection
// ---------------------------------------------------------------------------

func TestLatencyInjection(t *testing.T) {
	_, addrs := startTestSim(t, func(c *simConfig) { c.Latency = 80 * time.Millisecond })
	hw := dialHW(t, addrs.HW)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	start := time.Now()
	if _, err := hw.Write([]byte("delayed")); err != nil {
		t.Fatalf("hw write: %v", err)
	}
	fr := c.mustRecv(wire.TypeData, 2*time.Second)
	elapsed := time.Since(start)
	if string(fr.Payload) != "delayed" {
		t.Fatalf("payload = %q", fr.Payload)
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("latency injection did not delay data (elapsed %v)", elapsed)
	}
}

func TestDropInjection(t *testing.T) {
	_, addrs := startTestSim(t, func(c *simConfig) { c.Drop = 1.0 })
	hw := dialHW(t, addrs.HW)
	c := dialWSP(t, addrs.WSP)
	defer c.close()
	c.handshake()

	if _, err := hw.Write([]byte("lost-bytes")); err != nil {
		t.Fatalf("hw write: %v", err)
	}
	if _, err := c.recv(300 * time.Millisecond); err == nil {
		t.Fatal("expected the injected drop to swallow the DATA frame")
	} else {
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("expected a read timeout after the drop, got %v", err)
		}
	}

	c.send(wire.TypeStats, nil)
	st := decodeStats(t, c.mustRecv(wire.TypeStats, 2*time.Second))
	if st.uartOverflow < uint32(len("lost-bytes")) {
		t.Fatalf("uart_overflows = %d, want >= %d", st.uartOverflow, len("lost-bytes"))
	}
}

// ---------------------------------------------------------------------------
// address discovery contract (used by the integration tests)
// ---------------------------------------------------------------------------

func TestAddrFileAndReadyLine(t *testing.T) {
	s, addrs := startTestSim(t, nil)

	path := filepath.Join(t.TempDir(), "sim.json")
	if err := s.WriteAddrFile(path); err != nil {
		t.Fatalf("WriteAddrFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read addrfile: %v", err)
	}
	var fromFile simAddrs
	if err := json.Unmarshal(raw, &fromFile); err != nil {
		t.Fatalf("parse addrfile %q: %v", raw, err)
	}
	if fromFile.WSP != addrs.WSP || fromFile.RAW != addrs.RAW || fromFile.HW != addrs.HW ||
		fromFile.Admin != addrs.Admin || fromFile.WSPPort != addrs.WSPPort ||
		fromFile.RAWPort != addrs.RAWPort || fromFile.HWPort != addrs.HWPort {
		t.Fatalf("addrfile = %+v, want %+v", fromFile, addrs)
	}

	line := s.ReadyLine()
	const prefix = "SIM_READY "
	if len(line) <= len(prefix) || line[:len(prefix)] != prefix {
		t.Fatalf("ReadyLine = %q", line)
	}
	var fromLine simAddrs
	if err := json.Unmarshal([]byte(line[len(prefix):]), &fromLine); err != nil {
		t.Fatalf("parse ready line: %v", err)
	}
	if fromLine.WSPPort != addrs.WSPPort || fromLine.HWPort != addrs.HWPort {
		t.Fatalf("ready line = %+v, want %+v", fromLine, addrs)
	}

	// The advertised WSP port really accepts a handshake.
	c := dialWSP(t, fromFile.WSP)
	defer c.close()
	c.handshake()
}

// ---------------------------------------------------------------------------
// connection churn / cleanup
// ---------------------------------------------------------------------------

func TestConnectionChurn(t *testing.T) {
	_, addrs := startTestSim(t, nil)
	// Close and immediately reconnect several times.  Slot release is
	// asynchronous (the server notices the peer EOF on its next read), so
	// reconnectWSP retries briefly, as the protocol allows (new client within
	// one second after the previous one disconnects).
	for i := 0; i < 5; i++ {
		c := reconnectWSP(t, addrs.WSP)
		c.send(wire.TypePing, []byte{byte(i)})
		c.mustRecv(wire.TypePong, 2*time.Second)
		c.close()
	}
	c := reconnectWSP(t, addrs.WSP)
	defer c.close()
	c.send(wire.TypePing, []byte{0xFF})
	c.mustRecv(wire.TypePong, 2*time.Second)
}
