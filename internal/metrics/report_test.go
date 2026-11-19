package metrics

import (
	"strings"
	"testing"

	"mediapack/internal/clock"
	"mediapack/internal/demux"
	"mediapack/internal/ladder"
	"mediapack/internal/segment"
)

func videoTrack(id uint32, size uint32, seconds int64) *demux.Track {
	tr := &demux.Track{ID: id, Kind: "video", Timescale: 90000}
	for i := int64(0); i < seconds*30; i++ {
		dts := i * 3000
		tr.Samples = append(tr.Samples, demux.Sample{
			Index: uint32(i), DTS: dts, PTS: dts, Duration: 3000,
			Size: size, Keyframe: i%30 == 0, Available: true,
		})
	}
	return tr
}

func TestAssemblePairsBitratesAndDeviation(t *testing.T) {
	low := videoTrack(1, 100, 10)  // 100B*30fps*8 = 24000 bps
	high := videoTrack(2, 200, 10) // 48000 bps
	c := &demux.Container{Path: "x", Tracks: []*demux.Track{low, high}}
	bounds := []segment.Boundary{{Ticks: 0}, {Ticks: 5 * 90000}}
	align := ladder.Align([]*demux.Track{low, high}, bounds, clock.TicksFromMillis(80))
	m, err := Assemble(Input{
		Container: c, Bounds: bounds, Alignment: align,
		TargetBitrates: []uint64{48000, 24000}, // unsorted on purpose
		SegmentTargetMs: 5000, ToleranceMs: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Representations) != 2 {
		t.Fatalf("reps: %d", len(m.Representations))
	}
	// Lowest-bitrate track pairs with the lowest target.
	if m.Representations[0].TrackID != 1 || m.Representations[0].TargetBitrate != 24000 {
		t.Fatalf("rep0: %+v", m.Representations[0])
	}
	if m.Representations[0].ActualBitrate != 24000 {
		t.Fatalf("actual: %d", m.Representations[0].ActualBitrate)
	}
	if m.Representations[0].BitrateDeviationPct != 0 {
		t.Fatalf("deviation: %f", m.Representations[0].BitrateDeviationPct)
	}
	if m.Quality.Samples != 600 {
		t.Fatalf("quality samples: %d", m.Quality.Samples)
	}
	if len(m.Segments) != 2 || m.Segments[1].EndMs != 10000 {
		t.Fatalf("segments: %+v", m.Segments)
	}
}

func TestAssembleRejectsTargetCountMismatch(t *testing.T) {
	low := videoTrack(1, 100, 10)
	c := &demux.Container{Tracks: []*demux.Track{low}}
	_, err := Assemble(Input{
		Container: c, Bounds: []segment.Boundary{{Ticks: 0}},
		TargetBitrates: []uint64{1, 2},
	})
	if err == nil {
		t.Fatal("expected count mismatch error")
	}
}

func TestAssembleRejectsNoVideo(t *testing.T) {
	c := &demux.Container{Tracks: []*demux.Track{{ID: 9, Kind: "audio", Timescale: 48000}}}
	if _, err := Assemble(Input{Container: c}); err == nil {
		t.Fatal("expected no-video error")
	}
}

func TestRenderContainsRequiredKeys(t *testing.T) {
	low := videoTrack(1, 100, 10)
	c := &demux.Container{Path: "x", Tracks: []*demux.Track{low}}
	bounds := []segment.Boundary{{Ticks: 0}}
	m, err := Assemble(Input{
		Container: c, Bounds: bounds, Alignment: ladder.Align([]*demux.Track{low}, bounds, 1),
		TargetBitrates: []uint64{24000}, SegmentTargetMs: 2000, ToleranceMs: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	Render(&sb, m)
	for _, key := range []string{
		"segments:", "representations:", "target_bitrate=", "actual_bitrate=",
		"bitrate_deviation_pct=", "quality_samples:", "quality_skipped:",
		"max_alignment_error_ms:", "missing_intervals:", "checkpoint_recovered:",
	} {
		if !strings.Contains(sb.String(), key) {
			t.Fatalf("report missing key %q:\n%s", key, sb.String())
		}
	}
}
