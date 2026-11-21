package main

import (
	"testing"
)

func TestGenWorldDeterministic(t *testing.T) {
	p := defaultParams()
	w1, w2 := GenWorld(p), GenWorld(p)
	if len(w1.VideoArrival) != len(w2.VideoArrival) {
		t.Fatal("arrival length differs")
	}
	for i := range w1.VideoArrival {
		a, b := w1.VideoArrival[i], w2.VideoArrival[i]
		if a.SequenceNumber != b.SequenceNumber || a.Timestamp != b.Timestamp {
			t.Fatalf("non-deterministic arrival at %d", i)
		}
	}
	if p.Digest() != p.Digest() || len(p.Digest()) != 64 {
		t.Fatal("digest unstable")
	}
}

func TestGenWorldCorruptions(t *testing.T) {
	p := defaultParams()
	w := GenWorld(p)
	n := p.Segments * p.PktsPerSeg
	want := n - p.DropCount + p.Duplicates
	if len(w.VideoArrival) != want {
		t.Fatalf("arrival=%d want %d", len(w.VideoArrival), want)
	}
	// dropped packets absent from arrival
	seen := map[uint16]bool{}
	for _, pkt := range w.VideoArrival {
		seen[pkt.SequenceNumber] = true
	}
	for s := p.DropStart; s < p.DropStart+p.DropCount; s++ {
		if seen[uint16(s)] {
			t.Fatalf("dropped seq %d present", s)
		}
	}
	if len(w.Parity) != n/2 {
		t.Fatalf("parity count %d", len(w.Parity))
	}
	if len(w.AudioArrival) != n {
		t.Fatalf("audio arrival %d", len(w.AudioArrival))
	}
}

func TestParseBandwidth(t *testing.T) {
	bw, err := parseBandwidth("2m,700k,3m")
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{2_000_000, 700_000, 3_000_000}
	for i := range want {
		if bw[i] != want[i] {
			t.Fatalf("bw=%v want %v", bw, want)
		}
	}
	if _, err := parseBandwidth("junk"); err == nil {
		t.Fatal("bad bandwidth must fail")
	}
}
