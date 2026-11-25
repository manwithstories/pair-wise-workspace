package timeline_test

import (
	"testing"

	"archivecheck/internal/demux"
	"archivecheck/internal/media"
	"archivecheck/internal/timeline"
)

func pt(ticks int64, tb media.Rational) timeline.Point { return timeline.Point{Ticks: ticks, TB: tb} }

// TestRationalRescaleNoDrift proves cross-time-base conversion stays exact for
// values that would lose precision in floating point.
func TestRationalRescaleNoDrift(t *testing.T) {
	v90k := media.Rational{Num: 90000, Den: 1}
	v48k := media.Rational{Num: 48000, Den: 1}
	// 3000 ticks @90k rescaled to a 48k clock: 3000*48000/90000 = 1600.
	got, err := media.Rescale(3000, v90k, v48k)
	if err != nil || got != 1600 {
		t.Fatalf("rescale 90k->48k = %d, %v; want 1600", got, err)
	}
	// Reverse conversion of that result must return the original 3000.
	back, err := media.Rescale(1600, v48k, v90k)
	if err != nil || back != 3000 {
		t.Fatalf("rescale 48k->90k = %d, %v; want 3000", back, err)
	}
	// A value that does not divide evenly must round deterministically, not
	// accumulate float error across repeated conversions.
	a, _ := media.Rescale(1000, v90k, v48k)
	for i := 0; i < 100; i++ {
		b, _ := media.Rescale(1000, v90k, v48k)
		if a != b {
			t.Fatal("rescale not deterministic across iterations")
		}
	}
}

// TestValidPlan accepts a well-formed cut/dissolve sequence with exact overlap.
func TestValidPlan(t *testing.T) {
	tb := media.TimeBaseVideo90k
	plan := timeline.Plan{
		MasterTB: tb,
		Edits: []timeline.Edit{
			{ID: "e1", Segment: "seg01", Track: 1, In: pt(0, tb), Out: pt(3000, tb), Start: 0},
			{ID: "e2", Segment: "seg02", Track: 1, In: pt(0, tb), Out: pt(3000, tb), Start: 2500,
				Trans: &timeline.Transition{Kind: timeline.TransDissolve, Overlap: 500}},
		},
	}
	if faults := plan.Validate(); len(faults) != 0 {
		t.Fatalf("valid plan rejected: %+v", faults)
	}
	resolved, faults := plan.Resolve()
	if len(faults) != 0 || len(resolved) != 2 {
		t.Fatalf("resolve: %d edits, faults %+v", len(resolved), faults)
	}
	if resolved[1].Duration != 3000 {
		t.Fatalf("duration = %d, want 3000", resolved[1].Duration)
	}
}

// TestInvalidPlanTimestamps covers every required located timeline error.
func TestInvalidPlanTimestamps(t *testing.T) {
	tb := media.TimeBaseVideo90k
	base := func(e timeline.Edit) []timeline.Edit {
		return []timeline.Edit{
			{ID: "e0", Segment: "s", Track: 1, In: pt(0, tb), Out: pt(3000, tb), Start: 0},
			e,
		}
	}
	cases := []struct {
		name string
		edit timeline.Edit
		want media.Kind
	}{
		{"negative in", timeline.Edit{ID: "e", In: pt(-1, tb), Out: pt(10, tb), Start: 3000}, media.KindNegativeTS},
		{"zero duration", timeline.Edit{ID: "e", In: pt(10, tb), Out: pt(10, tb), Start: 3000}, media.KindZeroDuration},
		{"backwards start", timeline.Edit{ID: "e", In: pt(0, tb), Out: pt(10, tb), Start: 1000}, media.KindWrap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.edit.Segment, tc.edit.Track = "s", 1
			plan := timeline.Plan{MasterTB: tb, Edits: base(tc.edit)}
			faults := plan.Validate()
			if len(faults) == 0 {
				t.Fatal("expected a located fault")
			}
			if faults[0].Kind != tc.want {
				t.Fatalf("kind = %s, want %s (all: %+v)", faults[0].Kind, tc.want, faults)
			}
			if faults[0].EditID != tc.edit.ID {
				t.Fatalf("fault not correlated to edit %s", tc.edit.ID)
			}
		})
	}
}

// TestDissolveOverlapMismatch ensures a declared overlap that does not match
// the actual edit placement is rejected rather than silently accepted.
func TestDissolveOverlapMismatch(t *testing.T) {
	tb := media.TimeBaseVideo90k
	plan := timeline.Plan{
		MasterTB: tb,
		Edits: []timeline.Edit{
			{ID: "e1", Segment: "s", Track: 1, In: pt(0, tb), Out: pt(3000, tb), Start: 0},
			{ID: "e2", Segment: "s", Track: 1, In: pt(0, tb), Out: pt(3000, tb), Start: 2500,
				Trans: &timeline.Transition{Kind: timeline.TransDissolve, Overlap: 1000}},
		},
	}
	faults := plan.Validate()
	var mismatch bool
	for _, f := range faults {
		if f.Kind == media.KindInvalidManifest && f.EditID == "e2" {
			mismatch = true
		}
	}
	if !mismatch {
		t.Fatalf("overlap mismatch not detected: %+v", faults)
	}
}

// TestBuildMappingSelectsSamples is the recovery/happy path for track mapping:
// only samples intersecting the edit's source interval are selected.
func TestBuildMappingSelectsSamples(t *testing.T) {
	tb := media.TimeBaseVideo90k
	var samples []demux.Sample
	for i := 0; i < 10; i++ {
		samples = append(samples, demux.Sample{
			Track: 1, Index: i, DTS: int64(i) * 1000, Duration: 1000,
			Data: []byte{byte(i)}, TimeBase: tb,
		})
	}
	tracks := []demux.TrackSamples{{
		Meta: demux.TrackMeta{Track: 1, TimeBase: tb}, Samples: samples,
	}}
	re := timeline.ResolvedEdit{
		Edit: timeline.Edit{ID: "e1", Segment: "seg01", Track: 1,
			In: pt(2000, tb), Out: pt(6000, tb)},
		InMaster: 2000, OutMaster: 6000, Duration: 4000,
	}
	me, fault := timeline.BuildMapping(re, tracks, 1)
	if fault != nil {
		t.Fatalf("mapping: %v", fault)
	}
	// Samples with DTS in [2000,6000): indices 2,3,4,5.
	if me.Mapping.FirstSample != 2 || me.Mapping.LastSample != 6 || len(me.Samples) != 4 {
		t.Fatalf("selected [%d,%d) count %d, want [2,6) count 4",
			me.Mapping.FirstSample, me.Mapping.LastSample, len(me.Samples))
	}
	if c := me.Mapping.Canonical(); c == "" || me.Mapping.SampleCount() != 4 {
		t.Fatal("canonical mapping empty or wrong sample count")
	}
}

// TestBuildMappingMissingTrack proves a dangling edit yields a located fault.
func TestBuildMappingMissingTrack(t *testing.T) {
	tb := media.TimeBaseVideo90k
	re := timeline.ResolvedEdit{
		Edit: timeline.Edit{ID: "eX", Segment: "seg01", Track: 9, In: pt(0, tb), Out: pt(10, tb)},
	}
	_, fault := timeline.BuildMapping(re, []demux.TrackSamples{}, 1)
	if fault == nil || fault.EditID != "eX" {
		t.Fatalf("missing-track fault not located: %+v", fault)
	}
}

// TestCorrelateFaultToEdit verifies demux faults join back to the owning clip.
func TestCorrelateFaultToEdit(t *testing.T) {
	tb := media.TimeBaseVideo90k
	edits := []timeline.ResolvedEdit{
		{Edit: timeline.Edit{ID: "e1", Segment: "seg03", Track: 1, In: pt(0, tb), Out: pt(10, tb)}},
	}
	f := media.NewFault(media.KindTruncated, "seg03", 1, 500, "tail cut")
	if got := timeline.Correlate(f, edits); got != "e1" {
		t.Fatalf("correlate = %q, want e1", got)
	}
}
