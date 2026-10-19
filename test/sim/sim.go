package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wirelessserial/sim/wire"
)

// simConfig configures a simulated device instance.
type simConfig struct {
	WSPAddr   string
	RAWAddr   string
	HWAddr    string
	AdminAddr string

	Baud uint32
	Name string

	Latency time.Duration
	Drop    float64

	HelloTimeout time.Duration
	IdleTimeout  time.Duration

	Verbose bool
	Logf    func(format string, args ...interface{})
}

func (c *simConfig) applyDefaults() {
	if c.WSPAddr == "" {
		c.WSPAddr = "127.0.0.1:0"
	}
	if c.RAWAddr == "" {
		c.RAWAddr = "127.0.0.1:0"
	}
	if c.HWAddr == "" {
		c.HWAddr = "127.0.0.1:0"
	}
	if c.Baud == 0 {
		c.Baud = 115200
	}
	if c.Name == "" {
		c.Name = "wserial-sim"
	}
	if c.HelloTimeout == 0 {
		c.HelloTimeout = 5 * time.Second
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 120 * time.Second
	}
	if c.Drop < 0 {
		c.Drop = 0
	}
	if c.Drop > 1 {
		c.Drop = 1
	}
	if c.Logf == nil {
		if c.Verbose {
			c.Logf = log.Printf
		} else {
			c.Logf = func(string, ...interface{}) {}
		}
	}
}

// simAddrs is the JSON contract consumed by the integration tests.  The
// `*_port` fields are redundant convenience copies of the host:port strings.
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

// simServer owns the listeners and the simulated device.
type simServer struct {
	cfg simConfig
	dev *device

	wspLis   net.Listener
	rawLis   net.Listener
	hwLis    net.Listener
	adminLis net.Listener

	adminSrv *http.Server

	wspAddr   string
	rawAddr   string
	hwAddr    string
	adminAddr string

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// startSim starts all listeners.  Port 0 selects an ephemeral port; the
// actual addresses are available through Addrs().
func startSim(cfg simConfig) (*simServer, error) {
	cfg.applyDefaults()

	initial := uartCfg{baud: cfg.Baud, dataBits: 8, parity: 0, stopBits: 1, flow: 0}
	if err := initial.validate(); err != nil {
		return nil, fmt.Errorf("invalid initial baud: %w", err)
	}

	s := &simServer{
		cfg: cfg,
		dev: newDevice(initial, cfg.Name, cfg.Latency, cfg.Drop, cfg.Logf),
	}

	var err error
	if s.wspLis, err = net.Listen("tcp", cfg.WSPAddr); err != nil {
		return nil, fmt.Errorf("listen wsp %s: %w", cfg.WSPAddr, err)
	}
	if s.rawLis, err = net.Listen("tcp", cfg.RAWAddr); err != nil {
		s.closeListeners()
		return nil, fmt.Errorf("listen raw %s: %w", cfg.RAWAddr, err)
	}
	if s.hwLis, err = net.Listen("tcp", cfg.HWAddr); err != nil {
		s.closeListeners()
		return nil, fmt.Errorf("listen hw %s: %w", cfg.HWAddr, err)
	}
	if cfg.AdminAddr != "" {
		if s.adminLis, err = net.Listen("tcp", cfg.AdminAddr); err != nil {
			s.closeListeners()
			return nil, fmt.Errorf("listen admin %s: %w", cfg.AdminAddr, err)
		}
	}

	s.wspAddr = s.wspLis.Addr().String()
	s.rawAddr = s.rawLis.Addr().String()
	s.hwAddr = s.hwLis.Addr().String()
	if s.adminLis != nil {
		s.adminAddr = s.adminLis.Addr().String()
	}

	wspPort, err := portOf(s.wspLis.Addr())
	if err != nil {
		s.closeListeners()
		return nil, fmt.Errorf("wsp port: %w", err)
	}
	rawPort, err := portOf(s.rawLis.Addr())
	if err != nil {
		s.closeListeners()
		return nil, fmt.Errorf("raw port: %w", err)
	}
	s.dev.setPorts(uint16(wspPort), uint16(rawPort))

	if s.adminLis != nil {
		mux := http.NewServeMux()
		mux.HandleFunc("/status", s.handleStatus)
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok\n"))
		})
		s.adminSrv = &http.Server{Handler: mux}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			err := s.adminSrv.Serve(s.adminLis)
			if err != nil && !errors.Is(err, http.ErrServerClosed) && s.cfg.Verbose {
				log.Printf("admin: %v", err)
			}
		}()
	}

	s.wg.Add(3)
	go s.acceptLoop(&s.wg, s.wspLis, s.serveWSP)
	go s.acceptLoop(&s.wg, s.rawLis, s.serveRAW)
	go s.acceptLoop(&s.wg, s.hwLis, s.serveHW)

	s.cfg.Logf("sim: listening wsp=%s raw=%s hw=%s admin=%s",
		s.wspAddr, s.rawAddr, s.hwAddr, s.adminAddr)
	return s, nil
}

func (s *simServer) closeListeners() {
	for _, l := range []net.Listener{s.wspLis, s.rawLis, s.hwLis, s.adminLis} {
		if l != nil {
			_ = l.Close()
		}
	}
}

// Close stops the listeners and every active connection and waits for all
// handler goroutines to exit.
func (s *simServer) Close() error {
	s.closeOnce.Do(func() {
		s.closeListeners()
		if s.adminSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.adminSrv.Shutdown(ctx)
			cancel()
		}
		s.dev.close()
		s.wg.Wait()
	})
	return nil
}

// Addrs returns the actual listening addresses.
func (s *simServer) Addrs() simAddrs {
	a := simAddrs{
		WSP:   s.wspAddr,
		RAW:   s.rawAddr,
		HW:    s.hwAddr,
		Admin: s.adminAddr,
		PID:   os.Getpid(),
	}
	if p, err := portOf(s.wspLis.Addr()); err == nil {
		a.WSPPort = p
	}
	if p, err := portOf(s.rawLis.Addr()); err == nil {
		a.RAWPort = p
	}
	if p, err := portOf(s.hwLis.Addr()); err == nil {
		a.HWPort = p
	}
	if s.adminLis != nil {
		if p, err := portOf(s.adminLis.Addr()); err == nil {
			a.AdminPort = p
		}
	}
	return a
}

// ReadyLine is printed on stdout once the simulator is accepting connections,
// so that scripts can wait for readiness without racing the addrfile write.
func (s *simServer) ReadyLine() string {
	b, _ := json.Marshal(s.Addrs())
	return "SIM_READY " + string(b)
}

// WriteAddrFile atomically writes the actual addresses as JSON.  Integration
// tests can poll for this file before connecting.
func (s *simServer) WriteAddrFile(path string) error {
	b, err := json.MarshalIndent(s.Addrs(), "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sim-addr-*")
	if err != nil {
		return fmt.Errorf("create temp addrfile: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write addrfile: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync addrfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close addrfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename addrfile: %w", err)
	}
	return nil
}

// handleStatus serves a JSON snapshot of the simulated device.
func (s *simServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := struct {
		Addrs  simAddrs     `json:"addrs"`
		Device deviceStatus `json:"device"`
	}{Addrs: s.Addrs(), Device: s.dev.status()}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// --- accept loops / connection handlers -----------------------------

func (s *simServer) acceptLoop(wg *sync.WaitGroup, lis net.Listener,
	serve func(net.Conn)) {
	defer wg.Done()
	for {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve(conn)
		}()
	}
}

const busyMessage = "device busy: only one serial client is allowed"

// serveWSP is the device-side WSP/1 session state machine.
func (s *simServer) serveWSP(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	c := &serialClient{kind: serialWSP, conn: conn}
	if err := s.dev.setSerial(c); err != nil {
		if errors.Is(err, errDeviceBusy) {
			// docs §4 rule 6: send ERROR(BUSY) and close.
			payload := append([]byte{wire.ErrBusy}, busyMessage...)
			_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = conn.Write(wire.EncodeFrame(wire.TypeError, 0, payload))
			s.dev.log("wsp: rejected second client with ERROR(BUSY)")
		}
		return
	}
	defer s.dev.clearSerial(c)

	br := bufio.NewReaderSize(conn, 4096)

	// --- handshake phase: HELLO must arrive within HelloTimeout ---
	helloDone := false
	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.HelloTimeout))
	for !helloDone {
		fr, err := wire.ReadFrame(br)
		if err != nil {
			var we *wire.WireError
			if errors.As(err, &we) {
				s.dev.noteWireError(we.Code)
				if we.Fatal {
					_ = s.dev.sendError(c, we.Code, we.Msg)
					return
				}
				_ = s.dev.sendError(c, we.Code, we.Msg)
				continue
			}
			return // timeout or EOF before HELLO
		}
		s.dev.addFramesIn(fr.Type, len(fr.Payload))
		if fr.Type != wire.TypeHello {
			_ = s.dev.sendError(c, wire.ErrNotHello, "HELLO required before other frames")
			return
		}
		if len(fr.Payload) != 4 {
			_ = s.dev.sendError(c, wire.ErrBadLength, "HELLO payload must be 4 bytes")
			return
		}
		if fr.Payload[0] != wire.Version {
			_ = s.dev.sendError(c, wire.ErrBadVersion, "unsupported HELLO proto_ver")
			return
		}
		helloDone = true
	}
	if err := s.dev.sendFrame(c, wire.TypeHelloAck, s.dev.infoPayload()); err != nil {
		return
	}

	// --- DATA state ---
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		fr, err := wire.ReadFrame(br)
		if err != nil {
			var we *wire.WireError
			if errors.As(err, &we) {
				s.dev.noteWireError(we.Code)
				_ = s.dev.sendError(c, we.Code, we.Msg)
				if we.Fatal {
					return
				}
				continue
			}
			return // idle timeout / EOF
		}
		s.dev.addFramesIn(fr.Type, len(fr.Payload))
		if closeConn := s.handleClientFrame(c, fr); closeConn {
			return
		}
	}
}

// handleClientFrame dispatches one frame received after the handshake.
// It returns true when the session must be closed.
func (s *simServer) handleClientFrame(c *serialClient, fr wire.Frame) bool {
	d := s.dev
	sendErrAndKeep := func(code byte, msg string) bool {
		if err := d.sendError(c, code, msg); err != nil {
			d.log("wsp: send ERROR failed: %v", err)
		}
		return false
	}
	sendErrAndClose := func(code byte, msg string) bool {
		_ = d.sendError(c, code, msg)
		return true
	}

	switch fr.Type {
	case wire.TypeHello:
		if len(fr.Payload) != 4 {
			return sendErrAndClose(wire.ErrBadLength, "HELLO payload must be 4 bytes")
		}
		if fr.Payload[0] != wire.Version {
			return sendErrAndClose(wire.ErrBadVersion, "unsupported HELLO proto_ver")
		}
		if err := d.sendFrame(c, wire.TypeHelloAck, d.infoPayload()); err != nil {
			return true
		}
		return false

	case wire.TypeData:
		if len(fr.Payload) == 0 {
			return sendErrAndKeep(wire.ErrInvalidArg, "DATA payload must be 1..1024 bytes")
		}
		d.fromSerial(fr.Payload)
		return false

	case wire.TypeUARTCfg:
		if len(fr.Payload) != 8 {
			return sendErrAndKeep(wire.ErrBadLength,
				fmt.Sprintf("UART_CFG payload must be 8 bytes, got %d", len(fr.Payload)))
		}
		cfg := parseUARTCfg(fr.Payload)
		if err := d.setUARTCfg(cfg); err != nil {
			return sendErrAndKeep(wire.ErrInvalidArg, err.Error())
		}
		if err := d.sendFrame(c, wire.TypeUARTCfgAck, d.cfgPayload()); err != nil {
			return true
		}
		return false

	case wire.TypeLineCtrl:
		if len(fr.Payload) != 1 {
			return sendErrAndKeep(wire.ErrBadLength, "LINE_CTRL payload must be 1 byte")
		}
		st := d.applyLineCtrl(fr.Payload[0])
		if err := d.sendFrame(c, wire.TypeLineStatus, []byte{st, 0}); err != nil {
			return true
		}
		return false

	case wire.TypeStats:
		if err := d.sendFrame(c, wire.TypeStats, d.statsPayload()); err != nil {
			return true
		}
		return false

	case wire.TypePing:
		if len(fr.Payload) > 64 {
			return sendErrAndKeep(wire.ErrInvalidArg, "PING payload must be 0..64 bytes")
		}
		if err := d.sendFrame(c, wire.TypePong, fr.Payload); err != nil {
			return true
		}
		return false

	case wire.TypePong, wire.TypeError:
		// Nothing to answer.
		return false

	case wire.TypeInfo:
		if err := d.sendFrame(c, wire.TypeInfo, d.infoPayload()); err != nil {
			return true
		}
		return false

	case wire.TypeResetTarget:
		if len(fr.Payload) != 4 {
			return sendErrAndKeep(wire.ErrBadLength,
				fmt.Sprintf("RESET_TARGET payload must be 4 bytes, got %d", len(fr.Payload)))
		}
		if err := d.applyReset(fr.Payload); err != nil {
			return sendErrAndKeep(wire.ErrInvalidArg, err.Error())
		}
		return false

	default:
		return sendErrAndKeep(wire.ErrUnsupported,
			fmt.Sprintf("unsupported frame type 0x%X", fr.Type))
	}
}

// serveRAW is the byte-transparent RAW mode (port 3334 equivalent).
func (s *simServer) serveRAW(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	c := &serialClient{kind: serialRAW, conn: conn}
	if err := s.dev.setSerial(c); err != nil {
		// RAW mode has no framing: cannot report ERROR(BUSY); just close.
		s.dev.log("raw: rejected connection (%v)", err)
		return
	}
	defer s.dev.clearSerial(c)
	s.dev.log("raw: client connected (%s)", conn.RemoteAddr())

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			s.dev.fromSerial(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// serveHW is the "target hardware" side: the test process connects here to
// act as the UART peer of the simulated device.
func (s *simServer) serveHW(conn net.Conn) {
	h := &hwConn{conn: conn}
	if err := s.dev.attachHW(h); err != nil {
		_ = conn.Close()
		return
	}
	defer s.dev.detachHW(h)

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			s.dev.fromHardware(buf[:n])
		}
		if err != nil {
			return
		}
	}
}
