package media

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func TestRTPClockConvertsExactTimes(t *testing.T) {
	c, err := NewRTPClock(90000)
	if err != nil {
		t.Fatal(err)
	}
	// 1/30 s at 90 kHz is exactly 3000 ticks; no floating-point drift.
	if got := c.Timestamp(NewRat(1, 30)); got != 3000 {
		t.Fatalf("timestamp=%d want 3000", got)
	}
	if got := c.Timestamp(NewRat(2, 30)); got != 6000 {
		t.Fatalf("timestamp=%d want 6000", got)
	}
	if _, err := NewRTPClock(0); !errors.Is(err, ErrUsage) {
		t.Fatalf("zero rate should be a usage error, got %v", err)
	}
}

func TestRTPClockExtensionDetectsAmbiguity(t *testing.T) {
	c, err := NewRTPClock(90000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Extend(0xFFFFFF00, 0); err != nil {
		t.Fatal(err)
	}
	// A forward step of 0x200 wraps the 32-bit space.
	v, err := c.Extend(0x00000100, 16)
	if err != nil {
		t.Fatal(err)
	}
	if v <= 1<<31 {
		t.Fatalf("wrapped value %d was not expanded", v)
	}
	// Exactly half the range forward from the previous value is ambiguous.
	half := uint32(0x00000100 + 0x80000000)
	_, ambErr := c.Extend(half, 32)
	if !errors.Is(ambErr, ErrTimestampAmbiguous) {
		t.Fatalf("half-range step should be ambiguous, got %v", ambErr)
	}
	if off := OffsetOf(ambErr); off != 32 {
		t.Fatalf("offset=%d want 32", off)
	}
}

func TestParseRTPPacketsUsesPionBoundaries(t *testing.T) {
	var packets [][]byte
	var concatenated []byte
	for i := 0; i < 3; i++ {
		p := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96, SequenceNumber: uint16(i), Timestamp: uint32(i * 90),
				SSRC: 7, Marker: i == 2,
			},
			Payload: []byte{0xde, 0xad, 0xbe, 0xef},
		}
		if err := p.SetExtension(1, []byte{1, 2, 3, 4}); err != nil {
			t.Fatal(err)
		}
		b, err := p.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, b)
		concatenated = append(concatenated, b...)
	}
	bounds, err := ParseRTPPackets(packets)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounds) != 3 {
		t.Fatalf("packets=%d", len(bounds))
	}
	total := 0
	for i, b := range bounds {
		total += b.Length
		if int(b.Header.SequenceNumber) != i {
			t.Fatalf("packet %d sequence=%d", i, b.Header.SequenceNumber)
		}
		if len(b.Payload) != 4 {
			t.Fatalf("packet %d payload=%d", i, len(b.Payload))
		}
	}
	if total != len(concatenated) {
		t.Fatalf("boundaries cover %d of %d bytes", total, len(concatenated))
	}

	// Each datagram's boundary is exactly its own extent.
	one, err := ParseRTPDatagram(packets[1])
	if err != nil {
		t.Fatal(err)
	}
	if one.Length != len(packets[1]) {
		t.Fatalf("single packet length=%d want %d", one.Length, len(packets[1]))
	}

	// A truncated packet is a length error at its own offset.
	_, truncErr := ParseRTPPackets([][]byte{concatenated[:8]})
	if !errors.Is(truncErr, ErrInvalidLength) {
		t.Fatalf("class=%s err=%v", ClassOf(truncErr), truncErr)
	}
	if off := OffsetOf(truncErr); off != 0 {
		t.Fatalf("offset=%d want 0", off)
	}
}

func TestRTPClockSummarizeCountsWraps(t *testing.T) {
	idx := make(SliceIndex, 4)
	base := 0xFFFFFFF0
	for i := range idx {
		idx[i] = Sample{
			Seq:      uint32(i),
			PTS:      NewRat(int64(uint32(base+i*16)), 90000),
			Offset:   int64(i * 10),
			Duration: NewRat(1, 90000),
		}
	}
	c, err := NewRTPClock(90000)
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Summarize(idx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Wraps != 1 {
		t.Fatalf("wraps=%d want 1 (info=%s)", info.Wraps, info)
	}
	if info.Samples != 4 {
		t.Fatalf("samples=%d", info.Samples)
	}
}
