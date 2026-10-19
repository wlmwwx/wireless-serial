package wsp

import "fmt"

// Frame is a fully decoded WSP frame.
type Frame struct {
	Type    FrameType
	Flags   uint8
	Payload []byte
}

// Encode serialises one frame, producing exactly the bytes defined in
// docs/protocol.md §2. flags is normally 0; len(payload) must be <= 1024.
func Encode(t FrameType, flags uint8, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("wsp: payload 过大: %d > %d", len(payload), MaxPayload)
	}
	buf := make([]byte, HeaderLen+len(payload))
	buf[0] = Magic0
	buf[1] = Magic1
	buf[2] = (Version << 4) | uint8(t&0x0F)
	buf[3] = flags
	buf[4] = byte(len(payload) & 0xFF)
	buf[5] = byte(len(payload) >> 8)
	copy(buf[HeaderLen:], payload)

	crc := crc16Update(0xFFFF, buf[:6])
	crc = crc16Update(crc, buf[HeaderLen:])
	buf[6] = byte(crc & 0xFF)
	buf[7] = byte(crc >> 8)
	return buf, nil
}

// AppendEncode appends one encoded frame to dst.
func AppendEncode(dst []byte, t FrameType, flags uint8, payload []byte) ([]byte, error) {
	start := len(dst)
	dst = append(dst, make([]byte, HeaderLen+len(payload))...)
	buf := dst[start:]
	if len(payload) > MaxPayload {
		return dst[:start], fmt.Errorf("wsp: payload 过大: %d > %d", len(payload), MaxPayload)
	}
	buf[0] = Magic0
	buf[1] = Magic1
	buf[2] = (Version << 4) | uint8(t&0x0F)
	buf[3] = flags
	buf[4] = byte(len(payload) & 0xFF)
	buf[5] = byte(len(payload) >> 8)
	copy(buf[HeaderLen:], payload)
	crc := crc16Update(0xFFFF, buf[:6])
	crc = crc16Update(crc, buf[HeaderLen:])
	buf[6] = byte(crc & 0xFF)
	buf[7] = byte(crc >> 8)
	return dst, nil
}
