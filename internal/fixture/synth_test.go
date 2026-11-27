package fixture

import (
	"testing"

	"relayctl/internal/cmaf"
)

func TestGenerateGoodDeterministic(t *testing.T) {
	s1, err := Generate(CaseDriftWrapVFR, 41)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := Generate(CaseDriftWrapVFR, 41)
	if string(s1.CMAF) != string(s2.CMAF) {
		t.Fatal("CMAF bytes must be identical for the same seed")
	}
	if len(s1.Packets) != len(s2.Packets) {
		t.Fatal("packet count must be stable")
	}
	// The generated fMP4 must parse cleanly through the production parser.
	frags, gaps, err := cmaf.ParseFragmentStream(s1.CMAF)
	if err != nil {
		t.Fatalf("good fixture must parse: %v", err)
	}
	if len(frags) != 2 || len(gaps) != 0 {
		t.Fatalf("frags=%d gaps=%v, want 2 fragments no gaps", len(frags), gaps)
	}
}

func TestGoodHasReorderDuplicateAndSingleLoss(t *testing.T) {
	s, _ := Generate(CaseDriftWrapVFR, 41)
	plan := s.Manifest.SequencePlan
	if len(plan.SingleLosses) != 1 {
		t.Fatalf("want exactly one single loss, got %v", plan.SingleLosses)
	}
	if len(plan.Duplicates) != 1 {
		t.Fatalf("want one duplicate packet, got %v", plan.Duplicates)
	}
	// Order must not be strictly ascending (reordering happened).
	ascending := true
	for i := 1; i < len(plan.Order); i++ {
		if plan.Order[i] < plan.Order[i-1] {
			ascending = false
			break
		}
	}
	if ascending {
		t.Fatal("expected a non-ascending delivery order")
	}
	// Every FEC group covers groupSize contiguous media sequences.
	for _, g := range plan.FECGroups {
		if len(g) == 0 {
			t.Fatal("empty FEC group")
		}
	}
}

func TestGoodTfdtNearWrap(t *testing.T) {
	s, _ := Generate(CaseDriftWrapVFR, 41)
	const mod33 = uint64(1) << 33
	var sawNear bool
	for _, tr := range s.Manifest.Tracks {
		if tr.Kind == Video && tr.BaseTfdt > mod33-uint64(tr.Timescale)*3 {
			sawNear = true
		}
	}
	if !sawNear {
		t.Fatal("video base tfdt must sit near the 33-bit modulus")
	}
}

func TestGenerateDamagedTruncatesMdat(t *testing.T) {
	s, err := Generate(CaseTruncateMdat, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Manifest.ExpectUnrecoverable || len(s.Manifest.SequencePlan.BurstLosses) < 2 {
		t.Fatal("damaged case needs a multi-packet burst loss")
	}
	want := s.Manifest.ExpectedDamageOffset
	if len(s.DamagedCMAF) >= len(s.CMAF) {
		t.Fatal("damaged stream must be shorter than the original")
	}
	// The production parser must locate damage at exactly the recorded mdat
	// box start, giving a delta of zero.
	_, _, err = cmaf.ParseFragmentStream(s.DamagedCMAF)
	off, ok := cmaf.DamageOffset(err)
	if !ok {
		t.Fatalf("damaged fixture must yield a BoxError, got %v", err)
	}
	if off != want {
		t.Fatalf("damage offset %d != expected %d (delta %d)", off, want, off-want)
	}
}

func TestGenerateUnknownCase(t *testing.T) {
	if _, err := Generate("nope", 1); err == nil {
		t.Fatal("unknown case must error")
	}
}

func TestTruncateMdatHelper(t *testing.T) {
	stream := []byte("0123456789")
	if got := TruncateMdat(stream, 4); string(got) != "0123" {
		t.Fatalf("truncate = %q", got)
	}
}
