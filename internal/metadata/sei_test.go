package metadata

import (
	"errors"
	"testing"

	"fastpost/internal/model"
)

// firstFault extracts the first structured fault from an error returned by
// a metadata parser. Parsers aggregate faults inside *model.InputError.
func firstFault(t *testing.T, err error) model.Fault {
	t.Helper()
	var in *model.InputError
	if !errors.As(err, &in) || len(in.Faults) == 0 {
		t.Fatalf("err = %v, want an *InputError with at least one fault", err)
	}
	return in.Faults[0]
}

var testRate = RationalLike{Num: 30000, Den: 1001}

// escapeRBSP applies Annex-B emulation-prevention encoding: after two or
// more zero bytes, a following byte 0x00..0x03 is preceded by 0x03.
func escapeRBSP(in []byte) []byte {
	var out []byte
	zeros := 0
	for _, b := range in {
		if zeros >= 2 && b <= 3 {
			out = append(out, 0x03)
			zeros = 2 // spec: count stays at 2 after insertion
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// buildTimingNAL constructs an Annex-B SEI NAL unit carrying one timing
// message and an rbsp_trailing marker. Layout of one escaped NAL:
//
//	[00 00 01][06][pType][pSize][16 body bytes][80 marker]
//	 0..2       3   4      5      6..21          22
func buildTimingNAL(frame int32, skew, pos int64) []byte {
	var payload []byte
	payload = append(payload, byte(timingPayloadType))
	var body [16]byte
	// frameNum uint32, skew uint32, position uint64 (big-endian).
	body[0] = byte(uint32(frame) >> 24)
	body[1] = byte(uint32(frame) >> 16)
	body[2] = byte(uint32(frame) >> 8)
	body[3] = byte(uint32(frame))
	body[4] = byte(uint32(skew) >> 24)
	body[5] = byte(uint32(skew) >> 16)
	body[6] = byte(uint32(skew) >> 8)
	body[7] = byte(uint32(skew))
	for i := 0; i < 8; i++ {
		body[8+i] = byte(uint64(pos) >> (56 - 8*i))
	}
	payload = append(payload, byte(len(body)))
	payload = append(payload, body[:]...)
	payload = append(payload, 0x80) // rbsp_trailing_bits
	nal := []byte{0, 0, 0, 1, 0x06}
	return append(nal, escapeRBSP(payload)...)
}

func TestParseSEIRoundTrip(t *testing.T) {
	data := buildTimingNAL(7, 120, 3003)
	got, err := ParseSEI("cam1", data, testRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	ev := got[0]
	if ev.FrameNum != 7 || ev.Skew != 120 {
		t.Fatalf("frame/skew = %d/%d", ev.FrameNum, ev.Skew)
	}
	// Body starts at absolute offset 7: 3-byte start(0..2), NAL header(3),
	// pType(4), pSize(5..6 are index 0,1 of payload), body index 2 => 4+1+2=7.
	if ev.Event.Position != 3003 || ev.Event.Offset != 7 {
		t.Fatalf("position/offset = %d/%d, want 3003/7", ev.Event.Position, ev.Event.Offset)
	}
	if ev.Event.Track != "cam1" || ev.Event.Kind != KindSEI {
		t.Fatalf("track/kind = %s/%s", ev.Event.Track, ev.Event.Kind)
	}
}

func TestParseSEIMultipleNALs(t *testing.T) {
	data := append(buildTimingNAL(1, 0, 0), buildTimingNAL(2, 0, 3003)...)
	got, err := ParseSEI("cam2", data, testRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	if got[1].Event.Offset <= got[0].Event.Offset {
		t.Fatalf("offsets not increasing: %d then %d", got[0].Event.Offset, got[1].Event.Offset)
	}
}

func TestParseSEITruncatedAtPayloadSize(t *testing.T) {
	// Start code + NAL header + payloadType byte, then cut. The next byte
	// (absolute offset 5) would be payload_size and is missing.
	data := []byte{0, 0, 1, 0x06, byte(timingPayloadType)}
	_, err := ParseSEI("cam3", data, testRate)
	f := firstFault(t, err)
	if f.Kind != model.FaultSEITruncated {
		t.Fatalf("kind = %v, want sei-truncated", f.Kind)
	}
	// headerOff=3, payload base=4; payload_size would be payload index 1 => 5.
	if f.Offset != 5 {
		t.Fatalf("fault offset = %d, want 5", f.Offset)
	}
}

func TestParseSEITruncatedBodyFirstMissingByte(t *testing.T) {
	// headerOff=3 so payload base=4. The 6-byte payload occupies absolute
	// offsets 4..9, so the first byte the 16-byte claim cannot provide is
	// absolute offset 10.
	payload := append([]byte{byte(timingPayloadType), 16}, 1, 2, 3, 4)
	data := append([]byte{0, 0, 1, 0x06}, payload...)
	_, err := ParseSEI("cam4", data, testRate)
	f := firstFault(t, err)
	if f.Kind != model.FaultSEITruncated {
		t.Fatalf("kind = %v, want sei-truncated", f.Kind)
	}
	if f.Offset != 10 {
		t.Fatalf("fault offset = %d, want 10", f.Offset)
	}
}

func TestParseSEIEmulationPrevention(t *testing.T) {
	// A body whose last position bytes are all zero forces several
	// 0x000003 escape insertions; decoding must still recover the 16-byte
	// body and frame position exactly.
	data := buildTimingNAL(9, 0, 0)
	got, err := ParseSEI("cam5", data, testRate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1 (EP stripping broke framing)", len(got))
	}
	if got[0].FrameNum != 9 || got[0].Event.Position != 0 {
		t.Fatalf("frame/position = %d/%d, want 9/0", got[0].FrameNum, got[0].Event.Position)
	}
}

func TestParseSEINegativePosition(t *testing.T) {
	// Position high bit set: uint64 1<<63 decodes to a negative int64.
	data := buildTimingNAL(1, 0, -1)
	_, err := ParseSEI("cam6", data, testRate)
	f := firstFault(t, err)
	if f.Kind != model.FaultUnmappableEvent {
		t.Fatalf("kind = %v, want unmappable-event", f.Kind)
	}
}

func TestParseSEIEmpty(t *testing.T) {
	if _, err := ParseSEI("cam7", nil, testRate); err != nil {
		t.Fatalf("empty stream should be tolerated, got %v", err)
	}
	if _, err := ParseSEI("cam8", []byte{0, 0, 1, 0x01}, testRate); err == nil {
		t.Fatal("non-SEI-only stream must fault")
	}
}
