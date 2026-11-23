package rtp

import (
	"testing"

	"mediad/diag"
)

func TestParseAndTimeMapping(t *testing.T) {
	lim := diag.DefaultLimits()
	s, err := NewStream("video-600", 90000, lim)
	if err != nil {
		t.Fatal(err)
	}
	// 90 kHz video clock: 30 fps => 3000 ticks per frame.
	raw := BuildPacket(0x1234, 0, 1000, false, []byte{0xAA})
	p, err := s.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.ExtTicks != 1000 || p.Time.Millis() != 0 || p.Seq != 0 || len(p.Payload) != 1 {
		t.Fatalf("first packet = %+v", p)
	}
	raw = BuildPacket(0x1234, 1, 1000+3000, true, []byte{0xBB})
	p, err = s.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// One frame later: 3000/90000 s = 33.333... ms -> 33.
	if p.Time.Millis() != 33 || !p.Marker || !p.Keyframe {
		t.Fatalf("second packet = %+v", p)
	}
	if s.PacketsSeen() != 2 {
		t.Fatalf("packet count = %d", s.PacketsSeen())
	}
}

func TestTimestampWraparound(t *testing.T) {
	lim := diag.DefaultLimits()
	s, _ := NewStream("v", 90000, lim)
	const nearMax uint32 = 0xFFFFFFF0
	// First packet anchors the base near the 32-bit ceiling.
	if _, err := s.Parse(BuildPacket(1, 0, nearMax, false, nil)); err != nil {
		t.Fatal(err)
	}
	// Next packet crosses the modulus: +32 ticks.
	p, err := s.Parse(BuildPacket(1, 1, 16, true, nil))
	if err != nil {
		t.Fatalf("wrapped packet rejected: %v", err)
	}
	want := int64(nearMax) + 32 // extended, no modulus truncation
	if p.ExtTicks != want {
		t.Fatalf("ext ticks = %d, want %d (ROC=%d)", p.ExtTicks, want, s.ROC())
	}
	if s.ROC() != 1 {
		t.Fatalf("ROC = %d, want 1", s.ROC())
	}
	// Time stays continuous: 32/90000 s rounds to 0 ms but is non-zero exact.
	if p.Time.IsZero() {
		t.Fatalf("wrapped time must not be zero: %s", p.Time.String())
	}
}

func TestMalformedPacket(t *testing.T) {
	s, _ := NewStream("v", 90000, diag.DefaultLimits())
	if _, err := s.Parse([]byte{0x80, 0x60}); diag.CodeOf(err) != diag.ERTP {
		t.Fatalf("truncated header must be ERTP, got %v", err)
	}
	// Version mismatch.
	bad := BuildPacket(1, 0, 0, false, nil)
	bad[0] = 0x00
	if _, err := s.Parse(bad); diag.CodeOf(err) != diag.ERTP {
		t.Fatalf("bad version must be ERTP, got %v", err)
	}
	// Stream state unchanged after failures: a valid packet still anchors.
	if _, err := s.Parse(BuildPacket(1, 0, 500, false, nil)); err != nil {
		t.Fatalf("stream poisoned by bad packet: %v", err)
	}
}

func TestTimestampHorizonLimit(t *testing.T) {
	lim := diag.DefaultLimits()
	lim.MaxTimestampMS = 1000
	s, _ := NewStream("v", 90000, lim)
	if _, err := s.Parse(BuildPacket(1, 0, 0, false, nil)); err != nil {
		t.Fatal(err)
	}
	// 2 seconds at 90 kHz = 180000 ticks, beyond the 1000 ms horizon.
	_, err := s.Parse(BuildPacket(1, 1, 180000, false, nil))
	if diag.CodeOf(err) != diag.EParam {
		t.Fatalf("horizon breach must be E_PARAM, got %v", err)
	}
}

func TestZeroClockRateRejected(t *testing.T) {
	if _, err := NewStream("v", 0, diag.DefaultLimits()); diag.CodeOf(err) != diag.EParam {
		t.Fatalf("zero clock rate must be E_PARAM, got %v", err)
	}
}

func TestRTPMatchesSegmentBoundaryTicks(t *testing.T) {
	// 30 fps: a segment boundary every 30 frames must land on the exact same
	// rational time whether computed from RTP ticks or frame indices.
	lim := diag.DefaultLimits()
	s, _ := NewStream("v", 90000, lim)
	_, _ = s.Parse(BuildPacket(1, 0, 0, true, nil))
	p, err := s.Parse(BuildPacket(1, 30, 30*3000, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	// 90000 ticks == exactly 1 second; no rounding residual allowed.
	if got := p.Time.Millis(); got != 1000 {
		t.Fatalf("boundary ms = %d, want 1000", got)
	}
	whole, remNum, _ := p.Time.FloorMillis()
	if whole != 1000 || remNum != 0 {
		t.Fatalf("boundary not exact: whole=%d remNum=%d", whole, remNum)
	}
}
