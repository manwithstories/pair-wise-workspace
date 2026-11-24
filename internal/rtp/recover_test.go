package rtp

import (
	"math/rand"
	"testing"

	"github.com/pion/rtp"
)

func packet(seq uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 3000}, Payload: []byte{byte(seq)}}
}

func collect(t *testing.T, r *Recoverer, seqs []uint16) []uint16 {
	t.Helper()
	var got []uint16
	for _, s := range seqs {
		out, err := r.Push(packet(s))
		if err != nil {
			t.Fatalf("push %d: %v", s, err)
		}
		for _, p := range out {
			got = append(got, p.SequenceNumber)
		}
	}
	for _, p := range r.Flush() {
		got = append(got, p.SequenceNumber)
	}
	return got
}

func TestInOrderPassthrough(t *testing.T) {
	r := New(8)
	got := collect(t, r, []uint16{1, 2, 3, 4, 5})
	if len(got) != 5 {
		t.Fatalf("want 5 got %d", len(got))
	}
	st := r.Stats()
	if st.Lost != 0 || st.Duplicates != 0 || st.Recovered != 0 {
		t.Fatalf("unexpected stats %+v", st)
	}
	if st.RecoveryRate() != 1 {
		t.Fatalf("rate %f", st.RecoveryRate())
	}
}

func TestReorderInsideWindowRecovers(t *testing.T) {
	r := New(8)
	// 3 arrives late, inside the reorder window.
	got := collect(t, r, []uint16{1, 2, 4, 5, 3, 6})
	want := []uint16{1, 2, 3, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: want %v got %v", want, got)
		}
	}
	st := r.Stats()
	if st.Lost != 0 {
		t.Fatalf("reordered packet must not be lost: %+v", st)
	}
	if st.Recovered == 0 {
		t.Fatalf("reorder must be counted as recovery: %+v", st)
	}
	if st.RecoveryRate() < 0.95 {
		t.Fatalf("recovery rate too low: %f", st.RecoveryRate())
	}
}

func TestLossOutsideWindowCounted(t *testing.T) {
	r := New(4)
	// Skip 3 entirely; window of 4 forces 3 to be declared lost.
	got := collect(t, r, []uint16{1, 2, 4, 5, 6, 7, 8, 9})
	st := r.Stats()
	if st.Lost != 1 {
		t.Fatalf("want exactly one lost, stats %+v out=%v", st, got)
	}
	if st.Recovered != 0 {
		t.Fatalf("no reorder happened: %+v", st)
	}
	if st.LossRatio() <= 0 {
		t.Fatalf("loss ratio %f", st.LossRatio())
	}
	if st.Recovered != 0 {
		t.Fatalf("no reorder should be recovered: %+v", st)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("output not monotonic: %v", got)
		}
	}
}

func TestDuplicatesDoNotCountAsRecovery(t *testing.T) {
	r := New(8)
	got := collect(t, r, []uint16{1, 2, 2, 3, 3, 3, 4})
	if len(got) != 4 {
		t.Fatalf("duplicates must be dropped, got %v", got)
	}
	st := r.Stats()
	if st.Duplicates != 3 {
		t.Fatalf("want 3 duplicates got %+v", st)
	}
	if st.Recovered != 0 {
		t.Fatalf("duplicates must not count as recovery: %+v", st)
	}
	if st.Lost != 0 {
		t.Fatalf("no loss: %+v", st)
	}
}

func TestDuplicateAfterDelivery(t *testing.T) {
	r := New(8)
	got := collect(t, r, []uint16{1, 2, 3, 3})
	if len(got) != 3 {
		t.Fatalf("late duplicate delivered: %v", got)
	}
	if st := r.Stats(); st.Duplicates != 1 || st.Recovered != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestWrapAround(t *testing.T) {
	r := New(8)
	// Cross 65535 -> 0.
	got := collect(t, r, []uint16{65533, 65534, 65535, 0, 1, 2})
	if len(got) != 6 {
		t.Fatalf("wrap lost packets: %v", got)
	}
	want := []uint16{65533, 65534, 65535, 0, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wrap order: want %v got %v", want, got)
		}
	}
	st := r.Stats()
	if st.Lost != 0 {
		t.Fatalf("wrap must not lose: %+v", st)
	}
	if st.Wraps != 1 {
		t.Fatalf("want 1 wrap got %d", st.Wraps)
	}
}

func TestWrapWithReorder(t *testing.T) {
	r := New(8)
	// 0 arrives before 65534 and 65535.
	got := collect(t, r, []uint16{65533, 0, 65534, 65535, 1})
	want := []uint16{65533, 65534, 65535, 0, 1}
	if len(got) != len(want) {
		t.Fatalf("want %v got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wrap reorder: want %v got %v", want, got)
		}
	}
	if st := r.Stats(); st.Lost != 0 || st.Recovered == 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestBurstRecovery(t *testing.T) {
	r := New(64)
	// A burst of 40 packets whose first packet arrives on time, then the rest
	// is shuffled. Every packet is inside the 64-slot window.
	seqs := make([]uint16, 40)
	for i := range seqs {
		seqs[i] = uint16(200 + i)
	}
	rest := seqs[1:]
	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	seqs = append([]uint16{200}, rest...)
	got := collect(t, r, seqs)
	if len(got) != 40 {
		t.Fatalf("want 40 delivered got %d", len(got))
	}
	st := r.Stats()
	if st.RecoveryRate() < 0.95 {
		t.Fatalf("burst recovery rate %f", st.RecoveryRate())
	}
	if st.Lost != 0 {
		t.Fatalf("in-window shuffle must not lose: %+v", st)
	}
}

func TestLatePacketAfterLossIsNotRecovered(t *testing.T) {
	r := New(4)
	// Drain past sequence 3 before it arrives: 4,5,6,7 fill the window.
	if _, err := r.Push(packet(1)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []uint16{2, 4, 5, 6, 7, 8} {
		if _, err := r.Push(packet(s)); err != nil {
			t.Fatal(err)
		}
	}
	if st := r.Stats(); st.Lost != 1 {
		t.Fatalf("want 3 declared lost, stats %+v", st)
	}
	// Now 3 arrives, after its slot was passed: it must not be recovered.
	if out, _ := r.Push(packet(3)); len(out) != 0 {
		t.Fatalf("late packet must not be delivered: %v", out)
	}
	st := r.Stats()
	if st.Recovered != 0 {
		t.Fatalf("late packet counted as recovery: %+v", st)
	}
	if st.LateDropped+st.ReorderDropped != 1 {
		t.Fatalf("late packet not accounted: %+v", st)
	}
}

func TestFlushDeclaresRemainingLoss(t *testing.T) {
	r := New(8)
	if _, err := r.Push(packet(10)); err != nil {
		t.Fatal(err)
	}
	// 10 delivered immediately; 11 is missing and 12 is buffered.
	out, _ := r.Push(packet(12))
	if len(out) != 0 {
		t.Fatalf("12 should not be delivered yet: %v", out)
	}
	tail := r.Flush()
	if len(tail) != 1 || tail[0].SequenceNumber != 12 {
		t.Fatalf("flush should release 12: %+v", tail)
	}
	if st := r.Stats(); st.Lost != 1 {
		t.Fatalf("want 1 lost from flush got %+v", st)
	}
}

func TestNilPacketRejected(t *testing.T) {
	r := New(4)
	if _, err := r.Push(nil); err == nil {
		t.Fatal("want error for nil packet")
	}
}
