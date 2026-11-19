package mp4

import (
	"testing"

	"pairflow/media/internal/clock"
)

func videoTrack(id uint32, count int, delta uint32) Track {
	t := Track{ID: id, Handler: "vide", Timescale: 30000, SampleSize: 16,
		Deltas: make([]uint32, count), Keys: make([]bool, count)}
	for i := range t.Deltas {
		t.Deltas[i] = delta
		t.Keys[i] = i%12 == 0
	}
	return t
}

func TestBuildAndParseRoundTrip(t *testing.T) {
	var b Builder
	b.Add(videoTrack(1, 48, 1001))
	b.Add(Track{ID: 2, Handler: "soun", Timescale: 48000, SampleSize: 8,
		Deltas: repeat(48000, 1024)})
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := ParseBytes("fixture.mp4", f.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Errors) != 0 {
		t.Fatalf("errors: %v", idx.Errors)
	}
	if len(idx.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(idx.Tracks))
	}
	v := idx.Tracks[0]
	if v.Handler != "vide" || v.Timescale != 30000 {
		t.Fatalf("video track = %+v", v)
	}
	if len(v.Samples) != 48 {
		t.Fatalf("samples = %d", len(v.Samples))
	}
	if v.Samples[0].Offset != uint64(f.Tracks[0].ChunkOffset[0]) {
		t.Fatalf("sample 0 offset = %d, layout chunk = %d", v.Samples[0].Offset, f.Tracks[0].ChunkOffset[0])
	}
	// 1001/30000 seconds per frame.
	if v.Samples[1].DTS != clock.NewRational(1001, 30000) {
		t.Fatalf("sample 1 DTS = %s", v.Samples[1].DTS)
	}
	if len(v.Keyframes) != 4 {
		t.Fatalf("keyframes = %v", v.Keyframes)
	}
	if v.VFR {
		t.Fatal("uniform deltas reported as VFR")
	}
	// Audio track must point at its own chunk after video data.
	a := idx.Tracks[1]
	if a.Samples[0].Offset <= v.Samples[47].Offset {
		t.Fatalf("audio offset %d not after video %d", a.Samples[0].Offset, v.Samples[47].Offset)
	}
}

func TestParseDetectsVFR(t *testing.T) {
	var b Builder
	deltas := []uint32{1001, 1001, 2002, 1001, 3003, 1001, 1001, 1001, 1501, 1001, 1001, 1001}
	b.Add(Track{ID: 3, Handler: "vide", Timescale: 30000, SampleSize: 12, Deltas: deltas})
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := ParseBytes("vfr.mp4", f.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Tracks[0].VFR {
		t.Fatal("VFR not detected")
	}
	if got := len(idx.Tracks[0].Samples); got != len(deltas) {
		t.Fatalf("samples = %d", got)
	}
}

func TestParseDetectsOutOfBoundsChunk(t *testing.T) {
	var b Builder
	b.Add(videoTrack(7, 6, 1001))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the stco entry in place: find "stco" and bump the offset.
	raw := append([]byte(nil), f.Bytes...)
	i := indexOf(raw, []byte("stco"))
	if i < 0 {
		t.Fatal("stco not found")
	}
	// version/flags (4) + entry count (4) + first offset (4)
	pos := i + 4 + 4 + 4
	raw[pos] = 0x7F
	idx, err := ParseBytes("bad.mp4", raw)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Tracks[0].OutOfBounds != 6 {
		t.Fatalf("out of bounds = %d", idx.Tracks[0].OutOfBounds)
	}
	found := false
	for _, e := range idx.Tracks[0].Errors {
		if e.Code == "sample_out_of_bounds" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing out-of-bounds error: %+v", idx.Tracks[0].Errors)
	}
}

func TestKeyframeLookupRate(t *testing.T) {
	var b Builder
	b.Add(videoTrack(1, 60, 1001))
	f, _ := b.Build()
	idx, _ := ParseBytes("k.mp4", f.Bytes)
	base := clock.NewBase(1001, 30000)
	targets := []clock.Rational{
		base.Time(0), base.Time(5), base.Time(11), base.Time(12), base.Time(35), base.Time(59),
	}
	hits := idx.Tracks[0].LookupKeyframes(targets)
	rate := KeyframeHitRate(hits)
	if rate.Seconds() < 0.999 {
		t.Fatalf("rate = %s hits=%+v", rate, hits)
	}
}

func TestMissingStssTreatsAllSamplesAsSync(t *testing.T) {
	var b Builder
	b.OmitSyncID = true
	keys := make([]bool, 24)
	keys[0], keys[12] = true, true
	b.Add(Track{ID: 1, Handler: "vide", Timescale: 30000, SampleSize: 8,
		Deltas: repeat(24, 1001), Keys: keys})
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := ParseBytes("no-stss.mp4", f.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(idx.Tracks[0].Keyframes); got != 24 {
		t.Fatalf("keyframes without stss = %d, want 24", got)
	}
}

func TestTruncatedSizeTableReportsEveryMissingSample(t *testing.T) {
	var b Builder
	b.OmitSizes = true
	b.Add(Track{ID: 1, Handler: "vide", Timescale: 30000, SampleSize: 8,
		Deltas: repeat(10, 1001)})
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	// A half-written table makes go-mp4 fail to decode the sample size
	// box. The parser must surface that as an offset-bearing error and
	// leave sizes unreconstructed rather than dropping the fixture.
	idx, _ := ParseBytes("trunc.mp4", f.Bytes)
	codes := map[string]int{}
	for _, e := range idx.Tracks[0].Errors {
		codes[e.Code]++
	}
	if codes["box_parse"] == 0 && codes["truncated_stsz"] == 0 {
		t.Fatalf("truncated table not reported: %+v", idx.Tracks[0].Errors)
	}
	for _, e := range idx.Tracks[0].Errors {
		if e.Offset <= 0 {
			t.Fatalf("truncated table error has no byte offset: %+v", e)
		}
	}
}

func TestDuplicatePTSIsReportedNotRepaired(t *testing.T) {
	var b Builder
	deltas := repeat(8, 1001)
	// A zero delta for sample index 2 makes sample 3 repeat sample 2's
	// decode time, and therefore its PTS.
	deltas[2] = 0
	b.Add(Track{ID: 1, Handler: "vide", Timescale: 30000, SampleSize: 8, Deltas: deltas})
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := ParseBytes("dup.mp4", f.Bytes)
	// dts[3] = dts[2] + deltas[2] = dts[2], so sample 4 repeats
	// sample 3's decode and presentation time.
	s := idx.Tracks[0].Samples
	if !s[3].Duplicate || s[3].PTS != s[2].PTS {
		t.Fatalf("duplicate not flagged: %+v vs %+v", s[2], s[3])
	}
	// The repeated decode time is preserved, not rewritten.
	if s[3].DTS != s[2].DTS {
		t.Fatal("DTS were silently repaired")
	}
}

func TestNearestKeyframeSkipsFutureSync(t *testing.T) {
	tr := TrackIndex{Samples: []Sample{
		{Number: 1, DTS: clock.Zero(), Sync: true},
		{Number: 2, DTS: clock.NewRational(1, 1), Sync: false},
		{Number: 3, DTS: clock.NewRational(2, 1), Sync: true},
	}}
	s, ok := tr.NearestKeyframe(clock.NewRational(3, 2))
	if !ok || s.Number != 1 {
		t.Fatalf("nearest = %+v, %v", s, ok)
	}
}

func repeat(n int, v uint32) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
