package timeline

import "testing"

func mkObs(rep string, seq uint32, tfdt uint64, ts uint32, dur uint32, digest string) Observation {
	return Observation{
		ID: digest, Digest: digest, Path: rep + "/" + digest,
		Representation: rep, Sequence: seq, HasSequence: true,
		Timescale: ts, TFDT: tfdt,
		Samples: []SampleTiming{{Duration: dur, Sync: true}},
	}
}

func hasCode(r *Result, code string) bool {
	for _, is := range r.Issues {
		if is.Code == code {
			return true
		}
	}
	return false
}

func TestGapAndOutOfOrder(t *testing.T) {
	gap := Repair([]Observation{mkObs("r", 1, 0, 90000, 90000, "a"), mkObs("r", 3, 180000, 90000, 90000, "c")})
	gaps, _, _, _, _, _, _ := gap.Counters()
	if gaps != 1 || !hasCode(gap, IssueGap) {
		t.Fatalf("want one gap, issues=%+v", gap.Issues)
	}

	ooo := Repair([]Observation{
		mkObs("r", 1, 0, 90000, 90000, "a"),
		mkObs("r", 3, 180000, 90000, 90000, "c"),
		mkObs("r", 2, 90000, 90000, 90000, "b"),
	})
	_, oooN, _, _, _, _, _ := ooo.Counters()
	if oooN != 1 || !hasCode(ooo, IssueOutOfOrder) {
		t.Fatalf("want one out_of_order, issues=%+v", ooo.Issues)
	}
	// canonical order must be sequence 1,2,3 regardless of arrival
	seqs := []uint32{ooo.Fragments[0].Sequence, ooo.Fragments[1].Sequence, ooo.Fragments[2].Sequence}
	if seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("canonical order wrong: %v", seqs)
	}
}

func TestDigestDedup(t *testing.T) {
	a := mkObs("r", 1, 0, 90000, 90000, "same")
	a.Path = "z/path"
	b := mkObs("r", 1, 0, 90000, 90000, "same")
	b.Path = "a/path"
	r := Repair([]Observation{a, b})
	if len(r.Duplicates) != 1 {
		t.Fatalf("want 1 duplicate, got %d", len(r.Duplicates))
	}
	if r.Duplicates[0].Kept != "a/path" {
		t.Fatalf("smallest path must be keeper, got %q", r.Duplicates[0].Kept)
	}
	if len(r.Fragments) != 1 {
		t.Fatalf("duplicate must collapse to one fragment, got %d", len(r.Fragments))
	}
}

func TestTFDTRegressionMissingRTPWritesNewPeriod(t *testing.T) {
	r := Repair([]Observation{
		mkObs("r", 1, 90000, 90000, 90000, "a"),
		mkObs("r", 2, 1000, 90000, 90000, "b"),
	})
	if !hasCode(r, IssueTFDTRegression) || !hasCode(r, IssueNewPeriodMissingRTP) {
		t.Fatalf("want regression + missing-rtp period, issues=%+v", r.Issues)
	}
	var second *Fragment
	for _, f := range r.Fragments {
		if f.Sequence == 2 {
			second = f
		}
	}
	if second.Period != 1 || second.StartDTS != 1000 {
		t.Fatalf("new period must keep raw tfdt and not fake continuity: %+v", second)
	}
}

func TestTFDTRegressionContradictoryRTPWritesNewPeriod(t *testing.T) {
	a := mkObs("r", 1, 90000, 90000, 90000, "a")
	a.RTP = RTPEvidence{Present: true, Timestamp: 0, ClockRate: 90000}
	b := mkObs("r", 2, 1000, 90000, 90000, "b")
	b.RTP = RTPEvidence{Present: true, Timestamp: 1234, ClockRate: 90000}
	r := Repair([]Observation{a, b})
	if !hasCode(r, IssueNewPeriodRTPConflict) {
		t.Fatalf("want rtp conflict period, issues=%+v", r.Issues)
	}
}

func TestRTPCorroboratedContinuityRepairs(t *testing.T) {
	a := mkObs("r", 1, 90000, 90000, 90000, "a")
	a.RTP = RTPEvidence{Present: true, Timestamp: 0, ClockRate: 90000}
	b := mkObs("r", 2, 1000, 90000, 90000, "b") // tfdt wrong/backwards
	b.RTP = RTPEvidence{Present: true, Timestamp: 90000, ClockRate: 90000}
	r := Repair([]Observation{a, b})
	if !hasCode(r, IssueRTPContinuityRepair) {
		t.Fatalf("want rtp continuity repair, issues=%+v", r.Issues)
	}
	for _, f := range r.Fragments {
		if f.Sequence == 2 && (!f.RTPRepaired || f.StartDTS != 180000) {
			t.Fatalf("expected continuous start 180000, got %+v", f)
		}
	}
}

func TestRationalConversionAndOverflow(t *testing.T) {
	r := Repair([]Observation{mkObs("r", 1, 48000, 48000, 48000, "a")})
	if r.Fragments[0].StartPTS90k != 90000 {
		t.Fatalf("48k->90k conversion = %d, want 90000", r.Fragments[0].StartPTS90k)
	}
	bad := Repair([]Observation{mkObs("r", 1, ^uint64(0), 90000, 10, "z")})
	if _, _, _, _, _, _, ovf := bad.Counters(); ovf == 0 {
		t.Fatalf("want overflow issue for max uint64 tfdt")
	}
}
