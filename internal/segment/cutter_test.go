package segment

import (
	"testing"

	"mediapack/internal/clock"
	"mediapack/internal/demux"
)

// videoTrack builds a synthetic video track with keyframes at the given
// canonical-second positions.
func videoTrack(kfSeconds []int64, seconds int64) *demux.Track {
	tr := &demux.Track{ID: 1, Kind: "video", Timescale: 90000}
	kf := map[int64]bool{}
	for _, k := range kfSeconds {
		kf[k] = true
	}
	for i := int64(0); i < seconds*30; i++ {
		dts := i * 3000
		tr.Samples = append(tr.Samples, demux.Sample{
			Index: uint32(i), DTS: dts, PTS: dts, Duration: 3000,
			Size: 100, Keyframe: kf[i/30], Available: true,
		})
	}
	return tr
}

func TestCutLandsOnKeyframes(t *testing.T) {
	tr := videoTrack([]int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 10)
	bounds := Cut(tr, clock.TicksFromMillis(2000))
	if len(bounds) != 5 {
		t.Fatalf("bounds: %v", bounds)
	}
	for i, b := range bounds {
		want := int64(i) * 2 * 90000
		if b.Ticks != want {
			t.Fatalf("bound %d: got %d want %d", i, b.Ticks, want)
		}
	}
}

func TestCutSnapsToNearestKeyframe(t *testing.T) {
	// Keyframes only at 0, 2.1s, 4.9s: target 2s snaps to 2.1s, 4s to 4.9s.
	tr := &demux.Track{ID: 1, Kind: "video", Timescale: 90000}
	for _, sec := range []float64{0, 2.1, 4.9} {
		tr.Samples = append(tr.Samples, demux.Sample{
			Index: uint32(sec * 30), DTS: int64(sec * 90000), PTS: int64(sec * 90000),
			Duration: 3000, Size: 100, Keyframe: true, Available: true,
		})
	}
	// Fill duration via a long last sample.
	tr.Samples = append(tr.Samples, demux.Sample{
		Index: 1000, DTS: 8 * 90000, PTS: 8 * 90000, Duration: 2 * 90000,
		Size: 100, Available: true,
	})
	bounds := Cut(tr, clock.TicksFromMillis(2000))
	if len(bounds) != 3 {
		t.Fatalf("bounds: %v", bounds)
	}
	if bounds[1].Ticks != int64(2.1*90000) {
		t.Fatalf("bound1: %d", bounds[1].Ticks)
	}
	if bounds[2].Ticks != int64(4.9*90000) {
		t.Fatalf("bound2: %d", bounds[2].Ticks)
	}
}

func TestCutEmptyTrack(t *testing.T) {
	tr := &demux.Track{ID: 1, Kind: "video", Timescale: 90000}
	bounds := Cut(tr, clock.TicksFromMillis(2000))
	if len(bounds) != 1 || bounds[0].Ticks != 0 {
		t.Fatalf("bounds: %v", bounds)
	}
}

func TestCutSkipsUnavailableKeyframes(t *testing.T) {
	tr := videoTrack([]int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 10)
	// Make the 2s keyframe unavailable: the 2s target must snap elsewhere.
	for i := range tr.Samples {
		if tr.Samples[i].PTS == 2*90000 {
			tr.Samples[i].Available = false
		}
	}
	bounds := Cut(tr, clock.TicksFromMillis(2000))
	for _, b := range bounds {
		if b.Ticks == 2*90000 {
			t.Fatal("cut landed on unavailable keyframe")
		}
	}
}

func TestSegmentIndexNoOverlap(t *testing.T) {
	bounds := []Boundary{{Ticks: 0}, {Ticks: 100}, {Ticks: 200}}
	if got := SegmentIndex(0, bounds); got != 0 {
		t.Fatal(got)
	}
	if got := SegmentIndex(99, bounds); got != 0 {
		t.Fatal(got)
	}
	if got := SegmentIndex(100, bounds); got != 1 {
		t.Fatal(got)
	}
	if got := SegmentIndex(500, bounds); got != 2 {
		t.Fatal(got)
	}
}

func TestAssignAudio(t *testing.T) {
	audio := &demux.Track{ID: 2, Kind: "audio", Timescale: 90000}
	for i := 0; i < 10; i++ {
		audio.Samples = append(audio.Samples, demux.Sample{
			Index: uint32(i), DTS: int64(i) * 30000, PTS: int64(i) * 30000,
			Duration: 30000, Size: 10, Available: true,
		})
	}
	bounds := []Boundary{{Ticks: 0}, {Ticks: 90000}, {Ticks: 180000}}
	groups := AssignAudio(audio, bounds)
	if len(groups) != 3 {
		t.Fatalf("groups: %d", len(groups))
	}
	if len(groups[0]) != 3 || len(groups[1]) != 3 || len(groups[2]) != 4 {
		t.Fatalf("assignment: %v", groups)
	}
	// No sample appears in two segments.
	seen := map[uint32]int{}
	for gi, g := range groups {
		for _, idx := range g {
			if prev, dup := seen[idx]; dup {
				t.Fatalf("sample %d in segments %d and %d", idx, prev, gi)
			}
			seen[idx] = gi
		}
	}
}
