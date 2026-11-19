package verify

import (
	"testing"
)

// TestRoundTripAcrossWrap exercises RTP sequence numbers across the
// 16-bit wrap boundary.
func TestRoundTripAcrossWrap(t *testing.T) {
	if err := RoundTrip(65500, 200, 0x00A14C00); err != nil {
		t.Fatalf("round trip across wrap: %v", err)
	}
}

// TestConsecutiveLossRuns: loss runs within the retransmit window are
// recovered; longer runs surface as unrecovered ranges with wrap-aware
// sequence numbers.
func TestConsecutiveLossRuns(t *testing.T) {
	// Deterministic total loss: everything lost in one long run.
	rep := SimulateLoss(50, 65500, 1.0, 1, 4)
	if rep.Lost != 50 || rep.Recovered != 0 {
		t.Fatalf("total loss: lost=%d recovered=%d", rep.Lost, rep.Recovered)
	}
	if len(rep.UnrecoveredRanges) != 1 {
		t.Fatalf("want 1 unrecovered range, got %v", rep.UnrecoveredRanges)
	}
	r := rep.UnrecoveredRanges[0]
	if r[0] != 65500 || r[1] != 65500+49 {
		t.Fatalf("range across wrap: %v", r)
	}

	// No loss: nothing to recover, rate stays zero.
	rep = SimulateLoss(50, 0, 0.0, 1, 4)
	if rep.Lost != 0 || rep.Recovered != 0 || len(rep.UnrecoveredRanges) != 0 {
		t.Fatalf("no-loss report: %+v", rep)
	}

	// Moderate loss with a wide window recovers everything.
	rep = SimulateLoss(1000, 60000, 0.05, 7, 1000)
	if rep.Lost == 0 {
		t.Fatal("expected some loss at 5%")
	}
	if rep.Recovered != rep.Lost || rep.RecoveryRate != 100.0 {
		t.Fatalf("wide window: lost=%d recovered=%d rate=%.1f",
			rep.Lost, rep.Recovered, rep.RecoveryRate)
	}
}
