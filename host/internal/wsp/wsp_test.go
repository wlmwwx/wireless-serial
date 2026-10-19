package wsp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// mustHex decodes a space separated hex string.
func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestCRC16Vector(t *testing.T) {
	if got := CRC16([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("CRC16(123456789) = 0x%04X, want 0x29B1", got)
	}
}

// TestEncodeVectors checks the five byte-exact vectors from docs/protocol.md.
func TestEncodeVectors(t *testing.T) {
	cases := []struct {
		name    string
		typ     FrameType
		payload []byte
		want    []byte
	}{
		{"DATA Hi", TypeData, []byte("Hi"), mustHex(t, "57 53 12 00 02 00 11 0A 48 69")},
		{"HELLO", TypeHello, []byte{0x01, 0x00, 0x00, 0x04}, mustHex(t, "57 53 10 00 04 00 09 37 01 00 00 04")},
		{"STATS request", TypeStats, nil, mustHex(t, "57 53 17 00 00 00 AF 07")},
		{"PING", TypePing, []byte{0x01, 0x02, 0x03, 0x04}, mustHex(t, "57 53 18 00 04 00 E0 12 01 02 03 04")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.typ, 0, tc.payload)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("Encode = % X, want % X", got, tc.want)
			}
		})
	}
}

// TestEncodeTooLong rejects payloads above MaxPayload.
func TestEncodeTooLong(t *testing.T) {
	if _, err := Encode(TypeData, 0, make([]byte, MaxPayload+1)); err == nil {
		t.Fatal("Encode accepted an oversized payload")
	}
	if _, err := Encode(TypeData, 0, make([]byte, MaxPayload)); err != nil {
		t.Fatalf("Encode rejected a maximum sized payload: %v", err)
	}
}

// rawFrame builds a frame by hand, allowing invalid version/length fields.
func rawFrame(version, typ, flags byte, payload []byte) []byte {
	buf := make([]byte, HeaderLen+len(payload))
	buf[0] = Magic0
	buf[1] = Magic1
	buf[2] = version<<4 | typ
	buf[3] = flags
	buf[4] = byte(len(payload) & 0xFF)
	buf[5] = byte(len(payload) >> 8)
	crc := crc16Update(0xFFFF, buf[:6])
	crc = crc16Update(crc, payload)
	buf[6] = byte(crc & 0xFF)
	buf[7] = byte(crc >> 8)
	copy(buf[HeaderLen:], payload)
	return buf
}

// TestParseVectors feeds each documented wire frame and checks the callback.
func TestParseVectors(t *testing.T) {
	cases := []struct {
		name    string
		wire    string
		typ     FrameType
		payload []byte
	}{
		{"DATA Hi", "57 53 12 00 02 00 11 0A 48 69", TypeData, []byte("Hi")},
		{"HELLO", "57 53 10 00 04 00 09 37 01 00 00 04", TypeHello, []byte{0x01, 0x00, 0x00, 0x04}},
		{"STATS request", "57 53 17 00 00 00 AF 07", TypeStats, nil},
		{"PING", "57 53 18 00 04 00 E0 12 01 02 03 04", TypePing, []byte{0x01, 0x02, 0x03, 0x04}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser()
			var got []Frame
			code := p.Feed(mustHex(t, tc.wire), func(typ FrameType, flags uint8, payload []byte) {
				cp := append([]byte(nil), payload...)
				got = append(got, Frame{Type: typ, Flags: flags, Payload: cp})
			})
			if code != ParseOK {
				t.Fatalf("Feed returned %v", code)
			}
			if len(got) != 1 {
				t.Fatalf("got %d frames, want 1", len(got))
			}
			if got[0].Type != tc.typ {
				t.Fatalf("type = %v, want %v", got[0].Type, tc.typ)
			}
			if !bytes.Equal(got[0].Payload, tc.payload) {
				t.Fatalf("payload = % X, want % X", got[0].Payload, tc.payload)
			}
		})
	}
}

// TestParserByteByByte feeds a multi-frame stream one byte at a time; the
// result must be identical to feeding it in one chunk.
func TestParserByteByByte(t *testing.T) {
	var stream []byte
	for _, f := range [][]byte{
		rawFrame(1, 0x2, 0, []byte("hello")),
		rawFrame(1, 0x9, 0, nil),
		rawFrame(1, 0x2, 0, bytes.Repeat([]byte{0xAB}, 300)),
	} {
		stream = append(stream, f...)
	}

	want := collectFrames(t, stream)
	if len(want) != 3 {
		t.Fatalf("stream parse got %d frames, want 3", len(want))
	}

	for split := 0; split < len(stream); split++ {
		p := NewParser()
		var got []Frame
		h := func(typ FrameType, flags uint8, payload []byte) {
			got = append(got, Frame{Type: typ, Flags: flags, Payload: append([]byte(nil), payload...)})
		}
		p.Feed(stream[:split], h)
		p.Feed(stream[split:], h)
		if !framesEqual(got, want) {
			t.Fatalf("split at %d produced different frames", split)
		}
	}

	// And one byte at a time.
	p := NewParser()
	var got []Frame
	for _, b := range stream {
		p.Feed([]byte{b}, func(typ FrameType, flags uint8, payload []byte) {
			got = append(got, Frame{Type: typ, Flags: flags, Payload: append([]byte(nil), payload...)})
		})
	}
	if !framesEqual(got, want) {
		t.Fatal("byte-by-byte parse produced different frames")
	}
}

// TestParserGarbageResync ensures junk before a valid frame is skipped.
func TestParserGarbageResync(t *testing.T) {
	p := NewParser()
	var frames []Frame
	stream := append([]byte("WX\x00garbageW"), rawFrame(1, 0x2, 0, []byte("ok"))...)
	code := p.Feed(stream, func(typ FrameType, flags uint8, payload []byte) {
		frames = append(frames, Frame{Type: typ, Payload: append([]byte(nil), payload...)})
	})
	if code != ParseOK {
		t.Fatalf("Feed = %v, want OK", code)
	}
	if len(frames) != 1 || string(frames[0].Payload) != "ok" {
		t.Fatalf("frames = %+v, want one DATA 'ok'", frames)
	}
	if p.Resync == 0 {
		t.Fatal("Resync counter did not increase")
	}
}

// TestParserCRCErrorResync ensures a corrupt frame is dropped and the parser
// recovers on the next frame.
func TestParserCRCErrorResync(t *testing.T) {
	bad := rawFrame(1, 0x2, 0, []byte("bad"))
	bad[6] ^= 0xFF // corrupt CRC
	good := rawFrame(1, 0x2, 0, []byte("good"))

	p := NewParser()
	var frames []Frame
	h := func(typ FrameType, flags uint8, payload []byte) {
		frames = append(frames, Frame{Type: typ, Payload: append([]byte(nil), payload...)})
	}
	code := p.Feed(append(bad, good...), h)
	if code != ParseErrCRC {
		t.Fatalf("Feed = %v, want E_CRC", code)
	}
	if p.BadCRC != 1 {
		t.Fatalf("BadCRC = %d, want 1", p.BadCRC)
	}
	if len(frames) != 1 || string(frames[0].Payload) != "good" {
		t.Fatalf("frames = %+v, want only the good one", frames)
	}
}

// TestParserOversizeLength rejects a declared length above 1024.
func TestParserOversizeLength(t *testing.T) {
	// Hand-build a header claiming 1025 bytes (0x0401 little endian).
	hdr := []byte{Magic0, Magic1, 0x12, 0x00, 0x01, 0x04}
	crc := crc16Update(0xFFFF, hdr)
	wire := append(append(append([]byte{}, hdr...), byte(crc), byte(crc>>8)), []byte("junkjunk")...)

	p := NewParser()
	frames := 0
	code := p.Feed(wire, func(FrameType, uint8, []byte) { frames++ })
	if code != ParseErrLength {
		t.Fatalf("Feed = %v, want E_LENGTH", code)
	}
	if frames != 0 {
		t.Fatalf("delivered %d frames, want 0", frames)
	}
}

// TestParserVersionMismatch rejects frames carrying a different version.
func TestParserVersionMismatch(t *testing.T) {
	p := NewParser()
	frames := 0
	code := p.Feed(rawFrame(2, 0x2, 0, []byte("x")), func(FrameType, uint8, []byte) { frames++ })
	if code != ParseErrVersion {
		t.Fatalf("Feed = %v, want E_VERSION", code)
	}
	if p.BadVersion != 1 {
		t.Fatalf("BadVersion = %d, want 1", p.BadVersion)
	}
	if frames != 0 {
		t.Fatalf("delivered %d frames, want 0", frames)
	}
}

// TestParserMagicRestart covers the "W W S" restart path.
func TestParserMagicRestart(t *testing.T) {
	stream := append([]byte{Magic0, Magic0}, rawFrame(1, 0x2, 0, []byte("hi"))...)
	p := NewParser()
	var got []byte
	code := p.Feed(stream, func(_ FrameType, _ uint8, payload []byte) { got = append(got, payload...) })
	if code != ParseOK {
		t.Fatalf("Feed = %v", code)
	}
	if string(got) != "hi" {
		t.Fatalf("payload = %q, want \"hi\"", got)
	}
}

// TestParserFlagsAndUnknownType checks flags and unknown frame types pass
// through untouched.
func TestParserFlagsAndUnknownType(t *testing.T) {
	p := NewParser()
	var f Frame
	code := p.Feed(rawFrame(1, 0xF, 0xA5, []byte("future")), func(typ FrameType, flags uint8, payload []byte) {
		f = Frame{Type: typ, Flags: flags, Payload: append([]byte(nil), payload...)}
	})
	if code != ParseOK || f.Type != FrameType(0xF) || f.Flags != 0xA5 || string(f.Payload) != "future" {
		t.Fatalf("got %+v code=%v", f, code)
	}
}

func collectFrames(t *testing.T, stream []byte) []Frame {
	t.Helper()
	p := NewParser()
	var out []Frame
	p.Feed(stream, func(typ FrameType, flags uint8, payload []byte) {
		out = append(out, Frame{Type: typ, Flags: flags, Payload: append([]byte(nil), payload...)})
	})
	return out
}

func framesEqual(a, b []Frame) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Flags != b[i].Flags || !bytes.Equal(a[i].Payload, b[i].Payload) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Structure codecs
// ---------------------------------------------------------------------------

func TestHelloRoundTrip(t *testing.T) {
	h := NewHello()
	b := h.Marshal()
	if len(b) != 4 || b[0] != 1 || b[2] != 0x00 || b[3] != 0x04 {
		t.Fatalf("Hello marshal = % X", b)
	}
	got, err := ParseHello(b)
	if err != nil || got != h {
		t.Fatalf("ParseHello = %+v, %v; want %+v", got, err, h)
	}
	if _, err := ParseHello(b[:3]); err == nil {
		t.Fatal("ParseHello accepted a short payload")
	}
}

func buildDevInfoPayload() []byte {
	p := make([]byte, devInfoSize)
	p[0] = 1 // proto_ver
	p[1], p[2], p[3] = 0, 2, 3
	binary.LittleEndian.PutUint32(p[4:], 1024)
	binary.LittleEndian.PutUint32(p[8:], 921600)
	p[12], p[13], p[14], p[15] = 8, 2, 1, 1
	copy(p[16:32], "wserial-test")
	copy(p[32:38], []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF})
	copy(p[38:46], "esp32c3")
	binary.LittleEndian.PutUint32(p[46:], 12345)
	binary.LittleEndian.PutUint16(p[50:], 3333)
	binary.LittleEndian.PutUint16(p[52:], 3334)
	return p
}

func TestDevInfoParse(t *testing.T) {
	info, err := ParseDevInfo(buildDevInfoPayload())
	if err != nil {
		t.Fatalf("ParseDevInfo: %v", err)
	}
	if info.ProtoVer != 1 || info.FWVersion() != "v0.2.3" {
		t.Fatalf("unexpected version fields: %+v", info)
	}
	if info.Baud != 921600 || info.DataBits != 8 || info.Parity != ParityEven || info.StopBits != 1 || info.Flow != FlowRTSCTS {
		t.Fatalf("unexpected uart fields: %+v", info)
	}
	if info.Name != "wserial-test" || info.Chip != "esp32c3" {
		t.Fatalf("unexpected strings: %+v", info)
	}
	if info.MACString() != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("MACString = %q", info.MACString())
	}
	if info.WSPPort != 3333 || info.RAWPort != 3334 || info.UptimeS != 12345 {
		t.Fatalf("unexpected ports/uptime: %+v", info)
	}
	if got := info.UartCfg(); got.Baud != 921600 || got.Parity != ParityEven || got.Flow != FlowRTSCTS {
		t.Fatalf("UartCfg = %+v", got)
	}

	// Longer payloads must be accepted (versioning rule: append-only fields).
	longer := append(buildDevInfoPayload(), 0xDE, 0xAD, 0xBE, 0xEF)
	if _, err := ParseDevInfo(longer); err != nil {
		t.Fatalf("ParseDevInfo rejected a longer payload: %v", err)
	}
	if _, err := ParseDevInfo(longer[:devInfoSize-1]); err == nil {
		t.Fatal("ParseDevInfo accepted a short payload")
	}
}

func TestUartCfgCodec(t *testing.T) {
	cfg := UartCfg{Baud: 115200, DataBits: 8, Parity: ParityNone, StopBits: 1, Flow: FlowNone}
	b, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := []byte{0x00, 0xC2, 0x01, 0x00, 0x08, 0x00, 0x01, 0x00}
	if !bytes.Equal(b, want) {
		t.Fatalf("Marshal = % X, want % X", b, want)
	}
	got, err := ParseUartCfg(b)
	if err != nil || got != cfg {
		t.Fatalf("ParseUartCfg = %+v, %v", got, err)
	}
	if _, err := ParseUartCfg(b[:7]); err == nil {
		t.Fatal("ParseUartCfg accepted a short payload")
	}
}

func TestUartCfgValidation(t *testing.T) {
	good := UartCfg{Baud: 9600, DataBits: 8, Parity: 0, StopBits: 1, Flow: 0}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := []UartCfg{
		{Baud: 49, DataBits: 8, StopBits: 1},
		{Baud: 5000001, DataBits: 8, StopBits: 1},
		{Baud: 9600, DataBits: 4, StopBits: 1},
		{Baud: 9600, DataBits: 9, StopBits: 1},
		{Baud: 9600, DataBits: 8, Parity: 3, StopBits: 1},
		{Baud: 9600, DataBits: 8, StopBits: 0},
		{Baud: 9600, DataBits: 8, StopBits: 3},
		{Baud: 9600, DataBits: 8, StopBits: 1, Flow: 2},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Fatalf("case %d: invalid config accepted: %+v", i, c)
		}
		if _, err := c.Marshal(); err == nil {
			t.Fatalf("case %d: Marshal accepted invalid config: %+v", i, c)
		}
	}
}

func TestLineStatusAndStatsCodec(t *testing.T) {
	ls, err := ParseLineStatus([]byte{StatusDTR | StatusCTS, 0})
	if err != nil || ls.State != StatusDTR|StatusCTS {
		t.Fatalf("ParseLineStatus = %+v, %v", ls, err)
	}
	if _, err := ParseLineStatus([]byte{0}); err == nil {
		t.Fatal("ParseLineStatus accepted a short payload")
	}

	st := Stats{UartRXBytes: 1, UartTXBytes: 2, FramesIn: 3, FramesOut: 4, CRCErrors: 5, UartOverflows: 6}
	p := make([]byte, 24)
	for i, v := range []uint32{1, 2, 3, 4, 5, 6} {
		binary.LittleEndian.PutUint32(p[i*4:], v)
	}
	got, err := ParseStats(p)
	if err != nil || got != st {
		t.Fatalf("ParseStats = %+v, %v", got, err)
	}
	if !strings.Contains(got.String(), "uart_rx=1") {
		t.Fatalf("Stats.String = %q", got.String())
	}
	if _, err := ParseStats(p[:23]); err == nil {
		t.Fatal("ParseStats accepted a short payload")
	}
}

func TestErrorPayload(t *testing.T) {
	pe := ParseErrorPayload([]byte{byte(ErrInvalidArg), 'b', 'a', 'd'})
	if pe.Code != ErrInvalidArg || pe.Msg != "bad" {
		t.Fatalf("ParseErrorPayload = %+v", pe)
	}
	if pe.Fatal() {
		t.Fatal("INVALID_ARG must not be fatal")
	}
	if !(&ProtocolError{Code: ErrBusy}).Fatal() {
		t.Fatal("BUSY must be fatal")
	}
	if errStr := (&ProtocolError{Code: ErrBusy}).Error(); !strings.Contains(errStr, "BUSY") {
		t.Fatalf("Error() = %q", errStr)
	}
	empty := ParseErrorPayload(nil)
	if empty.Code != ErrNone {
		t.Fatalf("empty payload code = %v", empty.Code)
	}
}

func TestResetCmdCodec(t *testing.T) {
	cmd := NewResetPulse(120)
	b := cmd.Marshal()
	want := []byte{2, 0, 120, 0}
	if !bytes.Equal(b, want) {
		t.Fatalf("Marshal = % X, want % X", b, want)
	}
	got, err := ParseResetCmd(b)
	if err != nil || got != cmd {
		t.Fatalf("ParseResetCmd = %+v, %v", got, err)
	}
	if _, err := ParseResetCmd(b[:3]); err == nil {
		t.Fatal("ParseResetCmd accepted a short payload")
	}
}
