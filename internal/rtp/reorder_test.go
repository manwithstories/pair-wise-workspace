package rtp_test

import (
	"testing"

	"github.com/pion/rtp"

	"medialab/internal/clock"
	"medialab/internal/model"
	mrtp "medialab/internal/rtp"
)

func testPacket(seq uint16, ts uint32, payload string, offset int64) *mrtp.InPacket {
	return &mrtp.InPacket{
		P:         rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts}, Payload: []byte(payload)},
		TrackID:   7,
		RawOffset: offset,
	}
}

func TestDuplicateIsIdempotentAndConflictIsIsolated(t *testing.T) {
	b := mrtp.NewReorderBuffer(8)
	original := testPacket(42, 1000, "same-payload", 128)
	if r := b.Push(original); len(r.Ready) != 1 {
		t.Fatalf("first push ready=%d", len(r.Ready))
	}

	dup := testPacket(42, 1000, "same-payload", 192)
	r := b.Push(dup)
	if r.Disposition != mrtp.DispositionDuplicate || len(r.Ready) != 0 {
		t.Fatalf("duplicate result=%+v", r)
	}
	if b.Stats.DuplicateDropped != 1 || b.Stats.DeliveredPackets != 1 {
		t.Fatalf("duplicate changed delivery state: %+v", b.Stats)
	}

	conflict := testPacket(42, 1000, "different-payload", 256)
	cr := b.Push(conflict)
	if cr.Disposition != mrtp.DispositionIsolated {
		t.Fatalf("conflict disposition=%s", cr.Disposition)
	}
	if got := model.FirstOffset(cr.Fault); got != 256 {
		t.Fatalf("conflict offset=%d, want 256", got)
	}
	if len(b.Quarantine) != 1 || b.Quarantine[0].FirstOffset != 128 {
		t.Fatalf("quarantine=%+v", b.Quarantine)
	}
}

func TestReorderDrainAcrossSequenceWrap(t *testing.T) {
	b := mrtp.NewReorderBuffer(16)
	pushOrder := []uint16{65530, 65533, 65532, 65531, 0, 65534, 65535}
	var got []uint16
	for i, seq := range pushOrder {
		r := b.Push(testPacket(seq, uint32(seq)*3000, "p", int64(i*64)))
		for _, p := range r.Ready {
			got = append(got, p.P.SequenceNumber)
		}
	}
	want := []uint16{65530, 65531, 65532, 65533, 65534, 65535, 0}
	if len(got) != len(want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order=%v, want %v", got, want)
		}
	}
	if b.Stats.ReorderedPackets != 3 {
		t.Fatalf("reordered=%d, want 3", b.Stats.ReorderedPackets)
	}
}

func TestTimestampWrapUsesRationalClockDelta(t *testing.T) {
	if d := clock.DeltaUint32(^uint32(0), 0); d != 1 {
		t.Fatalf("wrap delta=%d, want 1", d)
	}
	u := &mrtp.TimestampUnwrapper{}
	stamps := []uint32{^uint32(0) - 2, ^uint32(0), 1}
	first := u.Unwrap(stamps[0])
	u.Unwrap(stamps[1])
	last := u.Unwrap(stamps[2])
	if last <= first {
		t.Fatalf("unwrapped timestamps not monotonic: %d -> %d", first, last)
	}
}

func TestReorderWindowIsBounded(t *testing.T) {
	b := mrtp.NewReorderBuffer(2)
	b.Push(testPacket(10, 0, "a", 0))
	r := b.Push(testPacket(14, 0, "far", 64)) // delta 4 > window 2
	if r.Disposition != mrtp.DispositionOverflow {
		t.Fatalf("disposition=%s, want overflow", r.Disposition)
	}
	if b.Stats.OverflowDropped != 1 || len(b.Quarantine) != 0 {
		t.Fatalf("stats=%+v quarantine=%d", b.Stats, len(b.Quarantine))
	}
}

func TestParsePacketRoundTripWithPion(t *testing.T) {
	p := testPacket(77, 90000, "payload", 512)
	raw, err := p.P.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mrtp.ParsePacket(raw, 512, 7)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if parsed.P.SequenceNumber != 77 || string(parsed.P.Payload) != "payload" || parsed.RawOffset != 512 {
		t.Fatalf("parsed=%+v", parsed)
	}
}
