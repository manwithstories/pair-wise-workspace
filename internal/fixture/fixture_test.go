package fixture

import (
	"bytes"
	"testing"

	"relayaudit/internal/clock"
	"relayaudit/internal/wire"
)

func mustGenerate(t *testing.T, spec Spec) *Segment {
	t.Helper()
	seg, err := Generate(spec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return seg
}

func TestGenerateIsReproducible(t *testing.T) {
	a := mustGenerate(t, Spec{Seed: 73, Size: 1 << 18})
	b := mustGenerate(t, Spec{Seed: 73, Size: 1 << 18})
	if !bytes.Equal(a.Bytes, b.Bytes) {
		t.Fatalf("same seed produced different bytes")
	}
	c := mustGenerate(t, Spec{Seed: 74, Size: 1 << 18})
	if bytes.Equal(a.Bytes, c.Bytes) {
		t.Fatalf("different seed produced identical bytes")
	}
}

func TestGenerateFitsSizeBudget(t *testing.T) {
	const size = 1 << 19
	seg := mustGenerate(t, Spec{Seed: 1, Size: size})
	if len(seg.Bytes) > size {
		t.Fatalf("segment %d exceeds budget %d", len(seg.Bytes), size)
	}
	if size-len(seg.Bytes) > wire.MaxRecordSize {
		t.Fatalf("segment %d wastes %d bytes of the budget", len(seg.Bytes), size-len(seg.Bytes))
	}
}

func TestGenerateHasFourTracksAndValidIndex(t *testing.T) {
	seg := mustGenerate(t, Spec{Seed: 5, Size: 1 << 19})
	h, err := wire.ParseHeader(seg.Bytes)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if h.Tracks != 4 {
		t.Fatalf("tracks = %d, want 4", h.Tracks)
	}
	if err := h.ValidateBounds(int64(len(seg.Bytes))); err != nil {
		t.Fatalf("bounds: %v", err)
	}
	idx, err := wire.ParseIndex(seg.Bytes, h)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := wire.VerifyIndex(idx, seg.Bytes, h); err != nil {
		t.Fatalf("verify index: %v", err)
	}
	seen := map[uint8]int{}
	for _, r := range seg.Records {
		if !r.ValidCRC() {
			t.Fatalf("record at %d has bad crc", r.Offset)
		}
		seen[r.Track]++
	}
	for _, track := range []uint8{1, 2, 3, 4} {
		if seen[track] == 0 {
			t.Fatalf("track %d has no records", track)
		}
	}
}

func TestGeneratedPTSWrapsDuringSegment(t *testing.T) {
	seg := mustGenerate(t, Spec{Seed: 9, Size: 1 << 21})
	first := seg.Records[0].PTS
	last := seg.Records[len(seg.Records)-1].PTS
	if !(last < first) {
		t.Fatalf("expected PTS to wrap: first=%d last=%d", first, last)
	}
	e := new(clock.Extender)
	var prev uint64
	for i, r := range seg.Records {
		v, _ := e.Next(r.PTS)
		if i > 0 && r.Track == seg.Records[i-1].Track && v < prev {
			t.Fatalf("extended clock regressed at record %d", i)
		}
		prev = v
	}
	if e.Wraps() == 0 {
		t.Fatalf("extender observed no wrap across the segment")
	}
}

func TestDamageIsRecordedAtExpectedOffsets(t *testing.T) {
	seg := mustGenerate(t, Spec{Seed: 73, Size: 1 << 19, CorruptRecord: 17, TruncateTail: 41, DropIndexPage: 2})
	if seg.Damage.CorruptRecord != 17 {
		t.Fatalf("corrupt record not recorded")
	}
	rec := seg.Records[16]
	if seg.Damage.CorruptRecordOff != rec.Offset {
		t.Fatalf("corrupt offset = %d, want %d", seg.Damage.CorruptRecordOff, rec.Offset)
	}
	if seg.Damage.CorruptByteOff <= rec.Offset || seg.Damage.CorruptByteOff >= rec.End() {
		t.Fatalf("corrupted byte %d outside its record [%d,%d)", seg.Damage.CorruptByteOff, rec.Offset, rec.End())
	}
	if seg.Damage.DroppedPageOffset != int64(wire.HeaderSize)+wire.PageSize {
		t.Fatalf("dropped page offset = %d", seg.Damage.DroppedPageOffset)
	}
	if seg.Damage.CleanBytes != len(seg.Bytes)+seg.Damage.TruncateTail {
		t.Fatalf("truncation bookkeeping inconsistent: clean=%d live=%d tail=%d",
			seg.Damage.CleanBytes, len(seg.Bytes), seg.Damage.TruncateTail)
	}
}

func TestGenerateRejectsBadSpec(t *testing.T) {
	if _, err := Generate(Spec{Seed: 1, Size: 100}); err == nil {
		t.Fatalf("tiny size accepted")
	}
	if _, err := Generate(Spec{Seed: 1, Size: 1 << 17, CorruptRecord: 1 << 20}); err == nil {
		t.Fatalf("out-of-range corrupt record accepted")
	}
	if _, err := Generate(Spec{Seed: 1, Size: 1 << 17, DropIndexPage: 100}); err == nil {
		t.Fatalf("out-of-range index page accepted")
	}
}
