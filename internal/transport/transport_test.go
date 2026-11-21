package transport

import (
	"testing"

	"github.com/pion/rtp"

	"mediasync/internal/model"
)

func pkt(seq uint16, ts uint32, payload byte) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, Marker: true},
		Payload: []byte{payload},
	}
}

// TestReorderDedupAndReorder verifies duplicate suppression and out-of-order
// delivery inside the window, including 16-bit sequence wraparound.
func TestReorderDedupAndReorder(t *testing.T) {
	b := NewReorderBuffer("v", "high", model.Clock90k, ReorderConfig{Window: 8})
	put := func(p *rtp.Packet) []model.Frame { return b.Push(p, 1, 2) }

	frames := put(pkt(0, 100, 0))
	if len(frames) != 1 || frames[0].Seq != 0 {
		t.Fatalf("first packet should emit immediately, got %+v", frames)
	}
	if dup := put(pkt(0, 100, 0)); dup != nil {
		t.Fatal("duplicate seq must not emit")
	}
	// Reorder: seq 2 before seq 1; nothing emits until seq 1 arrives.
	if f := put(pkt(2, 300, 2)); len(f) != 0 {
		t.Fatalf("gap must hold seq 2, got %+v", f)
	}
	frames = put(pkt(1, 200, 1))
	if len(frames) != 2 || frames[0].Seq != 1 || frames[1].Seq != 2 {
		t.Fatalf("reorder must emit 1 then 2, got %+v", frames)
	}
	if !frames[0].Recovered || frames[1].Recovered {
		t.Fatalf("only the hole-filling seq 1 is recovered, got %+v %+v", frames[0], frames[1])
	}
	if st := b.Stats(); st.Duplicates != 1 || st.Recovered != 1 {
		t.Fatalf("counter mismatch: %+v", st)
	}

	// Wraparound near the 16-bit boundary.
	b2 := NewReorderBuffer("v", "high", model.Clock90k, ReorderConfig{Window: 8})
	b2.Push(pkt(65535, 900, 255), 1, 2)
	wrap := b2.Push(pkt(0, 1000, 0), 1, 2)
	if len(wrap) != 1 || wrap[0].Seq != 65536 {
		t.Fatalf("seq wraparound not extended, got %+v", wrap)
	}
}

// TestReorderBurstLoss proves burst losses beyond the window become explicit
// incomplete frames with no fabricated payload, while recoverable bursts heal.
func TestReorderBurstLoss(t *testing.T) {
	b := NewReorderBuffer("v", "mid", model.Clock90k, ReorderConfig{Window: 4})
	var frames []model.Frame
	emit := func(f []model.Frame) { frames = append(frames, f...) }

	emit(b.Push(pkt(0, 0, 0), 1, 2))
	// seq 1..3 lost, seq 4 arrives: burst inside window, no gap yet.
	emit(b.Push(pkt(4, 400, 4), 1, 2))
	for _, f := range frames {
		if f.Incomplete {
			t.Fatal("in-window burst must stay recoverable, no placeholder yet")
		}
	}
	// seq 1..3 retransmitted before the window expires.
	for seq := uint16(1); seq <= 3; seq++ {
		emit(b.Push(pkt(seq, uint32(seq)*100, byte(seq)), 1, 2))
	}
	recovered := 0
	for _, f := range frames {
		if f.Recovered {
			recovered++
		}
	}
	if recovered != 3 {
		t.Fatalf("want 3 recovered burst packets, got %d (frames=%d)", recovered, len(frames))
	}

	// Unrecoverable: lose 10..13, push 14..18 to cross the window threshold.
	frames = nil
	b3 := NewReorderBuffer("v", "low", model.Clock90k, ReorderConfig{Window: 4})
	b3.Push(pkt(9, 900, 9), 1, 2)
	for seq := uint16(14); seq <= 18; seq++ {
		emit(b3.Push(pkt(seq, uint32(seq)*100, byte(seq)), 1, 2))
	}
	gaps := 0
	for _, f := range frames {
		if f.Incomplete {
			gaps++
			if f.Payload != nil {
				t.Fatal("incomplete placeholder must never carry fabricated payload")
			}
		}
	}
	if gaps != 4 {
		t.Fatalf("want 4 unrecoverable gap placeholders, got %d frames=%+v", gaps, frames)
	}
	if st := b3.Stats(); st.Gaps != 4 {
		t.Fatalf("gap counters wrong: %+v", st)
	}
	// A late retransmission after gap declaration cannot resurrect the frame.
	if f := b3.Push(pkt(10, 1000, 10), 1, 2); len(f) != 0 {
		t.Fatalf("late packet past declared gap must be dropped, got %+v", f)
	}
}

// TestFlushClosesRemainingGaps verifies stream-end semantics.
func TestReorderFlush(t *testing.T) {
	b := NewReorderBuffer("v", "high", model.Clock90k, ReorderConfig{Window: 64})
	b.Push(pkt(0, 0, 0), 1, 2)
	b.Push(pkt(2, 200, 2), 1, 2) // seq 1 lost
	out := b.Flush()
	if len(out) != 2 || !(out[0].Seq == 1 && out[0].Incomplete) || out[1].Seq != 2 {
		t.Fatalf("flush must close the open gap, got %+v", out)
	}
}

// TestEstimatorAndSelectorExtremeDrop covers the sudden bandwidth cliff:
// downswitch must be immediate, upswitch delayed by hysteresis.
func TestEstimatorAndSelectorExtremeDrop(t *testing.T) {
	est := NewEstimator(0.5, 1000)
	est.Observe(0, 0)
	est.Observe(100, 12500) // 12500 bytes/100ms = 1 Mbps
	if got := est.BitsPerSecond(); got < 900_000 || got > 1_100_000 {
		t.Fatalf("initial estimate wrong: %d", got)
	}
	est.Observe(200, 1250) // cliff: 100 kbps
	if got := est.BitsPerSecond(); got > 600_000 {
		t.Fatalf("cliff not reflected quickly: %d", got)
	}

	reps := []Representation{{Name: "low", Bitrate: 200_000}, {Name: "mid", Bitrate: 700_000}, {Name: "high", Bitrate: 2_500_000}}
	sel, err := NewSelector(reps, 500)
	if err != nil {
		t.Fatal(err)
	}
	sel.Choose(3_000_000, 0) // high
	if sel.Current() != 2 {
		t.Fatal("setup failed")
	}
	if idx, changed := sel.Choose(150_000, 100); !changed || idx != 0 {
		t.Fatalf("bandwidth cliff must downswitch immediately to low, idx=%d changed=%v", idx, changed)
	}
	if _, changed := sel.Choose(3_000_000, 200); changed {
		t.Fatal("upswitch within hold window must be suppressed")
	}
	if idx, changed := sel.Choose(3_000_000, 700); !changed || idx != 2 {
		t.Fatalf("upswitch allowed after hold with headroom, idx=%d changed=%v", idx, changed)
	}
	if _, err := NewSelector(nil, 1); err == nil {
		t.Fatal("empty representations must error")
	}
}

// TestFanoutSingleContributorAndMemoryBound checks cross-representation
// suppression and that buffering stays within the reorder window.
func TestFanoutSingleContributorAndMemoryBound(t *testing.T) {
	reps := []Representation{{Name: "low", Bitrate: 100_000}, {Name: "high", Bitrate: 1_000_000}}
	f, err := NewFanout(FanoutConfig{
		Track: "v", Clock: model.Clock90k, Reps: reps, EstimatorAlpha: 0.5,
		TickRate: 1000, HoldTicks: 10, Reorder: ReorderConfig{Window: 16},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same RTP timestamp from both representations: only one frame survives.
	var pushed []model.Frame
	first, err := f.Push(SimPacket{RepIndex: 0, Seq: 0, RTPTS: 100, Payload: []byte{1}, SendTicks: 1, RecvTicks: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Push(SimPacket{RepIndex: 1, Seq: 0, RTPTS: 100, Payload: []byte{2}, SendTicks: 1, RecvTicks: 3})
	if err != nil {
		t.Fatal(err)
	}
	pushed = append(pushed, first...)
	pushed = append(pushed, second...)
	out := append(pushed, f.Flush()...)
	tsOwners := map[int64]int{}
	for _, fr := range out {
		tsOwners[fr.Timestamp]++
	}
	if tsOwners[100] != 1 {
		t.Fatalf("timestamp 100 must have exactly one contributor, got %d", tsOwners[100])
	}
	if f.Suppressed() != 1 {
		t.Fatalf("want 1 suppressed duplicate, got %d", f.Suppressed())
	}

	// Memory bound: stream 5000 packets with holes never resolved within window.
	f2, _ := NewFanout(FanoutConfig{
		Track: "v2", Clock: model.Clock90k, Reps: reps[:1], EstimatorAlpha: 0.5,
		TickRate: 1000, HoldTicks: 10, Reorder: ReorderConfig{Window: 16},
	})
	for i := 0; i < 5000; i++ {
		// Lose every 17th packet so gaps cross the window and flush as placeholders.
		if i%17 == 0 && i > 0 {
			continue
		}
		f2.Push(SimPacket{RepIndex: 0, Seq: uint16(i), RTPTS: uint32(i * 3000),
			Payload: []byte{byte(i)}, SendTicks: int64(i), RecvTicks: int64(i + 1)})
		if f2.PendingTotal() > 32 {
			t.Fatalf("pending=%d exceeds window-bound memory", f2.PendingTotal())
		}
	}
	f2.Flush()
	if f2.PendingTotal() != 0 {
		t.Fatal("flush must release all buffered state")
	}
}
