package rtp

import (
	"testing"

	"github.com/pion/rtp"

	"github.com/example/mediad/internal/model"
)

func pkt(seq, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: uint16(seq),
			Timestamp:      uint32(ts),
		},
		Payload: []byte{byte(seq)},
	}
}

func segOf(seq uint16) string {
	return "seg-" + string(rune('a'+seq/4))
}

func drain(b *Buffer) []*Unit {
	var out []*Unit
	for {
		u, ok := b.Pop()
		if !ok {
			return out
		}
		out = append(out, u)
	}
}

func TestReorderAndDuplicate(t *testing.T) {
	b, err := NewBuffer(Config{ClockRate: 90000, WindowSize: 8, SegmentOf: segOf})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []uint32{0, 2, 1, 3, 3, 4} {
		if err := b.Push(pkt(s, s*3000)); err != nil {
			t.Fatal(err)
		}
	}
	got := drain(b)
	if len(got) != 5 {
		t.Fatalf("units = %d, want 5", len(got))
	}
	for i, u := range got {
		if u.Seq != uint16(i) {
			t.Fatalf("unit %d seq = %d", i, u.Seq)
		}
	}
	st := b.Stats()
	if st.Duplicates != 1 || st.Reordered != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestTimestampWraparound(t *testing.T) {
	b, _ := NewBuffer(Config{ClockRate: 90000, WindowSize: 8})
	// timestamps step 3000 (one video frame at 30fps) across the boundary
	base := uint32(0xFFFFFFF0)
	for i := uint32(0); i < 8; i++ {
		if err := b.Push(pkt(i, base+i*3000)); err != nil {
			t.Fatal(err)
		}
	}
	got := drain(b)
	if len(got) != 8 {
		t.Fatalf("units = %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time <= got[i-1].Time {
			t.Fatalf("time not monotonic at %d: %d <= %d",
				i, got[i].Time, got[i-1].Time)
		}
	}
}

func TestBurstLossRecovered(t *testing.T) {
	b, _ := NewBuffer(Config{ClockRate: 90000, WindowSize: 8, SegmentOf: segOf})
	// burst: seqs 1..3 lost initially, repaired within the window
	for _, s := range []uint32{0, 4} {
		_ = b.Push(pkt(s, s*3000))
	}
	for _, s := range []uint32{1, 2, 3} {
		_ = b.Push(pkt(s, s*3000))
	}
	got := drain(b)
	if len(got) != 5 {
		t.Fatalf("units = %d, want 5", len(got))
	}
	st := b.Stats()
	if st.Recovered != 3 || st.UnrecoverablePackets != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if len(st.QuarantinedSegments) != 0 {
		t.Fatalf("unexpected quarantine: %v", st.QuarantinedSegments)
	}
}

func TestLossBeyondWindowQuarantines(t *testing.T) {
	b, _ := NewBuffer(Config{ClockRate: 90000, WindowSize: 4, SegmentOf: segOf})
	// seq 0 arrives, seq 1 lost forever, stream advances well past it
	for s := uint32(0); s <= 10; s++ {
		if s == 1 {
			continue
		}
		if err := b.Push(pkt(s, s*3000)); err != nil {
			t.Fatal(err)
		}
	}
	got := drain(b)
	st := b.Stats()
	if st.UnrecoverablePackets != 1 {
		t.Fatalf("unrecoverable = %d, stats %+v", st.UnrecoverablePackets, st)
	}
	if len(st.QuarantinedSegments) != 1 {
		t.Fatalf("quarantine = %v", st.QuarantinedSegments)
	}
	// emitted units must remain gap-free and ordered
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatal("output order broken after loss")
		}
	}
	outcomes, err := Classify(st)
	if err == nil {
		t.Fatal("unrecoverable loss must surface an error")
	}
	if me, ok := model.AsError(err); !ok || me.Kind != model.KindCorrupt {
		t.Fatalf("wrong error: %v", err)
	}
	if outcomes[OutcomeUnrecoverable] != 1 {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

func TestLatePacketAfterExpiryRejected(t *testing.T) {
	b, _ := NewBuffer(Config{ClockRate: 90000, WindowSize: 4, SegmentOf: segOf})
	for s := uint32(0); s <= 10; s++ {
		if s == 1 {
			continue
		}
		_ = b.Push(pkt(s, s*3000))
	}
	// seq 1 shows up long after its window expired
	if err := b.Push(pkt(1, 3000)); err != nil {
		t.Fatal(err)
	}
	if b.Stats().Recovered != 0 {
		t.Fatal("expired gap must not be recovered")
	}
}

func TestFlush(t *testing.T) {
	b, _ := NewBuffer(Config{ClockRate: 90000, WindowSize: 16, SegmentOf: segOf})
	_ = b.Push(pkt(0, 0))
	_ = b.Push(pkt(1, 3000))
	_ = b.Push(pkt(3, 9000)) // seq 2 missing, no further traffic
	rest := b.Flush()
	if len(rest) != 3 {
		t.Fatalf("flushed units = %d", len(rest))
	}
	if b.Stats().UnrecoverablePackets != 1 {
		t.Fatalf("stats = %+v", b.Stats())
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := NewBuffer(Config{}); err == nil {
		t.Fatal("zero clock rate must be rejected")
	}
}
