package rtpadapt_test

import (
	"testing"

	"github.com/pion/rtp"

	rtpadapt "pairwise/ingestcheck/internal/rtp"
)

// packet marshals a minimal datagram with the supplied sequence number.
func packet(t *testing.T, seq uint16) []byte {
	t.Helper()
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, SSRC: 42},
		Payload: []byte{0xAA, byte(seq)},
	}
	buf, err := p.Marshal()
	if err != nil {
		t.Fatalf("marshal %d: %v", seq, err)
	}
	return buf
}

func mustWindow(t *testing.T) *rtpadapt.Window {
	t.Helper()
	w, err := rtpadapt.New(rtpadapt.DefaultConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

func TestInOrderEmitsEveryPacket(t *testing.T) {
	w := mustWindow(t)
	for i := uint16(0); i < 10; i++ {
		if _, err := w.Feed(packet(t, 100+i), int64(i), 1); err != nil {
			t.Fatalf("feed %d: %v", i, err)
		}
	}
	w.Flush()
	st := w.Stats()
	if st.Emitted != 10 || st.Duplicate != 0 || st.Late != 0 || st.Lost != 0 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}

func TestReorderWithinWindow(t *testing.T) {
	w := mustWindow(t)
	// Deliver 1,3,2,4. The window must emit 1,2,3,4 in order.
	seqs := []uint16{1, 3, 2, 4}
	for i, s := range seqs {
		pkts, err := w.Feed(packet(t, s), int64(i), 1)
		if err != nil {
			t.Fatalf("feed %d: %v", s, err)
		}
		_ = pkts
	}
	w.Flush()
	// Re-feed through a recording window to observe emission order.
	w2 := mustWindow(t)
	var order []uint16
	for i, s := range seqs {
		pkts, _ := w2.Feed(packet(t, s), int64(i), 1)
		for _, p := range pkts {
			order = append(order, p.Seq)
		}
	}
	for _, p := range w2.Flush() {
		order = append(order, p.Seq)
	}
	want := []uint16{1, 2, 3, 4}
	if len(order) != len(want) {
		t.Fatalf("emitted %d packets, want %d: %v", len(order), len(want), order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order[%d] = %d, want %d (%v)", i, order[i], want[i], order)
		}
	}
}

func TestDuplicateDropped(t *testing.T) {
	w := mustWindow(t)
	for _, s := range []uint16{5, 6, 6, 7, 6} {
		if _, err := w.Feed(packet(t, s), 0, 1); err != nil {
			t.Fatalf("feed %d: %v", s, err)
		}
	}
	w.Flush()
	st := w.Stats()
	if st.Duplicate != 2 {
		t.Fatalf("duplicates = %d, want 2", st.Duplicate)
	}
	if st.Emitted != 3 {
		t.Fatalf("emitted = %d, want 3", st.Emitted)
	}
}

func TestWrapAround(t *testing.T) {
	w := mustWindow(t)
	// Cross the 16-bit boundary; the window must treat the stream as
	// continuous rather than as a massive backwards jump.
	var emitted int
	for i := 0; i < 8; i++ {
		seq := uint16(0xFFFC + i)
		pkts, err := w.Feed(packet(t, seq), int64(i), 1)
		if err != nil {
			t.Fatalf("feed %d: %v", seq, err)
		}
		emitted += len(pkts)
	}
	for _, p := range w.Flush() {
		emitted++
		_ = p
	}
	st := w.Stats()
	if st.Lost != 0 || st.Late != 0 || st.Duplicate != 0 {
		t.Fatalf("wrap produced spurious accounting: %+v", st)
	}
	if emitted != 8 {
		t.Fatalf("emitted %d packets across wrap, want 8", emitted)
	}
}

func TestBurstLossRecordedAsGap(t *testing.T) {
	w := mustWindow(t)
	// Deliver 0..3, skip 4..6, deliver 7..9. Flush must report one hole.
	for i := uint16(0); i <= 3; i++ {
		if _, err := w.Feed(packet(t, 10+i), int64(i), 1); err != nil {
			t.Fatalf("feed: %v", err)
		}
	}
	for i := uint16(7); i <= 9; i++ {
		if _, err := w.Feed(packet(t, 10+i), int64(i), 1); err != nil {
			t.Fatalf("feed: %v", err)
		}
	}
	w.Flush()
	st := w.Stats()
	if len(st.Gaps) != 1 {
		t.Fatalf("gaps = %v, want one", st.Gaps)
	}
	if st.Gaps[0].End-st.Gaps[0].Start != 3 {
		t.Fatalf("gap length = %d, want 3", st.Gaps[0].End-st.Gaps[0].Start)
	}
	if st.Lost != 3 {
		t.Fatalf("lost = %d, want 3", st.Lost)
	}
}

func TestLatePacketOutsideWindowCounted(t *testing.T) {
	w := mustWindow(t)
	// Advance the drain head well past the duplicate-detection horizon.
	for i := uint16(0); i < 150; i++ {
		if _, err := w.Feed(packet(t, 200+i), int64(i), 1); err != nil {
			t.Fatalf("feed: %v", err)
		}
	}
	// A sequence far below the drain head is late, not a duplicate.
	if _, err := w.Feed(packet(t, 205), 0, 1); err != nil {
		t.Fatalf("feed late: %v", err)
	}
	w.Flush()
	st := w.Stats()
	if st.Late != 1 {
		t.Fatalf("late = %d, want 1", st.Late)
	}
	if st.Duplicate != 0 {
		t.Fatalf("duplicate = %d, want 0 (packet was outside the horizon)", st.Duplicate)
	}
}

func TestCapacityBoundsHeld(t *testing.T) {
	cfg := rtpadapt.DefaultConfig()
	cfg.Capacity = 4
	w, err := rtpadapt.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Feed a long run with one missing packet at the head: memory must stay
	// bounded because the window gives up on the hole.
	if _, err := w.Feed(packet(t, 0), 0, 1); err != nil {
		t.Fatalf("feed: %v", err)
	}
	_ = w.Flush()
	for i := uint16(100); i < 200; i++ {
		if _, err := w.Feed(packet(t, i), int64(i), 1); err != nil {
			t.Fatalf("feed: %v", err)
		}
	}
	if peak := w.Stats().MaxHeldPeak; peak > cfg.Capacity+1 {
		t.Fatalf("held peak = %d exceeds capacity bound %d", peak, cfg.Capacity)
	}
}

func TestMalformedDatagramRejected(t *testing.T) {
	w := mustWindow(t)
	if _, err := w.Feed([]byte{0x00, 0x01}, 0, 1); err == nil {
		t.Fatal("short datagram accepted")
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	for _, c := range []rtpadapt.Config{
		{Capacity: 0, MaxSeen: 1},
		{Capacity: 1, MaxSeen: 0},
	} {
		if _, err := rtpadapt.New(c); err == nil {
			t.Fatalf("config %+v accepted", c)
		}
	}
}
