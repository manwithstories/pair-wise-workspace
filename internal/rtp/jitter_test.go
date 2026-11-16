package rtpx

import (
	"testing"
	"time"
)

const rate = 90000

func at(ms float64) time.Time {
	return time.Unix(0, 0).Add(time.Duration(ms * float64(time.Millisecond)))
}

func pkt(seq uint16, ts uint32, arrivalMs float64) Packet {
	return Packet{SSRC: 7, Sequence: seq, Timestamp: ts, Arrival: at(arrivalMs), Payload: 10}
}

// paced builds a packet whose arrival matches its RTP timestamp exactly, so a
// stream with no faults has zero transit variation and therefore ~zero jitter.
func paced(seq uint16, step uint32, i int) Packet {
	ts := uint32(i) * step
	return pkt(seq, ts, float64(ts)/float64(rate)*1000)
}

func TestSummaryWithoutPackets(t *testing.T) {
	if _, err := NewAnalyzer(Config{}).Summary(); err != ErrNoPackets {
		t.Fatalf("want ErrNoPackets, got %v", err)
	}
}

func TestPerfectStreamHasNoLossOrJitter(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 32})
	for i := 0; i < 100; i++ {
		a.Observe(paced(uint16(1000+i), 3000, i))
	}
	sum, err := a.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Received != 100 || sum.Unique != 100 || sum.Lost != 0 {
		t.Fatalf("unexpected counts: %+v", sum)
	}
	if sum.Duplicates != 0 || sum.Reordered != 0 || sum.LateOutside != 0 {
		t.Fatalf("unexpected fault counts: %+v", sum)
	}
	if sum.JitterAvgMs > 1 {
		t.Fatalf("jitter too high on an evenly paced stream: %v", sum.JitterAvgMs)
	}
	if sum.JitterMaxMs < sum.JitterAvgMs {
		t.Fatalf("max jitter below average: %+v", sum)
	}
}

func TestDuplicatesCountedOnce(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 32})
	for i := 0; i < 10; i++ {
		a.Observe(pkt(uint16(i), uint32(i*3000), float64(i)))
	}
	// Replay the last three, as a network duplicate would.
	for _, s := range []uint16{7, 8, 9} {
		a.Observe(pkt(s, uint32(int(s)*3000), 20))
	}
	sum, _ := a.Summary()
	if sum.Duplicates != 3 {
		t.Fatalf("want 3 duplicates, got %d", sum.Duplicates)
	}
	if sum.Unique != 10 || sum.Lost != 0 {
		t.Fatalf("duplicates must not affect unique/lost: %+v", sum)
	}
	if sum.Received != 13 {
		t.Fatalf("want 13 received, got %d", sum.Received)
	}
}

func TestReorderedInsideWindow(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 8})
	// Deliver 0..9 but push 8 and 9 ahead of 5,6,7.
	for _, s := range []uint16{0, 1, 2, 3, 4, 8, 9, 5, 6, 7} {
		a.Observe(pkt(s, uint32(s)*3000, float64(s)))
	}
	sum, _ := a.Summary()
	if sum.Reordered != 3 {
		t.Fatalf("want 3 reordered, got %d (%+v)", sum.Reordered, sum)
	}
	if sum.LateOutside != 0 {
		t.Fatalf("in-window arrivals must not be counted late: %+v", sum)
	}
	if sum.Lost != 0 {
		t.Fatalf("reordering is not loss: %+v", sum)
	}
}

func TestLateOutsideWindow(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 4})
	for i := 0; i < 40; i++ {
		a.Observe(paced(uint16(i), 3000, i))
	}
	// Sequence 2 arrives 30 packets too late.
	a.Observe(pkt(2, 2*3000, 100))
	sum, _ := a.Summary()
	if sum.LateOutside != 1 {
		t.Fatalf("want 1 late-outside-window packet, got %d", sum.LateOutside)
	}
	if sum.Reordered != 0 {
		t.Fatalf("late arrival must not be called reordered: %+v", sum)
	}
	if sum.Lost != 0 {
		t.Fatalf("late packet filled the gap, so loss must be zero: %+v", sum)
	}
}

func TestLatePacketFillsReportedGap(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 16})
	a.Observe(pkt(0, 0, 0))
	a.Observe(pkt(2, 6000, 2)) // gap at 1
	if sum, _ := a.Summary(); sum.Lost != 1 {
		t.Fatalf("want the gap counted as lost, got %+v", sum)
	}
	a.Observe(pkt(1, 3000, 3)) // the gap fills late, inside the window
	sum, _ := a.Summary()
	if sum.Lost != 0 {
		t.Fatalf("a filled gap is not permanent loss: %+v", sum)
	}
	if sum.Reordered != 1 {
		t.Fatalf("want 1 reordered, got %+v", sum)
	}
}

func TestSequenceWraparound(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 16})
	start := uint16(65530)
	for i := 0; i < 20; i++ {
		a.Observe(paced(start+uint16(i), 3000, i))
	}
	sum, _ := a.Summary()
	if sum.Lost != 0 {
		t.Fatalf("wraparound must not look like loss: %+v", sum)
	}
	if sum.Unique != 20 {
		t.Fatalf("want 20 unique, got %d", sum.Unique)
	}
	if len(sum.Streams) != 1 || sum.Streams[0].WrapCount != 1 {
		t.Fatalf("want 1 wrap, got %+v", sum.Streams)
	}
	if sum.Streams[0].FirstSequence != start {
		t.Fatalf("first sequence wrong: %+v", sum.Streams[0])
	}
	if sum.Streams[0].LastSequence != start+19 {
		t.Fatalf("last sequence wrong: %+v", sum.Streams[0])
	}
}

func TestReorderAcrossWraparound(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 16})
	// 65534, 65535, 0, 1 with 0 arriving before 65535.
	for _, s := range []uint16{65534, 0, 1, 65535} {
		a.Observe(pkt(s, uint32(s)*3000, 0))
	}
	sum, _ := a.Summary()
	if sum.Unique != 4 || sum.Lost != 0 {
		t.Fatalf("wraparound reorder must not read as loss: %+v", sum)
	}
	if sum.Reordered != 1 {
		t.Fatalf("want 1 reordered, got %+v", sum)
	}
}

func TestMultipleSSRCsGroupedSeparately(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 8})
	for i := 0; i < 5; i++ {
		a.Observe(Packet{SSRC: 1, Sequence: uint16(i), Timestamp: uint32(i * 3000), Arrival: at(float64(i))})
		a.Observe(Packet{SSRC: 2, Sequence: uint16(100 + i), Timestamp: uint32(i * 3000), Arrival: at(float64(i))})
	}
	sum, _ := a.Summary()
	if len(sum.Streams) != 2 {
		t.Fatalf("want two streams, got %d", len(sum.Streams))
	}
	for _, st := range sum.Streams {
		if st.Unique != 5 || st.Lost != 0 {
			t.Fatalf("per-SSRC counts wrong: %+v", st)
		}
	}
	if sum.Streams[0].SSRC >= sum.Streams[1].SSRC {
		t.Fatalf("streams must be sorted by SSRC: %+v", sum.Streams)
	}
}

func TestJitterRespondsToDelay(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 8})
	// 20 ms pacing with timestamps to match, then one packet 40 ms late.
	// Per RFC 3550 a single deviation of D moves the estimate by D/16, so the
	// expected maximum is ~2.5 ms — the estimator smooths spikes, it does not
	// report the raw deviation.
	for i := 0; i < 10; i++ {
		a.Observe(paced(uint16(i), 1800, i))
	}
	a.Observe(pkt(10, 10*1800, 10*20+40))
	sum, _ := a.Summary()
	if sum.JitterMaxMs < 2 || sum.JitterMaxMs > 4 {
		t.Fatalf("one 40ms spike should give ~2.5ms smoothed jitter, got %v", sum.JitterMaxMs)
	}
	// With one spike the average and the maximum coincide; the invariant that
	// matters is that the max never falls below the average.
	if sum.JitterMaxMs < sum.JitterAvgMs {
		t.Fatalf("average above maximum: %+v", sum)
	}
}

func TestSeenSetStaysBounded(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 8})
	for i := 0; i < 5000; i++ {
		a.Observe(paced(uint16(i), 3000, i))
	}
	a.mu.Lock()
	n := len(a.stream[7].seen)
	a.mu.Unlock()
	if n > 4*(8+1) {
		t.Fatalf("seen set grew to %d entries; it must track the reorder window", n)
	}
}

func TestObserverIsConcurrencySafe(t *testing.T) {
	a := NewAnalyzer(Config{ClockRate: rate, ReorderWindow: 32})
	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				seq := uint16(w*200 + i)
				a.Observe(pkt(seq, uint32(seq)*3000, float64(i)))
			}
		}(w)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	sum, err := a.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Received != 800 {
		t.Fatalf("want 800 observations, got %d", sum.Received)
	}
}
