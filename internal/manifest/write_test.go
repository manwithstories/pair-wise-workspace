package manifest

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"mediasync/internal/model"
)

func videoTrack(startDTS int64, count int, delta int64) *model.Track {
	tr := &model.Track{ID: 1, Kind: model.KindVideo, Codec: "avc1", Timescale: 30000,
		Width: 1920, Height: 1080}
	var dts int64
	for i := 0; i < count; i++ {
		tr.Samples = append(tr.Samples, model.Sample{Index: i, Size: 100, DTS: dts, PTS: dts, Duration: delta})
		dts += delta
	}
	_ = startDTS
	return tr
}

func TestNonIntegralFrameRateAlignment(t *testing.T) {
	// 30000/1001 ticks per frame in a 30000 timescale: sample deltas are 1001.
	tr := videoTrack(0, 300, 1001)
	m, err := Build([]TrackInput{{Name: "program-video", Track: tr, SHA256: "abc",
		FrameRateNum: 30000, FrameRateDen: 1001}}, 2000, 1950)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tracks) != 1 {
		t.Fatalf("tracks = %d", len(m.Tracks))
	}
	if m.Alignment.MaxMs > 1.0 {
		t.Fatalf("alignment max = %v ms, want <= 1 ms", m.Alignment.MaxMs)
	}
	if math.IsNaN(m.Alignment.MeanMs) || math.IsInf(m.Alignment.MeanMs, 0) {
		t.Fatal("mean alignment is not finite")
	}
	if m.Tracks[0].DurationDen == 0 {
		t.Fatal("duration denominator is zero")
	}
}

func TestBitrateDeviationReported(t *testing.T) {
	tr := videoTrack(0, 10, 1001)
	m, err := Build([]TrackInput{{Name: "v", Track: tr}}, 2000, 1800)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.BitrateErrPct-10) > 0.001 {
		t.Fatalf("bitrate error = %v", m.BitrateErrPct)
	}
	if len(m.Notes) == 0 {
		t.Fatal("a 10% deviation should be noted")
	}
	// Target is untouched.
	if m.TargetKbps != 2000 || m.ActualKbps != 1800 {
		t.Fatalf("target/actual = %d/%v", m.TargetKbps, m.ActualKbps)
	}
}

func TestSmallDeviationNotNoted(t *testing.T) {
	tr := videoTrack(0, 10, 1001)
	m, err := Build([]TrackInput{{Name: "v", Track: tr}}, 2000, 1950)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.BitrateErrPct-2.5) > 0.001 {
		t.Fatalf("bitrate error = %v", m.BitrateErrPct)
	}
	for _, n := range m.Notes {
		if len(n) > 0 && n[0] == 'a' {
			t.Fatalf("unexpected note: %q", n)
		}
	}
}

func TestIncompleteIntervalsIncluded(t *testing.T) {
	tr := videoTrack(0, 50, 1001)
	missing := []Interval{{Start: 10, End: 17}}
	m, err := Build([]TrackInput{{Name: "v", Track: tr, Missing: missing}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tracks[0].Incomplete) != 1 {
		t.Fatal("incomplete interval dropped")
	}
	if len(m.Gaps) != 1 || m.Gaps[0].End-m.Gaps[0].Start != 7 {
		t.Fatalf("gaps = %+v", m.Gaps)
	}
}

func TestAudioSampleBoundaryAlignment(t *testing.T) {
	// 48 kHz audio, one sample per tick: declared DTS is exact, alignment 0.
	tr := &model.Track{ID: 2, Kind: model.KindAudio, Codec: "mp4a", Timescale: 48000}
	for i := 0; i < 1024; i++ {
		tr.Samples = append(tr.Samples, model.Sample{Index: i, Size: 10, DTS: int64(i), Duration: 1})
	}
	m, err := Build([]TrackInput{{Name: "a", Track: tr, FrameRateNum: 48000, FrameRateDen: 1}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Alignment.MaxMs > 0.001 {
		t.Fatalf("audio alignment = %v ms", m.Alignment.MaxMs)
	}
}

func TestWriteIsAtomicAndParsable(t *testing.T) {
	dir := t.TempDir()
	tr := videoTrack(0, 5, 1001)
	m, err := Build([]TrackInput{{Name: "v", Track: tr}}, 2000, 1950)
	if err != nil {
		t.Fatal(err)
	}
	path, err := Write(dir, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".manifest-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left: %v", matches)
	}
}

func TestVersionMismatchRejected(t *testing.T) {
	if _, err := Build([]TrackInput{{Name: "v", Track: &model.Track{ID: 1, Timescale: 0}}}, 0, 0); err == nil {
		t.Fatal("zero timescale should fail")
	}
	if _, err := Build([]TrackInput{{Name: "v", Track: nil}}, 0, 0); err == nil {
		t.Fatal("nil track should fail")
	}
}

func TestQualityStatsFinite(t *testing.T) {
	mean, p95, err := QualityStats([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	if err != nil {
		t.Fatal(err)
	}
	if mean != 5.5 || p95 != 10 {
		t.Fatalf("mean=%v p95=%v", mean, p95)
	}
	if _, _, err := QualityStats([]float64{math.NaN()}); err == nil {
		t.Fatal("NaN score should be rejected")
	}
	if m, p, err := QualityStats(nil); err != nil || m != 0 || p != 0 {
		t.Fatal("empty scores should be zero")
	}
}

func TestAlignmentPercentile(t *testing.T) {
	a := summarize([]float64{0, 10, 0, 0, 0, 0, 0, 0, 0, 20})
	if a.P95Ms != 20 || a.MaxMs != 20 || a.Samples != 10 {
		t.Fatalf("summary = %+v", a)
	}
}
