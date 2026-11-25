package fingerprint

import (
	"testing"

	"streamcut/internal/media"
)

func videoAU(seed byte, pts media.Time90) media.AccessUnit {
	return media.AccessUnit{
		Kind: media.KindVideoH264, PTS: pts, DTS: pts,
		HasPTS: true, HasDTS: true,
		Data: []byte{0, 0, 0, 1, 0x65, seed, seed + 1},
	}
}

func audioAU(seed byte, pts media.Time90) media.AccessUnit {
	return media.AccessUnit{
		Kind: media.KindAudioPCM16LE, PTS: pts, HasPTS: true,
		Data: []byte{seed, seed + 1, seed + 2, seed + 3},
	}
}

// TestDuplicateContentMatchesAcrossTimestamps proves the fingerprint ignores
// transport framing and timestamps: identical payloads replayed later in the
// program compare equal.
func TestDuplicateContentMatchesAcrossTimestamps(t *testing.T) {
	cfg := ConfigHash{
		VideoConfig: []byte{0x67, 0x42, 0x00},
		AudioConfig: []byte("pcm16le-48k-mono"),
	}
	a := SegmentInput{
		Video: []media.AccessUnit{videoAU(1, 0), videoAU(2, 3000)},
		Audio: []media.AccessUnit{audioAU(9, 0)},
		Cfg:   cfg,
	}
	// Same bytes, shifted timestamps (the replay case in the fixtures).
	b := SegmentInput{
		Video: []media.AccessUnit{videoAU(1, 9_000_000), videoAU(2, 9_003_000)},
		Audio: []media.AccessUnit{audioAU(9, 9_000_000)},
		Cfg:   cfg,
	}
	if ContentHash(a) != ContentHash(b) {
		t.Fatal("identical content at different timestamps must hash equal")
	}
	idx := NewIndex()
	id1, dup1, _ := idx.Observe(a)
	if dup1 {
		t.Fatal("first occurrence is not a duplicate")
	}
	id2, dup2, canonical := idx.Observe(b)
	if !dup2 || idx.Hits() != 1 {
		t.Fatalf("replay must be a duplicate (dup=%v hits=%d)", dup2, idx.Hits())
	}
	// Equal content means equal stable id; the canonical entry is the first.
	if canonical != id1 || id2 != id1 {
		t.Fatalf("canonical resolution wrong: id1=%s id2=%s canon=%s",
			id1, id2, canonical)
	}
}

// TestDifferentCodecConfigPreventsMatch verifies the codec-configuration
// guard: equal payloads with different SPS/PPS are not duplicates.
func TestDifferentCodecConfigPreventsMatch(t *testing.T) {
	au := []media.AccessUnit{videoAU(7, 0)}
	a := SegmentInput{Video: au, Cfg: ConfigHash{VideoConfig: []byte{0x67, 0xAA}}}
	b := SegmentInput{Video: au, Cfg: ConfigHash{VideoConfig: []byte{0x67, 0xBB}}}
	if ContentHash(a) == ContentHash(b) {
		t.Fatal("different codec config must change the fingerprint")
	}
	idx := NewIndex()
	idx.Observe(a)
	if _, dup, _ := idx.Observe(b); dup {
		t.Fatal("segments with different config must not dedupe")
	}
}

// TestDeterministicIDAcrossRuns ensures two independent indices assign the
// same id to the same content, which the resume demo relies on.
func TestDeterministicIDAcrossRuns(t *testing.T) {
	in := SegmentInput{
		Video: []media.AccessUnit{videoAU(3, 0)},
		Cfg:   ConfigHash{VideoConfig: []byte{0x67, 0x42}},
	}
	id1 := SegmentID(in)
	id2 := SegmentID(in)
	if id1 != id2 || id1 == "" {
		t.Fatalf("segment id must be stable: %q vs %q", id1, id2)
	}
	x1, x2 := NewIndex(), NewIndex()
	a, _, _ := x1.Observe(in)
	b, _, _ := x2.Observe(in)
	if a != b {
		t.Fatalf("fresh indices must agree on ids: %q vs %q", a, b)
	}
}
