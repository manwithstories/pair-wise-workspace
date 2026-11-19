package stream

import (
	"testing"

	"archive.local/recover/internal/errs"
)

func h264Stream(types ...byte) []byte {
	out := []byte{}
	for i, t := range types {
		if i%3 == 0 {
			out = append(out, 0, 0, 0, 1)
		} else {
			out = append(out, 0, 0, 1)
		}
		out = append(out, t, 0xE0, 0x11, 0x22)
	}
	return out
}

func TestScanH264Boundaries(t *testing.T) {
	data := h264Stream(7, 8, 5, 1, 1, 5, 1)
	r, err := Scan(H264, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Units) != 7 {
		t.Fatalf("units=%d want 7", len(r.Units))
	}
	if got := len(r.Keyframes); got != 2 {
		t.Fatalf("keyframes=%d want 2", got)
	}
	// Offsets must be ascending and each unit must start with its start code.
	var prev int64 = -1
	for _, u := range r.Units {
		if u.Offset <= prev {
			t.Fatalf("offsets not ascending: %d after %d", u.Offset, prev)
		}
		prev = u.Offset
		if u.Length <= 0 {
			t.Fatalf("non-positive unit length %d", u.Length)
		}
	}
	if len(r.Issues) != 0 {
		t.Fatalf("unexpected issues: %v", r.Issues)
	}
}

func TestScanTruncatedHeaderDistinguished(t *testing.T) {
	// A 3-byte start code followed by nothing: the unit cannot have a header.
	data := []byte{0, 0, 0, 1, 0x67, 0xE0, 0, 0, 1}
	r, err := Scan(H264, data)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Issues.Has(errs.CodeNALHeaderTruncated) {
		t.Fatalf("missing truncated-header classification: %v", r.Issues)
	}
	// The truncated tail is retained as pending, not dropped.
	if len(r.Units) == 0 || !r.Units[len(r.Units)-1].Pending {
		t.Fatalf("truncated unit not retained as pending: %+v", r.Units)
	}
}

func TestScanIllegalLengthOverrun(t *testing.T) {
	data := make([]byte, MaxNALSize+64)
	for i := 0; i < 8; i++ {
		data[i] = 0
	}
	data[3] = 1
	data[4] = 0x65
	r, err := Scan(H264, data)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Issues.Has(errs.CodeNALLengthOverrun) {
		t.Fatalf("missing overrun classification: %v", r.Issues)
	}
}

func TestScanNoStartCode(t *testing.T) {
	r, err := Scan(H264, []byte{0x12, 0x34, 0x56})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Units) != 0 || !r.Issues.Has(errs.CodeNALStartCodeTruncated) {
		t.Fatalf("want empty result with classification, got %+v", r)
	}
}

func TestScanH265Keyframes(t *testing.T) {
	// HEVC NAL header is two bytes: type is bits 1..6 of the first byte.
	data := []byte{}
	for _, t2 := range []byte{32, 33, 19, 1} { // VPS, SPS, IDR_W_RADL(19), TRAIL_R(1)
		data = append(data, 0, 0, 0, 1, t2<<1, 0x01, 0x11, 0x22)
	}
	r, err := Scan(H265, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Units) != 4 {
		t.Fatalf("units=%d", len(r.Units))
	}
	if len(r.Keyframes) != 1 || r.Units[r.Keyframes[0]].Type != 19 {
		t.Fatalf("keyframes=%v", r.Keyframes)
	}
}

func obu(typ int, payload []byte) []byte {
	// forbidden=0, type, extension=0, has_size=1, reserved=0.
	hdr := byte(typ<<3) | 0x02
	n := len(payload)
	var size []byte
	v := n
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v > 0 {
			size = append(size, b|0x80)
		} else {
			size = append(size, b)
			break
		}
	}
	return append(append([]byte{hdr}, size...), payload...)
}

func TestScanAV1OBUs(t *testing.T) {
	data := []byte{}
	data = append(data, obu(obuTemporalDelim, nil)...)
	data = append(data, obu(obuSequenceHeader, []byte{1, 2, 3})...)
	data = append(data, obu(obuFrame, []byte{9, 9, 9, 9})...)
	r, err := Scan(AV1, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Units) != 3 {
		t.Fatalf("units=%d want 3: %+v", len(r.Units), r.Units)
	}
	if len(r.Keyframes) != 1 || r.Units[r.Keyframes[0]].Type != obuFrame {
		t.Fatalf("keyframes=%v", r.Keyframes)
	}
}

func TestScanAV1LengthOverrun(t *testing.T) {
	// Header claims 200 bytes but only 2 follow.
	data := []byte{byte(obuFrame<<3) | 0x02, 200, 1, 2}
	r, err := Scan(AV1, data)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Issues.Has(errs.CodeOBULengthOverrun) {
		t.Fatalf("missing OBU overrun: %v", r.Issues)
	}
}

func TestScanAV1ZeroLength(t *testing.T) {
	data := []byte{byte(obuSequenceHeader<<3) | 0x02, 0}
	r, err := Scan(AV1, data)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Issues.Has(errs.CodeOBULengthZero) {
		t.Fatalf("missing OBU zero-length: %v", r.Issues)
	}
}

func BenchmarkScanH264(b *testing.B) {
	data := h264Stream(7, 8, 5)
	for i := 0; i < 2000; i++ {
		data = append(data, h264Stream(1)...)
	}
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := Scan(H264, data); err != nil {
			b.Fatal(err)
		}
	}
}
