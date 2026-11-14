// Package psi implements self-contained PSI section assembly, CRC and table parsing.
package psi

// crcTable is the MPEG-2 CRC32 table: polynomial 0x04C11DB7, MSB first,
// no input/output reflection, init 0xFFFFFFFF, no final XOR.
var crcTable [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = (c << 1) ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		crcTable[i] = c
	}
}

// CRC32MPEG computes the MPEG-2 CRC32 of data.
func CRC32MPEG(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc = (crc << 8) ^ crcTable[byte(crc>>24)^b]
	}
	return crc
}
