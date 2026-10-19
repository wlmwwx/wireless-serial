package wire

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// vectors copied from docs/protocol.md §2.2.
func TestProtocolVectors(t *testing.T) {
	if got := CRC16([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("CRC16(\"123456789\") = 0x%04X, want 0x29B1", got)
	}

	cases := []struct {
		name    string
		typ     byte
		payload []byte
		wantHex string
	}{
		{"DATA \"Hi\"", TypeData, []byte("Hi"), "57531200020011 0A4869"},
		{"HELLO", TypeHello, mustHex(t, "01000004"), "57531000040009 3701000004"},
		{"STATS request", TypeStats, nil, "575317000000AF07"},
		{"PING", TypePing, mustHex(t, "01020304"), "575318000400E0 1201020304"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, string(bytes.ReplaceAll([]byte(tc.wantHex), []byte(" "), nil)))
			got := EncodeFrame(tc.typ, 0, tc.payload)
			if !bytes.Equal(got, want) {
				t.Fatalf("encode = %X, want %X", got, want)
			}

			fr, err := ReadFrame(bufio.NewReader(bytes.NewReader(want)))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if fr.Type != tc.typ || fr.Flags != 0 || !bytes.Equal(fr.Payload, tc.payload) {
				t.Fatalf("decoded = %+v, want type=%d payload=%X", fr, tc.typ, tc.payload)
			}
		})
	}
}

func TestEncodeRejectsOversize(t *testing.T) {
	big := make([]byte, MaxPayload+1)
	if got := EncodeFrame(TypeData, 0, big); got != nil {
		t.Fatalf("EncodeFrame(%d bytes) = %d bytes, want nil", len(big), len(got))
	}
	ok := make([]byte, MaxPayload)
	if got := EncodeFrame(TypeData, 0, ok); len(got) != MaxFrame {
		t.Fatalf("EncodeFrame(1024) = %d bytes, want %d", len(got), MaxFrame)
	}
}

type chunkReader struct {
	data []byte
	max  int
	off  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.max {
		n = r.max
	}
	if n > len(r.data)-r.off {
		n = len(r.data) - r.off
	}
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

func TestReadFrameChunked(t *testing.T) {
	payloads := [][]byte{
		nil,
		{0x00},
		[]byte("hello"),
		bytes.Repeat([]byte{0x00, 0xFF, 0x57, 0x53}, 256), // 1024 bytes, full of magic
	}
	var stream []byte
	for i, p := range payloads {
		typ := byte(TypeData + i)
		stream = append(stream, EncodeFrame(typ, byte(i), p)...)
	}

	for _, chunk := range []int{1, 2, 3, 5, 7, 8, 9, 13} {
		t.Run(fmt.Sprintf("chunk=%d", chunk), func(t *testing.T) {
			br := bufio.NewReaderSize(&chunkReader{data: stream, max: chunk}, 1)
			for i, want := range payloads {
				fr, err := ReadFrame(br)
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				if fr.Type != byte(TypeData+i) || int(fr.Flags) != i {
					t.Fatalf("frame %d: type=%d flags=%d", i, fr.Type, fr.Flags)
				}
				if !bytes.Equal(fr.Payload, want) {
					t.Fatalf("frame %d: payload mismatch (%d vs %d bytes)", i, len(fr.Payload), len(want))
				}
			}
		})
	}
}

func TestResyncGarbage(t *testing.T) {
	frame := EncodeFrame(TypeData, 0, []byte("payload"))
	stream := append([]byte("garbage\x00\x57"), frame...)
	// note: the trailing stray 'W' is not magic, so it is skipped.
	br := bufio.NewReader(bytes.NewReader(stream))
	fr, err := ReadFrame(br)
	if err != nil {
		t.Fatalf("ReadFrame after garbage: %v", err)
	}
	if fr.Type != TypeData || string(fr.Payload) != "payload" {
		t.Fatalf("decoded %+v", fr)
	}

	// "W W S" must be treated as a frame start (same rule as the C parser).
	bad := append([]byte{'W', 'W', 'S', 0, 0, 0, 0}, frame...)
	br = bufio.NewReader(bytes.NewReader(bad))
	_, err = ReadFrame(br)
	var we *WireError
	if !errors.As(err, &we) || we.Code != ErrBadVersion || !we.Fatal {
		t.Fatalf("WWS prefix: err = %v, want fatal BAD_VERSION", err)
	}
}

func TestWireErrors(t *testing.T) {
	t.Run("bad version", func(t *testing.T) {
		f := EncodeFrame(TypeData, 0, []byte("x"))
		f[2] = (2 << 4) | (f[2] & 0x0F)
		_, err := ReadFrame(bufio.NewReader(bytes.NewReader(f)))
		var we *WireError
		if !errors.As(err, &we) || we.Code != ErrBadVersion || !we.Fatal {
			t.Fatalf("err = %v, want fatal BAD_VERSION", err)
		}
	})

	t.Run("bad length", func(t *testing.T) {
		// 1025 = 0x0401
		hdr := []byte{Magic0, Magic1, 0x12, 0x00, 0x01, 0x04}
		_, err := ReadFrame(bufio.NewReader(bytes.NewReader(hdr)))
		var we *WireError
		if !errors.As(err, &we) || we.Code != ErrBadLength || !we.Fatal {
			t.Fatalf("err = %v, want fatal BAD_LENGTH", err)
		}
	})

	t.Run("bad crc then recover", func(t *testing.T) {
		bad := EncodeFrame(TypePing, 0, []byte{1, 2, 3})
		bad[len(bad)-1] ^= 0xFF
		good := EncodeFrame(TypePing, 0, []byte{4, 5, 6})
		br := bufio.NewReader(bytes.NewReader(append(bad, good...)))

		_, err := ReadFrame(br)
		var we *WireError
		if !errors.As(err, &we) || we.Code != ErrBadCRC || we.Fatal {
			t.Fatalf("first frame err = %v, want recoverable BAD_CRC", err)
		}
		fr, err := ReadFrame(br)
		if err != nil {
			t.Fatalf("second frame: %v", err)
		}
		if fr.Type != TypePing || !bytes.Equal(fr.Payload, []byte{4, 5, 6}) {
			t.Fatalf("second frame = %+v", fr)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		f := EncodeFrame(TypeData, 0, []byte("abcdef"))
		_, err := ReadFrame(bufio.NewReader(bytes.NewReader(f[:len(f)-2])))
		if err == nil {
			t.Fatal("expected an error for a truncated frame")
		}
	})
}

func TestChunk(t *testing.T) {
	if got := Chunk(nil); got != nil {
		t.Fatalf("Chunk(nil) = %v", got)
	}
	data := make([]byte, 2500)
	for i := range data {
		data[i] = byte(i)
	}
	chunks := Chunk(data)
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(chunks))
	}
	if len(chunks[0]) != MaxPayload || len(chunks[1]) != MaxPayload || len(chunks[2]) != 2500-2*MaxPayload {
		t.Fatalf("chunk sizes = %d,%d,%d", len(chunks[0]), len(chunks[1]), len(chunks[2]))
	}
	var joined []byte
	for _, c := range chunks {
		joined = append(joined, c...)
	}
	if !bytes.Equal(joined, data) {
		t.Fatal("Chunk() did not preserve bytes")
	}
}

func TestTypeAndErrorNames(t *testing.T) {
	if TypeName(TypeData) != "DATA" || TypeName(0xD) != "TYPE_0xD" {
		t.Fatalf("TypeName mismatch: %q %q", TypeName(TypeData), TypeName(0xD))
	}
	if ErrName(ErrBusy) != "BUSY" || ErrName(42) != "ERROR_42" {
		t.Fatalf("ErrName mismatch: %q %q", ErrName(ErrBusy), ErrName(42))
	}
}
