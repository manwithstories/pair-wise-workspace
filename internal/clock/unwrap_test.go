package clock

import (
	"errors"
	"testing"
)

func TestUnwrapCleanWrap(t *testing.T) {
	// Stream that crosses the 32-bit boundary by one frame's worth of delta.
	const step uint32 = 3000
	pkts := make([]Packet, 5)
	ts := uint32(0xFFFFF000)
	for i := range pkts {
		pkts[i] = Packet{Seq: uint16(i), TS: ts}
		ts += step
	}
	res, err := UnwrapTimestamps(pkts, UnwrapConfig{ClockRate: 90_000})
	if err != nil {
		t.Fatal(err)
	}
	// No ambiguity: every delta is small and forward.
	if len(res.Ambiguities) != 0 {
		t.Fatalf("unexpected ambiguities: %+v", res.Ambiguities)
	}
	for i, pt := range res.Packets {
		wantTSU := uint64(0xFFFFF000) + uint64(i)*uint64(step)
		if pt.TSUnwrapped != wantTSU {
			t.Fatalf("pkt %d tsu=%d want %d", i, pt.TSUnwrapped, wantTSU)
		}
		if pt.SeqWrapped != int64(i) {
			t.Fatalf("pkt %d seqU=%d want %d", i, pt.SeqWrapped, i)
		}
		// 3000 units @90k == 33333us-ish; verify monotonic and exact.
		if i > 0 && pt.Ticks <= res.Packets[i-1].Ticks {
			t.Fatalf("ticks not monotonic at %d", i)
		}
	}
	// First packet's ticks equal its raw timestamp mapped through the rate.
	first, _ := Rate(90_000)
	want0, _ := first.Scale(int64(0xFFFFF000))
	if res.Packets[0].Ticks != want0 {
		t.Fatalf("tick0=%d want %d", res.Packets[0].Ticks, want0)
	}
}

func TestUnwrapOutOfOrderNotWrap(t *testing.T) {
	// Arrival order: 10, 11, 12 then a late packet 9 (duplicate-ish reorder),
	// then 13. Timestamps move forward in send order; the late packet must be
	// placed correctly rather than counted as a backward wrap.
	pkts := []Packet{
		{Seq: 10, TS: 1000},
		{Seq: 11, TS: 2000},
		{Seq: 12, TS: 3000},
		{Seq: 9, TS: 0}, // late, out of order
		{Seq: 13, TS: 4000},
	}
	res, err := UnwrapTimestamps(pkts, UnwrapConfig{ClockRate: 90_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ambiguities) != 0 {
		t.Fatalf("reorder must not be ambiguous: %+v", res.Ambiguities)
	}
	// Arrival-order output keeps original indices.
	wantTSU := map[int]uint64{0: 1000, 1: 2000, 2: 3000, 3: 0, 4: 4000}
	for i, pt := range res.Packets {
		if pt.TSUnwrapped != wantTSU[i] {
			t.Fatalf("arrival %d tsu=%d want %d", i, pt.TSUnwrapped, wantTSU[i])
		}
	}
	// Sequence extension must remain based on arrival order (16-bit safe here).
	if res.Packets[3].SeqWrapped != 9 || res.Packets[4].SeqWrapped != 13 {
		t.Fatalf("seq extension wrong: %+v", res.Packets)
	}
}

func TestUnwrapSequenceWrap(t *testing.T) {
	// Sequence numbers wrap from 65535 to 0.
	pkts := []Packet{
		{Seq: 65534, TS: 100},
		{Seq: 65535, TS: 200},
		{Seq: 0, TS: 300},
		{Seq: 1, TS: 400},
	}
	res, err := UnwrapTimestamps(pkts, UnwrapConfig{ClockRate: 90_000})
	if err != nil {
		t.Fatal(err)
	}
	wantSeq := []int64{65534, 65535, 65536, 65537}
	for i, pt := range res.Packets {
		if pt.SeqWrapped != wantSeq[i] {
			t.Fatalf("pkt %d seqU=%d want %d", i, pt.SeqWrapped, wantSeq[i])
		}
	}
}

func TestUnwrapAmbiguousJump(t *testing.T) {
	// A jump of exactly 2^31 is not uniquely decidable.
	pkts := []Packet{
		{Seq: 1, TS: 0},
		{Seq: 2, TS: uint32(1) << 31},
		{Seq: 3, TS: uint32(1)<<31 + 100},
	}
	res, err := UnwrapTimestamps(pkts, UnwrapConfig{ClockRate: 90_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ambiguities) != 1 {
		t.Fatalf("want 1 ambiguity span, got %+v", res.Ambiguities)
	}
	if !res.Packets[1].Ambiguous {
		t.Fatalf("jump packet must be marked ambiguous")
	}
	if res.Ambiguities[0].SeqStart != 2 || res.Ambiguities[0].SeqEnd != 3 {
		t.Fatalf("span seqs wrong: %+v", res.Ambiguities[0])
	}
}

func TestUnwrapEmptyAndBadClock(t *testing.T) {
	if _, err := UnwrapTimestamps(nil, UnwrapConfig{ClockRate: 90_000}); !errors.Is(err, ErrEmptyStream) {
		t.Fatalf("err=%v", err)
	}
	if _, err := UnwrapTimestamps([]Packet{{Seq: 1, TS: 1}},
		UnwrapConfig{ClockRate: 0}); !errors.Is(err, ErrInvalidTimescale) {
		t.Fatalf("err=%v", err)
	}
}

func TestUnwrapReorderedWrap(t *testing.T) {
	// Wrap happens near a reorder boundary: arrival 65535, 0(wrap), then a
	// late 65534, then 1. Resolution must follow send order.
	pkts := []Packet{
		{Seq: 65535, TS: 0xFFFFFFF0},
		{Seq: 0, TS: 0x00000010}, // +0x20 across the boundary
		{Seq: 65534, TS: 0xFFFFFFE0},
		{Seq: 1, TS: 0x00000030},
	}
	res, err := UnwrapTimestamps(pkts, UnwrapConfig{ClockRate: 90_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ambiguities) != 0 {
		t.Fatalf("unexpected ambiguity: %+v", res.Ambiguities)
	}
	// Send order is 65534, 65535, 65536, 65537 with linear TS 0xE0..0x30
	// extended across the boundary.
	want := map[int]uint64{
		0: 0xFFFFFFF0,
		1: 0x100000010,
		2: 0xFFFFFFE0,
		3: 0x100000030,
	}
	for i, pt := range res.Packets {
		if pt.TSUnwrapped != want[i] {
			t.Fatalf("arrival %d tsu=%#x want %#x", i, pt.TSUnwrapped, want[i])
		}
	}
}
