package transport

import (
	"testing"

	"relay.local/relay/internal/fault"
)

func TestRecoverDeduplicates(t *testing.T) {
	in := []Packet{
		{Seq: 1, Timestamp: 1000, Payload: []byte("a")},
		{Seq: 2, Timestamp: 2000, Payload: []byte("b")},
		{Seq: 2, Timestamp: 2000, Payload: []byte("b-duplicate")},
		{Seq: 2, Timestamp: 2000, Payload: []byte("b-late"), Arrival: 99},
		{Seq: 3, Timestamp: 3000, Payload: []byte("c")},
	}
	r := Recover(in, Options{})
	if r.Stats.Duplicates != 2 {
		t.Fatalf("duplicates = %d, want 2", r.Stats.Duplicates)
	}
	if len(r.Packets) != 3 {
		t.Fatalf("packets = %d, want 3", len(r.Packets))
	}
	// The earliest arrival wins.
	if string(r.Packets[1].Payload) != "b" {
		t.Fatalf("kept payload %q, want the earliest arrival", r.Packets[1].Payload)
	}
}

func TestRecoverReorderWithinWindow(t *testing.T) {
	// Sequence 3 arrives after 4 and 5 but within a window of 8.
	in := []Packet{
		{Seq: 1}, {Seq: 2}, {Seq: 4}, {Seq: 5}, {Seq: 3, Arrival: 50}, {Seq: 6},
	}
	r := Recover(in, Options{Window: 8})
	if r.Stats.Reordered != 1 || r.Stats.Recovered != 1 {
		t.Fatalf("stats = %+v, want 1 reordered and 1 recovered", r.Stats)
	}
	if r.Stats.Dropped != 0 || r.Stats.Lost != 0 {
		t.Fatalf("unexpected loss: %+v", r.Stats)
	}
	want := []uint16{1, 2, 3, 4, 5, 6}
	for i, s := range want {
		if r.Packets[i].Seq != s {
			t.Fatalf("output = %v, want %v", seqs(r.Packets), want)
		}
	}
}

func TestRecoverDropsBeyondWindow(t *testing.T) {
	// Sequence 3 arrives only after everything else: its reorder distance is
	// 6, beyond a window of 2, so it must be deterministically dropped.
	in := []Packet{
		{Seq: 1}, {Seq: 2}, {Seq: 4}, {Seq: 5}, {Seq: 6}, {Seq: 7}, {Seq: 8}, {Seq: 9},
		{Seq: 3, Arrival: 100},
	}
	r := Recover(in, Options{Window: 2})
	if r.Stats.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", r.Stats.Dropped)
	}
	if r.Stats.Recovered != 0 {
		t.Fatalf("recovered = %d, want 0", r.Stats.Recovered)
	}
	for _, p := range r.Packets {
		if p.Seq == 3 {
			t.Fatal("packet beyond the window was kept")
		}
	}
	found := false
	for _, g := range r.Gaps {
		if g.Reason == "reorder window full" {
			found = true
		}
	}
	if !found {
		t.Fatalf("window gap not recorded: %v", r.Gaps)
	}
}

func TestRecoverCountsLoss(t *testing.T) {
	// Missing sequences 3 and 4 are a pure loss (never arrived).
	in := []Packet{{Seq: 1}, {Seq: 2}, {Seq: 5}, {Seq: 6}}
	r := Recover(in, Options{})
	if r.Stats.Lost != 2 {
		t.Fatalf("lost = %d, want 2", r.Stats.Lost)
	}
	if len(r.Gaps) != 1 || r.Gaps[0].Count != 2 {
		t.Fatalf("gaps = %+v", r.Gaps)
	}
}

func TestRecoverExpandsWrap(t *testing.T) {
	in := []Packet{
		{Seq: 1, Timestamp: 0xfffff000},
		{Seq: 2, Timestamp: 0xfffffff0},
		{Seq: 3, Timestamp: 0x00000010},
		{Seq: 4, Timestamp: 0x00001000},
	}
	r := Recover(in, Options{})
	if r.Stats.Wraps != 1 {
		t.Fatalf("wraps = %d, want 1", r.Stats.Wraps)
	}
	for i := 1; i < len(r.Ticks); i++ {
		if r.Ticks[i] <= r.Ticks[i-1] {
			t.Fatalf("ticks not monotonic: %v", r.Ticks)
		}
	}
}

func TestRecoverRejectsOversizePayload(t *testing.T) {
	in := []Packet{{Seq: 1, Payload: make([]byte, 100)}}
	r := Recover(in, Options{MaxPayload: 10})
	if len(r.Packets) != 0 {
		t.Fatal("oversize payload was accepted")
	}
	fe, ok := fault.As(r.Faults.Items[0])
	if !ok || fe.Kind != fault.KindLimit || fe.Recoverable {
		t.Fatalf("expected fatal limit fault, got %v", r.Faults)
	}
}

func TestRecoverIsDeterministic(t *testing.T) {
	in := []Packet{
		{Seq: 5}, {Seq: 3}, {Seq: 4}, {Seq: 3, Arrival: 20}, {Seq: 6}, {Seq: 7}, {Seq: 2},
	}
	first := Recover(in, Options{Window: 4})
	for trial := 0; trial < 5; trial++ {
		got := Recover(in, Options{Window: 4})
		if got.Stats != first.Stats {
			t.Fatalf("stats differ: %+v vs %+v", got.Stats, first.Stats)
		}
		if len(got.Packets) != len(first.Packets) {
			t.Fatalf("packet count differs")
		}
		for i := range got.Packets {
			if got.Packets[i].Seq != first.Packets[i].Seq || got.Ticks[i] != first.Ticks[i] {
				t.Fatalf("packet %d differs", i)
			}
		}
	}
}

func TestReassemblerEmitsOnMarker(t *testing.T) {
	r := &Reassembler{}
	if got := r.Push(Packet{Payload: []byte("ab")}); got != nil {
		t.Fatalf("partial unit emitted: %q", got)
	}
	got := r.Push(Packet{Payload: []byte("cd"), Marker: true})
	if string(got) != "abcd" {
		t.Fatalf("unit = %q, want abcd", got)
	}
	if r.Pending() != 0 {
		t.Fatalf("pending = %d after marker", r.Pending())
	}
}

func seqs(p []Packet) []uint16 {
	out := make([]uint16, len(p))
	for i := range p {
		out[i] = p[i].Seq
	}
	return out
}
