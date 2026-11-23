package stream

import "testing"

// nalBytes builds one Annex-B NAL unit with a valid rbsp stop bit.
func nalBytes(typ int, payload ...byte) []byte {
	b := []byte{0, 0, 0, 1, byte(typ)}
	b = append(b, payload...)
	return append(b, 0x80)
}

func h264Stream() []byte {
	var b []byte
	b = append(b, nalBytes(9, 0x10)...)        // AUD
	b = append(b, nalBytes(7, 0x01, 0x02)...)  // SPS
	b = append(b, nalBytes(5, 0x11, 0x22)...)  // IDR -> AU0 keyframe
	b = append(b, nalBytes(9, 0x10)...)        // AUD
	b = append(b, nalBytes(1, 0x33)...)        // non-IDR -> AU1
	return b
}

func TestParseH264(t *testing.T) {
	units, ranges := Parse(CodecH264, h264Stream())
	if len(ranges) != 0 {
		t.Fatalf("ranges=%v", ranges)
	}
	if len(units) != 2 {
		t.Fatalf("units=%d, want 2", len(units))
	}
	if !units[0].Keyframe || units[1].Keyframe {
		t.Fatalf("keyframe flags wrong: %+v", units)
	}
	if units[1].Offset <= units[0].Offset {
		t.Fatalf("offsets not increasing: %+v", units)
	}
}

func TestParseH264Truncated(t *testing.T) {
	full := h264Stream()
	trunc := full[:len(full)-2] // cut last NAL before its stop bit
	units, ranges := Parse(CodecH264, trunc)
	if len(ranges) != 1 {
		t.Fatalf("ranges=%v, want 1", ranges)
	}
	if !units[len(units)-1].Corrupted {
		t.Fatalf("last AU not marked corrupted: %+v", units)
	}
	if ranges[0].End != int64(len(trunc)) {
		t.Fatalf("range=%+v", ranges[0])
	}
}

func TestParseH265Keyframe(t *testing.T) {
	var b []byte
	b = append(b, nalBytes(35<<1, 0x01)...) // AUD (type 35 in 6-bit field)
	b = append(b, nalBytes(19<<1, 0x01)...) // IDR_W_RADL
	units, _ := Parse(CodecH265, b)
	if len(units) != 1 || !units[0].Keyframe {
		t.Fatalf("units=%+v", units)
	}
}

// obu builds one OBU with a leb128 size field.
func obu(typ int, payload ...byte) []byte {
	h := byte(typ<<3) | 0x02 // has_size
	out := []byte{h, byte(len(payload))}
	return append(out, payload...)
}

func TestParseAV1(t *testing.T) {
	var b []byte
	b = append(b, obu(1, 0x0a)...)       // sequence header
	b = append(b, obu(2)...)             // temporal delimiter
	b = append(b, obu(6, 0x01, 0x00)...) // frame, keyframe flag set
	b = append(b, obu(2)...)             // temporal delimiter
	b = append(b, obu(6, 0x00, 0x00)...) // frame, not key
	units, ranges := Parse(CodecAV1, b)
	if len(ranges) != 0 {
		t.Fatalf("ranges=%v", ranges)
	}
	if len(units) != 2 || !units[0].Keyframe || units[1].Keyframe {
		t.Fatalf("units=%+v", units)
	}
}

func TestParseAV1Truncated(t *testing.T) {
	b := obu(2)
	b = append(b, obu(6, 0x01, 0x00, 0x00)...)
	units, ranges := Parse(CodecAV1, b[:len(b)-1]) // overrun size field
	if len(ranges) != 1 {
		t.Fatalf("ranges=%v", ranges)
	}
	if len(units) != 0 {
		t.Fatalf("units=%+v", units)
	}
}

func TestParseEmpty(t *testing.T) {
	for _, c := range []Codec{CodecH264, CodecH265, CodecAV1} {
		units, ranges := Parse(c, nil)
		if len(units) != 0 || len(ranges) != 0 {
			t.Fatalf("codec %v: units=%v ranges=%v", c, units, ranges)
		}
	}
}
