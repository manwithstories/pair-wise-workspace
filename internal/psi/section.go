// Package psi implements MPEG-2 PSI section reassembly, CRC and
// section to TS packet marshalling.
package psi

const (
	stuffingByte   = 0xFF
	maxSectionSize = 4096
	tsPacketSize   = 188
)

var crcTable [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		crcTable[i] = c
	}
}

// CRC32 computes the MPEG-2 CRC32 (poly 0x04C11DB7, init 0xFFFFFFFF,
// no reflection, no final xor). A section including its CRC yields 0.
func CRC32(b []byte) uint32 {
	c := uint32(0xFFFFFFFF)
	for _, v := range b {
		c = crcTable[byte(c>>24)^v] ^ (c << 8)
	}
	return c
}

// Assembler reassembles PSI sections from TS packet payloads of one PID.
// It supports multiple sections per packet, sections spanning packets
// and non-zero pointer_field. Sections with a bad CRC are dropped and
// counted. Sections passed to the emit callback alias an internal
// buffer: copy them if they must be retained.
type Assembler struct {
	buf  []byte
	want int

	Sections  int64
	CRCErrors int64
}

func (a *Assembler) reset() {
	a.buf = a.buf[:0]
	a.want = 0
}

// Write feeds one payload. emit is called for every CRC-valid section.
func (a *Assembler) Write(pusi bool, payload []byte, emit func(sec []byte)) {
	pos := 0
	if pusi {
		if len(payload) == 0 {
			return
		}
		pointer := int(payload[0])
		pos = 1
		if pointer > 0 {
			end := 1 + pointer
			if end > len(payload) {
				a.reset()
				return
			}
			if len(a.buf) > 0 {
				a.buf = append(a.buf, payload[1:end]...)
				if a.want > 0 && len(a.buf) >= a.want {
					a.check(a.buf[:a.want], emit)
				}
			}
			a.reset()
			pos = end
		} else if len(a.buf) > 0 {
			// New section starts immediately: the interrupted
			// previous section is dropped.
			a.reset()
		}
	}
	for pos < len(payload) {
		b := payload[pos]
		if len(a.buf) == 0 && b == stuffingByte {
			return
		}
		a.buf = append(a.buf, b)
		pos++
		if len(a.buf) == 3 {
			a.want = 3 + (int(a.buf[1]&0x0F)<<8 | int(a.buf[2]))
			if a.want > maxSectionSize {
				a.reset()
			}
		}
		if a.want > 0 && len(a.buf) == a.want {
			a.check(a.buf, emit)
			a.reset()
		}
	}
}

func (a *Assembler) check(sec []byte, emit func([]byte)) {
	if len(sec) < 4 || CRC32(sec) != 0 {
		a.CRCErrors++
		return
	}
	a.Sections++
	emit(sec)
}

// MarshalSection wraps a PSI section into 188 byte TS packets on pid.
// The first packet carries PUSI and pointer_field 0; the tail is
// stuffed with 0xFF. cc is used for every produced packet.
func MarshalSection(pid uint16, cc uint8, sec []byte) []byte {
	var out []byte
	first := true
	for len(sec) > 0 || first {
		pkt := make([]byte, tsPacketSize)
		pkt[0] = 0x47
		pkt[1] = byte(pid >> 8)
		if first {
			pkt[1] |= 0x40
		}
		pkt[2] = byte(pid)
		pkt[3] = 0x10 | (cc & 0x0F)
		payload := pkt[4:]
		if first {
			payload[0] = 0x00
			payload = payload[1:]
		}
		n := copy(payload, sec)
		for i := n; i < len(payload); i++ {
			payload[i] = stuffingByte
		}
		sec = sec[n:]
		out = append(out, pkt...)
		first = false
	}
	return out
}
