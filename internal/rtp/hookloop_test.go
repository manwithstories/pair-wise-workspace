package rtp

import "testing"

// TestGapRecoveredWhenPacketArrivesBeforeWindowCloses covers the recovery
// path: a missing sequence that shows up while the gap is still open is
// recorded as recovered and never counted as a final miss.
func TestGapRecoveredWhenPacketArrivesBeforeWindowCloses(t *testing.T) {
	cfg := Config{Window: 8, MaxRetries: 0, MaxGapRun: 16, BufferBudget: 32}
	r := NewReorderer("v", 0, cfg)
	var out []Packet
	push := func(seq uint64) {
		o, _, _ := r.Push(Packet{Seq: seq}, nil)
		out = append(out, o...)
	}
	push(0)
	push(2) // gap at 1 opens but stays inside the window
	o, closed := r.Close(nil)
	_ = o
	// End of stream finalizes the still-open gap.
	if len(closed) != 1 || closed[0].First != 1 {
		t.Fatalf("closed = %+v", closed)
	}
	if r.Stats.FinalGaps != 1 {
		t.Fatalf("final gaps = %d", r.Stats.FinalGaps)
	}

	// Same shape, but 1 arrives in the stream: no final gap at all.
	r2 := NewReorderer("v", 0, cfg)
	r2.Push(Packet{Seq: 0}, nil)
	r2.Push(Packet{Seq: 2}, nil)
	r2.Push(Packet{Seq: 1}, nil)
	r2.Close(nil)
	if r2.Stats.FinalGaps != 0 {
		t.Fatalf("packet present but gap finalized: %+v", r2.Stats)
	}
	if r2.Stats.Recovered != 1 {
		t.Fatalf("recovered = %d, want 1", r2.Stats.Recovered)
	}
}
