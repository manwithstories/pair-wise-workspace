package rtp

import "testing"

func collect(t *testing.T, packets []Packet, cfg Config, hook RetryHook) ([]uint64, []Gap, *Reorderer) {
	t.Helper()
	r := NewReorderer("v", 0, cfg)
	var ordered []uint64
	var fired []Gap
	for _, p := range packets {
		out, closed, _ := r.Push(p, hook)
		for _, o := range out {
			ordered = append(ordered, o.Seq)
		}
		fired = append(fired, closed...)
	}
	out, closed := r.Close(hook)
	for _, o := range out {
		ordered = append(ordered, o.Seq)
	}
	fired = append(fired, closed...)
	return ordered, fired, r
}

func TestReorderWithinWindow(t *testing.T) {
	in := []Packet{{Seq: 0}, {Seq: 2}, {Seq: 1}, {Seq: 3}}
	ordered, gaps, r := collect(t, in, DefaultConfig(), nil)
	want := []uint64{0, 1, 2, 3}
	if len(ordered) != len(want) {
		t.Fatalf("ordered = %v", ordered)
	}
	for i := range want {
		if ordered[i] != want[i] {
			t.Fatalf("ordered = %v, want %v", ordered, want)
		}
	}
	for _, g := range gaps {
		if !g.Recovered {
			t.Fatalf("gap %v should be recovered", g)
		}
	}
	if r.Stats.Delivered != 4 || r.Stats.MissingSeqs != 0 {
		t.Fatalf("stats = %+v", r.Stats)
	}
}

func TestDuplicatePacketsTolerated(t *testing.T) {
	in := []Packet{{Seq: 0}, {Seq: 1}, {Seq: 0}, {Seq: 1}, {Seq: 2}, {Seq: 2}}
	ordered, _, r := collect(t, in, DefaultConfig(), nil)
	if len(ordered) != 3 {
		t.Fatalf("ordered = %v", ordered)
	}
	if r.Stats.Duplicates != 3 {
		t.Fatalf("duplicates = %d", r.Stats.Duplicates)
	}
}

func TestGapInsideWindowRetriedAndRecovered(t *testing.T) {
	calls := 0
	hook := func(g Gap) (Packet, bool) {
		calls++
		if g.First == 1 {
			return Packet{Seq: 1}, true
		}
		return Packet{}, false
	}
	// 1 is skipped on the wire and only arrives via the retransmit hook.
	ordered, gaps, r := collect(t, []Packet{{Seq: 0}, {Seq: 2}, {Seq: 3}}, Config{
		Window: 8, MaxRetries: 2, MaxGapRun: 16, BufferBudget: 64,
	}, hook)
	_ = calls
	want := []uint64{0, 1, 2, 3}
	if len(ordered) != len(want) {
		t.Fatalf("ordered = %v, want %v", ordered, want)
	}
	for i := range want {
		if ordered[i] != want[i] {
			t.Fatalf("ordered = %v, want %v", ordered, want)
		}
	}
	if !r.FinalGaps()[0].Recovered {
		t.Fatalf("recorded gap not recovered: %+v", r.FinalGaps())
	}
	var recovered bool
	for _, g := range gaps {
		if g.Recovered && g.First == 1 {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("gap 1 not marked recovered: %v", gaps)
	}
	if r.Stats.MissingSeqs != 0 {
		t.Fatalf("missing = %d", r.Stats.MissingSeqs)
	}
}

func TestGapOutsideWindowStaysFinal(t *testing.T) {
	// Sequence 1 never arrives and the stream runs well past the window.
	var in []Packet
	in = append(in, Packet{Seq: 0})
	for s := uint64(2); s <= 200; s++ {
		in = append(in, Packet{Seq: s})
	}
	ordered, gaps, r := collect(t, in, DefaultConfig(), func(g Gap) (Packet, bool) {
		return Packet{}, false
	})
	if len(gaps) != 1 {
		t.Fatalf("gaps = %v", gaps)
	}
	if gaps[0].First != 1 || gaps[0].Count != 1 || gaps[0].Recovered {
		t.Fatalf("unexpected gap %+v", gaps[0])
	}
	if r.Stats.FinalGaps != 1 || r.Stats.MissingSeqs != 1 {
		t.Fatalf("stats = %+v", r.Stats)
	}
	if ordered[0] != 0 || ordered[1] != 2 {
		t.Fatalf("ordered head = %v", ordered[:3])
	}
	// Everything after the gap must still be delivered in order.
	for i := 2; i < len(ordered); i++ {
		if ordered[i] != ordered[i-1]+1 {
			t.Fatalf("order broken at %d: %v", i, ordered[i-2:i+2])
		}
	}
}

func TestLatePacketAfterFinalizedGap(t *testing.T) {
	in := []Packet{{Seq: 0}}
	for s := uint64(2); s <= 200; s++ {
		in = append(in, Packet{Seq: s})
	}
	in = append(in, Packet{Seq: 1}) // arrives far too late
	_, _, r := collect(t, in, DefaultConfig(), nil)
	if r.Stats.Late != 1 {
		t.Fatalf("late = %d, want 1", r.Stats.Late)
	}
	if r.Stats.Delivered != 200 {
		t.Fatalf("delivered = %d", r.Stats.Delivered)
	}
}

func TestBufferBudgetForcesGap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BufferBudget = 4
	r := NewReorderer("v", 0, cfg)
	r.Push(Packet{Seq: 0}, nil)
	for s := uint64(2); s <= 64; s++ {
		r.Push(Packet{Seq: s}, nil)
	}
	r.Close(nil)
	if r.Stats.Overflows == 0 {
		t.Fatalf("expected budget overflow")
	}
	if r.head() != 65 {
		t.Fatalf("cursor = %d", r.head())
	}
}

func (r *Reorderer) head() uint64 { return r.next }

func TestCloseFinalizesOpenGap(t *testing.T) {
	r := NewReorderer("a", 0, DefaultConfig())
	r.Push(Packet{Seq: 0}, nil)
	r.Push(Packet{Seq: 2}, nil)
	out, gaps := r.Close(nil)
	var seqs []uint64
	for _, p := range out {
		seqs = append(seqs, p.Seq)
	}
	if len(gaps) != 1 || gaps[0].First != 1 || gaps[0].Count != 1 {
		t.Fatalf("gaps = %v", gaps)
	}
	if len(seqs) != 1 || seqs[0] != 2 {
		t.Fatalf("out = %v", seqs)
	}
}

func TestEndOfStreamRetryRecoversLastGap(t *testing.T) {
	r := NewReorderer("v", 0, Config{Window: 8, MaxRetries: 1, MaxGapRun: 16, BufferBudget: 64})
	r.Push(Packet{Seq: 0}, nil)
	r.Push(Packet{Seq: 2}, nil) // holds 2 back, gap at 1 opens
	out, gaps := r.Close(func(g Gap) (Packet, bool) {
		if g.First == 1 {
			return Packet{Seq: 1}, true
		}
		return Packet{}, false
	})
	var seqs []uint64
	for _, p := range out {
		seqs = append(seqs, p.Seq)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("out = %v", seqs)
	}
	if len(gaps) != 1 || !gaps[0].Recovered {
		t.Fatalf("gaps = %v", gaps)
	}
}
