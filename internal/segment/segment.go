// Package segment defines the canonical, content-addressed segment encoding.
// The same Header/Samples always produce identical bytes, which is what makes
// resumed-run hashes equal to an uninterrupted run.
package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"math"

	"relaycheck/internal/media"
)

// Magic and Version prefix every committed fragment.
var Magic = [8]byte{'R', 'L', 'Y', 'S', 'E', 'G', '0', '1'}

const version = uint16(1)

// Gap is an output-frame interval lost in transport and silenced/marked.
type Gap struct {
	StartFrame int64 `json:"start_frame"`
	EndFrame   int64 `json:"end_frame"`
}

// Header is the exact, ordered segment identity and timing.
type Header struct {
	Index      int32
	TrackID    int64
	Seed       int64
	StartNum   int64
	StartDen   int64
	EndNum     int64
	EndDen     int64
	SampleRate uint32
	SourceRate uint32
	Frames     uint32
	Channels   uint16
	LUFS100    int32 // target integrated loudness * 100
	GapCount   uint16
}

// Segment is one canonical fragment.
type Segment struct {
	H       Header
	Gaps    []Gap
	Samples []int16 // interleaved, H.Channels channels
}

// SHA256 returns the hex SHA-256 of the canonical encoding.
func (s *Segment) SHA256() string {
	sum := sha256.Sum256(s.Encode())
	return hex.EncodeToString(sum[:])
}

type encoder struct{ b []byte }

func (e *encoder) u16(v uint16) {
	e.b = append(e.b, byte(v>>8), byte(v))
}
func (e *encoder) u32(v uint32) {
	e.b = append(e.b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
func (e *encoder) u64(v uint64) {
	for i := 7; i >= 0; i-- {
		e.b = append(e.b, byte(v>>(8*i)))
	}
}
func (e *encoder) i64(v int64) { e.u64(uint64(v)) }
func (e *encoder) i32(v int32) { e.u32(uint32(v)) }

// Encode produces deterministic canonical bytes.
func (s *Segment) Encode() []byte {
	e := &encoder{}
	e.b = append(e.b, Magic[:]...)
	e.u16(version)
	e.i32(s.H.Index)
	e.i64(s.H.TrackID)
	e.i64(s.H.Seed)
	e.i64(s.H.StartNum)
	e.i64(s.H.StartDen)
	e.i64(s.H.EndNum)
	e.i64(s.H.EndDen)
	e.u32(s.H.SampleRate)
	e.u32(s.H.SourceRate)
	e.u32(s.H.Frames)
	e.u16(s.H.Channels)
	e.i32(s.H.LUFS100)
	gaps := s.Gaps
	if len(gaps) > math.MaxUint16 {
		gaps = gaps[:math.MaxUint16]
	}
	e.u16(uint16(len(gaps)))
	for _, g := range gaps {
		e.i64(g.StartFrame)
		e.i64(g.EndFrame)
	}
	e.u32(uint32(len(s.Samples)))
	for _, v := range s.Samples {
		e.u16(uint16(v))
	}
	return e.b
}

type decoder struct {
	b   []byte
	off int
}

func (d *decoder) need(n int) *media.Fault {
	if d.off+n > len(d.b) || d.off < 0 {
		return media.NewFault(media.CodeParseError, "segment decode out of range at %d (need %d, len %d)", d.off, n, len(d.b))
	}
	return nil
}
func (d *decoder) u16() (uint16, *media.Fault) {
	if f := d.need(2); f != nil {
		return 0, f
	}
	v := uint16(d.b[d.off])<<8 | uint16(d.b[d.off+1])
	d.off += 2
	return v, nil
}
func (d *decoder) u32() (uint32, *media.Fault) {
	if f := d.need(4); f != nil {
		return 0, f
	}
	v := uint32(d.b[d.off])<<24 | uint32(d.b[d.off+1])<<16 | uint32(d.b[d.off+2])<<8 | uint32(d.b[d.off+3])
	d.off += 4
	return v, nil
}
func (d *decoder) u64() (uint64, *media.Fault) {
	if f := d.need(8); f != nil {
		return 0, f
	}
	var v uint64
	for i := 0; i < 8; i++ {
		v = v<<8 | uint64(d.b[d.off+i])
	}
	d.off += 8
	return v, nil
}
func (d *decoder) i64() (int64, *media.Fault) { v, f := d.u64(); return int64(v), f }
func (d *decoder) i32() (int32, *media.Fault) { v, f := d.u32(); return int32(v), f }

// Decode parses and validates a canonical segment.
func Decode(b []byte) (*Segment, *media.Fault) {
	if len(b) < len(Magic)+2 {
		return nil, media.NewFault(media.CodeParseError, "segment too short: %d", len(b))
	}
	for i := range Magic {
		if b[i] != Magic[i] {
			return nil, media.NewFault(media.CodeParseError, "bad segment magic")
		}
	}
	d := &decoder{b: b, off: len(Magic)}
	ver, f := d.u16()
	if f != nil {
		return nil, f
	}
	if ver != version {
		return nil, media.NewFault(media.CodeParseError, "unsupported segment version %d", ver)
	}
	s := &Segment{}
	if s.H.Index, f = d.i32(); f != nil {
		return nil, f
	}
	if s.H.TrackID, f = d.i64(); f != nil {
		return nil, f
	}
	if s.H.Seed, f = d.i64(); f != nil {
		return nil, f
	}
	for _, p := range []*int64{&s.H.StartNum, &s.H.StartDen, &s.H.EndNum, &s.H.EndDen} {
		if *p, f = d.i64(); f != nil {
			return nil, f
		}
	}
	if s.H.SampleRate, f = d.u32(); f != nil {
		return nil, f
	}
	if s.H.SourceRate, f = d.u32(); f != nil {
		return nil, f
	}
	if s.H.Frames, f = d.u32(); f != nil {
		return nil, f
	}
	if s.H.Channels, f = d.u16(); f != nil {
		return nil, f
	}
	if l, f2 := d.i32(); f2 != nil {
		return nil, f2
	} else {
		s.H.LUFS100 = l
	}
	gc, f := d.u16()
	if f != nil {
		return nil, f
	}
	s.H.GapCount = gc
	s.Gaps = make([]Gap, gc)
	for i := 0; i < int(gc); i++ {
		if s.Gaps[i].StartFrame, f = d.i64(); f != nil {
			return nil, f
		}
		if s.Gaps[i].EndFrame, f = d.i64(); f != nil {
			return nil, f
		}
	}
	n, f := d.u32()
	if f != nil {
		return nil, f
	}
	if uint64(n) > uint64(len(b)-d.off)/2 {
		return nil, media.NewFault(media.CodeParseError, "sample count %d overruns segment", n)
	}
	s.Samples = make([]int16, n)
	for i := uint32(0); i < n; i++ {
		v, f2 := d.u16()
		if f2 != nil {
			return nil, f2
		}
		s.Samples[i] = int16(v)
	}
	if d.off != len(b) {
		return nil, media.NewFault(media.CodeParseError, "trailing %d bytes in segment", len(b)-d.off)
	}
	return s, nil
}

// HashBytes returns the hex SHA-256 of raw bytes.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
