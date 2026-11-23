package inspect

import (
	"testing"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
)

func newTestTimeline() *clock.Timeline {
	tl := clock.NewTimeline()
	tl.Add(clock.TrackClock{TrackID: 1, Timescale: 90000})
	return tl
}

func TestDamageLogMfhdGapAndRegression(t *testing.T) {
	tl := newTestTimeline()
	model := clock.NewErrorModel(40, 20)
	log := NewDamageLog(1, 33_333, model)
	fs := NewFragmentState(1, tl)

	mk := func(seq uint32, base uint64, samples int) cmaf.Fragment {
		fr := cmaf.Fragment{SequenceNumber: seq, HasMfhd: true, HasTfdt: true, TfdtTicks: map[uint32]uint64{1: base}, MoofOffset: uint64(seq) * 100}
		for i := 0; i < samples; i++ {
			fr.Samples = append(fr.Samples, cmaf.Sample{
				TrackID: 1, Index: i, DurationTicks: 3000, DecodeTimeTicks: base + uint64(i*3000), TimeKnown: true,
			})
		}
		return fr
	}
	fs.ObserveFragment(mk(1, 0, 3), 1, log)
	fs.ObserveFragment(mk(4, 9000, 3), 2, log)  // gap 2 -> 3
	fs.ObserveFragment(mk(4, 18000, 3), 3, log) // duplicate 4
	fs.ObserveFragment(mk(5, 0, 3), 4, log)     // tfdt regression

	gaps, dups, regs := fs.Counters()
	if gaps != 1 || dups != 1 || regs != 1 {
		t.Fatalf("counters gaps=%d dups=%d regs=%d", gaps, dups, regs)
	}
	counts := model.Counts()
	if counts["mfhd_gap"] == 0 || counts["mfhd_duplicate"] == 0 || counts["tfdt_regression"] == 0 {
		t.Fatalf("model counts = %v", counts)
	}
	cov := log.Coverage()
	if len(cov) == 0 {
		t.Fatal("expected mappable damage coverage")
	}
	for _, r := range log.Ranges() {
		if !r.PositionKnown {
			t.Fatalf("unexpected unknown position: %+v", r)
		}
		if r.Kind == clock.ErrTfdtRegression && r.StartUs != 0 {
			t.Fatalf("regression range should start at regressed tfdt 0: %+v", r)
		}
	}
}

func TestDamageLogUnknownPositionKept(t *testing.T) {
	log := NewDamageLog(1, 40_000, nil)
	log.Add(0, 0, clock.ErrSegmentTruncated, 3, 1234, false, "no tfdt")
	ranges := log.Ranges()
	if len(ranges) != 1 || ranges[0].PositionKnown || ranges[0].Offset != 1234 {
		t.Fatalf("ranges = %+v", ranges)
	}
	if cov := log.Coverage(); len(cov) != 0 {
		t.Fatalf("unknown range must not enter coverage: %+v", cov)
	}
}

func TestDamageLogTruncatedSamples(t *testing.T) {
	tl := newTestTimeline()
	log := NewDamageLog(1, 33_333, nil)
	fs := NewFragmentState(1, tl)
	fr := cmaf.Fragment{
		SequenceNumber: 1, HasMfhd: true, TfdtTicks: map[uint32]uint64{1: 0},
		Samples: []cmaf.Sample{
			{TrackID: 1, Index: 0, DurationTicks: 3000, TimeKnown: true},
			{TrackID: 1, Index: 1, DurationTicks: 3000, Truncated: true, TimeKnown: true, Offset: 999},
		},
	}
	fs.ObserveFragment(fr, 1, log)
	found := false
	for _, r := range log.Ranges() {
		if r.Kind == clock.ErrSampleOutOfBounds {
			found = true
			if r.Offset != 999 {
				t.Fatalf("offset = %d", r.Offset)
			}
			if r.DurationMS() != 33.333 {
				t.Fatalf("duration = %v", r.DurationMS())
			}
		}
	}
	if !found {
		t.Fatalf("no out-of-bounds range: %+v", log.Ranges())
	}
}
