package timeline

import (
	"testing"

	"relay/internal/fixture"
	"relay/internal/ingest"
	"relay/internal/media"
)

// TestRollbackKeepsDTSMonotonicAndCTO builds a track whose tfdt jumps
// backwards and asserts the repair advances DTS losslessly while every
// per-sample composition time offset is preserved byte-for-byte.
func TestRollbackKeepsDTSMonotonicAndCTO(t *testing.T) {
	clock := media.Clock{TrackID: 101, Timescale: 90000}
	specs := []fixture.FragmentSpec{
		{TrackID: 101, Sequence: 1, Tfdt: 0, Samples: []fixture.SampleSpec{
			{Duration: 3000, Payload: fixture.SamplePayload(101, 1, 1, 32), Sync: true},
			{Duration: 3000, Payload: fixture.SamplePayload(101, 1, 2, 32), CompositionOffset: 500},
		}},
		{TrackID: 101, Sequence: 2, Tfdt: 6000, Samples: []fixture.SampleSpec{
			{Duration: 3000, Payload: fixture.SamplePayload(101, 2, 1, 32), Sync: true},
			{Duration: 3000, Payload: fixture.SamplePayload(101, 2, 2, 32), CompositionOffset: 500},
		}},
		// tfdt 1000 is a rollback behind seq2's end (12000).
		{TrackID: 101, Sequence: 3, Tfdt: 1000, Samples: []fixture.SampleSpec{
			{Duration: 3000, Payload: fixture.SamplePayload(101, 3, 1, 32), Sync: true},
			{Duration: 3000, Payload: fixture.SamplePayload(101, 3, 2, 32), CompositionOffset: 500},
		}},
	}
	var frags []*ingest.Fragment
	for _, s := range specs {
		f, err := ingest.ParseFragment("mem", 0, fixture.BuildFragment(s))
		if err != nil {
			t.Fatal(err)
		}
		frags = append(frags, f)
	}

	rep := RepairTrack(101, clock, frags)
	if rep.Fault != nil {
		t.Fatalf("rollback must be repairable: %v", rep.Fault)
	}
	if len(rep.Accepted) != 3 {
		t.Fatalf("expected 3 repaired fragments, got %d", len(rep.Accepted))
	}

	// Repaired tfdt of seq3 must equal seq2's end (12000), and global DTS
	// must be strictly non-decreasing.
	if rep.Accepted[2].RepairedTfdt != 12000 {
		t.Fatalf("rollback not advanced to previous end: got %d want 12000",
			rep.Accepted[2].RepairedTfdt)
	}
	for i := 1; i < len(rep.Accepted); i++ {
		if rep.Accepted[i].GlobalDTS < rep.Accepted[i-1].GlobalDTS {
			t.Fatalf("global DTS regressed at %d", i)
		}
	}

	// Composition offsets survive untouched on every repaired fragment.
	for _, rf := range rep.Accepted {
		if len(rf.Samples) != 2 {
			t.Fatal("sample count changed during repair")
		}
		if rf.Samples[1].CompositionOffset != 500 {
			t.Fatalf("composition offset altered: %d", rf.Samples[1].CompositionOffset)
		}
	}
}

// TestOverflowIsolatesTrack forces a clock conversion overflow and asserts
// the whole track is quarantined with no accepted fragments.
func TestOverflowIsolatesTrack(t *testing.T) {
	huge := fixture.FragmentSpec{
		TrackID: 101, Sequence: 1, Tfdt: media.MaxTime - 10,
		Samples: []fixture.SampleSpec{
			{Duration: 3000, Payload: fixture.SamplePayload(101, 1, 1, 16), Sync: true},
		},
	}
	f, err := ingest.ParseFragment("mem", 0, fixture.BuildFragment(huge))
	if err != nil {
		t.Fatal(err)
	}
	rep := RepairTrack(101, media.Clock{TrackID: 101, Timescale: 1}, []*ingest.Fragment{f})
	if rep.Fault == nil {
		t.Fatal("expected an isolation fault for overflowing conversion")
	}
	if !rep.Fault.Isolation() {
		t.Fatal("overflow fault must isolate the track")
	}
	if len(rep.Accepted) != 0 {
		t.Fatal("an overflowing track must not publish fragments")
	}
}
