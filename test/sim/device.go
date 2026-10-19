package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"

	"wirelessserial/sim/wire"
)

// Device-side UART configuration (wsp_uart_cfg_t on the wire).
type uartCfg struct {
	baud     uint32
	dataBits byte
	parity   byte
	stopBits byte
	flow     byte
}

func (c uartCfg) validate() error {
	if c.baud < 50 || c.baud > 5000000 {
		return fmt.Errorf("baud %d out of range 50..5000000", c.baud)
	}
	if c.dataBits < 5 || c.dataBits > 8 {
		return fmt.Errorf("data_bits %d out of range 5..8", c.dataBits)
	}
	if c.parity > 2 {
		return fmt.Errorf("parity %d out of range 0..2", c.parity)
	}
	if c.stopBits < 1 || c.stopBits > 2 {
		return fmt.Errorf("stop_bits %d out of range 1..2", c.stopBits)
	}
	if c.flow > 1 {
		return fmt.Errorf("flow %d out of range 0..1", c.flow)
	}
	return nil
}

func (c uartCfg) marshal() []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:4], c.baud)
	b[4] = c.dataBits
	b[5] = c.parity
	b[6] = c.stopBits
	b[7] = c.flow
	return b
}

func parseUARTCfg(b []byte) uartCfg {
	return uartCfg{
		baud:     binary.LittleEndian.Uint32(b[0:4]),
		dataBits: b[4],
		parity:   b[5],
		stopBits: b[6],
		flow:     b[7],
	}
}

// Protocol statistics (wsp_stats_t on the wire).
type devStats struct {
	uartRX       uint32
	uartTX       uint32
	framesIn     uint32
	framesOut    uint32
	crcErrors    uint32
	uartOverflow uint32
}

func (s devStats) marshal() []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint32(b[0:4], s.uartRX)
	binary.LittleEndian.PutUint32(b[4:8], s.uartTX)
	binary.LittleEndian.PutUint32(b[8:12], s.framesIn)
	binary.LittleEndian.PutUint32(b[12:16], s.framesOut)
	binary.LittleEndian.PutUint32(b[16:20], s.crcErrors)
	binary.LittleEndian.PutUint32(b[20:24], s.uartOverflow)
	return b
}

type serialKind int

const (
	serialNone serialKind = iota
	serialWSP
	serialRAW
)

func (k serialKind) String() string {
	switch k {
	case serialWSP:
		return "wsp"
	case serialRAW:
		return "raw"
	default:
		return ""
	}
}

var (
	errDeviceBusy   = errors.New("device busy")
	errDeviceClosed = errors.New("device closed")
)

// serialClient is the single active serial client (WSP or RAW; they are
// mutually exclusive because the target UART is an exclusive resource).
type serialClient struct {
	kind serialKind
	conn net.Conn

	wmu sync.Mutex
}

const writeTimeout = 5 * time.Second

func (c *serialClient) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.conn.Write(b)
	return err
}

// hwConn is the TCP connection on which the test process plays the target
// hardware: bytes read from it are "received from the target UART", bytes
// written to it are "sent to the target UART".
type hwConn struct {
	conn net.Conn

	wmu sync.Mutex
}

func (h *hwConn) write(b []byte) error {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	_ = h.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := h.conn.Write(b)
	return err
}

// maxPendingHW bounds the buffer used while no hardware-side client is
// attached; older bytes beyond this cap are dropped and counted.
const maxPendingHW = 1 << 20

type frameLog struct {
	Type byte   `json:"type"`
	Name string `json:"name"`
	Len  int    `json:"len"`
}

// device is the simulated WSP/1 device: protocol state machine + UART bridge.
type device struct {
	mu   sync.Mutex
	cfg  uartCfg
	name string
	mac  [6]byte
	chip string

	start time.Time

	wspPort uint16
	rawPort uint16

	lineState byte
	st        devStats

	serial    *serialClient
	hw        *hwConn
	hwPending []byte

	latency time.Duration
	drop    float64

	uartCfgCount uint32
	lastReset    struct {
		action  byte
		pulseMS uint16
		count   uint32
	}

	framesLog []frameLog

	closed bool
	logf   func(format string, args ...interface{})
}

func newDevice(cfg uartCfg, name string, latency time.Duration, drop float64,
	logf func(string, ...interface{})) *device {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	d := &device{
		cfg:     cfg,
		name:    name,
		chip:    "esp32c3",
		start:   time.Now(),
		latency: latency,
		drop:    drop,
		logf:    logf,
	}
	copy(d.mac[:], []byte{0x02, 0x00, 0x00, 0x12, 0x34, 0x56})
	return d
}

func (d *device) log(format string, args ...interface{}) {
	if d.logf != nil {
		d.logf(format, args...)
	}
}

func (d *device) setPorts(wspPort, rawPort uint16) {
	d.mu.Lock()
	d.wspPort = wspPort
	d.rawPort = rawPort
	d.mu.Unlock()
}

func (d *device) setSerial(c *serialClient) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errDeviceClosed
	}
	if d.serial != nil {
		return errDeviceBusy
	}
	d.serial = c
	return nil
}

func (d *device) clearSerial(c *serialClient) {
	d.mu.Lock()
	if d.serial == c {
		d.serial = nil
	}
	d.mu.Unlock()
}

func (d *device) serialKind() serialKind {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.serial == nil {
		return serialNone
	}
	return d.serial.kind
}

func (d *device) attachHW(h *hwConn) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return errDeviceClosed
	}
	old := d.hw
	pending := d.hwPending
	d.hwPending = nil
	d.hw = h
	d.mu.Unlock()

	if old != nil {
		_ = old.conn.Close()
	}
	if len(pending) != 0 {
		d.log("hw: flushing %d buffered byte(s) to target", len(pending))
		if err := h.write(pending); err != nil {
			return err
		}
	}
	d.log("hw: target-hardware side connected (%s)", h.conn.RemoteAddr())
	return nil
}

func (d *device) detachHW(h *hwConn) {
	d.mu.Lock()
	if d.hw == h {
		d.hw = nil
	}
	d.mu.Unlock()
	_ = h.conn.Close()
	d.log("hw: target-hardware side disconnected")
}

func (d *device) hwConnected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hw != nil
}

func (d *device) close() {
	d.mu.Lock()
	d.closed = true
	c := d.serial
	h := d.hw
	d.serial = nil
	d.hw = nil
	d.mu.Unlock()
	if c != nil {
		_ = c.conn.Close()
	}
	if h != nil {
		_ = h.conn.Close()
	}
}

// --- WSP protocol payload builders ----------------------------------

// infoPayload builds a 62-byte wsp_dev_info_t (HELLO_ACK / INFO payload).
func (d *device) infoPayload() []byte {
	d.mu.Lock()
	cfg := d.cfg
	name := d.name
	mac := d.mac
	chip := d.chip
	wspPort := d.wspPort
	rawPort := d.rawPort
	uptime := uint32(time.Since(d.start).Seconds())
	d.mu.Unlock()

	b := make([]byte, 62)
	b[0] = wire.Version
	b[1] = 0 // fw_major
	b[2] = 2 // fw_minor
	b[3] = 0 // fw_patch
	binary.LittleEndian.PutUint32(b[4:8], wire.MaxPayload)
	binary.LittleEndian.PutUint32(b[8:12], cfg.baud)
	b[12] = cfg.dataBits
	b[13] = cfg.parity
	b[14] = cfg.stopBits
	b[15] = cfg.flow
	copy(b[16:32], name)
	copy(b[32:38], mac[:])
	copy(b[38:46], chip)
	binary.LittleEndian.PutUint32(b[46:50], uptime)
	binary.LittleEndian.PutUint16(b[50:52], wspPort)
	binary.LittleEndian.PutUint16(b[52:54], rawPort)
	return b
}

func (d *device) cfgPayload() []byte {
	d.mu.Lock()
	cfg := d.cfg
	d.mu.Unlock()
	return cfg.marshal()
}

func (d *device) statsPayload() []byte {
	d.mu.Lock()
	st := d.st
	d.mu.Unlock()
	return st.marshal()
}

// setUARTCfg validates and applies a UART_CFG.  On error the previous
// configuration is kept (docs §3.3).
func (d *device) setUARTCfg(c uartCfg) error {
	if err := c.validate(); err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg = c
	d.uartCfgCount++
	d.mu.Unlock()
	d.log("cfg: applied baud=%d data=%d parity=%d stop=%d flow=%d",
		c.baud, c.dataBits, c.parity, c.stopBits, c.flow)
	return nil
}

// applyLineCtrl applies a LINE_CTRL mask and returns the new LINE_STATUS
// state byte.
func (d *device) applyLineCtrl(mask byte) byte {
	mask &= wire.LineDTR | wire.LineRTS | wire.LineBREAK
	d.mu.Lock()
	d.lineState = (mask & (wire.LineDTR | wire.LineRTS)) |
		((mask & wire.LineBREAK) << 3)
	st := d.lineState
	d.mu.Unlock()
	d.log("line: ctrl=0x%02X state=0x%02X", mask, st)
	return st
}

// applyReset validates and records a RESET_TARGET command.
func (d *device) applyReset(payload []byte) error {
	if len(payload) != 4 {
		return fmt.Errorf("RESET_TARGET payload must be 4 bytes, got %d", len(payload))
	}
	action := payload[0]
	pulse := binary.LittleEndian.Uint16(payload[2:4])
	if action > 2 {
		return fmt.Errorf("reset action %d invalid (0..2)", action)
	}
	if action == 2 && (pulse < 1 || pulse > 5000) {
		return fmt.Errorf("reset pulse_ms %d out of range 1..5000", pulse)
	}
	d.mu.Lock()
	d.lastReset.action = action
	d.lastReset.pulseMS = pulse
	d.lastReset.count++
	d.mu.Unlock()
	d.log("reset: action=%d pulse_ms=%d", action, pulse)
	return nil
}

// --- frame counters / log -------------------------------------------

func (d *device) addFramesIn(typ byte, n int) {
	d.mu.Lock()
	d.st.framesIn++
	d.framesLog = append(d.framesLog, frameLog{Type: typ, Name: wire.TypeName(typ), Len: n})
	if len(d.framesLog) > 128 {
		d.framesLog = d.framesLog[len(d.framesLog)-128:]
	}
	d.mu.Unlock()
	d.log("<- %s len=%d", wire.TypeName(typ), n)
}

func (d *device) noteWireError(code byte) {
	if code == wire.ErrBadCRC {
		d.mu.Lock()
		d.st.crcErrors++
		d.mu.Unlock()
	}
	d.log("!! wire error %s", wire.ErrName(code))
}

func (d *device) addFramesOut() {
	d.mu.Lock()
	d.st.framesOut++
	d.mu.Unlock()
}

// --- WSP frame sending ----------------------------------------------

func (d *device) sendFrame(c *serialClient, typ byte, payload []byte) error {
	f := wire.EncodeFrame(typ, 0, payload)
	if f == nil {
		return fmt.Errorf("cannot encode %s frame: payload %d bytes", wire.TypeName(typ), len(payload))
	}
	if err := c.write(f); err != nil {
		return err
	}
	d.addFramesOut()
	d.log("-> %s len=%d", wire.TypeName(typ), len(payload))
	return nil
}

func (d *device) sendError(c *serialClient, code byte, msg string) error {
	payload := make([]byte, 0, 1+len(msg))
	payload = append(payload, code)
	payload = append(payload, msg...)
	d.log("-> ERROR %s (%s)", wire.ErrName(code), msg)
	return d.sendFrame(c, wire.TypeError, payload)
}

// --- UART bridge -----------------------------------------------------

// fromSerial handles bytes that must be written to the target UART.
func (d *device) fromSerial(data []byte) {
	if len(data) == 0 {
		return
	}
	d.mu.Lock()
	d.st.uartTX += uint32(len(data))
	hw := d.hw
	toHW := data
	if hw == nil {
		room := maxPendingHW - len(d.hwPending)
		if room > 0 {
			n := len(data)
			if n > room {
				n = room
			}
			d.hwPending = append(d.hwPending, data[:n]...)
			toHW = data[n:]
		}
		if len(toHW) != 0 {
			d.st.uartOverflow += uint32(len(toHW))
			d.log("uart: no target attached, dropping %d byte(s)", len(toHW))
			toHW = nil
		}
	}
	d.mu.Unlock()

	if hw != nil && len(toHW) != 0 {
		if err := hw.write(toHW); err != nil {
			d.log("uart: write to target failed: %v", err)
		}
	}
}

// fromHardware handles bytes received from the target UART and forwards them
// to the active serial client, applying the optional latency/drop injection.
func (d *device) fromHardware(data []byte) {
	if len(data) == 0 {
		return
	}
	d.mu.Lock()
	d.st.uartRX += uint32(len(data))
	client := d.serial
	latency := d.latency
	drop := d.drop
	d.mu.Unlock()

	if client == nil {
		d.mu.Lock()
		d.st.uartOverflow += uint32(len(data))
		d.mu.Unlock()
		d.log("uart: no serial client, dropping %d byte(s)", len(data))
		return
	}
	if drop > 0 && rand.Float64() < drop {
		d.mu.Lock()
		d.st.uartOverflow += uint32(len(data))
		d.mu.Unlock()
		d.log("uart: simulated drop of %d byte(s)", len(data))
		return
	}
	if latency > 0 {
		time.Sleep(latency)
	}
	if client.kind == serialRAW {
		if err := client.write(data); err != nil {
			d.log("raw: write to client failed: %v", err)
		}
		return
	}
	for _, chunk := range wire.Chunk(data) {
		if err := d.sendFrame(client, wire.TypeData, chunk); err != nil {
			d.log("wsp: write DATA to client failed: %v", err)
			return
		}
	}
}

// --- introspection (used by the optional admin HTTP endpoint) --------

type lastResetInfo struct {
	Action  byte   `json:"action"`
	PulseMS uint16 `json:"pulse_ms"`
	Count   uint32 `json:"count"`
}

type deviceStatus struct {
	DeviceName     string        `json:"device_name"`
	Baud           uint32        `json:"baud"`
	DataBits       byte          `json:"data_bits"`
	Parity         byte          `json:"parity"`
	StopBits       byte          `json:"stop_bits"`
	Flow           byte          `json:"flow"`
	LineState      byte          `json:"line_state"`
	SerialClient   string        `json:"serial_client"`
	HWConnected    bool          `json:"hw_connected"`
	UptimeS        uint32        `json:"uptime_s"`
	Stats          devStatsJSON  `json:"stats"`
	UARTCfgApplied uint32        `json:"uart_cfg_applied"`
	LastReset      lastResetInfo `json:"last_reset"`
	RecentFrames   []frameLog    `json:"recent_frames"`
	MaxPayload     int           `json:"max_payload"`
}

type devStatsJSON struct {
	UARTRXBytes   uint32 `json:"uart_rx_bytes"`
	UARTTXBytes   uint32 `json:"uart_tx_bytes"`
	FramesIn      uint32 `json:"frames_in"`
	FramesOut     uint32 `json:"frames_out"`
	CRCErrors     uint32 `json:"crc_errors"`
	UARTOverflows uint32 `json:"uart_overflows"`
}

func (d *device) status() deviceStatus {
	d.mu.Lock()
	defer d.mu.Unlock()

	st := deviceStatus{
		DeviceName:     d.name,
		Baud:           d.cfg.baud,
		DataBits:       d.cfg.dataBits,
		Parity:         d.cfg.parity,
		StopBits:       d.cfg.stopBits,
		Flow:           d.cfg.flow,
		LineState:      d.lineState,
		SerialClient:   "",
		HWConnected:    d.hw != nil,
		UptimeS:        uint32(time.Since(d.start).Seconds()),
		UARTCfgApplied: d.uartCfgCount,
		LastReset: lastResetInfo{
			Action:  d.lastReset.action,
			PulseMS: d.lastReset.pulseMS,
			Count:   d.lastReset.count,
		},
		MaxPayload: wire.MaxPayload,
	}
	if d.serial != nil {
		st.SerialClient = d.serial.kind.String()
	}
	st.Stats = devStatsJSON{
		UARTRXBytes:   d.st.uartRX,
		UARTTXBytes:   d.st.uartTX,
		FramesIn:      d.st.framesIn,
		FramesOut:     d.st.framesOut,
		CRCErrors:     d.st.crcErrors,
		UARTOverflows: d.st.uartOverflow,
	}
	st.RecentFrames = append([]frameLog(nil), d.framesLog...)
	if st.RecentFrames == nil {
		st.RecentFrames = []frameLog{}
	}
	return st
}

// portOf extracts the port number from a listener address.
func portOf(addr net.Addr) (int, error) {
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, err
	}
	if port < 0 || port > 65535 {
		return 0, fmt.Errorf("invalid port %d", port)
	}
	return port, nil
}
