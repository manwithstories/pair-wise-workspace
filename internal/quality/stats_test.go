package quality

import (
	"testing"

	"mediascan/internal/fault"
	"mediascan/internal/rtp"
)

func goodOutcomes(n int) []FrameOutcome {
	out := make([]FrameOutcome, n)
	for i := range out {
		out[i] = FrameOutcome{Index: i, ChecksumOK: true, Reachable: true}
	}
	return out
}

func TestAllValid(t *testing.T) {
	s, err := Build(goodOutcomes(10), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ValidFrames != 10 || s.ValidPctX100 != 10000 || len(s.CorruptRanges) != 0 {
		t.Fatalf("stats wrong: %+v", s)
	}
}

func TestChecksumFaultHitsStatsAndRanges(t *testing.T) {
	out := goodOutcomes(4)
	out[1].ChecksumOK = false
	cf := []fault.Fault{{
		Cat: fault.CatBadChecksum, Ref: "f1",
		Span: fault.Interval{Start: 3000, End: 6000}, Msg: "bad hash",
	}}
	s, err := Build(out, cf, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.ChecksumFault != 1 {
		t.Fatalf("checksum faults = %d", s.ChecksumFault)
	}
	if s.ValidFrames != 3 {
		t.Fatalf("valid = %d, want 3", s.ValidFrames)
	}
	if s.ValidPctX100 != 7500 {
		t.Fatalf("valid pct = %d, want 7500", s.ValidPctX100)
	}
	if len(s.CorruptRanges) != 1 || s.CorruptRanges[0].Start != 3000 {
		t.Fatalf("corrupt ranges = %+v", s.CorruptRanges)
	}
}

func TestBurstLossHitsReachabilityAndRanges(t *testing.T) {
	out := goodOutcomes(6)
	out[2].Lost = true
	out[3].Lost = true
	rr := &rtp.Result{
		Bursts: []rtp.Burst{{
			FromSeq: 2, ToSeq: 3, Count: 2,
			StartTick: 6000, EndTick: 12000,
		}},
	}
	s, err := Build(out, nil, rr, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.LostFrames != 2 || s.ValidFrames != 4 {
		t.Fatalf("loss accounting wrong: %+v", s)
	}
	if len(s.CorruptRanges) != 1 || s.CorruptRanges[0].End != 12000 {
		t.Fatalf("burst not mapped to range: %+v", s.CorruptRanges)
	}
}

func TestMergedCorruptRanges(t *testing.T) {
	out := goodOutcomes(8)
	out[1].ChecksumOK = false
	out[2].ChecksumOK = false
	out[5].Lost = true
	cf := []fault.Fault{
		{Cat: fault.CatBadChecksum, Span: fault.Interval{Start: 3000, End: 6000}},
		{Cat: fault.CatBadChecksum, Span: fault.Interval{Start: 6000, End: 9000}},
	}
	rr := &rtp.Result{Bursts: []rtp.Burst{{
		FromSeq: 5, ToSeq: 5, Count: 1, StartTick: 15000, EndTick: 18000,
	}}}
	s, err := Build(out, cf, rr, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// adjacent checksum intervals merge into one; burst stays separate
	if len(s.CorruptRanges) != 2 {
		t.Fatalf("ranges = %+v, want 2 merged", s.CorruptRanges)
	}
	if s.ValidFrames != 5 {
		t.Fatalf("valid = %d, want 5", s.ValidFrames)
	}
}

func TestUnreachableFrames(t *testing.T) {
	out := goodOutcomes(3)
	out[0].Reachable = false // no preceding keyframe
	s, err := Build(out, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Unreachable != 1 || s.ValidFrames != 2 {
		t.Fatalf("unreachable accounting: %+v", s)
	}
}
