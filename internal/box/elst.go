package box

import (
	"encoding/binary"
	"errors"
)

// ErrTableRange reports a table whose declared entries exceed its payload.
var ErrTableRange = errors.New("table_out_of_range")

// ElstEntry is one edit list entry. Rate is 16.16 fixed point.
// MediaTime == -1 marks an empty edit (presentation gap).
// MediaTime < -1 marks a wrapped 32-bit media_time written as unsigned.
type ElstEntry struct {
	SegmentDuration uint64 // in movie timescale
	MediaTime       int64  // in media timescale
	Rate            int32  // 16.16 fixed point
}

// Empty reports whether the entry is an empty edit.
func (e ElstEntry) Empty() bool { return e.MediaTime == -1 }

// ParseElst parses an elst payload (version 0 and 1, ISO BMFF & QuickTime).
func ParseElst(p []byte) ([]ElstEntry, error) {
	if len(p) < 8 {
		return nil, ErrTableRange
	}
	version := p[0]
	n := binary.BigEndian.Uint32(p[4:8])
	var entrySize int
	if version == 1 {
		entrySize = 20
	} else {
		entrySize = 12
	}
	if uint64(n)*uint64(entrySize) > uint64(len(p)-8) {
		return nil, ErrTableRange
	}
	out := make([]ElstEntry, 0, n)
	off := 8
	for i := uint32(0); i < n; i++ {
		var e ElstEntry
		if version == 1 {
			e.SegmentDuration = binary.BigEndian.Uint64(p[off : off+8])
			e.MediaTime = int64(binary.BigEndian.Uint64(p[off+8 : off+16]))
			e.Rate = rateFromBytes(p[off+16 : off+20])
		} else {
			e.SegmentDuration = uint64(binary.BigEndian.Uint32(p[off : off+4]))
			// Read as signed: devices that write wrapped 32-bit unsigned
			// media_time surface here as values < -1.
			e.MediaTime = int64(int32(binary.BigEndian.Uint32(p[off+4 : off+8])))
			e.Rate = rateFromBytes(p[off+8 : off+12])
		}
		out = append(out, e)
		off += entrySize
	}
	return out, nil
}

// rateFromBytes combines int16 integer part and uint16 fraction into 16.16.
func rateFromBytes(b []byte) int32 {
	i := int32(int16(binary.BigEndian.Uint16(b[0:2])))
	f := int32(binary.BigEndian.Uint16(b[2:4]))
	if i < 0 {
		return i<<16 | -f
	}
	return i<<16 | f
}
