package rtp

import (
	"testing"

	"github.com/pion/rtp"

	"medpipe/internal/clock"
)

func TestMapperRationalConversion(t *testing.T) {
	// Audio 48 kHz timescale to 48 kHz RTP clock: identity.
	m, err := NewMapper(48000, AudioClockRate)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := m.ToRTP(clock.MustNew(0, 48000))
	if err != nil || ts != 0 {
		t.Fatalf("origin ts=%d err=%v", ts, err)
	}
	ts, _ = m.ToRTP(clock.MustNew(48000, 48000)) // 1 second
	if ts != 48000 {
		t.Fatalf("1s mapped to %d, want 48000", ts)
	}
}

func TestMapperNonZeroOrigin(t *testing.T) {
	// A stream whose first sample is at 96000 media ticks still starts near 0.
	m, _ := NewMapper(48000, AudioClockRate)
	first, _ := m.ToRTP(clock.MustNew(96000, 48000))
	if first != 0 {
		t.Fatalf("non-zero origin gave %d, want 0", first)
	}
	second, _ := m.ToRTP(clock.MustNew(100800, 48000)) // +4800 ticks = 100 ms
	if second != 4800 {
		t.Fatalf("relative ts=%d, want 4800", second)
	}
}

func TestMapperDifferentTimescale(t *testing.T) {
	// Video 90 kHz RTP clock from a 30 kHz track timescale: factor 3.
	m, _ := NewMapper(30000, VideoClockRate)
	ts, _ := m.ToRTP(clock.MustNew(0, 30000))
	if ts != 0 {
		t.Fatalf("video origin = %d, want 0", ts)
	}
	ts, _ = m.ToRTP(clock.MustNew(30000, 30000)) // 1s media
	if ts != 90000 {
		t.Fatalf("video 1s = %d, want 90000", ts)
	}
}

func TestMapperRejectsTimescaleMismatch(t *testing.T) {
	m, _ := NewMapper(48000, AudioClockRate)
	if _, err := m.ToRTP(clock.MustNew(10, 44100)); err == nil {
		t.Fatal("cross-timescale tick accepted")
	}
	if _, err := NewMapper(0, 48000); err == nil {
		t.Fatal("zero timescale mapper accepted")
	}
}

func TestPacketizerMonotonicAndPionValid(t *testing.T) {
	m, _ := NewMapper(48000, AudioClockRate)
	pkz := NewPacketizer(m, 0x1234, 97)
	for i := 0; i < 5; i++ {
		tick := clock.MustNew(uint64(i)*1024, 48000)
		pkt, err := pkz.Packet(tick, []byte{byte(i)}, i == 4)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if pkt.SequenceNumber != uint16(i) {
			t.Fatalf("seq = %d, want %d", pkt.SequenceNumber, i)
		}
		if pkt.SSRC != 0x1234 || pkt.PayloadType != 97 {
			t.Fatalf("header fields wrong: %+v", pkt.Header)
		}
		if i == 4 && !pkt.Marker {
			t.Fatal("marker bit not set on final packet")
		}
	}
}

func TestPacketizerBackwardsRejected(t *testing.T) {
	m, _ := NewMapper(48000, AudioClockRate)
	pkz := NewPacketizer(m, 1, 97)
	if _, err := pkz.Packet(clock.MustNew(1000, 48000), nil, false); err != nil {
		t.Fatal(err)
	}
	// Earlier media time than the origin-relative stream.
	_, err := pkz.Packet(clock.MustNew(500, 48000), nil, false)
	if !clock.IsKind(err, clock.KindCorrupt) {
		t.Fatalf("backward timestamp not rejected: %v", err)
	}
}

func TestPacketizerWraparound(t *testing.T) {
	// Drive the 32-bit timestamp near its boundary and confirm a wrap is
	// treated as forward progress, not a backwards jump. The mapper origin
	// is the first tick, so pick an origin whose RTP value lands at the edge.
	m, _ := NewMapper(1, 1) // 1:1 so each tick adds one RTP unit
	pkz := NewPacketizer(m, 7, 96)
	origin := uint64(0)
	// First packet establishes the origin (RTP 0).
	if _, err := pkz.Packet(clock.MustNew(origin, 1), nil, false); err != nil {
		t.Fatal(err)
	}
	// Advance to just under the uint32 edge, then across it.
	edge, err := pkz.Packet(clock.MustNew(0xFFFFFFF0, 1), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if edge.Timestamp != uint32(0xFFFFFFF0) {
		t.Fatalf("boundary ts = %#x, want 0xfffffff0", edge.Timestamp)
	}
	wrapped, err := pkz.Packet(clock.MustNew(0xFFFFFFF0+0x20, 1), nil, true)
	if err != nil {
		t.Fatalf("wraparound rejected as backwards: %v", err)
	}
	// 0x20 units after 0xFFFFFFF0 wraps modulo 2^32 to 0x10.
	if wrapped.Timestamp != 0x10 {
		t.Fatalf("wrapped ts = %#x, want 0x10", wrapped.Timestamp)
	}
}

func TestPacketPionRoundTrip(t *testing.T) {
	m, _ := NewMapper(90000, VideoClockRate)
	pkz := NewPacketizer(m, 42, 96)
	// First tick sets the origin; the second at +90000 maps to RTP 90000.
	if _, err := pkz.Packet(clock.MustNew(0, 90000), nil, false); err != nil {
		t.Fatal(err)
	}
	pkt, err := pkz.Packet(clock.MustNew(90000, 90000), []byte{1, 2, 3}, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var decoded rtp.Packet
	if err := decoded.Unmarshal(raw); err != nil {
		t.Fatalf("pion unmarshal: %v", err)
	}
	if decoded.Version != 2 || decoded.Timestamp != 90000 ||
		len(decoded.Payload) != 3 {
		t.Fatalf("decoded header wrong: %+v", decoded.Header)
	}
}
