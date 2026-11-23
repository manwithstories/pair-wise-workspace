package stream

import "testing"

// RTP sequence numbers wrap at 2^16; reordering and loss detection must
// work across the wrap point.
func TestSequenceWraparound(t *testing.T) {
	r := NewReorderer(8)
	got := feed(t, r, 65533, 65534, 65535, 0, 1, 2)
	if !eq(got, []uint16{65533, 65534, 65535, 0, 1, 2}) {
		t.Fatalf("got %v", got)
	}
	if r.Stats.Unrecovered != 0 || r.Stats.Late != 0 {
		t.Fatalf("stats %+v", r.Stats)
	}
}

func TestReorderAcrossWraparound(t *testing.T) {
	r := NewReorderer(8)
	// 0 and 1 arrive swapped across the wrap boundary.
	got := feed(t, r, 65534, 65535, 1, 0, 2)
	if !eq(got, []uint16{65534, 65535, 0, 1, 2}) {
		t.Fatalf("got %v", got)
	}
	if r.Stats.Recovered != 1 {
		t.Fatalf("recovered=%d want 1", r.Stats.Recovered)
	}
}
