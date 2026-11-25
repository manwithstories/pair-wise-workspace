package codec

import (
	"testing"

	"mediad/internal/timeline"
)

func h264AU(key bool) []byte {
	var b []byte
	b = append(b, []byte{0, 0, 0, 1, 0x67, 0x42}...) // SPS
	b = append(b, []byte{0, 0, 0, 1, 0x68, 0xce}...) // PPS
	if key {
		b = append(b, []byte{0, 0, 0, 1, 0x65, 0x88}...) // IDR
	} else {
		b = append(b, []byte{0, 0, 0, 1, 0x41, 0x9a}...) // non-IDR
	}
	return b
}

func h265AU(key bool) []byte {
	var b []byte
	b = append(b, 0, 0, 0, 1, 0x40, 0x01, 0x0c) // VPS
	b = append(b, 0, 0, 0, 1, 0x42, 0x01, 0x01) // SPS
	b = append(b, 0, 0, 0, 1, 0x44, 0x01, 0xc0) // PPS
	if key {
		b = append(b, 0, 0, 0, 1, 0x26, 0x01, 0xaf) // IDR_W_RADL type 19
	} else {
		b = append(b, 0, 0, 0, 1, 0x02, 0x01, 0xaa) // TRAIL_R type 1
	}
	return b
}

func av1OBU(typ int, payload ...byte) []byte {
	h := byte(typ<<3) | 0x02
	out := []byte{h}
	out = append(out, EncodeLEB128(uint64(len(payload)))...)
	return append(out, payload...)
}

func av1AU(key bool) []byte {
	b := av1OBU(OBUSequenceHeader, 0x01, 0x02, 0x03)
	if key {
		// show_existing_frame=0, frame_type=KEY(0), show_frame=1.
		b = append(b, av1OBU(OBUFrame, 0x10, 0xaa, 0xbb)...)
	} else {
		// frame_type=INTER(1), show_frame=1.
		b = append(b, av1OBU(OBUFrame, 0x30, 0xcc, 0xdd)...)
	}
	return b
}

func TestValidateAllCodecs(t *testing.T) {
	cases := []struct {
		codec Codec
		key   []byte
		non   []byte
	}{
		{H264, h264AU(true), h264AU(false)},
		{H265, h265AU(true), h265AU(false)},
		{AV1, av1AU(true), av1AU(false)},
	}
	for _, tc := range cases {
		blob := append(append([]byte{}, tc.key...), tc.non...)
		idx := []timeline.Index{
			{Offset: 0, Length: len(tc.key), Keyframe: true},
			{Offset: int64(len(tc.key)), Length: len(tc.non), Keyframe: false},
		}
		rs := ValidateStream(tc.codec, blob, idx)
		if len(rs) != 2 {
			t.Fatalf("%s: reports=%d", tc.codec, len(rs))
		}
		for _, r := range rs {
			if !r.Valid {
				t.Fatalf("%s AU %d should be valid: %s", tc.codec, r.AUIndex, r.Error)
			}
			if r.DetectedKeyframe != r.ClaimedKeyframe {
				t.Fatalf("%s key mismatch report=%+v", tc.codec, r)
			}
			if r.Units == 0 {
				t.Fatalf("%s no units parsed", tc.codec)
			}
		}
	}
}

func TestKeyframeClaimMismatchIsIsolated(t *testing.T) {
	au := h264AU(false)
	idx := []timeline.Index{{Offset: 0, Length: len(au), Keyframe: true}}
	rs := ValidateStream(H264, au, idx)
	if rs[0].Valid || rs[0].FirstErrorByte < 0 {
		t.Fatalf("expected key claim error with byte offset: %+v", rs[0])
	}
}

func TestOutOfBoundsOffsetReportsByteAndDoesNotShiftLaterAU(t *testing.T) {
	a0 := h264AU(true)
	a1 := h264AU(false)
	a2 := h264AU(false)
	var blob []byte
	off1 := len(a0)
	off2 := off1 + len(a1)
	blob = append(blob, a0...)
	blob = append(blob, a1...)
	blob = append(blob, a2...)
	idx := []timeline.Index{
		{Offset: 0, Length: len(a0), Keyframe: true},
		{Offset: 1 << 20, Length: len(a1), Keyframe: false},
		{Offset: int64(off2), Length: len(a2), Keyframe: false},
	}
	rs := ValidateStream(H264, blob, idx)
	if rs[1].Valid || rs[1].FirstErrorByte != 1<<20 {
		t.Fatalf("bad offset report wrong: %+v", rs[1])
	}
	if !rs[2].Valid || rs[2].Offset != int64(off2) {
		t.Fatalf("later AU must remain at original offset: %+v", rs[2])
	}
	if !rs[0].Valid {
		t.Fatalf("first AU should be valid: %+v", rs[0])
	}
}

func TestTruncatedAUReportsFirstMissingByte(t *testing.T) {
	au := av1AU(false)
	idx := []timeline.Index{{Offset: 0, Length: len(au) + 32, Keyframe: false}}
	rs := ValidateStream(AV1, au, idx)
	if rs[0].Valid {
		t.Fatal("expected truncation")
	}
	if rs[0].FirstErrorByte != int64(len(au)) {
		t.Fatalf("first error byte=%d want EOF %d", rs[0].FirstErrorByte, len(au))
	}
}

func TestOversizedAURejected(t *testing.T) {
	idx := []timeline.Index{{Offset: 0, Length: MaxAUBytes + 1}}
	rs := ValidateStream(H264, nil, idx)
	if rs[0].Valid {
		t.Fatal("expected >8 MiB AU rejection")
	}
}

func TestAV1MalformedOBULengthHasOffset(t *testing.T) {
	// Header says frame OBU with has-size, then LEB128 size 200 but no body.
	au := []byte{0x32, 0xc8, 0x01}
	idx := []timeline.Index{{Offset: 100, Length: len(au)}}
	stream := make([]byte, 100)
	stream = append(stream, au...)
	rs := ValidateStream(AV1, stream, idx)
	if rs[0].Valid || rs[0].FirstErrorByte != int64(len(stream)) {
		t.Fatalf("expected truncation at first unavailable byte, got %+v", rs[0])
	}
}
