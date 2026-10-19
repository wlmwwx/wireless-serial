// Package bridge ties the three moving parts together: the local virtual
// serial port (or the terminal in monitor mode), the TCP link to the ESP32 and
// the WSP/1 session running on top of it.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/wireless-serial/wserial/internal/discovery"
	wlog "github.com/wireless-serial/wserial/internal/log"
	"github.com/wireless-serial/wserial/internal/transport"
	"github.com/wireless-serial/wserial/internal/vport"
	"github.com/wireless-serial/wserial/internal/wsp"
)

// ErrNoDevice is wrapped by NoDeviceError; use errors.Is to test for it.
var ErrNoDevice = errors.New("未发现无线串口设备")

// ErrDeviceBusy is wrapped around ERROR(BUSY) replies so callers can pick a
// distinct exit code.
var ErrDeviceBusy = errors.New("设备被占用")

// NoDeviceError reports a failed mDNS discovery.
type NoDeviceError struct {
	Devices []discovery.Device
}

// Error implements error.
func (e *NoDeviceError) Error() string {
	return "未通过 mDNS 发现 _wserial._tcp 设备。请确认设备已上电并加入同一 WiFi，" +
		"或使用 -H/--host 直接指定设备 IP（可用 wserial list 查找）"
}

// Unwrap allows errors.Is(err, ErrNoDevice).
func (e *NoDeviceError) Unwrap() error { return ErrNoDevice }

// Config holds every tunable shared by the run and monitor subcommands.
type Config struct {
	// Host is the device address; empty means "discover over mDNS".
	Host string
	// Port is the TCP port; PortSet distinguishes an explicit -P from the
	// command default so discovery can provide the real port.
	Port    int
	PortSet bool
	// Raw selects the 3334 pure byte-stream mode (no WSP framing).
	Raw bool
	// Device is the local virtual port path (Unix symlink or Windows COM port).
	Device string
	// Baud 0 means "keep the device's current UART configuration".
	Baud     uint32
	DataBits uint8
	Parity   uint8
	StopBits uint8
	Flow     uint8

	ResetOnStart bool
	NoReconnect  bool
	// PingInterval is the WSP keepalive period; <= 0 disables PINGs.
	PingInterval time.Duration

	ConnectTimeout  time.Duration
	DiscoverTimeout time.Duration
	// BackoffMin/BackoffMax are exposed for tests; the CLI uses 1s and 10s.
	BackoffMin time.Duration
	BackoffMax time.Duration

	Logger *wlog.Logger

	// openPort is a test hook; nil means vport.Open is used. Tests replace it
	// with an in-memory port so the bridge logic can be exercised on hosts
	// where creating a PTY is not permitted.
	openPort func(device string, opts vport.Options) (vport.Port, error)
}

func (c *Config) normalize() {
	if c.Port <= 0 {
		c.Port = wsp.DefaultWSPPort
	}
	if c.DataBits == 0 {
		c.DataBits = 8
	}
	if c.StopBits == 0 {
		c.StopBits = 1
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 5 * time.Second
	}
	if c.DiscoverTimeout <= 0 {
		c.DiscoverTimeout = 3 * time.Second
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = time.Second
	}
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = 10 * time.Second
	}
	if c.Logger == nil {
		c.Logger = wlog.Discard()
	}
}

// uartCfg returns the UART_CFG to push, if any.
func (c *Config) uartCfg() (*wsp.UartCfg, error) {
	if c.Baud == 0 {
		return nil, nil
	}
	cfg := wsp.UartCfg{
		Baud:     c.Baud,
		DataBits: c.DataBits,
		Parity:   c.Parity,
		StopBits: c.StopBits,
		Flow:     c.Flow,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// target is a resolved device address.
type target struct {
	dev     *discovery.Device
	host    string
	port    int
	rawPort int
}

func (t target) portFor(raw bool) int {
	if raw {
		if t.rawPort > 0 {
			return t.rawPort
		}
		return wsp.DefaultRAWPort
	}
	if t.port > 0 {
		return t.port
	}
	return wsp.DefaultWSPPort
}

func (t target) address(raw bool) string {
	host := t.host
	return net.JoinHostPort(host, fmt.Sprintf("%d", t.portFor(raw)))
}

// resolveTarget turns the CLI flags into a concrete address, using mDNS when
// no host was given.
func resolveTarget(ctx context.Context, cfg *Config) (target, error) {
	log := cfg.Logger
	if cfg.Host != "" {
		t := target{host: cfg.Host}
		if cfg.PortSet {
			t.port, t.rawPort = cfg.Port, cfg.Port
		} else {
			t.port, t.rawPort = wsp.DefaultWSPPort, wsp.DefaultRAWPort
		}
		return t, nil
	}

	log.Infof("未指定 --host，正在通过 mDNS 搜索 %s 设备（最多 %s）...", discovery.ServiceType, cfg.DiscoverTimeout)
	devs, err := discovery.Discover(ctx, cfg.DiscoverTimeout, log)
	if err != nil {
		return target{}, err
	}
	if len(devs) == 0 {
		log.Warnf("mDNS 未发现任何 %s 设备", discovery.ServiceType)
		return target{}, &NoDeviceError{}
	}
	log.Infof("发现 %d 台设备:\n%s", len(devs), discovery.FormatTable(devs))
	if len(devs) > 1 {
		log.Warnf("发现多台设备，默认使用 %q；可用 -H/--host 指定其他设备", devs[0].Name)
	}
	d := devs[0]
	host := d.IP
	if host == "" {
		host = d.Hostname
	}
	if host == "" {
		return target{}, fmt.Errorf("设备 %q 未在 mDNS 记录中提供可用地址", d.Name)
	}
	t := target{dev: &d, host: host, port: d.Port, rawPort: d.RAWPort}
	if cfg.PortSet {
		t.port, t.rawPort = cfg.Port, cfg.Port
	}
	if t.port <= 0 {
		t.port = wsp.DefaultWSPPort
	}
	if t.rawPort <= 0 {
		t.rawPort = wsp.DefaultRAWPort
	}
	return t, nil
}

// Run creates the virtual serial port and keeps it bridged to the device until
// ctx is cancelled. The PTY/COM port is never torn down across reconnects, so
// user programs can keep it open while the WiFi link flaps.
func Run(ctx context.Context, cfg Config) error {
	cfg.normalize()
	log := cfg.Logger

	if _, err := cfg.uartCfg(); err != nil {
		return err
	}
	tgt, err := resolveTarget(ctx, &cfg)
	if err != nil {
		return err
	}

	open := cfg.openPort
	if open == nil {
		open = vport.Open
	}
	vp, err := open(cfg.Device, vport.Options{
		Baud:     cfg.Baud,
		DataBits: cfg.DataBits,
		Parity:   cfg.Parity,
		StopBits: cfg.StopBits,
		Flow:     cfg.Flow,
	})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := vp.Close(); cerr != nil {
			log.Warnf("清理虚拟串口时出错: %v", cerr)
		}
	}()

	if w, ok := vp.(vport.Warner); ok {
		for _, msg := range w.Warnings() {
			log.Warnf("%s", msg)
		}
	}
	if t, ok := vp.(interface{ Target() string }); ok {
		log.Debugf("PTY 后端: %s", t.Target())
	}
	log.Infof("虚拟串口已就绪 %s", vp.Name())
	log.Infof("用户程序可打开该设备收发数据；按 Ctrl+C 退出")

	// One reader for the vport's whole lifetime: data is queued and, when the
	// link is down, backpressure naturally pauses the writer.
	done := make(chan struct{})
	inCh := make(chan []byte, 64)
	inErr := make(chan error, 1)
	go pumpReader(vp, inCh, inErr, done, ctx)
	defer close(done)

	ep := endpoint{
		write: func(p []byte) error { return writeFull(vp, p) },
		in:    inCh,
		inErr: inErr,
	}
	var line vport.LineMonitor
	if lm, ok := vp.(vport.LineMonitor); ok {
		line = lm
	}
	return connectLoop(ctx, &cfg, tgt, ep, line)
}

// endpoint abstracts the user side of the bridge: write delivers device bytes
// to the user, while in/inErr carry the bytes the user produced.
type endpoint struct {
	write func([]byte) error
	in    <-chan []byte
	inErr <-chan error
}

// connectLoop dials, handshakes and bridges until either ctx is cancelled
// (returns nil) or a fatal error occurs (returns it). Transient failures are
// retried with exponential backoff unless NoReconnect is set.
func connectLoop(ctx context.Context, cfg *Config, tgt target, ep endpoint, line vport.LineMonitor) error {
	log := cfg.Logger
	backoff := transport.NewBackoff(cfg.BackoffMin, cfg.BackoffMax)
	var pending []byte
	first := true

	for {
		if !first {
			delay := time.Duration(0)
			if cfg.NoReconnect {
				return nil
			}
			delay = backoff.Next()
			log.Infof("%s 后重连 %s ...", delay, tgt.address(cfg.Raw))
			if err := transport.Wait(ctx, delay); err != nil {
				return nil
			}
		}
		first = false

		if ctx.Err() != nil {
			return nil
		}
		addr := tgt.address(cfg.Raw)
		conn, err := transport.Dial(ctx, tgt.host, tgt.portFor(cfg.Raw), cfg.ConnectTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warnf("%v", err)
			if cfg.NoReconnect {
				return err
			}
			continue
		}

		var runErr error
		if cfg.Raw {
			log.Infof("RAW 模式已连接 %s（TCP %d，纯字节透传，无握手）", addr, tgt.portFor(true))
			pending, runErr = bridgeRaw(ctx, conn, cfg, ep, pending)
			_ = conn.Close()
		} else {
			sess := wsp.NewSession(conn)
			hs, herr := handshake(ctx, sess, cfg)
			if herr != nil {
				_ = sess.Close()
				if ctx.Err() != nil {
					return nil
				}
				var pe *wsp.ProtocolError
				if errors.As(herr, &pe) && pe.Fatal() {
					log.Errorf("%v", herr)
					if pe.Code == wsp.ErrBusy {
						return fmt.Errorf("%w：%v", ErrDeviceBusy, herr)
					}
					return herr
				}
				log.Warnf("WSP 握手失败: %v", herr)
				if cfg.NoReconnect {
					return herr
				}
				continue
			}
			logHandshake(log, hs)

			if cfg.ResetOnStart {
				if rerr := sess.SendReset(wsp.NewResetPulse(120)); rerr != nil {
					log.Warnf("发送 RESET_TARGET 失败: %v", rerr)
				} else {
					log.Infof("已向目标发送复位脉冲（RESET_TARGET action=pulse, 120ms）")
				}
			}
			backoff.Reset()

			pending, runErr = bridgeWSP(ctx, sess, cfg, ep, line, pending)
			badCRC, badVer, resync := sess.ParserStats()
			_ = sess.Close()
			if badCRC+badVer+resync > 0 {
				log.Debugf("WSP 解析统计: crc_errors=%d bad_version=%d resync=%d", badCRC, badVer, resync)
			}
		}

		if ctx.Err() != nil {
			return nil
		}
		if runErr != nil {
			log.Warnf("连接中断: %v", runErr)
			if cfg.NoReconnect {
				return runErr
			}
			continue
		}
		// A clean end of the bridge without cancellation means the peer
		// closed the connection; treat it as a normal disconnect.
		log.Warnf("连接已由对端关闭")
		if cfg.NoReconnect {
			return nil
		}
	}
}

// handshake performs the WSP handshake while making sure that context
// cancellation (Ctrl+C) tears down a blocked read immediately.
func handshake(ctx context.Context, sess *wsp.Session, cfg *Config) (wsp.HandshakeResult, error) {
	opts := wsp.HandshakeOptions{}
	uartCfg, err := cfg.uartCfg()
	if err != nil {
		return wsp.HandshakeResult{}, err
	}
	opts.UartCfg = uartCfg

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-done:
		}
	}()
	defer close(done)
	return sess.Handshake(ctx, opts)
}

func logHandshake(log *wlog.Logger, hs wsp.HandshakeResult) {
	info := hs.Info
	name := info.Name
	if name == "" {
		name = "(未命名)"
	}
	chip := info.Chip
	if chip == "" {
		chip = "?"
	}
	log.Infof("已连接设备 %s（芯片 %s，固件 %s，MAC %s，协议 v%d）",
		name, chip, info.FWVersion(), info.MACString(), info.ProtoVer)
	log.Infof("设备当前串口配置: %s", info.UartCfg())
	if hs.UartCfgAck != nil {
		log.Infof("串口配置已生效: %s", *hs.UartCfgAck)
	}
	if hs.CfgErr != nil {
		log.Warnf("串口配置未被设备接受: %v（继续使用设备当前配置）", hs.CfgErr)
	}
}

// bridgeWSP runs the framed data path for one connection. It returns any
// unsent bytes so the next connection can send them first.
func bridgeWSP(ctx context.Context, sess *wsp.Session, cfg *Config, ep endpoint, line vport.LineMonitor, pending []byte) ([]byte, error) {
	log := cfg.Logger
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	sendDone := make(chan sendResult, 1)

	// Cancel watcher: unblock the reads when the CLI is interrupted.
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			_ = sess.Close()
		case <-stop:
		}
	}()

	// Device -> user.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			f, err := sess.NextFrame()
			if err != nil {
				pushErr(errCh, err)
				return
			}
			switch f.Type {
			case wsp.TypeData:
				// Hot data path: no logging here by design.
				if werr := ep.write(f.Payload); werr != nil {
					pushErr(errCh, fmt.Errorf("写入虚拟串口失败: %w", werr))
					return
				}
			case wsp.TypePong:
				log.Tracef("收到 PONG（%d 字节）", len(f.Payload))
			case wsp.TypeError:
				pe := wsp.ParseErrorPayload(f.Payload)
				log.Warnf("%v", pe)
				if pe.Fatal() {
					pushErr(errCh, pe)
					return
				}
			case wsp.TypeLineStatus:
				if st, perr := wsp.ParseLineStatus(f.Payload); perr == nil {
					log.Tracef("收到 LINE_STATUS state=0x%02x (DTR=%v RTS=%v CTS=%v DSR=%v DCD=%v BREAK=%v)",
						st.State,
						st.State&wsp.StatusDTR != 0, st.State&wsp.StatusRTS != 0,
						st.State&wsp.StatusCTS != 0, st.State&wsp.StatusDSR != 0,
						st.State&wsp.StatusDCD != 0, st.State&wsp.StatusBreak != 0)
				}
			case wsp.TypeStats:
				if st, perr := wsp.ParseStats(f.Payload); perr == nil {
					log.Infof("设备统计: %s", st)
				}
			case wsp.TypeInfo:
				if info, perr := wsp.ParseDevInfo(f.Payload); perr == nil {
					log.Infof("设备信息: name=%q chip=%q fw=%s uart=%s", info.Name, info.Chip, info.FWVersion(), info.UartCfg())
				}
			case wsp.TypeUartCfgAck:
				if c, perr := wsp.ParseUartCfg(f.Payload); perr == nil {
					log.Debugf("收到 UART_CFG_ACK: %s", c)
				}
			default:
				log.Debugf("忽略帧 type=%s (0x%02x) len=%d", f.Type, uint8(f.Type), len(f.Payload))
			}
		}
	}()

	// User -> device, with resend bookkeeping.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p := pending
		for {
			if len(p) == 0 {
				p = nil
				select {
				case <-stop:
					sendDone <- sendResult{}
					return
				case <-ctx.Done():
					sendDone <- sendResult{}
					return
				case p = <-ep.in:
				}
			}
			consumed, werr := writeDataFrames(sess, p)
			if werr != nil {
				sendDone <- sendResult{pending: append([]byte(nil), p[consumed:]...), err: werr}
				pushErr(errCh, fmt.Errorf("发送 DATA 失败: %w", werr))
				return
			}
			p = nil
		}
	}()

	// Keepalive PINGs.
	if cfg.PingInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(cfg.PingInterval)
			defer t.Stop()
			var seq uint32
			for {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					payload := []byte{byte(seq), byte(seq >> 8), byte(seq >> 16), byte(seq >> 24)}
					if err := sess.SendPing(payload); err != nil {
						pushErr(errCh, fmt.Errorf("发送 PING 失败: %w", err))
						return
					}
					log.Tracef("发送 PING seq=%d", seq)
					seq++
				}
			}
		}()
	}

	// DTR/RTS mirroring: poll the local port and forward changes as LINE_CTRL.
	if line != nil && !cfg.Raw {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			last := uint8(0xFF)
			for {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				mask, err := line.LineMask()
				if err != nil {
					log.Debugf("本地端口不支持线路状态读取，停止 DTR/RTS 映射: %v", err)
					return
				}
				if mask == last {
					continue
				}
				last = mask
				if err := sess.SendLineCtrl(mask); err != nil {
					pushErr(errCh, fmt.Errorf("发送 LINE_CTRL 失败: %w", err))
					return
				}
				log.Debugf("LINE_CTRL -> DTR=%v RTS=%v", mask&wsp.LineDTR != 0, mask&wsp.LineRTS != 0)
			}
		}()
	}

	var runErr error
	select {
	case runErr = <-errCh:
	case runErr = <-ep.inErr:
		runErr = fmt.Errorf("读取本地串口失败: %w", runErr)
	case <-ctx.Done():
	}

	close(stop)
	_ = sess.Close()
	wg.Wait()
	res := <-sendDone
	if ctx.Err() != nil {
		return res.pending, nil
	}
	if runErr == nil {
		runErr = res.err
	}
	return res.pending, runErr
}

// sendResult is what the sender goroutine hands back at teardown.
type sendResult struct {
	pending []byte
	err     error
}

// writeDataFrames sends p as DATA frames and reports how many bytes were
// written before an error, so the remainder can be retried later.
func writeDataFrames(sess *wsp.Session, p []byte) (int, error) {
	consumed := 0
	for consumed < len(p) {
		n := len(p) - consumed
		if n > wsp.MaxPayload {
			n = wsp.MaxPayload
		}
		if err := sess.WriteFrame(wsp.TypeData, 0, p[consumed:consumed+n]); err != nil {
			return consumed, err
		}
		consumed += n
	}
	return consumed, nil
}

// pushErr performs a non-blocking send on the bridge error channel.
func pushErr(ch chan<- error, err error) {
	select {
	case ch <- err:
	default:
	}
}

// writeFull writes all of p to w, tolerating short writes.
func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// pumpReader forwards bytes read from r into ch until r fails or done is
// closed. It is the single reader for a virtual port's lifetime.
func pumpReader(r io.Reader, ch chan<- []byte, errCh chan<- error, done <-chan struct{}, ctx context.Context) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			cp := make([]byte, n)
			copy(cp, buf[:n])
			select {
			case ch <- cp:
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
	}
}
