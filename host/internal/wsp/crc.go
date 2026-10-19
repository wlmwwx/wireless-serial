package wsp

// crc16Update applies one chunk of data to a running CRC-16/CCITT-FALSE
// (poly 0x1021, init 0xFFFF, no reflection, no final xor). It is a direct
// transliteration of crc16_update() in firmware/main/wsp_proto.c.
func crc16Update(crc uint16, data []byte) uint16 {
	for _, b := range data {
		crc ^= uint16(b) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// CRC16 computes CRC-16/CCITT-FALSE over data. CRC16("123456789") == 0x29B1.
func CRC16(data []byte) uint16 {
	return crc16Update(0xFFFF, data)
}
