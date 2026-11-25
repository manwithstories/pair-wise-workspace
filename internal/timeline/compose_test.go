package timeline

import (
	"testing"

	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/model"
)

// makeTrack builds a track with the given durations in a 90 kHz timebase.
func makeTrack(id uint32, durations []int64) *model.Track {
	tb := model.MustTimebase(90000, 1)
	tr := &model.Track{Number: int(id), ID: id, Kind: model.Video, Codec: model.CodecMJPEG, Timebase: tb}
	var dts int64
	for i, d := range durations {
		tr.Samples = append(tr.Samples, model.Sample{
			Index: i, TrackID: id, DTS: dts, Duration: d, Sync: i%5 == 0, Codec: model.CodecMJPEG,
		})
		dts += d
	}
	tr.Duration = dts
	return tr
}

func TestFitDriftWithinBound(t *testing.T) {
	var anchors []Anchor
	for i := 0; i < 12; i++ {
		local := int64(i) * 9000
		node := local + local*180/1_000_000
		anchors = append(anchors, Anchor{Index: i, NodeTick: node, Local90k: local})
	}
	fit := FitDrift(anchors)
	if fit.Unaligned {
		t.Fatalf("unexpected unaligned: %s", fit.Reason)
	}
	if fit.PPM < 170 || fit.PPM > 190 {
		t.Fatalf("ppm = %d, want ~180", fit.PPM)
	}
}

func TestFitDriftExceedsBound(t *testing.T) {
	var anchors []Anchor
	for i := 0; i < 12; i++ {
		local := int64(i) * 9000
		node := local + local*800/1_000_000
		anchors = append(anchors, Anchor{Index: i, NodeTick: node, Local90k: local})
	}
	fit := FitDrift(anchors)
	if !fit.Unaligned {
		t.Fatalf("800 ppm must exceed the %d ppm bound", MaxDriftPPM)
	}
	if fit.PPM <= MaxDriftPPM {
		t.Fatalf("ppm = %d, want > %d", fit.PPM, MaxDriftPPM)
	}
}

func TestFitDriftNeedsTwoAnchors(t *testing.T) {
	if fit := FitDrift(nil); !fit.Unaligned {
		t.Fatal("empty anchors must be unalignable")
	}
	fit := FitDrift([]Anchor{{NodeTick: 5, Local90k: 5}})
	if !fit.Unaligned {
		t.Fatal("a single anchor cannot establish drift")
	}
}

func TestComposePreservesVariableFrameDurations(t *testing.T) {
	durs := []int64{3003, 3003, 4503, 6006, 3003, 3003, 4503, 6006}
	tr := makeTrack(1, durs)
	comp := Compose([]TrackSource{{Track: tr}}, ComposeOptions{})
	if len(comp.Entries) != len(durs) {
		t.Fatalf("entries = %d, want %d", len(comp.Entries), len(durs))
	}
	if comp.VFRTracks != 1 {
		t.Fatalf("VFR tracks = %d, want 1", comp.VFRTracks)
	}
	if comp.MaxIntervals != 3 {
		t.Fatalf("distinct intervals = %d, want 3", comp.MaxIntervals)
	}
	for i, e := range comp.Entries {
		if e.Dur90k != durs[i] {
			t.Errorf("entry %d duration = %d, want %d (no fixed-rate padding)", i, e.Dur90k, durs[i])
		}
	}
	// No gap or overlap: the end of one entry is the start of the next.
	for i := 1; i < len(comp.Entries); i++ {
		if comp.Entries[i].Start90k != comp.Entries[i-1].End() {
			t.Errorf("entry %d starts at %d, previous ends at %d",
				i, comp.Entries[i].Start90k, comp.Entries[i-1].End())
		}
	}
}

func TestComposeSortKey(t *testing.T) {
	a := makeTrack(2, []int64{1000, 1000})
	b := makeTrack(1, []int64{1000, 1000})
	comp := Compose([]TrackSource{{Track: a}, {Track: b}}, ComposeOptions{})
	if len(comp.Entries) != 4 {
		t.Fatalf("entries = %d", len(comp.Entries))
	}
	// Ties on start time are broken by track number, then sample index.
	want := []struct{ track, sample int }{
		{1, 0}, {2, 0}, {1, 1}, {2, 1},
	}
	for i, w := range want {
		e := comp.Entries[i]
		if e.Track != w.track || e.SampleIdx != w.sample {
			t.Errorf("entry %d = track %d sample %d, want %d/%d",
				i, e.Track, e.SampleIdx, w.track, w.sample)
		}
	}
}

func TestComposeExcludesUnalignedDrift(t *testing.T) {
	tr := makeTrack(1, []int64{3000, 3000, 3000})
	var anchors []Anchor
	for i := 0; i < 6; i++ {
		local := int64(i) * 9000
		node := local + local*900/1_000_000
		anchors = append(anchors, Anchor{Index: i, NodeTick: node, Local90k: local})
	}
	comp := Compose([]TrackSource{{Track: tr, Anchors: anchors}}, ComposeOptions{})
	if len(comp.Entries) != 0 {
		t.Fatalf("unalignable track produced %d entries", len(comp.Entries))
	}
	if len(comp.Unaligned) != 1 {
		t.Fatalf("unaligned = %v, want [1]", comp.Unaligned)
	}
	fe, ok := fault.As(comp.Faults.Items[0])
	if !ok || fe.Kind != fault.KindDrift {
		t.Fatalf("expected drift fault, got %v", comp.Faults)
	}
	if !fe.Recoverable {
		t.Fatal("drift exclusion should be recoverable")
	}
}

func TestComposeRetimesNodeSamplesThroughDrift(t *testing.T) {
	// The node runs fast by 400 ppm; the fitted inverse must bring its
	// timestamps back onto local time within a tick or two.
	const ppm = 400
	base := int64(90_000)
	tr := makeTrack(1, []int64{3000, 3000, 3000, 3000})
	var anchors []Anchor
	nodeTicks := make([]int64, len(tr.Samples))
	for i := range tr.Samples {
		local := base + int64(i)*3000
		node := local + local*ppm/1_000_000
		anchors = append(anchors, Anchor{Index: i, NodeTick: node, Local90k: local})
		nodeTicks[i] = node
	}
	comp := Compose([]TrackSource{{
		Track: tr, Anchors: anchors, FromNode: true, NodeTicks: nodeTicks,
	}}, ComposeOptions{})
	if len(comp.Entries) != 4 {
		t.Fatalf("entries = %d, faults = %v", len(comp.Entries), comp.Faults)
	}
	for i, e := range comp.Entries {
		want := base + int64(i)*3000
		if diff := e.Start90k - want; diff < -2 || diff > 2 {
			t.Errorf("entry %d start = %d, want ~%d (diff %d)", i, e.Start90k, want, diff)
		}
	}
}

func TestComposeRequiresMJPEGOnly(t *testing.T) {
	tr := makeTrack(1, []int64{3000})
	tr.Codec = model.CodecH264
	comp := Compose([]TrackSource{{Track: tr}}, ComposeOptions{RequireMJPEG: true})
	if len(comp.Entries) != 0 {
		t.Fatalf("undecodable track produced %d entries", len(comp.Entries))
	}
	if comp.Faults.Len() != 1 {
		t.Fatalf("faults = %v", comp.Faults)
	}
}

func TestComposeBaseDropsEarlySamples(t *testing.T) {
	tr := makeTrack(1, []int64{3000, 3000, 3000})
	comp := Compose([]TrackSource{{Track: tr}}, ComposeOptions{Base90k: 3000})
	if len(comp.Entries) != 2 || comp.Discarded != 1 {
		t.Fatalf("entries = %d discarded = %d, want 2/1", len(comp.Entries), comp.Discarded)
	}
}

func TestComposeEntryCapIsFatal(t *testing.T) {
	tr := makeTrack(1, []int64{3000, 3000, 3000})
	comp := Compose([]TrackSource{{Track: tr}}, ComposeOptions{MaxEntries: 2})
	if len(comp.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(comp.Entries))
	}
	fe, ok := fault.As(comp.Faults.Items[0])
	if !ok || fe.Kind != fault.KindLimit || fe.Recoverable {
		t.Fatalf("expected fatal limit fault, got %v", comp.Faults)
	}
}
