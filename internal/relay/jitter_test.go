package relay

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

func testPacket(seq uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Timestamp: uint32(seq) * 960, SequenceNumber: seq}}
}

func TestRFC3550JitterTracksDelayVariation(t *testing.T) {
	tracker := NewTracker(48000)
	base := time.Unix(10, 0)
	for n := uint16(0); n < 100; n++ {
		arrival := base.Add(time.Duration(n) * 20 * time.Millisecond)
		if n > 50 {
			arrival = arrival.Add(time.Duration(n%4) * time.Millisecond)
		}
		tracker.Observe(testPacket(n), arrival)
	}
	stats := tracker.Stats()
	if stats.Samples != 100 || stats.P95 <= 0 || stats.Max < stats.P95 {
		t.Fatalf("invalid jitter stats: %+v", stats)
	}
}

func TestReorderBufferOrderingAndLate(t *testing.T) {
	buffer := NewReorderBuffer(3, 40*time.Millisecond)
	base := time.Unix(20, 0)
	var frames []Frame
	frames = append(frames, buffer.Push(testPacket(1), base)...)
	frames = append(frames, buffer.Push(testPacket(3), base.Add(40*time.Millisecond))...)
	frames = append(frames, buffer.Push(testPacket(2), base.Add(45*time.Millisecond))...)
	if len(frames) != 3 || frames[0].Packet.SequenceNumber != 1 ||
		frames[1].Packet.SequenceNumber != 2 || frames[2].Packet.SequenceNumber != 3 {
		t.Fatalf("not reordered: %+v", frames)
	}
	if got := buffer.Push(testPacket(2), base.Add(50*time.Millisecond)); len(got) != 0 {
		t.Fatal("late packet must not be forwarded")
	}
	late, _, _ := buffer.Snapshot()
	if late != 1 {
		t.Fatalf("expected one late packet, got %d", late)
	}
}

func TestReorderBufferFlushSkipsGap(t *testing.T) {
	buffer := NewReorderBuffer(4, 20*time.Millisecond)
	base := time.Unix(30, 0)
	buffer.Push(testPacket(10), base)
	buffer.Push(testPacket(12), base.Add(20*time.Millisecond))
	frames := buffer.Flush(base.Add(41 * time.Millisecond))
	if len(frames) != 1 || frames[0].Packet.SequenceNumber != 12 {
		t.Fatalf("expected sequence 12 after skip, got %+v", frames)
	}
	_, skipped, _ := buffer.Snapshot()
	if skipped != 1 {
		t.Fatalf("expected one skipped packet, got %d", skipped)
	}
}

func TestReorderBufferSequenceWrap(t *testing.T) {
	buffer := NewReorderBuffer(2, 20*time.Millisecond)
	base := time.Unix(40, 0)
	var got []uint16
	for _, seq := range []uint16{65534, 0, 65535, 1} {
		for _, frame := range buffer.Push(testPacket(seq), base) {
			got = append(got, frame.Packet.SequenceNumber)
		}
	}
	want := []uint16{65534, 65535, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wrap order mismatch: %v", got)
		}
	}
}

func TestReorderBufferDropsPendingBehindJump(t *testing.T) {
	buffer := NewReorderBuffer(1, 20*time.Millisecond)
	base := time.Unix(50, 0)
	var got []uint16
	for _, seq := range []uint16{1, 3, 5} {
		for _, frame := range buffer.Push(testPacket(seq), base) {
			got = append(got, frame.Packet.SequenceNumber)
		}
	}
	for _, frame := range buffer.Flush(base.Add(time.Hour)) {
		got = append(got, frame.Packet.SequenceNumber)
	}
	want := []uint16{1, 5}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("stale pending packet was forwarded: %v", got)
	}
}
