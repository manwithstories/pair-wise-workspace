// Package tsio provides bounded-memory MPEG-TS packet reading and resync.
package tsio

import (
	"bufio"
	"errors"
	"io"
)

const (
	// PacketSize is the fixed MPEG-TS packet size used by the probe.
	PacketSize = 188
	// SyncByte starts every MPEG-TS packet.
	SyncByte = 0x47
	window   = PacketSize*2 + 1
)

// Counters records transport stream framing anomalies.
type Counters struct {
	SyncLossEvents    uint64 `json:"sync_loss_events"`
	SyncLossBytes     uint64 `json:"sync_loss_bytes"`
	ZeroFillSegments  uint64 `json:"zero_fill_segments"`
	ZeroFillBytes     uint64 `json:"zero_fill_bytes"`
	TrailingBytes     uint64 `json:"trailing_bytes"`
	IncompletePackets uint64 `json:"incomplete_packets"`
}

// Reader returns only complete, resynchronised 188-byte TS packets.
type Reader struct {
	r        *bufio.Reader
	buf      []byte
	offset   int64
	packet   uint64
	lastOff  int64
	finished bool
	counts   Counters
	tmp      [4096]byte
}

// New creates a streaming TS reader.
func New(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64*1024), buf: make([]byte, 0, window)}
}

// Read fills p with exactly one packet when p is at least 188 bytes.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) < PacketSize {
		return 0, errors.New("tsio: read buffer smaller than 188 bytes")
	}
	for {
		if r.finished {
			return 0, io.EOF
		}
		if err := r.fill(); err != nil {
			return 0, err
		}
		idx, ok := r.findPacket()
		if !ok {
			if len(r.buf) == 0 && r.canRead() {
				if err := r.fill(); err != nil {
					return 0, err
				}
				continue
			}
			if r.canRead() {
				next := nextSync(r.buf)
				if next > 0 {
					r.skip(next)
				} else if next == 0 {
					r.skip(1)
				} else {
					r.skip(len(r.buf))
				}
				continue
			}
			r.finish()
			return 0, io.EOF
		}
		if idx > 0 {
			r.skip(idx)
		}
		copy(p[:PacketSize], r.buf[:PacketSize])
		r.lastOff = r.offset
		r.offset += PacketSize
		r.packet++
		r.buf = r.buf[PacketSize:]
		return PacketSize, nil
	}
}

// PacketIndex returns the one-based index of the most recently returned packet.
func (r *Reader) PacketIndex() uint64 { return r.packet }

// PacketOffset returns the byte offset of the most recently returned packet.
func (r *Reader) PacketOffset() int64 { return r.lastOff }

// Counters returns a snapshot of framing anomaly counters.
func (r *Reader) Counters() Counters { return r.counts }

func (r *Reader) canRead() bool { return !r.finished }

func (r *Reader) fill() error {
	for len(r.buf) < window && r.canRead() {
		n, err := r.r.Read(r.tmp[:])
		if n > 0 {
			r.buf = append(r.buf, r.tmp[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.finished = true
				break
			}
			return err
		}
	}
	return nil
}

func (r *Reader) findPacket() (int, bool) {
	limit := len(r.buf) - PacketSize
	for i := 0; i <= limit; i++ {
		if r.buf[i] != SyncByte || !validHeader(r.buf[i:i+PacketSize]) {
			continue
		}
		next := i + PacketSize
		if next < len(r.buf) && (r.buf[next] == SyncByte || isZeroBlock(r.buf[next:])) {
			return i, true
		}
		if r.finished && next == len(r.buf) {
			return i, true
		}
	}
	if r.finished {
		for i := 0; i <= limit; i++ {
			if r.buf[i] == SyncByte && validHeader(r.buf[i:i+PacketSize]) {
				return i, true
			}
		}
	}
	return 0, false
}

func isZeroBlock(bs []byte) bool {
	if len(bs) < PacketSize {
		return false
	}
	for _, b := range bs[:PacketSize] {
		if b != 0 {
			return false
		}
	}
	return true
}

func nextSync(bs []byte) int {
	for i, b := range bs {
		if b == SyncByte {
			return i
		}
	}
	return -1
}

func validHeader(p []byte) bool {
	if len(p) != PacketSize || p[0] != SyncByte {
		return false
	}
	hasAF := p[3]&0x20 != 0
	hasPayload := p[3]&0x10 != 0
	if hasAF {
		l := int(p[4])
		if l > 183 || (hasPayload && l > 182) {
			return false
		}
	}
	return true
}

func (r *Reader) skip(n int) {
	r.countSkipped(r.buf[:n])
	r.offset += int64(n)
	r.buf = r.buf[n:]
}

func (r *Reader) countSkipped(bs []byte) {
	if len(bs) == 0 {
		return
	}
	r.counts.SyncLossEvents++
	r.counts.SyncLossBytes += uint64(len(bs))
	var run int
	for _, b := range bs {
		if b == 0 {
			run++
			r.counts.ZeroFillBytes++
			continue
		}
		if run >= PacketSize {
			r.counts.ZeroFillSegments++
		}
		run = 0
	}
	if run >= PacketSize {
		r.counts.ZeroFillSegments++
	}
}

func (r *Reader) finish() {
	if len(r.buf) == 0 {
		r.finished = true
		return
	}
	bs := r.buf
	z := 0
	for z < len(bs) && bs[z] == 0 {
		z++
	}
	if z > 0 {
		r.countSkipped(bs[:z])
		bs = bs[z:]
	}
	if len(bs) > 0 && bs[0] == SyncByte && len(bs) < PacketSize {
		r.counts.TrailingBytes = uint64(len(bs))
		r.counts.IncompletePackets = 1
	} else if len(bs) > 0 {
		r.countSkipped(bs)
	}
	r.buf = r.buf[:0]
	r.finished = true
}
