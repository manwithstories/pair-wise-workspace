package relay

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

func testPacket(seq uint16, timestamp uint32) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: timestamp, SSRC: 1}}
}

func TestJitterBufferReordersAndDeduplicates(t *testing.T) {
	stats := NewStats()
	buffer := NewJitterBufferWithOptions(stats, JitterOptions{Window: 64, ClockRate: 90000, ReorderWait: 10 * time.Millisecond})
	base := time.Now()
	out1 := buffer.Push(testPacket(1, 100), base)
	if len(out1) != 1 || out1[0].SequenceNumber != 1 {
		t.Fatalf("first output = %v", out1)
	}
	if out := buffer.Push(testPacket(3, 300), base.Add(2*time.Millisecond)); len(out) != 0 {
		t.Fatalf("gap should be buffered, got %d", len(out))
	}
	out := buffer.Push(testPacket(2, 200), base.Add(4*time.Millisecond))
	if len(out) != 2 || out[0].SequenceNumber != 2 || out[1].SequenceNumber != 3 {
		t.Fatalf("reordered output = %+v", out)
	}
	if out := buffer.Push(testPacket(2, 200), base.Add(6*time.Millisecond)); len(out) != 0 {
		t.Fatalf("duplicate emitted %d packets", len(out))
	}
	metrics := stats.Snapshot()
	if metrics.Received != 3 || metrics.Unique != 3 || metrics.Duplicates != 1 || metrics.Reordered != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

func TestJitterBufferDropsOutsideWindow(t *testing.T) {
	stats := NewStats()
	buffer := NewJitterBufferWithOptions(stats, JitterOptions{Window: 4, ClockRate: 90000, ReorderWait: time.Millisecond})
	buffer.Push(testPacket(20, 2000), time.Now())
	if out := buffer.Push(testPacket(10, 1000), time.Now()); len(out) != 0 {
		t.Fatal("late packet was emitted")
	}
	statsNow := stats.Snapshot()
	if statsNow.LateDropped != 1 || statsNow.Received != 2 || statsNow.Unique != 1 {
		t.Fatalf("metrics = %+v", statsNow)
	}
}

func TestJitterBufferSequenceWrap(t *testing.T) {
	stats := NewStats()
	buffer := NewJitterBuffer(stats)
	base := time.Now()
	got := make([]uint16, 0, 3)
	for i, seq := range []uint16{65535, 0, 1} {
		got = append(got, packetSequences(buffer.Push(testPacket(seq, uint32(1000+i*180)), base.Add(time.Duration(i)*2*time.Millisecond)))...)
	}
	if len(got) != 3 || got[0] != 65535 || got[1] != 0 || got[2] != 1 {
		t.Fatalf("wrapped output = %v", got)
	}
	metrics := stats.Snapshot()
	if metrics.Reordered != 0 || metrics.LateDropped != 0 || metrics.Duplicates != 0 {
		t.Fatalf("wrap metrics = %+v", metrics)
	}
}

func TestJitterBufferFlushReleasesLostGap(t *testing.T) {
	stats := NewStats()
	buffer := NewJitterBufferWithOptions(stats, JitterOptions{Window: 64, ReorderWait: 10 * time.Millisecond})
	base := time.Now()
	buffer.Push(testPacket(1, 100), base)
	buffer.Push(testPacket(3, 300), base.Add(time.Millisecond))
	if out := buffer.Flush(base.Add(2 * time.Millisecond)); len(out) != 0 {
		t.Fatalf("flushed before reorder wait: %v", packetSequences(out))
	}
	out := buffer.Flush(base.Add(20 * time.Millisecond))
	if len(out) != 1 || out[0].SequenceNumber != 3 {
		t.Fatalf("flushed = %v", packetSequences(out))
	}
}

func TestJitterEstimatorHandlesTimestampWrap(t *testing.T) {
	estimator := NewJitterEstimator(90000)
	base := time.Now()
	ts := uint32(0xFFFFFF00)
	estimator.Record(ts, base, nil)
	got := estimator.Record(ts+320, base.Add(2*time.Millisecond), nil)
	if got < 0 || got > 1 {
		t.Fatalf("wrapped jitter = %fms, want a small value", got)
	}
}

func packetSequences(packets []*rtp.Packet) []uint16 {
	out := make([]uint16, 0, len(packets))
	for _, pkt := range packets {
		out = append(out, pkt.SequenceNumber)
	}
	return out
}
