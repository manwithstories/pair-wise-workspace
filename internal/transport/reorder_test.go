package transport

import (
	"testing"

	"mediamix/internal/timeline"
)

func pk(seq uint16, payload string) Packet {
	return Packet{Track: "t", Seq: seq, Timestamp: uint32(seq) * 100,
		Marker: true, Payload: []byte(payload), ArrivalUs: timeline.Micros(seq) * 1000}
}

func TestReorderInOrder(t *testing.T) {
	b, err := NewReorderBuffer("t", Limits{MaxBuffered: 32})
	if err != nil {
		t.Fatal(err)
	}
	var got []uint16
	for i := 0; i < 5; i++ {
		out, err := b.Push(pk(uint16(i), string(rune('a'+i))), timeline.Micros(i*1000))
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		for _, p := range out {
			got = append(got, p.Seq)
		}
	}
	want := []uint16{0, 1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if s := b.Stats(); s.Delivered != 5 || s.Reordered != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestReorderRestoresOrderAcrossGap(t *testing.T) {
	b, _ := NewReorderBuffer("t", Limits{MaxBuffered: 32})
	// 0 delivered immediately; 2 buffered behind the missing 1; then 1 heals.
	out0, _ := b.Push(pk(0, "a"), 0)
	if len(out0) != 1 {
		t.Fatalf("seq0 should deliver, got %d", len(out0))
	}
	out1, _ := b.Push(pk(2, "c"), 2000)
	if len(out1) != 0 {
		t.Fatalf("seq2 must wait, got %d", len(out1))
	}
	g, ok := b.OpenGap()
	if !ok || g.StartExt != 1 || g.EndExt != 1 {
		t.Fatalf("gap = %+v ok=%v", g, ok)
	}
	out2, _ := b.Push(pk(1, "b"), 3000)
	if len(out2) != 2 || out2[0].Seq != 1 || out2[1].Seq != 2 {
		t.Fatalf("healing must deliver 1 then 2, got %+v", out2)
	}
	if b.Stats().Reordered != 1 {
		t.Fatalf("expected one reorder, got %+v", b.Stats())
	}
}

func TestReorderDeduplicatesAndFlagsConflict(t *testing.T) {
	b, _ := NewReorderBuffer("t", Limits{MaxBuffered: 32})
	if _, err := b.Push(pk(0, "a"), 0); err != nil {
		t.Fatal(err)
	}
	if out, err := b.Push(pk(0, "a"), 500); err != nil || len(out) != 0 {
		t.Fatalf("identical duplicate: out=%v err=%v", out, err)
	}
	if b.Stats().Duplicates != 1 {
		t.Fatalf("duplicates = %d", b.Stats().Duplicates)
	}
	// Same sequence, different payload: a conflict, not a duplicate.
	bad := pk(0, "z")
	if _, err := b.Push(bad, 600); err == nil {
		t.Fatal("conflicting duplicate must be rejected")
	}
	if b.Stats().Conflicts != 1 {
		t.Fatalf("conflicts = %d", b.Stats().Conflicts)
	}
}

func TestReorderHandlesWraparound(t *testing.T) {
	b, _ := NewReorderBuffer("t", Limits{MaxBuffered: 128})
	var delivered []uint16
	// Push sequences 65534, 65535, 0, 1 in wire order.
	for _, s := range []uint16{65534, 65535, 0, 1} {
		out, err := b.Push(pk(s, "x"), timeline.Micros(int(s))*1000)
		if err != nil {
			t.Fatalf("push %d: %v", s, err)
		}
		for _, p := range out {
			delivered = append(delivered, p.Seq)
		}
	}
	want := []uint16{65534, 65535, 0, 1}
	if len(delivered) != len(want) {
		t.Fatalf("got %v want %v", delivered, want)
	}
	for i := range want {
		if delivered[i] != want[i] {
			t.Fatalf("got %v want %v", delivered, want)
		}
	}
}

func TestReorderBoundedBufferSkipsHole(t *testing.T) {
	b, _ := NewReorderBuffer("t", Limits{MaxBuffered: 4})
	if _, err := b.Push(pk(0, "a"), 0); err != nil {
		t.Fatal(err)
	}
	// Sequence 1 never arrives; keep pushing until the bound forces a skip.
	var total int
	var err error
	for i := 2; i < 20; i++ {
		var out []Packet
		out, err = b.Push(pk(uint16(i), "x"), timeline.Micros(i*1000))
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		total += len(out)
	}
	if total == 0 {
		t.Fatal("bounded buffer must deliver rather than deadlock")
	}
	if b.Buffered() > 4 {
		t.Fatalf("buffer exceeded bound: %d", b.Buffered())
	}
}

func TestExtenderWraps(t *testing.T) {
	e := NewExtender()
	seqs := []uint16{65534, 65535, 0, 1, 2}
	prev := int64(-1)
	for _, s := range seqs {
		ext, err := e.Extend("t", s)
		if err != nil {
			t.Fatal(err)
		}
		if int64(ext) <= prev {
			t.Fatalf("extender not monotonic at %d: %d <= %d", s, ext, prev)
		}
		prev = int64(ext)
	}
}

func TestRecoveryHealsGap(t *testing.T) {
	b, _ := NewReorderBuffer("t", Limits{MaxBuffered: 32})
	b.Push(pk(0, "a"), 0)
	b.Push(pk(2, "c"), 1000)
	src := &mapSource{packets: map[uint64]Packet{
		1: {Track: "t", Seq: 1, Payload: []byte("b"), Timestamp: 100},
	}}
	eng := NewRecoveryEngine(DefaultRecoveryPolicy(), src)
	// Age the gap past the reorder hold so retransmission is attempted.
	out, outcome, rep := eng.RepairGap(b, 100_000)
	if outcome != Healed {
		t.Fatalf("outcome = %v, want Healed", outcome)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 delivered after healing, got %d", len(out))
	}
	if rep.Recovered != 1 {
		t.Fatalf("recovered = %d", rep.Recovered)
	}
	if rate := eng.RecoveryRate(); rate != 1 {
		t.Fatalf("recovery rate = %v", rate)
	}
}

func TestRecoveryUnrecoverableOnMissingSource(t *testing.T) {
	b, _ := NewReorderBuffer("t", Limits{MaxBuffered: 32})
	b.Push(pk(0, "a"), 0)
	b.Push(pk(2, "c"), 1000)
	src := &mapSource{packets: map[uint64]Packet{}} // nothing recoverable
	eng := NewRecoveryEngine(DefaultRecoveryPolicy(), src)
	_, outcome, rep := eng.RepairGap(b, 100_000)
	if outcome != Unrecoverable {
		t.Fatalf("outcome = %v, want Unrecoverable", outcome)
	}
	if rep.Unrecoverable != 1 {
		t.Fatalf("unrecoverable = %d", rep.Unrecoverable)
	}
}

func TestRecoveryRespectsBandwidthViability(t *testing.T) {
	b, _ := NewReorderBuffer("t", Limits{MaxBuffered: 32})
	b.Push(pk(0, "a"), 0)
	b.Push(pk(9, "j"), 1000) // 8-packet burst
	src := &mapSource{packets: map[uint64]Packet{}, maxBurst: 4}
	eng := NewRecoveryEngine(DefaultRecoveryPolicy(), src)
	// A blocked gap only becomes a loss once its deadline passes. Use a time
	// past the deadline (default 250 ms) to observe that transition.
	_, outcome, _ := eng.RepairGap(b, 1_000_000)
	if outcome != Unrecoverable {
		t.Fatalf("oversized burst must be unrecoverable after the deadline, got %v", outcome)
	}
}

func TestRecoveryRateNoGapsIsOne(t *testing.T) {
	eng := NewRecoveryEngine(DefaultRecoveryPolicy(), nil)
	if rate := eng.RecoveryRate(); rate != 1 {
		t.Fatalf("rate without gaps = %v, want 1", rate)
	}
}

func TestBandwidthControlsBufferedBound(t *testing.T) {
	c := NewController()
	low := c.MaxBuffered(0.5)
	high := c.MaxBuffered(20)
	if low >= high {
		t.Fatalf("low bandwidth bound %d must be smaller than high %d", low, high)
	}
	if low < 1 {
		t.Fatalf("bound must stay positive, got %d", low)
	}
	if got := c.MaxBuffered(0); got < 1 {
		t.Fatalf("zero sample clamped to positive, got %d", got)
	}
}

// mapSource is a RecoverySource backed by a fixed packet map.
type mapSource struct {
	packets  map[uint64]Packet
	maxBurst int
}

func (m *mapSource) Fetch(track string, ext uint64) (Packet, bool) {
	p, ok := m.packets[ext]
	return p, ok
}

func (m *mapSource) GapViable(track string, start uint64, count int) bool {
	return m.maxBurst == 0 || count <= m.maxBurst
}
