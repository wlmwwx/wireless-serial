// Command smoke is a minimal end-to-end check of a running simulator binary.
//
// It reads the JSON written by the simulator's -addrfile, performs a WSP/1
// handshake, verifies DATA in both directions through the target-hardware side
// and checks PING/PONG + STATS.  Used by test/run.sh; exits 0 on success.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"wirelessserial/sim/wire"
)

type addrFile struct {
	WSP string `json:"wsp"`
	RAW string `json:"raw"`
	HW  string `json:"hw"`
}

func main() {
	addrfile := flag.String("addrfile", "", "simulator addrfile (JSON) to connect to")
	timeout := flag.Duration("timeout", 5*time.Second, "per-operation timeout")
	flag.Parse()

	if err := run(*addrfile, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "smoke: FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(addrfile string, timeout time.Duration) error {
	if addrfile == "" {
		return fmt.Errorf("-addrfile is required")
	}
	raw, err := os.ReadFile(addrfile)
	if err != nil {
		return fmt.Errorf("read addrfile: %w", err)
	}
	var addrs addrFile
	if err := json.Unmarshal(raw, &addrs); err != nil {
		return fmt.Errorf("parse addrfile: %w", err)
	}
	if addrs.WSP == "" || addrs.HW == "" {
		return fmt.Errorf("addrfile missing wsp/hw address: %q", raw)
	}

	hw, err := net.DialTimeout("tcp", addrs.HW, timeout)
	if err != nil {
		return fmt.Errorf("dial hw %s: %w", addrs.HW, err)
	}
	defer func() { _ = hw.Close() }()

	wsp, err := net.DialTimeout("tcp", addrs.WSP, timeout)
	if err != nil {
		return fmt.Errorf("dial wsp %s: %w", addrs.WSP, err)
	}
	defer func() { _ = wsp.Close() }()
	br := bufio.NewReader(wsp)

	send := func(typ byte, payload []byte) error {
		f := wire.EncodeFrame(typ, 0, payload)
		if f == nil {
			return fmt.Errorf("cannot encode %s", wire.TypeName(typ))
		}
		_ = wsp.SetWriteDeadline(time.Now().Add(timeout))
		_, err := wsp.Write(f)
		return err
	}
	recv := func(want byte) (wire.Frame, error) {
		_ = wsp.SetReadDeadline(time.Now().Add(timeout))
		fr, err := wire.ReadFrame(br)
		if err != nil {
			return wire.Frame{}, err
		}
		if fr.Type != want {
			return wire.Frame{}, fmt.Errorf("got %s, want %s", wire.TypeName(fr.Type), wire.TypeName(want))
		}
		return fr, nil
	}

	// Handshake.
	if err := send(wire.TypeHello, []byte{wire.Version, 0, 0, 4}); err != nil {
		return fmt.Errorf("send HELLO: %w", err)
	}
	ack, err := recv(wire.TypeHelloAck)
	if err != nil {
		return fmt.Errorf("recv HELLO_ACK: %w", err)
	}
	if len(ack.Payload) != 62 {
		return fmt.Errorf("HELLO_ACK payload = %d bytes, want 62", len(ack.Payload))
	}

	// host -> target through the simulated UART.
	down := []byte("smoke-to-target")
	if err := send(wire.TypeData, down); err != nil {
		return fmt.Errorf("send DATA: %w", err)
	}
	gotDown := make([]byte, len(down))
	_ = hw.SetReadDeadline(time.Now().Add(timeout))
	if _, err := io.ReadFull(hw, gotDown); err != nil {
		return fmt.Errorf("read on hw side: %w", err)
	}
	if string(gotDown) != string(down) {
		return fmt.Errorf("target received %q, want %q", gotDown, down)
	}

	// target -> host through the simulated UART.
	up := []byte("smoke-to-host")
	if _, err := hw.Write(up); err != nil {
		return fmt.Errorf("write on hw side: %w", err)
	}
	fr, err := recv(wire.TypeData)
	if err != nil {
		return fmt.Errorf("recv DATA: %w", err)
	}
	if string(fr.Payload) != string(up) {
		return fmt.Errorf("host received %q, want %q", fr.Payload, up)
	}

	// PING/PONG.
	ping := []byte{0x01, 0x02, 0x03}
	if err := send(wire.TypePing, ping); err != nil {
		return fmt.Errorf("send PING: %w", err)
	}
	pong, err := recv(wire.TypePong)
	if err != nil {
		return fmt.Errorf("recv PONG: %w", err)
	}
	if string(pong.Payload) != string(ping) {
		return fmt.Errorf("PONG payload = %X, want %X", pong.Payload, ping)
	}

	// STATS sanity.
	if err := send(wire.TypeStats, nil); err != nil {
		return fmt.Errorf("send STATS: %w", err)
	}
	stats, err := recv(wire.TypeStats)
	if err != nil {
		return fmt.Errorf("recv STATS: %w", err)
	}
	if len(stats.Payload) != 24 {
		return fmt.Errorf("STATS payload = %d bytes, want 24", len(stats.Payload))
	}
	tx := binary.LittleEndian.Uint32(stats.Payload[4:8])
	rx := binary.LittleEndian.Uint32(stats.Payload[0:4])
	if tx < uint32(len(down)) || rx < uint32(len(up)) {
		return fmt.Errorf("STATS counters too small: tx=%d rx=%d", tx, rx)
	}

	fmt.Printf("SMOKE PASS: handshake ok, data %q/%q ok, ping/pong ok, stats tx=%d rx=%d (wsp=%s hw=%s)\n",
		down, up, tx, rx, addrs.WSP, addrs.HW)
	return nil
}
