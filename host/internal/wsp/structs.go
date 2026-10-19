package wsp

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// ProtocolError is returned when the device reports an ERROR frame or when the
// peer violates the protocol in a way the host must surface to the user.
type ProtocolError struct {
	Code ErrCode
	Msg  string
}

// Error implements error.
func (e *ProtocolError) Error() string {
	msg := strings.TrimSpace(e.Msg)
	if msg == "" {
		return fmt.Sprintf("设备返回 ERROR(%s)", e.Code)
	}
	return fmt.Sprintf("设备返回 ERROR(%s): %s", e.Code, msg)
}

// Fatal reports whether the connection should be torn down after receiving
// this error. Per docs/protocol.md §3.6 only BUSY and BAD_VERSION are fatal;
// everything else is informational and the session may continue.
func (e *ProtocolError) Fatal() bool {
	return e.Code == ErrBusy || e.Code == ErrBadVersion
}

// ParseErrorPayload decodes a WSP ERROR frame payload: one code byte followed
// by an optional UTF-8 message (not necessarily NUL terminated).
func ParseErrorPayload(p []byte) *ProtocolError {
	if len(p) == 0 {
		return &ProtocolError{Code: ErrNone, Msg: "空 ERROR payload"}
	}
	return &ProtocolError{Code: ErrCode(p[0]), Msg: string(p[1:])}
}

// Hello is the wsp_hello_t payload (4 bytes, C->D).
type Hello struct {
	ProtoVer   uint8
	Flags      uint8
	MaxPayload uint16
}

// NewHello returns the greeting used by this client.
func NewHello() Hello {
	return Hello{ProtoVer: Version, Flags: 0, MaxPayload: MaxPayload}
}

// Marshal encodes the 4-byte payload.
func (h Hello) Marshal() []byte {
	b := make([]byte, 4)
	b[0] = h.ProtoVer
	b[1] = h.Flags
	binary.LittleEndian.PutUint16(b[2:], h.MaxPayload)
	return b
}

// ParseHello decodes a wsp_hello_t payload (at least 4 bytes).
func ParseHello(p []byte) (Hello, error) {
	if len(p) < 4 {
		return Hello{}, fmt.Errorf("wsp: HELLO payload 长度为 %d，需要 4", len(p))
	}
	return Hello{
		ProtoVer:   p[0],
		Flags:      p[1],
		MaxPayload: binary.LittleEndian.Uint16(p[2:]),
	}, nil
}

// DevInfo is the wsp_dev_info_t payload (62 bytes, D->C), carried by both
// HELLO_ACK and INFO frames.
type DevInfo struct {
	ProtoVer   uint8
	FwMajor    uint8
	FwMinor    uint8
	FwPatch    uint8
	MaxPayload uint32
	Baud       uint32
	DataBits   uint8
	Parity     uint8
	StopBits   uint8
	Flow       uint8
	Name       string
	MAC        [6]byte
	Chip       string
	UptimeS    uint32
	WSPPort    uint16
	RAWPort    uint16
	Reserved   [8]byte
}

// devInfoSize is the fixed wsp_dev_info_t size on the wire.
const devInfoSize = 62

// ParseDevInfo decodes a wsp_dev_info_t payload. Payloads longer than 62 bytes
// are accepted and the trailing bytes ignored, per the versioning rules in
// docs/protocol.md §8.
func ParseDevInfo(p []byte) (DevInfo, error) {
	if len(p) < devInfoSize {
		return DevInfo{}, fmt.Errorf("wsp: dev_info payload 长度为 %d，需要 %d", len(p), devInfoSize)
	}
	return DevInfo{
		ProtoVer:   p[0],
		FwMajor:    p[1],
		FwMinor:    p[2],
		FwPatch:    p[3],
		MaxPayload: binary.LittleEndian.Uint32(p[4:]),
		Baud:       binary.LittleEndian.Uint32(p[8:]),
		DataBits:   p[12],
		Parity:     p[13],
		StopBits:   p[14],
		Flow:       p[15],
		Name:       cString(p[16:32]),
		MAC:        [6]byte(p[32:38]),
		Chip:       cString(p[38:46]),
		UptimeS:    binary.LittleEndian.Uint32(p[46:]),
		WSPPort:    binary.LittleEndian.Uint16(p[50:]),
		RAWPort:    binary.LittleEndian.Uint16(p[52:]),
		Reserved:   [8]byte(p[54:62]),
	}, nil
}

// cString converts a NUL-padded C char array to a Go string.
func cString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// MACString formats the WiFi MAC as AA:BB:CC:DD:EE:FF.
func (d DevInfo) MACString() string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X",
		d.MAC[0], d.MAC[1], d.MAC[2], d.MAC[3], d.MAC[4], d.MAC[5])
}

// FWVersion formats the firmware version as vMAJOR.MINOR.PATCH.
func (d DevInfo) FWVersion() string {
	return fmt.Sprintf("v%d.%d.%d", d.FwMajor, d.FwMinor, d.FwPatch)
}

// UartCfg returns the device's currently active UART configuration.
func (d DevInfo) UartCfg() UartCfg {
	return UartCfg{
		Baud:     d.Baud,
		DataBits: d.DataBits,
		Parity:   d.Parity,
		StopBits: d.StopBits,
		Flow:     d.Flow,
	}
}

// UartCfg is the wsp_uart_cfg_t payload (8 bytes, both directions).
type UartCfg struct {
	Baud     uint32
	DataBits uint8
	Parity   uint8
	StopBits uint8
	Flow     uint8
}

// Validate reports whether every field is inside the ranges allowed by the
// device. Invalid values must never be sent: the device would answer
// ERROR(INVALID_ARG) and keep its old configuration.
func (c UartCfg) Validate() error {
	if c.Baud < 50 || c.Baud > 5000000 {
		return fmt.Errorf("wsp: 非法波特率 %d（允许 50..5000000）", c.Baud)
	}
	if c.DataBits < 5 || c.DataBits > 8 {
		return fmt.Errorf("wsp: 非法数据位 %d（允许 5..8）", c.DataBits)
	}
	if c.Parity > 2 {
		return fmt.Errorf("wsp: 非法校验位 %d（允许 0/1/2）", c.Parity)
	}
	if c.StopBits < 1 || c.StopBits > 2 {
		return fmt.Errorf("wsp: 非法停止位 %d（允许 1/2）", c.StopBits)
	}
	if c.Flow > 1 {
		return fmt.Errorf("wsp: 非法流控 %d（允许 0=无, 1=RTS/CTS）", c.Flow)
	}
	return nil
}

// String implements fmt.Stringer.
func (c UartCfg) String() string {
	flow := "none"
	if c.Flow == FlowRTSCTS {
		flow = "rtscts"
	}
	return fmt.Sprintf("%d %d%c%d flow=%s", c.Baud, c.DataBits, parityLetter(c.Parity), c.StopBits, flow)
}

func parityLetter(p uint8) byte {
	switch p {
	case ParityOdd:
		return 'O'
	case ParityEven:
		return 'E'
	default:
		return 'N'
	}
}

// Marshal encodes the 8-byte payload. It returns an error for invalid values.
func (c UartCfg) Marshal() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:], c.Baud)
	b[4] = c.DataBits
	b[5] = c.Parity
	b[6] = c.StopBits
	b[7] = c.Flow
	return b, nil
}

// ParseUartCfg decodes a wsp_uart_cfg_t payload (at least 8 bytes).
func ParseUartCfg(p []byte) (UartCfg, error) {
	if len(p) < 8 {
		return UartCfg{}, fmt.Errorf("wsp: uart_cfg payload 长度为 %d，需要 8", len(p))
	}
	return UartCfg{
		Baud:     binary.LittleEndian.Uint32(p[0:]),
		DataBits: p[4],
		Parity:   p[5],
		StopBits: p[6],
		Flow:     p[7],
	}, nil
}

// LineStatus is the wsp_line_status_t payload (2 bytes, D->C).
type LineStatus struct {
	State    uint8
	Reserved uint8
}

// ParseLineStatus decodes a LINE_STATUS payload (at least 2 bytes).
func ParseLineStatus(p []byte) (LineStatus, error) {
	if len(p) < 2 {
		return LineStatus{}, fmt.Errorf("wsp: line_status payload 长度为 %d，需要 2", len(p))
	}
	return LineStatus{State: p[0], Reserved: p[1]}, nil
}

// Stats is the wsp_stats_t payload (24 bytes, D->C).
type Stats struct {
	UartRXBytes   uint32
	UartTXBytes   uint32
	FramesIn      uint32
	FramesOut     uint32
	CRCErrors     uint32
	UartOverflows uint32
}

// ParseStats decodes a wsp_stats_t payload (at least 24 bytes).
func ParseStats(p []byte) (Stats, error) {
	if len(p) < 24 {
		return Stats{}, fmt.Errorf("wsp: stats payload 长度为 %d，需要 24", len(p))
	}
	return Stats{
		UartRXBytes:   binary.LittleEndian.Uint32(p[0:]),
		UartTXBytes:   binary.LittleEndian.Uint32(p[4:]),
		FramesIn:      binary.LittleEndian.Uint32(p[8:]),
		FramesOut:     binary.LittleEndian.Uint32(p[12:]),
		CRCErrors:     binary.LittleEndian.Uint32(p[16:]),
		UartOverflows: binary.LittleEndian.Uint32(p[20:]),
	}, nil
}

// String implements fmt.Stringer.
func (s Stats) String() string {
	return fmt.Sprintf("uart_rx=%d uart_tx=%d frames_in=%d frames_out=%d crc_errors=%d uart_overflows=%d",
		s.UartRXBytes, s.UartTXBytes, s.FramesIn, s.FramesOut, s.CRCErrors, s.UartOverflows)
}

// ResetCmd is the wsp_reset_cmd_t payload (4 bytes, C->D).
type ResetCmd struct {
	Action  uint8
	PulseMS uint16
}

// NewResetPulse returns a RESET_TARGET pulse command.
func NewResetPulse(pulseMS uint16) ResetCmd {
	return ResetCmd{Action: ResetPulse, PulseMS: pulseMS}
}

// Marshal encodes the 4-byte payload.
func (r ResetCmd) Marshal() []byte {
	b := make([]byte, 4)
	b[0] = r.Action
	b[1] = 0
	binary.LittleEndian.PutUint16(b[2:], r.PulseMS)
	return b
}

// ParseResetCmd decodes a wsp_reset_cmd_t payload (at least 4 bytes).
func ParseResetCmd(p []byte) (ResetCmd, error) {
	if len(p) < 4 {
		return ResetCmd{}, fmt.Errorf("wsp: reset_cmd payload 长度为 %d，需要 4", len(p))
	}
	return ResetCmd{Action: p[0], PulseMS: binary.LittleEndian.Uint16(p[2:])}, nil
}
