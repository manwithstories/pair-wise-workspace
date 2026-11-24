package rtp

import (
	"math/rand"
	"testing"
)

// TestReorderAroundBurstLoss mirrors the demo's --loss-burst setting: a run
// with light local reordering plus one contiguous 12-packet network loss.
// Every reordered packet must be recovered inside the window; the burst shows
// up as genuine loss, not as duplicate or spurious recovery.
func TestReorderAroundBurstLoss(t *testing.T) {
	const (
		total  = 480
		burst  = 12
		window = 64
	)
	r := New(window)
	rng := rand.New(rand.NewSource(42))
	seqs := make([]uint16, total)
	for i := range seqs {
		seqs[i] = uint16(65000 + i)
	}
	// Local swaps: each packet delayed by at most five positions.
	for i := 0; i+6 < total; i += 7 {
		j := i + 1 + rng.Intn(5)
		seqs[i], seqs[j] = seqs[j], seqs[i]
	}
	burstStart := 200
	kept := append(append([]uint16{}, seqs[:burstStart]...), seqs[burstStart+burst:]...)

	delivered := 0
	for _, s := range kept {
		out, err := r.Push(packet(s))
		if err != nil {
			t.Fatal(err)
		}
		delivered += len(out)
	}
	delivered += len(r.Flush())
	st := r.Stats()
	if st.Duplicates != 0 {
		t.Fatalf("duplicates fabricated: %+v", st)
	}
	if st.Recovered == 0 {
		t.Fatalf("local reorder not recovered: %+v", st)
	}
	if st.Recovered+st.ReorderDropped == 0 || st.RecoveryRate() < 0.95 {
		t.Fatalf("recovery rate too low: %f (%+v)", st.RecoveryRate(), st)
	}
	if st.Lost < burst-1 || st.Lost > burst+4 {
		t.Fatalf("burst loss not reflected: want ~%d got %+v", burst, st)
	}
	if delivered < total-burst-4 {
		t.Fatalf("too few packets delivered: %d of %d", delivered, len(kept))
	}
}
