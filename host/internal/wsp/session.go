package wsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Default handshake timings from docs/protocol.md §4.
const (
	DefaultHelloTimeout = 5 * time.Second
	DefaultCfgTimeout   = 3 * time.Second
)

// ErrClosed is returned by Session methods after the underlying connection has
// been closed.
var ErrClosed = errors.New("wsp: 连接已关闭")

// Session owns one TCP connection to the device and speaks WSP/1 on it.
//
// One goroutine may read through NextFrame while any number of goroutines
// write through the Send*/WriteData methods; writes are serialised internally.
type Session struct {
	conn     net.Conn
	parser   *Parser
	readBuf  []byte
	frames   []Frame
	deferred []Frame

	writeMu sync.Mutex

	info     DevInfo
	haveInfo bool
}

// NewSession wraps conn. It does not perform a handshake.
func NewSession(conn net.Conn) *Session {
	return &Session{
		conn:    conn,
		parser:  NewParser(),
		readBuf: make([]byte, 32*1024),
	}
}

// Conn returns the underlying net.Conn.
func (s *Session) Conn() net.Conn { return s.conn }

// Close closes the underlying connection.
func (s *Session) Close() error {
	if s.conn == nil {
		return nil
	}
	return s.conn.Close()
}

// Info returns the device information received during the handshake.
func (s *Session) Info() (DevInfo, bool) { return s.info, s.haveInfo }

// ParserStats exposes the streaming parser's error counters.
func (s *Session) ParserStats() (badCRC, badVersion, resync uint32) {
	return s.parser.BadCRC, s.parser.BadVersion, s.parser.Resync
}

// writeAll writes one encoded frame atomically with respect to other writers.
func (s *Session) writeAll(buf []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for len(buf) > 0 {
		n, err := s.conn.Write(buf)
		if n > 0 {
			buf = buf[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

// WriteFrame serialises and sends one frame.
func (s *Session) WriteFrame(t FrameType, flags uint8, payload []byte) error {
	buf, err := Encode(t, flags, payload)
	if err != nil {
		return err
	}
	return s.writeAll(buf)
}

// WriteData sends raw UART bytes as one or more DATA frames of at most
// MaxPayload bytes each. It never splits a byte across frames.
func (s *Session) WriteData(p []byte) error {
	for len(p) > 0 {
		n := len(p)
		if n > MaxPayload {
			n = MaxPayload
		}
		if err := s.WriteFrame(TypeData, 0, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// SendHello sends a HELLO frame.
func (s *Session) SendHello(h Hello) error {
	return s.WriteFrame(TypeHello, 0, h.Marshal())
}

// SendUartCfg sends a UART_CFG frame after validating the configuration.
func (s *Session) SendUartCfg(cfg UartCfg) error {
	b, err := cfg.Marshal()
	if err != nil {
		return err
	}
	return s.WriteFrame(TypeUartCfg, 0, b)
}

// SendPing sends a PING frame; payload may be 0..64 bytes.
func (s *Session) SendPing(payload []byte) error {
	if len(payload) > 64 {
		return fmt.Errorf("wsp: PING payload 过长: %d > 64", len(payload))
	}
	return s.WriteFrame(TypePing, 0, payload)
}

// SendLineCtrl sends a LINE_CTRL bitmask.
func (s *Session) SendLineCtrl(mask uint8) error {
	return s.WriteFrame(TypeLineCtrl, 0, []byte{mask})
}

// SendStatsRequest sends an empty STATS request.
func (s *Session) SendStatsRequest() error {
	return s.WriteFrame(TypeStats, 0, nil)
}

// SendInfoRequest sends an empty INFO request.
func (s *Session) SendInfoRequest() error {
	return s.WriteFrame(TypeInfo, 0, nil)
}

// SendReset sends a RESET_TARGET command.
func (s *Session) SendReset(cmd ResetCmd) error {
	return s.WriteFrame(TypeResetTarget, 0, cmd.Marshal())
}

// NextFrame returns the next frame from the device, blocking until one is
// available or the connection fails. Recoverable framing errors (CRC, length,
// version, garbage) are counted and skipped.
//
// Frames that arrived during the handshake before the expected ACK are
// delivered first, in wire order.
func (s *Session) NextFrame() (Frame, error) {
	if len(s.deferred) > 0 {
		f := s.deferred[0]
		s.deferred = s.deferred[1:]
		return f, nil
	}
	return s.nextReceivedFrame()
}

// nextReceivedFrame pops an already-received frame or reads more bytes from the
// connection. It is used by the handshake, which must not consume frames it
// already deferred.
func (s *Session) nextReceivedFrame() (Frame, error) {
	for {
		if len(s.frames) > 0 {
			f := s.frames[0]
			s.frames = s.frames[1:]
			return f, nil
		}
		n, err := s.conn.Read(s.readBuf)
		if n > 0 {
			s.parser.Feed(s.readBuf[:n], func(t FrameType, flags uint8, payload []byte) {
				cp := make([]byte, len(payload))
				copy(cp, payload)
				s.frames = append(s.frames, Frame{Type: t, Flags: flags, Payload: cp})
			})
		}
		if err != nil {
			return Frame{}, err
		}
	}
}

// HandshakeOptions controls the WSP session setup.
type HandshakeOptions struct {
	// Hello is the greeting to send; the zero value means NewHello().
	Hello *Hello
	// UartCfg, when non-nil, is pushed right after HELLO_ACK.
	UartCfg *UartCfg
	// HelloTimeout bounds the wait for HELLO_ACK (default 5s).
	HelloTimeout time.Duration
	// CfgTimeout bounds the wait for UART_CFG_ACK (default 3s).
	CfgTimeout time.Duration
}

// HandshakeResult reports the negotiated session parameters.
type HandshakeResult struct {
	Info       DevInfo
	UartCfgAck *UartCfg // nil unless UART_CFG was sent and acknowledged
	CfgErr     *ProtocolError
}

// Handshake performs the HELLO / (optional) UART_CFG exchange described in
// docs/protocol.md §4. Frames that arrive before the expected ACK are queued
// and delivered by later NextFrame calls, so no serial data is lost.
func (s *Session) Handshake(ctx context.Context, opts HandshakeOptions) (HandshakeResult, error) {
	helloTimeout := opts.HelloTimeout
	if helloTimeout <= 0 {
		helloTimeout = DefaultHelloTimeout
	}
	cfgTimeout := opts.CfgTimeout
	if cfgTimeout <= 0 {
		cfgTimeout = DefaultCfgTimeout
	}

	hello := NewHello()
	if opts.Hello != nil {
		hello = *opts.Hello
	}
	if err := s.SendHello(hello); err != nil {
		return HandshakeResult{}, fmt.Errorf("发送 HELLO 失败: %w", err)
	}

	setReadDeadline(s.conn, ctx, helloTimeout)
	defer s.conn.SetReadDeadline(time.Time{})

	var result HandshakeResult
	gotAck := false
	for !gotAck {
		f, err := s.nextReceivedFrame()
		if err != nil {
			if isTimeout(err) {
				return HandshakeResult{}, fmt.Errorf("等待 HELLO_ACK 超时（%s）: %w", helloTimeout, err)
			}
			return HandshakeResult{}, fmt.Errorf("等待 HELLO_ACK 时连接中断: %w", err)
		}
		switch f.Type {
		case TypeHelloAck:
			info, err := ParseDevInfo(f.Payload)
			if err != nil {
				return HandshakeResult{}, fmt.Errorf("解析 HELLO_ACK 失败: %w", err)
			}
			if info.ProtoVer != Version {
				return HandshakeResult{}, &ProtocolError{
					Code: ErrBadVersion,
					Msg:  fmt.Sprintf("设备协议版本 %d，客户端仅支持 %d", info.ProtoVer, Version),
				}
			}
			s.info, s.haveInfo = info, true
			result.Info = info
			gotAck = true
		case TypeError:
			return HandshakeResult{}, ParseErrorPayload(f.Payload)
		default:
			s.queueFrame(f)
		}
	}

	if opts.UartCfg == nil {
		return result, nil
	}

	cfg := *opts.UartCfg
	if err := s.SendUartCfg(cfg); err != nil {
		return result, fmt.Errorf("发送 UART_CFG 失败: %w", err)
	}
	setReadDeadline(s.conn, ctx, cfgTimeout)
	waiting := true
	for waiting {
		f, err := s.nextReceivedFrame()
		if err != nil {
			if isTimeout(err) {
				result.CfgErr = &ProtocolError{Code: ErrInternal, Msg: fmt.Sprintf("等待 UART_CFG_ACK 超时（%s）", cfgTimeout)}
				break
			}
			return result, fmt.Errorf("等待 UART_CFG_ACK 时连接中断: %w", err)
		}
		switch f.Type {
		case TypeUartCfgAck:
			applied, err := ParseUartCfg(f.Payload)
			if err != nil {
				return result, fmt.Errorf("解析 UART_CFG_ACK 失败: %w", err)
			}
			result.UartCfgAck = &applied
			waiting = false
		case TypeError:
			pErr := ParseErrorPayload(f.Payload)
			if pErr.Fatal() {
				return result, pErr
			}
			result.CfgErr = pErr
			waiting = false
		default:
			s.queueFrame(f)
		}
	}
	s.conn.SetReadDeadline(time.Time{})
	return result, nil
}

// queueFrame defers an already-received frame so the normal data path sees it
// later, in wire order.
func (s *Session) queueFrame(f Frame) {
	s.deferred = append(s.deferred, f)
}

// setReadDeadline applies the shorter of timeout and the context deadline.
func setReadDeadline(conn net.Conn, ctx context.Context, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	if ctx != nil {
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
	}
	_ = conn.SetReadDeadline(deadline)
}

// isTimeout reports whether err is a net timeout.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// IsTimeout is the exported form of isTimeout, used by the bridge.
func IsTimeout(err error) bool { return isTimeout(err) }
