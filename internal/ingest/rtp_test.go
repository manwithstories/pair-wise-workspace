package ingest

import (
	"errors"
	"testing"
	"time"

	"mediacq/internal/clock"
	"mediacq/internal/testdata"
)

func newTestStream(t *testing.T, threshold float64) *RTPStream {
	t.Helper()
	s, err := NewRTPStream(RTPConfig{
		ClockRate:       30000,
		DriftThreshold:  threshold,
		DriftWindow:     int64(50 * time.Millisecond),
		MaxDriftWindows: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRTPUnwrapOnFixture(t *testing.T) {
	p := testdata.DefaultParams(17)
	p.WrapAt = 4294967200
	f, err := testdata.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := testdata.MarshalRTP(f.RTP)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestStream(t, 100)
	wraps := 0
	var prev clock.Time = -1
	for i, pkt := range raw {
		ref := clock.Time(time.Duration(i) * 33 * time.Millisecond)
		pt, err := s.Push(pkt, ref)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if pt.Wrapped {
			wraps++
		}
		if pt.Time < prev {
			t.Fatal("unified timeline moved backwards after wrap")
		}
		prev = pt.Time
	}
	if wraps == 0 {
		t.Fatal("expected at least one wrap expansion")
	}
}

func TestRTPLatePacketKeepsFrontier(t *testing.T) {
	s := newTestStream(t, 100)
	mk := func(ts uint32, seq uint16) []byte {
		pkts, _ := testdata.MarshalRTP([]testdata.RTPPacket{
			{Timestamp: ts, Sequence: seq, SSRC: 1, PayloadType: 96, Payload: []byte{1}},
		})
		return pkts[0]
	}
	if _, err := s.Push(mk(1000, 1), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Push(mk(5000, 2), clock.Time(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	front := s.Frontier()
	pt, err := s.Push(mk(2000, 3), clock.Time(20*time.Millisecond))
	if !errors.Is(err, ErrLatePacket) || !pt.Late {
		t.Fatalf("late packet: err=%v late=%v", err, pt.Late)
	}
	if s.Frontier() != front {
		t.Fatal("late packet rewrote the committed frontier")
	}
}

func TestRTPDriftDetection(t *testing.T) {
	s := newTestStream(t, 100)
	mk := func(ts uint32, seq uint16) []byte {
		pkts, _ := testdata.MarshalRTP([]testdata.RTPPacket{
			{Timestamp: ts, Sequence: seq, SSRC: 1, PayloadType: 96, Payload: []byte{1}},
		})
		return pkts[0]
	}
	// Sender ~200 ppm fast: advance sender ticks slightly more than reference.
	var sender uint32
	flagged := false
	for i := 0; i < 40; i++ {
		ref := clock.Time(time.Duration(i) * time.Second)
		nominal := uint32(int64(i) * 30000)
		sender = uint32(float64(nominal) * 1.0002)
		pt, err := s.Push(mk(sender, uint16(i+1)), ref)
		if err != nil {
			t.Fatal(err)
		}
		if pt.DriftFlag {
			flagged = true
		}
	}
	if !flagged {
		t.Fatalf("200 ppm drift was not flagged, last=%.2f", s.LastPPM())
	}
	if len(s.DriftWindows()) == 0 {
		t.Fatal("drift windows not recorded")
	}
}

func TestRTPRejectsZeroClock(t *testing.T) {
	if _, err := NewRTPStream(RTPConfig{ClockRate: 0}); !errors.Is(err, clock.ErrZeroClock) {
		t.Fatalf("want ErrZeroClock, got %v", err)
	}
}

func TestRTPGarbagePacket(t *testing.T) {
	s := newTestStream(t, 100)
	if _, err := s.Push([]byte{0x00}, 0); err == nil {
		t.Fatal("malformed RTP packet must error, not fabricate a point")
	}
}
