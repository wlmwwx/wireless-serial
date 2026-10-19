package wsp

// ParseCode mirrors the WSP_E_* return values of wsp_parser_feed() in
// firmware/main/wsp_proto.c. Zero is success; negative values are recoverable
// errors that were applied to a dropped frame while parsing continued.
type ParseCode int

const (
	ParseOK         ParseCode = 0
	ParseErrMagic   ParseCode = -1 // resynchronised after a garbage byte
	ParseErrCRC     ParseCode = -2 // frame dropped, parser resynchronised
	ParseErrLength  ParseCode = -3 // declared length > MaxPayload
	ParseErrNoMem   ParseCode = -4 // bad argument / not initialised
	ParseErrVersion ParseCode = -5 // unsupported protocol version
)

// String implements fmt.Stringer.
func (c ParseCode) String() string {
	switch c {
	case ParseOK:
		return "OK"
	case ParseErrMagic:
		return "E_MAGIC"
	case ParseErrCRC:
		return "E_CRC"
	case ParseErrLength:
		return "E_LENGTH"
	case ParseErrNoMem:
		return "E_NOMEM"
	case ParseErrVersion:
		return "E_VERSION"
	default:
		return "E_UNKNOWN"
	}
}

// FrameHandler receives one complete, CRC-valid frame. The payload slice
// aliases the parser's internal buffer and is only valid for the duration of
// the call; copy it if it must outlive the callback.
type FrameHandler func(t FrameType, flags uint8, payload []byte)

// Parser is a streaming WSP frame decoder. Its state machine, resynchronisation
// rules and error accounting are identical to the C reference implementation.
type Parser struct {
	state       uint8
	typ         uint8
	flags       uint8
	plen        uint16
	hdrUsed     uint16
	payloadUsed uint16
	crcCalc     uint16
	crcWire     uint16
	header      [6]byte
	payload     [MaxPayload]byte

	// Statistics, mirroring stat_* in wsp_parser_t.
	BadCRC     uint32 // frames dropped because of a CRC error
	BadVersion uint32 // frames dropped because of a version mismatch
	Resync     uint32 // garbage bytes skipped while hunting for the magic
}

// Parser states; the numeric values follow the C implementation.
const (
	stMagic0  uint8 = 0
	stMagic1  uint8 = 1
	stHeader  uint8 = 2
	stPayload uint8 = 3
	stCRC0    uint8 = 4
	stCRC1    uint8 = 5
)

// NewParser returns a parser in the idle state.
func NewParser() *Parser {
	p := &Parser{}
	p.Reset()
	return p
}

// Reset returns the parser to the idle state. Statistics are preserved.
func (p *Parser) Reset() {
	if p == nil {
		return
	}
	p.state = stMagic0
	p.typ = 0
	p.flags = 0
	p.plen = 0
	p.hdrUsed = 0
	p.payloadUsed = 0
	p.crcCalc = 0
	p.crcWire = 0
}

// headerDone parses the completed six-byte pre-CRC header.
// It mirrors header_done() in wsp_proto.c.
func (p *Parser) headerDone() ParseCode {
	p.typ = p.header[2] & 0x0F
	p.flags = p.header[3]
	p.plen = uint16(p.header[4]) | uint16(p.header[5])<<8

	ver := p.header[2] >> 4
	if ver != Version {
		p.BadVersion++
		p.state = stMagic0
		return ParseErrVersion
	}
	if p.plen > MaxPayload {
		p.state = stMagic0
		return ParseErrLength
	}
	p.crcCalc = crc16Update(0xFFFF, p.header[:])
	p.payloadUsed = 0
	if p.plen == 0 {
		p.state = stMagic0
		return ParseOK
	}
	p.state = stPayload
	return ParseOK
}

// Feed pushes len(data) bytes through the parser. Every byte is consumed;
// complete valid frames are delivered through h in order. The return value is
// the last recoverable error encountered while parsing this chunk (the
// offending frame is dropped and parsing continues).
func (p *Parser) Feed(data []byte, h FrameHandler) ParseCode {
	if p == nil {
		return ParseErrNoMem
	}
	lastErr := ParseOK
	for _, b := range data {
		switch p.state {
		case stMagic0:
			if b == Magic0 {
				p.header[0] = b
				p.hdrUsed = 1
				p.state = stMagic1
			} else {
				p.Resync++
			}

		case stMagic1:
			switch b {
			case Magic1:
				p.header[1] = b
				p.hdrUsed = 2
				p.state = stHeader
			case Magic0:
				// Possible restart: "W W S".
				p.header[0] = b
				p.hdrUsed = 1
				// Stay in stMagic1.
			default:
				p.Resync++
				p.state = stMagic0
			}

		case stHeader:
			p.header[p.hdrUsed] = b
			p.hdrUsed++
			if p.hdrUsed == 6 {
				if rc := p.headerDone(); rc != ParseOK {
					lastErr = rc // headerDone already reset the state
				} else {
					p.state = stCRC0
				}
			}

		case stCRC0:
			p.crcWire = uint16(b)
			p.state = stCRC1

		case stCRC1:
			p.crcWire |= uint16(b) << 8
			if p.plen == 0 {
				if p.crcWire != p.crcCalc {
					p.BadCRC++
					lastErr = ParseErrCRC
				} else if h != nil {
					h(FrameType(p.typ), p.flags, p.payload[:0])
				}
				p.state = stMagic0
			} else {
				p.state = stPayload
			}

		case stPayload:
			p.payload[p.payloadUsed] = b
			p.payloadUsed++
			if p.payloadUsed == p.plen {
				p.crcCalc = crc16Update(p.crcCalc, p.payload[:p.plen])
				if p.crcWire != p.crcCalc {
					p.BadCRC++
					lastErr = ParseErrCRC
				} else if h != nil {
					h(FrameType(p.typ), p.flags, p.payload[:p.plen])
				}
				p.state = stMagic0
			}

		default:
			p.state = stMagic0
		}
	}
	return lastErr
}
