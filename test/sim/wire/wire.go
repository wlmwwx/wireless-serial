// Package wire implements the WSP/1 frame codec used by the device simulator.
//
// It is deliberately independent from host/ (the host tool has its own Go
// implementation) and mirrors docs/protocol.md / firmware/include/wsp_proto.h:
//
//	offset size field
//	0      1    magic0 = 'W' (0x57)
//	1      1    magic1 = 'S' (0x53)
//	2      1    ver_type: high nibble = version (1), low nibble = type
//	3      1    flags (reserved)
//	4      2    payload length, little endian
//	6      2    CRC-16/CCITT-FALSE over bytes 0..5 + payload, little endian
//	8      L    payload
package wire

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	Magic0 = 0x57 // 'W'
	Magic1 = 0x53 // 'S'

	Version    = 1
	MaxPayload = 1024
	HeaderLen  = 8
	MaxFrame   = HeaderLen + MaxPayload

	// EmptyPayload is the size of a zero-length frame.
	EmptyPayload = HeaderLen
)

// Frame types (low nibble of byte 2).
const (
	TypeHello       = 0x0
	TypeHelloAck    = 0x1
	TypeData        = 0x2
	TypeUARTCfg     = 0x3
	TypeUARTCfgAck  = 0x4
	TypeLineCtrl    = 0x5
	TypeLineStatus  = 0x6
	TypeStats       = 0x7
	TypePing        = 0x8
	TypePong        = 0x9
	TypeError       = 0xA
	TypeResetTarget = 0xB
	TypeInfo        = 0xC
)

// ERROR codes (docs/protocol.md §3.6).
const (
	ErrNone        = 0
	ErrBadVersion  = 1
	ErrBadLength   = 2
	ErrBadCRC      = 3
	ErrNotHello    = 4
	ErrUnsupported = 5
	ErrInvalidArg  = 6
	ErrBusy        = 7
	ErrInternal    = 8
	ErrUARTFail    = 9
)

// LINE_CTRL / LINE_STATUS bits.
const (
	LineDTR   = 0x01
	LineRTS   = 0x02
	LineBREAK = 0x04

	StatusDTR   = 0x01
	StatusRTS   = 0x02
	StatusCTS   = 0x04
	StatusDSR   = 0x08
	StatusDCD   = 0x10
	StatusBREAK = 0x20
)

// TypeName returns a short human readable name for a frame type.
func TypeName(t byte) string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypeHelloAck:
		return "HELLO_ACK"
	case TypeData:
		return "DATA"
	case TypeUARTCfg:
		return "UART_CFG"
	case TypeUARTCfgAck:
		return "UART_CFG_ACK"
	case TypeLineCtrl:
		return "LINE_CTRL"
	case TypeLineStatus:
		return "LINE_STATUS"
	case TypeStats:
		return "STATS"
	case TypePing:
		return "PING"
	case TypePong:
		return "PONG"
	case TypeError:
		return "ERROR"
	case TypeResetTarget:
		return "RESET_TARGET"
	case TypeInfo:
		return "INFO"
	default:
		return fmt.Sprintf("TYPE_0x%X", t)
	}
}

// ErrName returns a short name for an ERROR code.
func ErrName(c byte) string {
	switch c {
	case ErrNone:
		return "NONE"
	case ErrBadVersion:
		return "BAD_VERSION"
	case ErrBadLength:
		return "BAD_LENGTH"
	case ErrBadCRC:
		return "BAD_CRC"
	case ErrNotHello:
		return "NOT_HELLO"
	case ErrUnsupported:
		return "UNSUPPORTED"
	case ErrInvalidArg:
		return "INVALID_ARG"
	case ErrBusy:
		return "BUSY"
	case ErrInternal:
		return "INTERNAL"
	case ErrUARTFail:
		return "UART_FAIL"
	default:
		return fmt.Sprintf("ERROR_%d", c)
	}
}

// CRC16 computes CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF, no
// reflection, no final XOR) over data.
func CRC16(data []byte) uint16 {
	return CRC16Update(0xFFFF, data)
}

// CRC16Update continues a CRC-16/CCITT-FALSE computation.
func CRC16Update(crc uint16, data []byte) uint16 {
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// EncodeFrame serialises one WSP/1 frame.  It returns nil when payload is
// larger than MaxPayload (the caller must never send such a frame).
func EncodeFrame(typ, flags byte, payload []byte) []byte {
	if len(payload) > MaxPayload {
		return nil
	}
	out := make([]byte, HeaderLen+len(payload))
	out[0] = Magic0
	out[1] = Magic1
	out[2] = byte(Version<<4) | (typ & 0x0F)
	out[3] = flags
	binary.LittleEndian.PutUint16(out[4:6], uint16(len(payload)))
	copy(out[HeaderLen:], payload)
	crc := CRC16Update(0xFFFF, out[:6])
	if len(payload) != 0 {
		crc = CRC16Update(crc, payload)
	}
	binary.LittleEndian.PutUint16(out[6:8], crc)
	return out
}

// Frame is one decoded WSP/1 frame.
type Frame struct {
	Type    byte
	Flags   byte
	Payload []byte
}

// WireError describes a protocol error detected while decoding a frame.
// Fatal errors leave the stream position undefined (the device should send
// ERROR and close); non-fatal errors are recoverable (the whole frame was
// consumed and decoding may continue).
type WireError struct {
	Code  byte
	Msg   string
	Fatal bool
}

func (e *WireError) Error() string {
	return fmt.Sprintf("%s: %s", ErrName(e.Code), e.Msg)
}

// Chunk splits raw bytes into WSP DATA payloads of at most MaxPayload bytes.
func Chunk(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	out := make([][]byte, 0, (len(data)+MaxPayload-1)/MaxPayload)
	for len(data) > 0 {
		n := len(data)
		if n > MaxPayload {
			n = MaxPayload
		}
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

// readMagic skips bytes until a frame magic ("W" "S") has been consumed.
// It implements the same resynchronisation rule as the C parser: a stray 'W'
// is remembered and re-tested, so "W W S" still starts a frame.
func readMagic(br *bufio.Reader) error {
	haveW := false
	for {
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		if !haveW {
			haveW = b == Magic0
			continue
		}
		switch b {
		case Magic1:
			return nil
		case Magic0:
			// Keep the newest 'W' as the candidate start.
		default:
			haveW = false
		}
	}
}

// ReadFrame reads one frame from br, resynchronising on garbage bytes.
//
// Errors returned:
//   - *WireError with Fatal=true: protocol violation after which the stream
//     cannot be trusted (bad version / bad length) — the device should send
//     ERROR and close the connection.
//   - *WireError with Fatal=false: the frame was complete but invalid (bad
//     CRC); the caller may reply ERROR(BAD_CRC) and continue reading.
//   - any other error: I/O error (timeout, EOF, ...).
func ReadFrame(br *bufio.Reader) (Frame, error) {
	if err := readMagic(br); err != nil {
		return Frame{}, err
	}

	var rest [4]byte // ver_type, flags, len_lo, len_hi
	if _, err := io.ReadFull(br, rest[:]); err != nil {
		return Frame{}, err
	}

	ver := rest[0] >> 4
	typ := rest[0] & 0x0F
	flags := rest[1]
	length := int(binary.LittleEndian.Uint16(rest[2:4]))

	if ver != Version {
		return Frame{}, &WireError{
			Code:  ErrBadVersion,
			Msg:   fmt.Sprintf("unsupported protocol version %d", ver),
			Fatal: true,
		}
	}
	if length > MaxPayload {
		return Frame{}, &WireError{
			Code:  ErrBadLength,
			Msg:   fmt.Sprintf("declared payload length %d exceeds %d", length, MaxPayload),
			Fatal: true,
		}
	}

	var crcWire [2]byte
	if _, err := io.ReadFull(br, crcWire[:]); err != nil {
		return Frame{}, err
	}
	payload := make([]byte, length)
	if length != 0 {
		if _, err := io.ReadFull(br, payload); err != nil {
			return Frame{}, err
		}
	}

	var pre [6]byte
	pre[0] = Magic0
	pre[1] = Magic1
	copy(pre[2:], rest[:])
	crc := CRC16Update(0xFFFF, pre[:])
	if length != 0 {
		crc = CRC16Update(crc, payload)
	}
	if crc != binary.LittleEndian.Uint16(crcWire[:]) {
		return Frame{}, &WireError{Code: ErrBadCRC, Msg: "CRC mismatch", Fatal: false}
	}

	return Frame{Type: typ, Flags: flags, Payload: payload}, nil
}
