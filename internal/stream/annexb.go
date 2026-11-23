// Package stream parses H.264/H.265 Annex-B and AV1 OBU raw streams into
// access units, marking truncated units as corrupt ranges and continuing.
package stream

// Codec identifies the elementary stream encoding.
type Codec int

const (
	CodecH264 Codec = iota
	CodecH265
	CodecAV1
)

func (c Codec) String() string {
	switch c {
	case CodecH264:
		return "h264"
	case CodecH265:
		return "h265"
	case CodecAV1:
		return "av1"
	}
	return "unknown"
}

// AccessUnit is one parsed access unit (a frame's worth of NAL units/OBUs).
type AccessUnit struct {
	Index     int
	Offset    int64 // byte offset of the first contributing NAL/OBU
	Size      int
	Keyframe  bool
	Corrupted bool // contains a truncated NAL/OBU
}

// CorruptRange locates a truncated region in the stream.
type CorruptRange struct {
	Start, End int64
	Reason     string
}

// nal is one located NAL unit.
type nal struct {
	off      int64
	typ      int
	payload  []byte
	truncated bool
}

// Parse splits data into access units. Truncated units are marked and
// recorded as corrupt ranges; scanning continues where resync is possible.
func Parse(codec Codec, data []byte) ([]AccessUnit, []CorruptRange) {
	if codec == CodecAV1 {
		return parseAV1(data)
	}
	nals, ranges := scanAnnexB(codec, data)
	return groupNALs(codec, nals), ranges
}

// scanAnnexB finds start-code-delimited NAL units. A NAL whose payload does
// not end with the rbsp trailing stop bit (0x80) is treated as truncated.
func scanAnnexB(codec Codec, data []byte) ([]nal, []CorruptRange) {
	type span struct{ start, hdrEnd int }
	var spans []span
	i := 0
	for i+3 <= len(data) {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			spans = append(spans, span{i, i + 3})
			i += 3
		} else if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			spans = append(spans, span{i, i + 4})
			i += 4
		} else {
			i++
		}
	}
	var nals []nal
	var ranges []CorruptRange
	for j, sp := range spans {
		end := len(data)
		if j+1 < len(spans) {
			end = spans[j+1].start
		}
		if sp.hdrEnd >= end {
			continue // empty NAL
		}
		hdr := data[sp.hdrEnd]
		typ := nalType(codec, hdr)
		payload := data[sp.hdrEnd+1 : end]
		n := nal{off: int64(sp.start), typ: typ, payload: payload}
		if len(payload) == 0 || payload[len(payload)-1] != 0x80 {
			n.truncated = true
			ranges = append(ranges, CorruptRange{int64(sp.start), int64(end), "truncated NAL: missing rbsp stop bit"})
		}
		nals = append(nals, n)
	}
	return nals, ranges
}

func nalType(codec Codec, hdr byte) int {
	if codec == CodecH265 {
		return int(hdr>>1) & 0x3F
	}
	return int(hdr) & 0x1F
}

func isAUD(codec Codec, typ int) bool {
	if codec == CodecH265 {
		return typ == 35
	}
	return typ == 9
}

func isIDR(codec Codec, typ int) bool {
	if codec == CodecH265 {
		return typ == 19 || typ == 20
	}
	return typ == 5
}

// groupNALs groups NAL units into access units on AUD boundaries (or on IDR
// when no AUD is present).
func groupNALs(codec Codec, nals []nal) []AccessUnit {
	var units []AccessUnit
	var cur *AccessUnit
	flush := func() {
		if cur != nil && cur.Size > 0 {
			units = append(units, *cur)
		}
		cur = nil
	}
	for _, n := range nals {
		// AUD starts a new access unit; so does an IDR when the current
		// unit already holds one (streams without AUD).
		if isAUD(codec, n.typ) || (isIDR(codec, n.typ) && cur != nil && cur.Keyframe) {
			flush()
		}
		if cur == nil {
			cur = &AccessUnit{Index: len(units), Offset: n.off}
		}
		cur.Size += len(n.payload) + 1
		cur.Keyframe = cur.Keyframe || isIDR(codec, n.typ)
		cur.Corrupted = cur.Corrupted || n.truncated
	}
	flush()
	return units
}

// parseAV1 parses a low-overhead OBU stream. Temporal delimiter OBUs mark
// access-unit boundaries. A truncated OBU cannot be resynchronized, so the
// remainder of the stream is reported as one corrupt range.
func parseAV1(data []byte) ([]AccessUnit, []CorruptRange) {
	var units []AccessUnit
	var ranges []CorruptRange
	var cur *AccessUnit
	flush := func() {
		if cur != nil {
			units = append(units, *cur)
			cur = nil
		}
	}
	off := 0
	for off < len(data) {
		hdr := data[off]
		typ := int(hdr>>3) & 0x0F
		hasSize := hdr&0x02 != 0
		pos := off + 1
		size := len(data) - pos
		if hasSize {
			s, n := leb128(data[pos:])
			if n == 0 {
				ranges = append(ranges, CorruptRange{int64(off), int64(len(data)), "truncated OBU: bad leb128 size"})
				flush()
				return units, ranges
			}
			size, pos = s, pos+n
		}
		if pos+size > len(data) {
			ranges = append(ranges, CorruptRange{int64(off), int64(len(data)), "truncated OBU: size overruns stream"})
			flush()
			return units, ranges
		}
		payload := data[pos : pos+size]
		switch typ {
		case 2: // temporal delimiter
			flush()
		case 3, 6: // frame header / frame
			if cur == nil {
				cur = &AccessUnit{Index: len(units), Offset: int64(off)}
			}
			if len(payload) > 0 && payload[0]&1 == 1 { // synthetic keyframe flag
				cur.Keyframe = true
			}
		}
		if cur != nil {
			cur.Size += 1 + (pos - off - 1) + size
		}
		off = pos + size
	}
	flush()
	return units, ranges
}

// leb128 decodes an unsigned LEB128 value, returning value and bytes read
// (0 on malformed input).
func leb128(b []byte) (int, int) {
	var v int
	for i := 0; i < len(b) && i < 8; i++ {
		v |= int(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}
