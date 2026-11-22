package clock

import (
	"testing"

	"github.com/pion/rtp"
)

func TestRTPTimelineBasic(t *testing.T) {
	tl, err := NewRTPTimeline(90_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkts := []rtp.Packet{
		{Header: rtp.Header{SequenceNumber: 1, Timestamp: 90_000}},
		{Header: rtp.Header{SequenceNumber: 2, Timestamp: 93_000}},
	}
	times := make([]int64, 0, len(pkts))
	for i := range pkts {
		us, err := tl.Push(&pkts[i])
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		times = append(times, us)
	}
	if times[0] != 0 || times[1] != 33_333 {
		t.Fatalf("times=%v", times)
	}
	if tl.Lost() != 0 {
		t.Fatalf("lost=%d", tl.Lost())
	}
}

func TestRTPTimelineSequenceGapAndReorder(t *testing.T) {
	tl, _ := NewRTPTimeline(48_000, 0)
	p1 := &rtp.Packet{Header: rtp.Header{SequenceNumber: 10, Timestamp: 960}}
	p2 := &rtp.Packet{Header: rtp.Header{SequenceNumber: 13, Timestamp: 1920}}
	if _, err := tl.Push(p1); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Push(p2); err != nil {
		t.Fatal(err)
	}
	if tl.Lost() != 2 {
		t.Fatalf("lost=%d want 2", tl.Lost())
	}
	p3 := &rtp.Packet{Header: rtp.Header{SequenceNumber: 12, Timestamp: 2400}}
	if _, err := tl.Push(p3); err == nil {
		t.Fatal("expected reorder error")
	}
	if _, err := tl.Push(nil); err == nil {
		t.Fatal("expected nil error")
	}
}

func TestRTPTimelineTimestampWrap(t *testing.T) {
	tl, _ := NewRTPTimeline(90_000, 0)
	p1 := &rtp.Packet{Header: rtp.Header{SequenceNumber: 1, Timestamp: 4294967290}}
	p2 := &rtp.Packet{Header: rtp.Header{SequenceNumber: 2, Timestamp: 4}}
	if _, err := tl.Push(p1); err != nil {
		t.Fatal(err)
	}
	us, err := tl.Push(p2)
	if err != nil {
		t.Fatal(err)
	}
	// Delta is 10 ticks after wrap.
	if us != TicksRoundTrip(10) {
		t.Fatalf("us=%d", us)
	}
}

func TicksRoundTrip(ticks int64) int64 {
	v, _ := TicksToMicros(ticks, 90_000)
	return v
}
