// Package model defines the shared frame record format, the unified media
// clock (90 kHz) and the error values exchanged by every mediq stage.
package model

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Record format constants. Every input sequence is a concatenation of
// records, each laid out as:
//
//	offset  size  field
//	0       4     magic "MFRM"
//	4       1     version (1)
//	5       1     flags
//	6       2     header length (72, big endian)
//	8       8     frame sequence number
//	16      8     timestamp in timebase ticks
//	24      4     timebase numerator (ticks per second)
//	28      4     timebase denominator
//	32      4     metadata length
//	36      4     frame length
//	40      32    SHA-256 of the frame bytes
//	72      ...   metadata bytes, then frame bytes
const (
	RecordMagic      = "MFRM"
	RecordVersion    = 1
	RecordHeaderSize = 72

	FlagMetadataPresent uint8 = 1 << 0

	DefaultMaxFrameBytes    = 16 << 20
	DefaultMaxMetadataBytes = 1 << 20
)

// Timebase is a rational tick rate: a timestamp counts ticks at
// Num/Den ticks per second. Examples: 90000/1 (MPEG clock) and
// 30000/1001 (NTSC field rate).
type Timebase struct {
	Num uint32 `json:"num"`
	Den uint32 `json:"den"`
}

func (t Timebase) Valid() bool { return t.Num > 0 && t.Den > 0 }

func (t Timebase) String() string {
	return strconv.FormatUint(uint64(t.Num), 10) + "/" + strconv.FormatUint(uint64(t.Den), 10)
}

// ParseTimebase parses "num/den".
func ParseTimebase(s string) (Timebase, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return Timebase{}, fmt.Errorf("invalid timebase %q: want num/den", s)
	}
	n, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return Timebase{}, fmt.Errorf("invalid timebase %q: %w", s, err)
	}
	d, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return Timebase{}, fmt.Errorf("invalid timebase %q: %w", s, err)
	}
	tb := Timebase{Num: uint32(n), Den: uint32(d)}
	if !tb.Valid() {
		return Timebase{}, fmt.Errorf("invalid timebase %q: components must be non-zero", s)
	}
	return tb, nil
}

// Clock90 converts a tick count to the unified 90 kHz clock, rounding half up.
// seconds = ticks * Den / Num, so clock90 = ticks * 90000 * Den / Num.
func (t Timebase) Clock90(ticks uint64) (int64, error) {
	if !t.Valid() {
		return 0, fmt.Errorf("invalid timebase %s", t)
	}
	num := uint64(t.Num)
	den := uint64(t.Den)
	if ticks <= math.MaxUint64/90000 {
		a := ticks * 90000
		if a <= math.MaxUint64/den {
			b := a * den
			q, r := b/num, b%num
			if r >= num-r { // r/num >= 1/2
				q++
			}
			if q <= math.MaxInt64 {
				return int64(q), nil
			}
		}
	}
	// Rare overflow path: keep the arithmetic exact.
	bigTicks := new(big.Int).SetUint64(ticks)
	bigTicks.Mul(bigTicks, big.NewInt(90000))
	bigTicks.Mul(bigTicks, new(big.Int).SetUint64(den))
	q, r := new(big.Int).QuoRem(bigTicks, new(big.Int).SetUint64(num), new(big.Int))
	r.Lsh(r, 1)
	if r.Cmp(new(big.Int).SetUint64(num)) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("timestamp %d with timebase %s overflows the 90 kHz clock", ticks, t)
	}
	return q.Int64(), nil
}

// RecordHeader is the fixed-size header preceding metadata and frame bytes.
type RecordHeader struct {
	Version   uint8
	Flags     uint8
	Seq       uint64
	Timestamp uint64
	Timebase  Timebase
	MetaLen   uint32
	FrameLen  uint32
	Hash      [32]byte
}

// MarshalBinary encodes a 72-byte record header.
func (h RecordHeader) MarshalBinary() ([]byte, error) {
	buf := make([]byte, RecordHeaderSize)
	copy(buf[0:4], RecordMagic)
	buf[4] = h.Version
	buf[5] = h.Flags
	binary.BigEndian.PutUint16(buf[6:8], RecordHeaderSize)
	binary.BigEndian.PutUint64(buf[8:16], h.Seq)
	binary.BigEndian.PutUint64(buf[16:24], h.Timestamp)
	binary.BigEndian.PutUint32(buf[24:28], h.Timebase.Num)
	binary.BigEndian.PutUint32(buf[28:32], h.Timebase.Den)
	binary.BigEndian.PutUint32(buf[32:36], h.MetaLen)
	binary.BigEndian.PutUint32(buf[36:40], h.FrameLen)
	copy(buf[40:72], h.Hash[:])
	return buf, nil
}

// ParseRecordHeader decodes and validates a fixed-size record header.
func ParseRecordHeader(buf []byte) (RecordHeader, error) {
	var h RecordHeader
	if len(buf) < RecordHeaderSize {
		return h, fmt.Errorf("short record header: %d < %d", len(buf), RecordHeaderSize)
	}
	if string(buf[0:4]) != RecordMagic {
		return h, fmt.Errorf("bad record magic %q", buf[0:4])
	}
	h.Version = buf[4]
	h.Flags = buf[5]
	hdrLen := binary.BigEndian.Uint16(buf[6:8])
	if hdrLen != RecordHeaderSize {
		return h, fmt.Errorf("unsupported record header length %d", hdrLen)
	}
	if h.Version != RecordVersion {
		return h, fmt.Errorf("unsupported record version %d", h.Version)
	}
	h.Seq = binary.BigEndian.Uint64(buf[8:16])
	h.Timestamp = binary.BigEndian.Uint64(buf[16:24])
	h.Timebase = Timebase{
		Num: binary.BigEndian.Uint32(buf[24:28]),
		Den: binary.BigEndian.Uint32(buf[28:32]),
	}
	h.MetaLen = binary.BigEndian.Uint32(buf[32:36])
	h.FrameLen = binary.BigEndian.Uint32(buf[36:40])
	copy(h.Hash[:], buf[40:72])
	return h, nil
}

// Frame is a decoded record travelling through the pipeline.
type Frame struct {
	Seq         uint64
	Timestamp   uint64
	Timebase    Timebase
	MetaLen     uint32
	MetaOffset  int64
	FrameOffset int64
	StartOffset int64  // first byte of this record's header
	EndOffset   int64  // first byte after this record, used to resume reading
	MediaTime   int64  // assigned by timeline repair, 90 kHz ticks, absolute
	Meta        []byte // RTP metadata carrier
	Payload     []byte
	Hash        [32]byte
}

func (f *Frame) Len() int { return len(f.Payload) }

// Validate checks the structural invariants shared by all stages.
func (f *Frame) Validate() error {
	if !f.Timebase.Valid() {
		return fmt.Errorf("frame %d: invalid timebase %s", f.Seq, f.Timebase)
	}
	return nil
}

// Sentinel errors used with errors.Is.
var (
	ErrCanceled           = errors.New("canceled")
	ErrCheckpointMismatch = errors.New("checkpoint mismatch")
	ErrVerifyFailed       = errors.New("output verification failed")
	ErrDamagedInput       = errors.New("input damaged")
)
