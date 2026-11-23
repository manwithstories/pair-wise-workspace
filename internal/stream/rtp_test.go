package stream

import "testing"

func feed(t *testing.T, r *Reorderer, seqs ...uint16) (delivered []uint16) {
	t.Helper()
	for _, s := range seqs {
		b, err := EncodePacket(s, uint32(s)*3000, []byte{byte(s), 1, 2, 3})
		if err != nil {
			t.Fatal(err)
		}
		p, err := DecodePacket(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range r.Push(p) {
			delivered = append(delivered, q.SequenceNumber)
		}
	}
	for _, q := range r.Flush() {
		delivered = append(delivered, q.SequenceNumber)
	}
	return delivered
}

func eq(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func increasing(a []uint16) bool {
	for i := 1; i < len(a); i++ {
		if a[i] <= a[i-1] {
			return false
		}
	}
	return true
}

func TestInOrder(t *testing.T) {
	r := NewReorderer(8)
	if got := feed(t, r, 0, 1, 2, 3); !eq(got, []uint16{0, 1, 2, 3}) {
		t.Fatalf("got %v", got)
	}
	if r.Stats.Recovered != 0 || r.Stats.Unrecovered != 0 {
		t.Fatalf("stats %+v", r.Stats)
	}
}

func TestReorderWithinWindowRecovered(t *testing.T) {
	r := NewReorderer(8)
	if got := feed(t, r, 0, 2, 3, 1, 4); !eq(got, []uint16{0, 1, 2, 3, 4}) {
		t.Fatalf("got %v", got)
	}
	if r.Stats.Recovered != 1 { // 1 fills the hole behind 2,3
		t.Fatalf("recovered=%d want 1", r.Stats.Recovered)
	}
}

func TestDuplicateDropped(t *testing.T) {
	r := NewReorderer(8)
	if got := feed(t, r, 0, 1, 1, 2); !eq(got, []uint16{0, 1, 2}) {
		t.Fatalf("got %v", got)
	}
	if r.Stats.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", r.Stats.Duplicates)
	}
}

func TestLossBurstPartialRecovery(t *testing.T) {
	// Window 8: burst loss of 100..104 (5 packets). Retransmissions of
	// 100..103 arrive inside the window; 104 arrives after the window has
	// slid past -> late, and its gap is counted unrecovered.
	r := NewReorderer(8)
	var seqs []uint16
	for s := uint16(0); s < 100; s++ {
		seqs = append(seqs, s)
	}
	seqs = append(seqs, 105, 106, 107)      // stream continues past the burst
	seqs = append(seqs, 100, 101, 102, 103) // retransmitted within the window
	for s := uint16(108); s < 114; s++ {
		seqs = append(seqs, s) // window slides past 104
	}
	got := feed(t, r, seqs...)
	feed(t, r, 104) // too late
	if r.Stats.Recovered != 4 || r.Stats.Unrecovered != 1 || r.Stats.Late != 1 {
		t.Fatalf("stats %+v", r.Stats)
	}
	if rate := r.Stats.RecoveryRate(); rate != 0.8 {
		t.Fatalf("recovery rate=%v want 0.8", rate)
	}
	if !increasing(got) {
		t.Fatalf("delivery not in order: %v", got)
	}
}

func TestGapBeyondWindowUnrecovered(t *testing.T) {
	r := NewReorderer(2)
	// 1 never arrives; window 2 forces the gap once 4 is buffered.
	if got := feed(t, r, 0, 2, 3, 4); !eq(got, []uint16{0, 2, 3, 4}) {
		t.Fatalf("got %v", got)
	}
	if r.Stats.Unrecovered != 1 {
		t.Fatalf("unrecovered=%d want 1", r.Stats.Unrecovered)
	}
}
