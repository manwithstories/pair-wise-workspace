package tsdemux

import (
	"errors"
	"io"
)

const (
	PacketSize188 = 188
	PacketSize192 = 192
	syncByte      = 0x47
	chunkSize     = 1 << 20
	// maxMisSync is the number of consecutive non-0x47 packet starts
	// tolerated before declaring loss of sync and resynchronising.
	maxMisSync = 5
)

// Packet is a parsed MPEG-TS packet. Payload aliases an internal buffer
// and is only valid until the next call to Next.
type Packet struct {
	Index         int64
	PID           uint16
	PUSI          bool
	TEI           bool
	Scrambled     bool
	HasPayload    bool
	CC            uint8
	Discontinuity bool
	HasPCR        bool
	PCR90         int64 // 90 kHz clock
	Payload       []byte
}

// Stats accumulates demuxer level counters.
type Stats struct {
	Packets    int64
	SyncLoss   int64
	TailBytes  int
	TEI        int64
	PacketSize int
}

// Demuxer reads 188/192 byte MPEG-TS packets from a stream with
// 0x47 synchronisation and resync after garbage.
type Demuxer struct {
	r    io.Reader
	buf  []byte
	pos  int
	eof  bool
	size int
	off  int // sync byte offset inside one packet slot (0 or 4)
	mis  int
	pkt  Packet

	Stats Stats
}

func New(r io.Reader) *Demuxer {
	return &Demuxer{r: r}
}

// Next returns the next packet or io.EOF. Trailing garbage that does not
// fill a whole packet is ignored and reported via Stats.TailBytes.
func (d *Demuxer) Next() (*Packet, error) {
	if d.size == 0 {
		if err := d.detect(); err != nil {
			return nil, err
		}
	}
	for {
		if err := d.fill(d.size); err != nil {
			return nil, err
		}
		if len(d.buf)-d.pos < d.size {
			d.Stats.TailBytes = len(d.buf) - d.pos
			return nil, io.EOF
		}
		if d.buf[d.pos+d.off] != syncByte {
			d.mis++
			if d.mis >= maxMisSync {
				d.Stats.SyncLoss++
				d.mis = 0
				if err := d.resync(); err != nil {
					return nil, err
				}
			} else {
				d.pos++
			}
			continue
		}
		d.mis = 0
		p := d.parse(d.buf[d.pos+d.off : d.pos+d.off+PacketSize188])
		d.pos += d.size
		d.Stats.Packets++
		return p, nil
	}
}

// fill compacts the buffer and reads until at least n bytes are
// available past pos or EOF is reached.
func (d *Demuxer) fill(n int) error {
	for len(d.buf)-d.pos < n && !d.eof {
		if d.pos > 0 {
			d.buf = append([]byte(nil), d.buf[d.pos:]...)
			d.pos = 0
		}
		chunk := make([]byte, chunkSize)
		m, err := d.r.Read(chunk)
		d.buf = append(d.buf, chunk[:m]...)
		if err == io.EOF {
			d.eof = true
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (d *Demuxer) detect() error {
	if err := d.fill(4 * PacketSize192); err != nil {
		return err
	}
	b := d.buf
	at := func(i int) bool { return i < len(b) && b[i] == syncByte }
	run := func(off, size int) int {
		n := 0
		for i := off; i < len(b); i += size {
			if !at(i) {
				break
			}
			n++
		}
		return n
	}
	switch {
	case run(0, PacketSize188) >= 2 && run(0, PacketSize188) >= run(4, PacketSize192):
		d.size, d.off = PacketSize188, 0
	case run(4, PacketSize192) >= 2:
		d.size, d.off = PacketSize192, 4
	case at(0) && d.eof && len(b) == PacketSize188:
		d.size, d.off = PacketSize188, 0
	default:
		found := false
		for j := 0; j+PacketSize188 < len(b); j++ {
			if at(j) && at(j+PacketSize188) {
				d.size, d.off, d.pos = PacketSize188, 0, j
				found = true
				break
			}
			if j+4+PacketSize192 < len(b) && at(j+4) && at(j+4+PacketSize192) {
				d.size, d.off, d.pos = PacketSize192, 4, j
				found = true
				break
			}
		}
		if !found {
			return errors.New("tsdemux: cannot locate 0x47 sync pattern")
		}
		d.Stats.SyncLoss++
	}
	d.Stats.PacketSize = d.size
	return nil
}

// resync scans forward for two consecutive 0x47 sync bytes one packet
// apart. Bytes skipped are discarded.
func (d *Demuxer) resync() error {
	for {
		b := d.buf
		for i := d.pos + 1; i+d.off+d.size < len(b); i++ {
			if b[i+d.off] == syncByte && b[i+d.off+d.size] == syncByte {
				d.pos = i
				return nil
			}
		}
		if d.eof {
			d.pos = len(d.buf)
			return nil
		}
		keep := d.size + d.off + 1
		if keep > len(d.buf)-d.pos {
			keep = len(d.buf) - d.pos
		}
		d.pos = len(d.buf) - keep
		if err := d.fill(2 * d.size); err != nil {
			return err
		}
	}
}

func (d *Demuxer) parse(b []byte) *Packet {
	p := &d.pkt
	p.Index = d.Stats.Packets
	p.TEI = b[1]&0x80 != 0
	p.PUSI = b[1]&0x40 != 0
	p.PID = uint16(b[1]&0x1F)<<8 | uint16(b[2])
	p.CC = b[3] & 0x0F
	p.Scrambled = b[3]&0xC0 != 0
	p.Discontinuity = false
	p.HasPCR = false
	p.HasPayload = false
	p.Payload = nil
	if p.TEI {
		d.Stats.TEI++
	}
	afc := (b[3] >> 4) & 0x3
	i := 4
	if afc == 2 || afc == 3 {
		afl := int(b[4])
		if afl > 0 && 5+afl <= PacketSize188 {
			flags := b[5]
			p.Discontinuity = flags&0x80 != 0
			if flags&0x10 != 0 && afl >= 7 {
				p.HasPCR = true
				p.PCR90 = int64(b[6])<<25 | int64(b[7])<<17 |
					int64(b[8])<<9 | int64(b[9])<<1 | int64(b[10])>>7
			}
		}
		i = 5 + afl
	}
	if (afc == 1 || afc == 3) && i < PacketSize188 {
		p.Payload = b[i:PacketSize188]
		p.HasPayload = true
	}
	return p
}
