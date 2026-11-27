package timeline

import (
	"testing"

	"relayaudit/internal/clock"
	"relayaudit/internal/fault"
	"relayaudit/internal/fixture"
	"relayaudit/internal/wire"
)

func fixtureRecords(t *testing.T, spec fixture.Spec) *fixture.Segment {
	t.Helper()
	seg, err := fixture.Generate(spec)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return seg
}

func TestBuildTracksNormalisesPerTrackOrigin(t *testing.T) {
	seg := fixtureRecords(t, fixture.Spec{Seed: 3, Size: 1 << 18})
	tracks, err := BuildTracks(seg.Records)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(tracks) != 4 {
		t.Fatalf("tracks = %d, want 4", len(tracks))
	}
	for id, tr := range tracks {
		if tr.Entries[0].Start != 0 {
			t.Fatalf("track %d first tick = %d, want 0", id, tr.Entries[0].Start)
		}
		for i := 1; i < len(tr.Entries); i++ {
			if tr.Entries[i].Start < tr.Entries[i-1].Start {
				t.Fatalf("track %d entries not monotonic", id)
			}
		}
		if tr.Last != tr.Entries[len(tr.Entries)-1].End {
			t.Fatalf("track %d Last disagrees with final entry", id)
		}
	}
	// Video cadence is 40 ms; both video tracks must agree on that spacing.
	if d := tracks[1].Entries[1].Start - tracks[1].Entries[0].Start; d != 3600 {
		t.Fatalf("video spacing = %d, want 3600", d)
	}
	if d := tracks[2].Entries[1].Start - tracks[2].Entries[0].Start; d != 3600 {
		t.Fatalf("backup video spacing = %d, want 3600", d)
	}
	if d := tracks[3].Entries[1].Start - tracks[3].Entries[0].Start; d != 1920 {
		t.Fatalf("audio spacing = %d, want 1920", d)
	}
}

func TestBuildTracksSurvivesPTSWrap(t *testing.T) {
	seg := fixtureRecords(t, fixture.Spec{Seed: 11, Size: 1 << 21})
	first := seg.Records[0].PTS
	last := seg.Records[len(seg.Records)-1].PTS
	if last > first {
		t.Skip("fixture did not wrap at this size")
	}
	tracks, err := BuildTracks(seg.Records)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The timeline must cover the whole segment duration, not stop at the
	// wrap boundary.
	if got, want := tracks[1].Last, seg.DurationTicks; got+3600 < want || got > want+clock.Rate {
		t.Fatalf("track 1 spans %d ticks, segment covers %d", got, want)
	}
	for _, tr := range tracks {
		for i := 1; i < len(tr.Entries); i++ {
			if tr.Entries[i].Start < tr.Entries[i-1].Start {
				t.Fatalf("track %d regressed at entry %d", tr.ID, i)
			}
		}
	}
}

func TestSamplePlanResolvesWithoutBoundaryFaults(t *testing.T) {
	seg := fixtureRecords(t, fixture.Spec{Seed: 73, Size: 1 << 21})
	plan := SamplePlan(seg.Records)
	if len(plan.Clips) != 6 || len(plan.Transitions) != 2 {
		t.Fatalf("plan shape = %d clips %d transitions", len(plan.Clips), len(plan.Transitions))
	}
	res, faults, err := Resolve(seg.Records, plan)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(faults) != 0 {
		for _, f := range faults {
			t.Logf("unexpected fault: %v", f)
		}
		t.Fatalf("aligned plan produced %d boundary faults", len(faults))
	}
	for _, c := range res.Clips {
		if c.StartAdjusted || c.EndAdjusted {
			t.Fatalf("clip %q was adjusted unexpectedly", c.Label)
		}
		if c.Duration() == 0 {
			t.Fatalf("clip %q has zero duration", c.Label)
		}
	}
	for _, tr := range res.Transitions {
		if tr.Kind != "cross" || tr.Duration() == 0 {
			t.Fatalf("transition %q wrong: %+v", tr.Label, tr)
		}
	}
}

func TestMisalignedBoundaryReportsRecordOffset(t *testing.T) {
	seg := fixtureRecords(t, fixture.Spec{Seed: 8, Size: 1 << 18})
	tracks, err := BuildTracks(seg.Records)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Pick a tick that sits strictly inside the 3rd record on track 1.
	e := tracks[1].Entries[2]
	mid := e.Start + (e.End-e.Start)/2
	plan := Plan{Clips: []Clip{{Track: 1, Label: "cut", Start: mid, End: tracks[1].Last}}}
	res, faults, err := Resolve(seg.Records, plan)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(faults) != 1 {
		t.Fatalf("faults = %d, want 1", len(faults))
	}
	f := faults[0]
	if f.Kind() != fault.Boundary {
		t.Fatalf("kind = %v, want boundary", f.Kind())
	}
	if f.Offset() != e.Rec.Offset {
		t.Fatalf("offset = %d, want record offset %d", f.Offset(), e.Rec.Offset)
	}
	if !res.Clips[0].StartAdjusted {
		t.Fatalf("clip start was not widened")
	}
	if res.Clips[0].Start != e.Start {
		t.Fatalf("clip start = %d, want record start %d", res.Clips[0].Start, e.Start)
	}
}

func TestMisalignedTransitionEndReportsOffset(t *testing.T) {
	seg := fixtureRecords(t, fixture.Spec{Seed: 8, Size: 1 << 18})
	tracks, _ := BuildTracks(seg.Records)
	e := tracks[1].Entries[5]
	mid := e.Start + 1
	plan := Plan{Transitions: []Transition{{
		From: 1, To: 2, Kind: "cut", Label: "t",
		Start: tracks[1].Entries[3].Start, End: mid,
	}}}
	res, faults, err := Resolve(seg.Records, plan)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(faults) != 1 || faults[0].Kind() != fault.Boundary {
		t.Fatalf("faults = %v", faults)
	}
	if faults[0].Offset() != e.Rec.Offset {
		t.Fatalf("offset = %d, want %d", faults[0].Offset(), e.Rec.Offset)
	}
	if res.Transitions[0].End != e.End {
		t.Fatalf("transition end = %d, want %d", res.Transitions[0].End, e.End)
	}
}

func TestResolveRejectsUnknownTrack(t *testing.T) {
	seg := fixtureRecords(t, fixture.Spec{Seed: 8, Size: 1 << 17})
	_, _, err := Resolve(seg.Records, Plan{Clips: []Clip{{Track: 9, Label: "x"}}})
	if err == nil {
		t.Fatalf("unknown track accepted")
	}
}

func TestFaultRecordOffsetsSorted(t *testing.T) {
	fs := []*fault.Fault{
		fault.New(fault.Boundary, 900, "b"),
		fault.New(fault.Boundary, 100, "a"),
		fault.New(fault.Boundary, 500, "c"),
	}
	got := FaultRecordOffsets(fs)
	want := []int64{100, 500, 900}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("offsets = %v, want %v", got, want)
		}
	}
}

func TestLabelsForRecord(t *testing.T) {
	seg := fixtureRecords(t, fixture.Spec{Seed: 2, Size: 1 << 18})
	plan := SamplePlan(seg.Records)
	res, _, err := Resolve(seg.Records, plan)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var labelled int
	for _, rec := range seg.Records {
		if len(res.LabelsFor(rec)) > 0 {
			labelled++
		}
	}
	if labelled == 0 {
		t.Fatalf("no records carry clip labels")
	}
}

func TestEntryAtFindsContainingRecord(t *testing.T) {
	seg := fixtureRecords(t, fixture.Spec{Seed: 4, Size: 1 << 18})
	res, _, err := Resolve(seg.Records, SamplePlan(seg.Records))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	e, ok := res.EntryAt(1, 0)
	if !ok || e.Start != 0 {
		t.Fatalf("EntryAt(1,0) = %+v %v", e, ok)
	}
	if _, ok := res.EntryAt(1, res.Tracks[1].Last+1000); ok {
		t.Fatalf("EntryAt past end succeeded")
	}
}

func TestMinHelperBehaviourViaPlan(t *testing.T) {
	// A tiny segment still yields a usable plan or a clean empty one.
	seg := fixtureRecords(t, fixture.Spec{Seed: 6, Size: 1 << 16})
	plan := SamplePlan(seg.Records)
	if len(plan.Clips) != 0 && len(plan.Clips) != 6 {
		t.Fatalf("unexpected plan size %d", len(plan.Clips))
	}
}

var _ = wire.HeaderSize
