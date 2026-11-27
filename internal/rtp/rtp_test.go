package rtp

import (
	"testing"

	"github.com/pion/rtp"

	"archive/internal/model"
)

func makeDatagram(t *testing.T, seq uint16, ts uint32, payload []byte) []byte {
	t.Helper()
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: 42},
		Payload: payload,
	}
	buf, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return buf
}

// payloadWithCRC builds a media payload and appends its CRC-32 trailer.
func payloadWithCRC(body []byte) []byte {
	out := make([]byte, 0, len(body)+TrailerLen)
	out = append(out, body...)
	return append(out, MakeCRCTrailer(body)...)
}

func TestParserUnwrapAndCRC(t *testing.T) {
	log := model.NewFaultLog()
	p := NewParser(90000, log)
	// Sequence numbers straddle the 16-bit wrap; timestamps advance 3000
	// ticks each packet (90 kHz -> 1/30 s between frames).
	seqs := []uint16{65534, 65535, 0, 1}
	var prev int64
	for i, s := range seqs {
		pkt, err := p.Parse(makeDatagram(t, s, uint32(i*3000), payloadWithCRC([]byte{byte(i)})))
		if err != nil {
			t.Fatalf("parse seq %d: %v", s, err)
		}
		if i > 0 && pkt.ExtSeq != prev+1 {
			t.Fatalf("wrap broken: %d -> %d", prev, pkt.ExtSeq)
		}
		prev = pkt.ExtSeq
		if !CheckCRC(pkt.Payload) {
			t.Fatal("valid payload failed CRC")
		}
	}
	// A flipped CRC trailer byte must fail validation.
	corrupt := payloadWithCRC([]byte{9})
	corrupt[len(corrupt)-1] ^= 0xFF
	if CheckCRC(corrupt) {
		t.Fatal("corrupt payload unexpectedly passed CRC")
	}
}

func TestParserRejectsOversize(t *testing.T) {
	log := model.NewFaultLog()
	p := NewParser(90000, log)
	huge := make([]byte, model.MaxPacketBytes+1)
	if _, err := p.Parse(huge); err == nil {
		t.Fatal("oversize datagram must be rejected")
	}
	if log.Count(model.FaultOversize) == 0 {
		t.Fatal("oversize fault not recorded")
	}
}

func TestJitterDedupReorderAndLate(t *testing.T) {
	log := model.NewFaultLog()
	jb := NewJitterBuffer(32, log)
	mk := func(seq int64) *MediaPacket { return &MediaPacket{ExtSeq: seq, Seq: uint16(seq)} }

	for _, s := range []int64{3, 1, 2, 2, 5, 4} { // dup on 2, mild reorder
		jb.Push(mk(s))
	}
	ordered, missing := jb.Drain()
	want := []int64{1, 2, 3, 4, 5}
	if len(ordered) != len(want) {
		t.Fatalf("ordered len = %d, want %d (missing=%v)", len(ordered), len(want), missing)
	}
	for i, p := range ordered {
		if p.ExtSeq != want[i] {
			t.Fatalf("order wrong at %d: got %d want %d", i, p.ExtSeq, want[i])
		}
	}
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	if log.Count(model.FaultDuplicatePacket) != 1 {
		t.Fatal("duplicate not recorded")
	}

	// Advance the floor, then deliver a packet outside the window.
	jb.AdvanceFloor(4)
	if jb.Push(mk(2)) {
		t.Fatal("late packet outside window must be rejected")
	}
	if log.Count(model.FaultLatePacket) != 1 {
		t.Fatal("late-packet fault not recorded")
	}
}

func TestJitterReportsMissing(t *testing.T) {
	log := model.NewFaultLog()
	jb := NewJitterBuffer(32, log)
	mk := func(seq int64) *MediaPacket { return &MediaPacket{ExtSeq: seq, Seq: uint16(seq)} }
	for _, s := range []int64{10, 11, 14} { // 12 and 13 lost
		jb.Push(mk(s))
	}
	_, missing := jb.Drain()
	if len(missing) != 2 || missing[0] != 12 || missing[1] != 13 {
		t.Fatalf("missing detection wrong: %v", missing)
	}
}

func TestRepairRetransmission(t *testing.T) {
	log := model.NewFaultLog()
	r := NewRepairer(log)
	orig := &MediaPacket{ExtSeq: 7, Payload: payloadWithCRC([]byte{1, 2, 3})}
	r.Cache().Store(orig)
	got := r.Try(7, map[int64]*MediaPacket{})
	if got == nil || !got.Retrans {
		t.Fatal("retransmission miss")
	}
	if !CheckCRC(got.Packet.Payload) || got.Packet.ExtSeq != 7 {
		t.Fatal("retransmitted packet corrupt")
	}
	if log.Count(model.FaultRetransmitted) != 1 {
		t.Fatal("retransmission fault not recorded")
	}
	// Mutating the returned copy must not poison the cache entry.
	got.Packet.Payload[0] ^= 0xFF
	if r.Cache().store[7].Payload[0] != 1 {
		t.Fatal("cache entry aliased by returned copy")
	}
}

func TestRepairParityAndGap(t *testing.T) {
	log := model.NewFaultLog()
	r := NewRepairer(log)
	p1 := payloadWithCRC([]byte{0x10, 0x20})
	p2 := payloadWithCRC([]byte{0x30, 0x40})
	p3 := payloadWithCRC([]byte{0x55, 0xAA})
	parity, ok := XORPayload([][]byte{p1, p2, p3})
	if !ok {
		t.Fatal("parity build failed")
	}
	r.AddGroup(&ParityGroup{Members: []int64{1, 2, 3}, ParitySeq: 100, payload: parity})
	present := map[int64]*MediaPacket{
		1: {ExtSeq: 1, Payload: p1},
		2: {ExtSeq: 2, Payload: p2},
	}
	got := r.Try(3, present)
	if got == nil || !got.Parity || !got.Packet.Recovered {
		t.Fatal("single-loss parity recovery failed")
	}
	if !EqualPayload(got.Packet.Payload, p3) || !CheckCRC(got.Packet.Payload) {
		t.Fatal("rebuilt payload does not match original / fails CRC")
	}
	if log.Count(model.FaultFECRecovered) != 1 {
		t.Fatal("FEC fault not recorded")
	}

	// Two members missing: parity cannot rebuild, Try returns nil and the
	// unrecoverable range becomes an explicit gap (no silent splice).
	if got := r.Try(2, map[int64]*MediaPacket{1: {ExtSeq: 1, Payload: p1}}); got != nil {
		t.Fatal("parity must not rebuild with two missing members")
	}
	gap := r.DeclareGap("seg-2", 2, 3, 40*model.Millisecond, 80*model.Millisecond, model.FaultUnrecoverable)
	if gap.Length() != 2 || len(log.Gaps()) != 1 {
		t.Fatal("explicit gap not declared")
	}
	if log.Count(model.FaultUnrecoverable) != 1 {
		t.Fatal("unrecoverable fault not recorded")
	}
}
