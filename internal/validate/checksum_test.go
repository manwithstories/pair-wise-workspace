package validate

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"mediq/internal/model"
)

func frame(seq uint64, ts uint64, payload []byte) *model.Frame {
	meta := EncodeMetadata(seq, ts, model.Timebase{Num: 90000, Den: 1})
	return &model.Frame{
		Seq:         seq,
		Timestamp:   ts,
		Timebase:    model.Timebase{Num: 90000, Den: 1},
		Meta:        meta,
		Payload:     payload,
		Hash:        sha256.Sum256(payload),
		MetaOffset:  72,
		FrameOffset: 72 + int64(len(meta)),
	}
}

func TestCheckFrameOK(t *testing.T) {
	fr := frame(1, 3000, []byte("hello"))
	if fault := CheckFrame(fr); fault != nil {
		t.Fatal(fault)
	}
}

func TestCheckFrameHashMismatch(t *testing.T) {
	fr := frame(7311, 3000, []byte("hello"))
	fr.Payload[0] ^= 0xFF
	fault := CheckFrame(fr)
	if fault == nil || fault.Code != model.CodeHashMismatch {
		t.Fatalf("got %+v", fault)
	}
	if fault.Seq != 7311 {
		t.Fatalf("frame %d", fault.Seq)
	}
	if fault.Offset != fr.FrameOffset {
		t.Fatalf("offset %d want %d", fault.Offset, fr.FrameOffset)
	}
	expected := hex.EncodeToString(fr.Hash[:])
	actual := hex.EncodeToString(validateHash(fr.Payload))
	if fault.Expected != expected || fault.Actual != actual {
		t.Fatalf("expected=%s actual=%s want=%s/%s", fault.Expected, fault.Actual, expected, actual)
	}
	if len(fault.Expected) != 64 || len(fault.Actual) != 64 {
		t.Fatal("digests must be 64 hex chars")
	}
	if !strings.Contains(fault.Error(), "frame=7311") {
		t.Fatalf("error text %q", fault.Error())
	}
}

func TestCheckFrameMetadataMismatch(t *testing.T) {
	fr := frame(2, 3000, []byte("x"))
	// Rewrite the metadata carrier's timestamp so it disagrees with the header.
	fr.Meta = EncodeMetadata(2, 9999, fr.Timebase)
	fault := CheckFrame(fr)
	if fault == nil || fault.Code != model.CodeMetadataMismatch {
		t.Fatalf("got %+v", fault)
	}
	if fault.Offset != fr.MetaOffset {
		t.Fatalf("offset %d", fault.Offset)
	}
}

func TestCheckFrameMetadataCorrupt(t *testing.T) {
	fr := frame(3, 3000, []byte("x"))
	fr.Meta = []byte{0x00, 0x01}
	fault := CheckFrame(fr)
	if fault == nil || fault.Code != model.CodeMetadataMismatch {
		t.Fatalf("got %+v", fault)
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		seq uint64
		ts  uint64
	}{
		{0, 0},
		{1, 3000},
		{65535, 0xFFFFFFFF},
		{70000, 1 << 40}, // beyond the 32-bit RTP field
	} {
		meta := EncodeMetadata(tc.seq, tc.ts, model.Timebase{Num: 30000, Den: 1001})
		md, fault := ParseMetadata(meta, tc.seq, 0)
		if fault != nil {
			t.Fatalf("%+v: %v", tc, fault)
		}
		if md.Seq != tc.seq || md.Timestamp != tc.ts || md.Timebase != (model.Timebase{Num: 30000, Den: 1001}) {
			t.Fatalf("%+v -> %+v", tc, md)
		}
	}
}

func TestParseMetadataRejectsFrameBytes(t *testing.T) {
	// Frame payloads embed a sequence number little-endian; feeding payload
	// bytes to the RTP parser must fail rather than be mistaken for metadata.
	if _, fault := ParseMetadata([]byte{1, 0, 0, 0, 0, 0, 0, 0}, 1, 72); fault == nil {
		t.Fatal("accepted non-RTP bytes")
	}
}

func validateHash(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
