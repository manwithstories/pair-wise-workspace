package rtp

import "testing"

// TestAdvanceFinalizesGapWhenWindowCloses exercises the in-stream path
// directly: once the observed high-water mark moves beyond the wait window,
// a gap must finalize and later packets must still be delivered.
func TestAdvanceFinalizesGapWhenWindowCloses(t *testing.T) {
	cfg := Config{Window: 2, MaxRetries: 0, MaxGapRun: 8, BufferBudget: 16}
	r := NewReorderer("v", 0, cfg)
	var out []Packet
	push := func(seq uint64) {
		o, _, _ := r.Push(Packet{Seq: seq}, nil)
		out = append(out, o...)
	}
	push(0)
	push(2) // gap at 1 opens
	push(3)
	push(4) // high - next = 3 >= window, so the gap finalizes
	push(5)
	o, closed := r.Close(nil)
	out = append(out, o...)
	if r.Stats.FinalGaps != 1 {
		t.Fatalf("final gaps = %d, want 1: %+v", r.Stats.FinalGaps, closed)
	}
	if r.Stats.MissingSeqs != 1 {
		t.Fatalf("missing = %d, want 1", r.Stats.MissingSeqs)
	}
	// Everything after the gap still arrives in order.
	var seqs []uint64
	for _, p := range out {
		seqs = append(seqs, p.Seq)
	}
	want := []uint64{0, 2, 3, 4, 5}
	if len(seqs) != len(want) {
		t.Fatalf("delivered %v, want %v", seqs, want)
	}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("delivered %v, want %v", seqs, want)
		}
	}
}
