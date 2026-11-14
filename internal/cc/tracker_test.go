package cc

import "testing"

func payload(b byte) []byte { return []byte{b, b, b} }

func TestTrackerLossAndJump(t *testing.T) {
	tr := New()
	tr.Observe(100, 0, false, true, payload(1), 0)
	tr.Observe(100, 1, false, true, payload(2), 188)
	// Gap of 2: one packet lost.
	tr.Observe(100, 3, false, true, payload(3), 376)
	if tr.LostEstimate != 1 || tr.LossEvents != 1 || tr.JumpEvents != 0 {
		t.Fatalf("loss: %+v", tr)
	}
	// Backward jump: event without loss.
	tr.Observe(100, 1, false, true, payload(4), 564)
	if tr.JumpEvents != 1 || tr.LostEstimate != 1 {
		t.Fatalf("jump: %+v", tr)
	}
	if len(tr.Events) != 2 || tr.Events[0].Offset != 376 || tr.Events[0].PID != 100 {
		t.Fatalf("events: %+v", tr.Events)
	}
}

func TestTrackerDuplicate(t *testing.T) {
	tr := New()
	tr.Observe(7, 0, false, true, payload(9), 0)
	tr.Observe(7, 0, false, true, payload(9), 188) // identical duplicate
	if tr.Duplicates != 1 || tr.LostEstimate != 0 || tr.JumpEvents != 0 {
		t.Fatalf("dup: %+v", tr)
	}
	// Same CC but different payload is an anomaly.
	tr.Observe(7, 0, false, true, payload(8), 376)
	if tr.JumpEvents != 1 {
		t.Fatalf("conflict: %+v", tr)
	}
}

func TestTrackerDiscontinuityReset(t *testing.T) {
	tr := New()
	tr.Observe(9, 5, false, true, payload(1), 0)
	tr.Observe(9, 12, true, true, payload(2), 188) // discontinuity_indicator resets
	tr.Observe(9, 13, false, true, payload(3), 376)
	if tr.JumpEvents != 0 || tr.LostEstimate != 0 {
		t.Fatalf("discontinuity: %+v", tr)
	}
}

func TestTrackerNoPayloadSkipped(t *testing.T) {
	tr := New()
	tr.Observe(9, 5, false, true, payload(1), 0)
	tr.Observe(9, 5, false, false, nil, 188) // adaptation-only: not checked
	tr.Observe(9, 6, false, true, payload(2), 376)
	if tr.JumpEvents != 0 {
		t.Fatalf("adaptation-only packet disturbed CC: %+v", tr)
	}
}
