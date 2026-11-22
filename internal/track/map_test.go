package track_test

import (
	"testing"

	"mediapack/internal/codec"
	"mediapack/internal/fixture"
	"mediapack/internal/index"
	"mediapack/internal/track"
)

func wrapStream(codecKind codec.Codec, frames, keyEvery, wrapAt int) []codec.AccessUnit {
	const mod = int64(1) << 32
	aus := make([]codec.AccessUnit, frames)
	for i := range aus {
		// Monotonic full-range PTS with only the low 32 bits retained, so a
		// wrap at frame wrapAt reuses a tick value seen earlier.
		raw := int64(i)*3000 + (mod - int64(wrapAt)*3000)
		aus[i] = codec.AccessUnit{
			Codec: codecKind, TrackID: 1, Seq: int64(i),
			PTS: raw & (mod - 1), Key: i%keyEvery == 0,
		}
	}
	return aus
}

func TestMapAndSummary(t *testing.T) {
	data, keySeqs, keyOffs, err := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H264, Seed: 1, Frames: 10, KeyEvery: 5,
	}, fixture.Fault{})
	if err != nil {
		t.Fatal(err)
	}
	aus, _, err := codec.ParseBytes(data, 1)
	if err != nil {
		t.Fatal(err)
	}
	blob := fixture.BuildIndex([]fixture.IndexEntry{
		{FrameSeq: keySeqs[0], Offset: keyOffs[0]},
		{FrameSeq: keySeqs[1], Offset: keyOffs[1]},
	})
	entries, reports, err := index.Parse(blob, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	tr := track.Map(1, track.Video, codec.H264, aus, entries, reports)
	s := track.Summarize([]*track.Track{tr})
	if s.TotalAU != 10 || s.Keyframes != 2 {
		t.Fatalf("counts wrong: %+v", s)
	}
	// 10 frames at 3000/90000 s each => 300 ms span.
	if s.DurationMs != 300 {
		t.Fatalf("duration = %d ms", s.DurationMs)
	}
}

func TestWraparoundReconstruction(t *testing.T) {
	// Uniform small steps crossing the 32-bit boundary once; after the wrap
	// the timeline continues monotonically on the unwrapped axis.
	ticks := []uint32{0xFFFFFFF8, 0xFFFFFFFC, 0, 4, 8, 12}
	aus := make([]codec.AccessUnit, len(ticks))
	for i, ts := range ticks {
		aus[i] = codec.AccessUnit{
			Codec: codec.H265, TrackID: 1, Seq: int64(i), PTS: int64(ts),
			Key: i == 0,
		}
	}
	tr := track.Map(1, track.Video, codec.H265, aus, nil, nil)
	if tr.WrapCount != 1 {
		t.Fatalf("wrap count = %d want 1", tr.WrapCount)
	}
	for i := 1; i < len(tr.UnwrappedPTS); i++ {
		if tr.UnwrappedPTS[i] <= tr.UnwrappedPTS[i-1] {
			t.Fatalf("unwrapped PTS not monotonic at %d: %d <= %d",
				i, tr.UnwrappedPTS[i], tr.UnwrappedPTS[i-1])
		}
	}
	// The post-wrap tick must sit one full modulus above where a naive
	// reading would place it.
	if tr.UnwrappedPTS[2] != 1<<32 {
		t.Fatalf("first post-wrap tick = %d", tr.UnwrappedPTS[2])
	}
}

func TestDuplicateAndNonMonotonicIndex(t *testing.T) {
	aus := wrapStream(codec.AV1, 8, 2, 99)
	blob := fixture.BuildIndex([]fixture.IndexEntry{
		{FrameSeq: 0, Offset: 0},
		{FrameSeq: 4, Offset: 300},
		{FrameSeq: 4, Offset: 250}, // duplicate + backwards offset
		{FrameSeq: 6, Offset: 500},
	})
	entries, reports, err := index.Parse(blob, 1024)
	if err != nil {
		t.Fatal(err)
	}
	tr := track.Map(2, track.Video, codec.AV1, aus, entries, reports)
	s := track.Summarize([]*track.Track{tr})
	if s.DuplicateKeys != 1 || s.NonMonotonic != 1 {
		t.Fatalf("anomaly counts wrong: %+v", s)
	}
	if len(tr.KeyframeSeqs) != 4 { // parser-derived keyframes unaffected
		t.Fatalf("parser keyframes = %v", tr.KeyframeSeqs)
	}
}

func TestTimebaseConversion(t *testing.T) {
	aus := wrapStream(codec.H264, 30, 10, 99) // one second of 30fps media
	tr := track.Map(1, track.Video, codec.H264, aus, nil, nil)
	s := track.Summarize([]*track.Track{tr})
	if s.LastPTSMs-s.FirstPTSMs < 966 || s.LastPTSMs-s.FirstPTSMs > 1000 {
		t.Fatalf("one second of media resolved to %d ms", s.DurationMs)
	}
}
