// Package rtp parses local RTP arrival records. Records are ProRes-style
// captures: a 16-byte arrival header (nanosecond arrival time, SSRC, sequence,
// RTP timestamp, clock rate) followed by one RTP packet. The parser only reads
// headers; payload bytes are counted and hashed, never interpreted.
package rtp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/pion/rtp"

	"mediapipe/internal/media"
)

// RecordHeaderWidth is the fixed size of the local arrival header.
const RecordHeaderWidth = 16

// Magic marks a record file.
var Magic = [4]byte{'M', 'P', 'R', 'T'}

// Limits bounds a parse run.
type Limits struct {
	MaxRecords    int
	MaxPacketSize int
}

func defaultLimits() Limits {
	return Limits{MaxRecords: 2_000_000, MaxPacketSize: 64 << 10}
}

func (l Limits) normalized() Limits {
	if l.MaxRecords <= 0 {
		l.MaxRecords = 10_000_000
	}
	if l.MaxPacketSize <= 0 {
		l.MaxPacketSize = 1 << 20
	}
	return l
}

// Arrival is one parsed record.
type Arrival struct {
	// ArrivalTime is the local nanosecond timestamp when the packet was seen.
	ArrivalTime media.Timestamp
	// SSRC identifies the stream; it maps to a source track.
	SSRC uint32
	// Sequence is the RTP sequence number as sent.
	Sequence uint16
	// RTPTimestamp is the media clock at the sender.
	RTPTimestamp uint32
	// ClockRate converts RTP timestamps to nanoseconds.
	ClockRate uint32
	// PayloadType is the negotiated payload type.
	PayloadType uint8
	Marker      bool
	PayloadSize int
	PayloadCRC  uint32
	// Offset is the byte offset of the record in the source file.
	Offset int64
	// Discontinuity is set when the sequence number jumped, meaning packets
	// were lost or reordered across records.
	Discontinuity bool
}

// ExtrapolatedTimestamp converts the RTP timestamp into the shared nanosecond
// time base using the declared clock rate. Sequence wrap and clock drift are
// out of scope; arrival records are short capture windows.
func (a Arrival) ExtrapolatedTimestamp() media.Timestamp {
	if a.ClockRate == 0 {
		return 0
	}
	return media.FromScale(int64(a.RTPTimestamp), a.ClockRate)
}

// Result is the outcome of parsing a capture.
type Result struct {
	Arrivals      []Arrival
	Streams       map[uint32]int
	Discontinuity int
	Gaps          int
	Lost          int
	Bytes         int64
}

// Parse reads a capture from r and returns every arrival. A truncated final
// record is reported as a fatal error with its offset.
func Parse(r io.Reader, limits Limits) (*Result, error) {
	limits = limits.normalized()
	br := bufio.NewReaderSize(r, 1<<16)
	res := &Result{Streams: make(map[uint32]int)}
	off := int64(0)
	var magic [4]byte
	n, err := io.ReadFull(br, magic[:])
	off += int64(n)
	if err != nil {
		if err == io.EOF {
			return res, nil
		}
		return res, media.At("rtp.magic", off, media.ClassFatal, "truncated capture magic")
	}
	if magic != Magic {
		return res, media.At("rtp.magic", 0, media.ClassFatal,
			"bad capture magic %q", magic)
	}
	lastSeq := make(map[uint32]uint16)
	for {
		start := off
		var hdr [RecordHeaderWidth]byte
		n, err := io.ReadFull(br, hdr[:])
		off += int64(n)
		if err != nil {
			if err == io.EOF {
				break
			}
			return res, media.At("rtp.record", off, media.ClassFatal,
				"truncated record header at %d", start)
		}
		arrivalNS := int64(binary.BigEndian.Uint64(hdr[0:8]))
		ssrc := binary.BigEndian.Uint32(hdr[8:12])
		size := int(binary.BigEndian.Uint16(hdr[12:14]))
		clockRate := uint32(binary.BigEndian.Uint16(hdr[14:16])) * 1000
		if size <= 0 || size > limits.MaxPacketSize {
			return res, media.At("rtp.record", start, media.ClassFatal,
				"invalid packet size %d", size)
		}
		buf := make([]byte, size)
		n, err = io.ReadFull(br, buf)
		off += int64(n)
		if err != nil {
			return res, media.At("rtp.record", start, media.ClassFatal,
				"truncated packet payload: wanted %d bytes, got %d", size, n)
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf); err != nil {
			return res, media.At("rtp.packet", start, media.ClassFatal,
				"invalid RTP packet: %v", err)
		}
		if len(res.Arrivals) >= limits.MaxRecords {
			return res, media.At("rtp.record", start, media.ClassFatal,
				"record count exceeds limit %d", limits.MaxRecords)
		}
		arr := Arrival{
			ArrivalTime:  media.Timestamp(arrivalNS),
			SSRC:         ssrc,
			Sequence:     pkt.SequenceNumber,
			RTPTimestamp: pkt.Timestamp,
			ClockRate:    clockRate,
			PayloadType:  pkt.PayloadType,
			Marker:       pkt.Marker,
			PayloadSize:  len(pkt.Payload),
			PayloadCRC:   crc32.ChecksumIEEE(pkt.Payload),
			Offset:       start,
		}
		if pkt.SSRC != 0 && pkt.SSRC != ssrc {
			return res, media.At("rtp.packet", start, media.ClassFatal,
				"SSRC mismatch: header %d packet %d", ssrc, pkt.SSRC)
		}
		if prev, ok := lastSeq[ssrc]; ok {
			expected := prev + 1
			if arr.Sequence != expected {
				arr.Discontinuity = true
				res.Discontinuity++
				gap := int(int16(arr.Sequence - expected))
				if gap > 0 {
					res.Gaps++
					res.Lost += gap
				}
			}
		}
		lastSeq[ssrc] = arr.Sequence
		res.Streams[ssrc]++
		res.Arrivals = append(res.Arrivals, arr)
	}
	res.Bytes = off
	return res, nil
}

// BuildRecord encodes one arrival record for fixtures. The RTP timestamp is
// taken from pkt; arrival is the local receive time.
func BuildRecord(arrival media.Timestamp, clockRate uint32, pkt *rtp.Packet) ([]byte, error) {
	raw, err := pkt.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal rtp: %w", err)
	}
	if len(raw) > 0xFFFF {
		return nil, fmt.Errorf("rtp packet too large: %d", len(raw))
	}
	out := make([]byte, RecordHeaderWidth+len(raw))
	binary.BigEndian.PutUint64(out[0:8], uint64(arrival))
	binary.BigEndian.PutUint32(out[8:12], pkt.SSRC)
	binary.BigEndian.PutUint16(out[12:14], uint16(len(raw)))
	binary.BigEndian.PutUint16(out[14:16], uint16(clockRate/1000))
	copy(out[RecordHeaderWidth:], raw)
	return out, nil
}

// WriteCapture writes the capture magic followed by records.
func WriteCapture(w io.Writer, records [][]byte) error {
	if _, err := w.Write(Magic[:]); err != nil {
		return err
	}
	for _, r := range records {
		if _, err := w.Write(r); err != nil {
			return err
		}
	}
	return nil
}
