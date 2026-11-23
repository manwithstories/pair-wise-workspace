package repair

import (
	"testing"

	"archive.local/archive/internal/model"
)

func repairedTrack(n int, syncEvery int, base int64) []Sample {
	out := make([]Sample, n)
	for i := range out {
		out[i] = Sample{Index: i, Unified: base + int64(i)*33_333,
			Offset: uint64(1000 + i*120), Size: 100, Sync: i%syncEvery == 0}
	}
	return out
}

func TestIndexDigestIsDeterministic(t *testing.T) {
	tracks := map[uint32][]Sample{
		1: repairedTrack(100, 10, 3_600_000_000),
		2: repairedTrack(80, 8, 3_600_000_000),
	}
	order := []uint32{1, 2}
	a, err := BuildIndex(tracks, order)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildIndex(tracks, order)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("digest changed between runs: %s vs %s", a.Digest, b.Digest)
	}
	if len(a.Digest) != 64 {
		t.Fatalf("digest length = %d", len(a.Digest))
	}
	// Rebuilt from logically identical but differently allocated data must
	// match too.
	dup := map[uint32][]Sample{}
	for k, v := range tracks {
		dup[k] = append([]Sample(nil), v...)
	}
	c, err := BuildIndex(dup, order)
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest != a.Digest {
		t.Fatalf("copy digest differs: %s vs %s", c.Digest, a.Digest)
	}
}

func TestIndexDigestChangesWithContent(t *testing.T) {
	tracks := map[uint32][]Sample{1: repairedTrack(100, 10, 3_600_000_000)}
	a, _ := BuildIndex(tracks, []uint32{1})
	tracks[1][50].Unified += 1
	b, _ := BuildIndex(tracks, []uint32{1})
	if a.Digest == b.Digest {
		t.Fatal("digest ignored a sample time change")
	}
}

func TestSparseKeyframeBackoff(t *testing.T) {
	tracks := map[uint32][]Sample{1: repairedTrack(60, 30, 0)}
	idx, err := BuildIndex(tracks, []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	ti := &idx.Tracks[0]
	if !ti.Sparse {
		t.Fatal("expected sparse track")
	}
	// Request time exactly one tick after sample 31; must resolve to the
	// keyframe at sample 30, not forward to sample 60.
	want := tracks[1][30].Unified
	res, err := ti.Resolve(want + 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Sample != 30 || res.Entry.Unified != want || !res.BackedOff {
		t.Fatalf("resolved = %+v", res)
	}
	// Every sample maps to a decodable entry at or before it.
	for i := range tracks[1] {
		e, ok := ti.ResolveSample(i)
		if !ok {
			t.Fatalf("sample %d has no entry", i)
		}
		if e.Sample > i {
			t.Fatalf("sample %d resolved forward to %d", i, e.Sample)
		}
	}
	e40, _ := ti.ResolveSample(40)
	if e40.Sample != 30 {
		t.Fatalf("sample 40 entry = %d", e40.Sample)
	}
	e59, _ := ti.ResolveSample(59)
	if e59.Sample != 30 {
		t.Fatalf("sample 59 entry = %d", e59.Sample)
	}
}

func TestIndexByteRanges(t *testing.T) {
	tracks := map[uint32][]Sample{1: repairedTrack(10, 5, 0)}
	idx, _ := BuildIndex(tracks, []uint32{1})
	ti := &idx.Tracks[0]
	if len(ti.Entries) != 2 {
		t.Fatalf("entries = %d", len(ti.Entries))
	}
	first := ti.Entries[0]
	if first.ByteStart != tracks[1][0].Offset {
		t.Fatalf("start = %d", first.ByteStart)
	}
	// First entry covers samples 0..4 inclusive; its end is sample 5's start.
	if first.ByteEnd != tracks[1][5].Offset {
		t.Fatalf("first end = %d want %d", first.ByteEnd, tracks[1][5].Offset)
	}
	last := ti.Entries[1]
	if last.ByteEnd != tracks[1][9].Offset+100 {
		t.Fatalf("last end = %d", last.ByteEnd)
	}
}

func TestIndexExactSeekDoesNotBackOff(t *testing.T) {
	tracks := map[uint32][]Sample{1: repairedTrack(40, 1, 0)}
	idx, _ := BuildIndex(tracks, []uint32{1})
	ti := &idx.Tracks[0]
	if ti.Sparse {
		t.Fatal("all-sync track should not be sparse")
	}
	res, _ := ti.Resolve(tracks[1][7].Unified)
	if res.BackedOff || res.Entry.Sample != 7 {
		t.Fatalf("res = %+v", res)
	}
}

func TestBlockFingerprintsDiffer(t *testing.T) {
	tracks := map[uint32][]Sample{1: repairedTrack(50, 10, 0)}
	idx, _ := BuildIndex(tracks, []uint32{1})
	tfp, err := idx.Tracks[0].BlockFingerprint(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	tfp2, _ := idx.Tracks[0].BlockFingerprint(2, 4)
	if tfp == tfp2 {
		t.Fatal("distinct blocks share a fingerprint")
	}
	if len(tfp) != 64 {
		t.Fatalf("fingerprint length = %d", len(tfp))
	}
	if _, err := idx.Tracks[0].BlockFingerprint(0, 99); err == nil {
		t.Fatal("out-of-range block accepted")
	}
}

func TestEmptyTrackIndexRejected(t *testing.T) {
	_, err := BuildIndex(map[uint32][]Sample{1: {}}, []uint32{1})
	if model.CodeOf(err) != model.CodeNoDecodePoint {
		t.Fatalf("code = %q", model.CodeOf(err))
	}
}

func TestCoverageRanges(t *testing.T) {
	tracks := map[uint32][]Sample{1: repairedTrack(10, 5, 0), 2: repairedTrack(10, 5, 0)}
	idx, _ := BuildIndex(tracks, []uint32{1, 2})
	if idx.Samples != 20 {
		t.Fatalf("samples = %d", idx.Samples)
	}
	if idx.Coverage.LastSample != 20 {
		t.Fatalf("coverage = %+v", idx.Coverage)
	}
}
