package rtp

import "testing"

// TestContiguousPacketAtFullBudgetIsDelivered guards a regression: when the
// buffer is at its budget, the next expected sequence must still be admitted
// and delivered. The buggy path declared a gap for it and then dropped the
// packet while leaving the cursor one behind.
func TestContiguousPacketAtFullBudgetIsDelivered(t *testing.T) {
	cfg := Config{Window: 4, MaxRetries: 0, MaxGapRun: 8, BufferBudget: 2}
	r := NewReorderer("v", 0, cfg)
	if _, _, st := r.Push(Packet{Seq: 0}, nil); st != StatusDelivered {
		t.Fatalf("first packet status = %v", st)
	}
	r.Push(Packet{Seq: 2}, nil)
	r.Push(Packet{Seq: 3}, nil)
	out, _, st := r.Push(Packet{Seq: 1}, nil)
	if st != StatusDelivered {
		t.Fatalf("contiguous packet at full budget status = %v", st)
	}
	var seqs []uint64
	for _, p := range out {
		seqs = append(seqs, p.Seq)
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("delivered %v, want [1 2 3]", seqs)
	}
	if r.Stats.MissingSeqs != 0 {
		t.Fatalf("missing sequences = %d, want 0", r.Stats.MissingSeqs)
	}
	if r.Pending() != 0 {
		t.Fatalf("packet stranded in buffer: pending=%d", r.Pending())
	}
}
