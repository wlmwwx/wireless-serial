// Package wsp implements the host side of the Wireless Serial Protocol v1
// (WSP/1). The wire format and semantics mirror docs/protocol.md and
// firmware/main/include/wsp_proto.h byte for byte.
package wsp

const (
	// Magic0 and Magic1 start every frame.
	Magic0 byte = 0x57 // 'W'
	Magic1 byte = 0x53 // 'S'

	// Version is the WSP protocol version carried in the high nibble of
	// the frame's third byte.
	Version = 1

	// MaxPayload is the largest DATA payload allowed on the wire.
	MaxPayload = 1024

	// HeaderLen is the fixed frame header size (excluding payload).
	HeaderLen = 8

	// MaxFrame is the largest possible frame on the wire.
	MaxFrame = HeaderLen + MaxPayload

	// DefaultWSPPort is the device's default WSP/TCP port.
	DefaultWSPPort = 3333

	// DefaultRAWPort is the device's default RAW/TCP port.
	DefaultRAWPort = 3334

	// DefaultHTTPPort is the device's HTTP port (unused by this tool).
	DefaultHTTPPort = 80
)

// FrameType is the low nibble of the frame's third byte.
type FrameType uint8

const (
	TypeHello       FrameType = 0x0 // C->D  wsp_hello_t
	TypeHelloAck    FrameType = 0x1 // D->C  wsp_dev_info_t
	TypeData        FrameType = 0x2 // both  raw UART bytes
	TypeUartCfg     FrameType = 0x3 // C->D  wsp_uart_cfg_t
	TypeUartCfgAck  FrameType = 0x4 // D->C  wsp_uart_cfg_t (applied)
	TypeLineCtrl    FrameType = 0x5 // C->D  uint8 bitmask
	TypeLineStatus  FrameType = 0x6 // D->C  wsp_line_status_t
	TypeStats       FrameType = 0x7 // C->D empty / D->C wsp_stats_t
	TypePing        FrameType = 0x8 // both  opaque echo payload
	TypePong        FrameType = 0x9 // both  echo of a PING payload
	TypeError       FrameType = 0xA // D->C  wsp_error_t
	TypeResetTarget FrameType = 0xB // C->D  wsp_reset_cmd_t
	TypeInfo        FrameType = 0xC // C->D empty / D->C wsp_dev_info_t
)

// String implements fmt.Stringer.
func (t FrameType) String() string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypeHelloAck:
		return "HELLO_ACK"
	case TypeData:
		return "DATA"
	case TypeUartCfg:
		return "UART_CFG"
	case TypeUartCfgAck:
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
		return "UNKNOWN"
	}
}

// LINE_CTRL bitmask (host -> device).
const (
	LineDTR   uint8 = 0x01
	LineRTS   uint8 = 0x02
	LineBreak uint8 = 0x04
)

// LINE_STATUS state bits (device -> host).
const (
	StatusDTR   uint8 = 0x01
	StatusRTS   uint8 = 0x02
	StatusCTS   uint8 = 0x04
	StatusDSR   uint8 = 0x08
	StatusDCD   uint8 = 0x10
	StatusBreak uint8 = 0x20
)

// UART parity values used by wsp_uart_cfg_t.
const (
	ParityNone uint8 = 0
	ParityOdd  uint8 = 1
	ParityEven uint8 = 2
)

// UART flow-control values used by wsp_uart_cfg_t.
const (
	FlowNone   uint8 = 0
	FlowRTSCTS uint8 = 1
)

// RESET_TARGET actions.
const (
	ResetAssert  uint8 = 0
	ResetRelease uint8 = 1
	ResetPulse   uint8 = 2
)

// ErrCode is a WSP ERROR payload error code.
type ErrCode uint8

const (
	ErrNone        ErrCode = 0
	ErrBadVersion  ErrCode = 1
	ErrBadLength   ErrCode = 2
	ErrBadCRC      ErrCode = 3
	ErrNotHello    ErrCode = 4
	ErrUnsupported ErrCode = 5
	ErrInvalidArg  ErrCode = 6
	ErrBusy        ErrCode = 7
	ErrInternal    ErrCode = 8
	ErrUartFail    ErrCode = 9
)

// String implements fmt.Stringer.
func (c ErrCode) String() string {
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
	case ErrUartFail:
		return "UART_FAIL"
	default:
		return "UNKNOWN"
	}
}
