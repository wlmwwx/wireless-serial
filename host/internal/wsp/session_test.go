package wsp

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// testPeer is a scriptable WSP device used in session tests.
type testPeer struct {
	conn   net.Conn
	frames chan Frame
	errs   chan error
}

func newTestPeer(conn net.Conn) *testPeer {
	p := &testPeer{conn: conn, frames: make(chan Frame, 64), errs: make(chan error, 1)}
	go func() {
		parser := NewParser()
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				parser.Feed(buf[:n], func(t FrameType, flags uint8, payload []byte) {
					cp := append([]byte(nil), payload...)
					p.frames <- Frame{Type: t, Flags: flags, Payload: cp}
				})
			}
			if err != nil {
				p.errs <- err
				return
			}
		}
	}()
	return p
}

func (p *testPeer) next(t *testing.T, timeout time.Duration) Frame {
	t.Helper()
	select {
	case f := <-p.frames:
		return f
	case err := <-p.errs:
		t.Fatalf("peer read failed: %v", err)
	case <-time.After(timeout):
		t.Fatal("timed out waiting for client frame")
	}
	return Frame{}
}

func (p *testPeer) send(t *testing.T, typ FrameType, payload []byte) {
	t.Helper()
	buf, err := Encode(typ, 0, payload)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := p.conn.Write(buf); err != nil {
		t.Fatalf("peer write: %v", err)
	}
}

func TestHandshakeSuccess(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	peer := newTestPeer(server)

	wantCfg := UartCfg{Baud: 921600, DataBits: 8, Parity: ParityNone, StopBits: 1, Flow: FlowNone}
	cfgBytes, err := wantCfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	serverDone := make(chan error, 1)
	go func() {
		f, err := peerWait(peer, 2*time.Second)
		if err != nil {
			serverDone <- err
			return
		}
		hello, perr := ParseHello(f.Payload)
		if f.Type != TypeHello || perr != nil || hello.ProtoVer != Version || hello.MaxPayload != MaxPayload {
			serverDone <- errors.New("bad HELLO")
			return
		}
		peer.send(t, TypeHelloAck, buildDevInfoPayload())

		f, err = peerWait(peer, 2*time.Second)
		if err != nil {
			serverDone <- err
			return
		}
		if f.Type != TypeUartCfg {
			serverDone <- errors.New("expected UART_CFG")
			return
		}
		gotCfg, perr := ParseUartCfg(f.Payload)
		if perr != nil || gotCfg != wantCfg {
			serverDone <- errors.New("bad UART_CFG payload")
			return
		}
		// Deliver some UART bytes before the ACK: the handshake must queue
		// them, not drop them.
		peer.send(t, TypeData, []byte("early"))
		peer.send(t, TypeUartCfgAck, cfgBytes)
		serverDone <- nil
	}()

	sess := NewSession(client)
	res, err := sess.Handshake(context.Background(), HandshakeOptions{
		UartCfg:      &wantCfg,
		HelloTimeout: 2 * time.Second,
		CfgTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
	if res.Info.Baud != 921600 || res.Info.Chip != "esp32c3" {
		t.Fatalf("unexpected info: %+v", res.Info)
	}
	if res.UartCfgAck == nil || *res.UartCfgAck != wantCfg {
		t.Fatalf("unexpected cfg ack: %+v", res.UartCfgAck)
	}
	if res.CfgErr != nil {
		t.Fatalf("unexpected cfg error: %v", res.CfgErr)
	}

	// The DATA frame sent before UART_CFG_ACK must still be delivered.
	f, err := sess.NextFrame()
	if err != nil {
		t.Fatalf("NextFrame: %v", err)
	}
	if f.Type != TypeData || string(f.Payload) != "early" {
		t.Fatalf("queued frame = %+v, want DATA 'early'", f)
	}
}

func peerWait(p *testPeer, timeout time.Duration) (Frame, error) {
	select {
	case f := <-p.frames:
		return f, nil
	case err := <-p.errs:
		return Frame{}, err
	case <-time.After(timeout):
		return Frame{}, errors.New("peer timeout")
	}
}

func TestHandshakeBusyIsFatal(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	peer := newTestPeer(server)

	go func() {
		if _, err := peerWait(peer, 2*time.Second); err != nil {
			return
		}
		peer.send(t, TypeError, append([]byte{byte(ErrBusy)}, []byte("busy!")...))
	}()

	sess := NewSession(client)
	_, err := sess.Handshake(context.Background(), HandshakeOptions{HelloTimeout: 2 * time.Second})
	if err == nil {
		t.Fatal("Handshake succeeded despite ERROR(BUSY)")
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != ErrBusy {
		t.Fatalf("err = %v, want ProtocolError(BUSY)", err)
	}
}

func TestHandshakeCfgRejectedIsNonFatal(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	peer := newTestPeer(server)

	cfg := UartCfg{Baud: 9600, DataBits: 8, StopBits: 1}
	go func() {
		if _, err := peerWait(peer, 2*time.Second); err != nil {
			return
		}
		peer.send(t, TypeHelloAck, buildDevInfoPayload())
		if _, err := peerWait(peer, 2*time.Second); err != nil {
			return
		}
		peer.send(t, TypeError, []byte{byte(ErrInvalidArg)})
	}()

	sess := NewSession(client)
	res, err := sess.Handshake(context.Background(), HandshakeOptions{
		UartCfg:      &cfg,
		HelloTimeout: 2 * time.Second,
		CfgTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if res.CfgErr == nil || res.CfgErr.Code != ErrInvalidArg {
		t.Fatalf("CfgErr = %+v, want INVALID_ARG", res.CfgErr)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	peer := newTestPeer(server)
	go func() { _, _ = peerWait(peer, 2*time.Second) }() // consume HELLO, never answer

	sess := NewSession(client)
	start := time.Now()
	_, err := sess.Handshake(context.Background(), HandshakeOptions{HelloTimeout: 120 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("err = %v, want timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
}

func TestNextFrameSkipsCorruptFrame(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	peer := newTestPeer(server)

	go func() {
		bad := rawFrame(1, 0x2, 0, []byte("bad"))
		bad[6] ^= 0xFF
		_, _ = server.Write(bad)
		peer.send(t, TypeData, []byte("good"))
	}()

	sess := NewSession(client)
	f, err := sess.NextFrame()
	if err != nil {
		t.Fatalf("NextFrame: %v", err)
	}
	if f.Type != TypeData || string(f.Payload) != "good" {
		t.Fatalf("frame = %+v", f)
	}
	if badCRC, _, _ := sess.ParserStats(); badCRC != 1 {
		t.Fatalf("BadCRC = %d, want 1", badCRC)
	}
}

func TestWriteDataChunks(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	peer := newTestPeer(server)

	payload := make([]byte, MaxPayload*2+10)
	for i := range payload {
		payload[i] = byte(i)
	}
	go func() {
		_ = NewSession(client).WriteData(payload)
	}()

	var got []byte
	for len(got) < len(payload) {
		f, err := peerWait(peer, 2*time.Second)
		if err != nil {
			t.Fatalf("peer: %v", err)
		}
		if f.Type != TypeData {
			t.Fatalf("unexpected frame type %v", f.Type)
		}
		if len(f.Payload) > MaxPayload {
			t.Fatalf("DATA frame too large: %d", len(f.Payload))
		}
		got = append(got, f.Payload...)
	}
	if len(got) != len(payload) {
		t.Fatalf("got %d bytes, want %d", len(got), len(payload))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], payload[i])
		}
	}
}
