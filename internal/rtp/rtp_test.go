package rtp

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func mkPacket(ssrc uint32, seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: ts, SSRC: ssrc,
		},
		Payload: []byte{0xAA, byte(seq)},
	}
}

func mkSR(ssrc uint32, ntp uint64, rtpTS uint32) []byte {
	b := make([]byte, 28)
	b[0], b[1] = 0x80, 200  // V=2, no reception report blocks; PT=SR
	b[2], b[3] = 0x00, 0x06 // length 6 -> (6+1)*4 = 28-byte SR block
	putU32(b[4:], ssrc)
	putU32(b[8:], uint32(ntp>>32))
	putU32(b[12:], uint32(ntp))
	putU32(b[16:], rtpTS)
	return b
}

func putU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func TestParseRoundTrip(t *testing.T) {
	p := mkPacket(7, 42, 1000)
	buf, err := p.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	dg, err := ParseDatagram(0, buf)
	if err != nil {
		t.Fatal(err)
	}
	if dg.Kind != KindRTP || dg.Packet.SSRC != 7 || dg.Packet.SequenceNumber != 42 {
		t.Fatalf("bad parse: %+v", dg)
	}
	sr := mkSR(7, 0x0000000100000000, 1000)
	dg2, err := ParseDatagram(1, sr)
	if err != nil {
		t.Fatal(err)
	}
	if dg2.Kind != KindRTCP || len(dg2.Reports) != 1 || dg2.Reports[0].SSRC != 7 {
		t.Fatalf("bad SR parse: %+v", dg2)
	}
	if _, err := ParseDatagram(2, []byte{0x00}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("short datagram: %v", err)
	}
}

func TestLoss(t *testing.T) {
	r := NewReorderer(8)
	const ssrc uint32 = 11
	// seq 0,1 arrive; 2,3 lost; 4 arrives revealing the gap; 2 retransmitted.
	order := []uint16{0, 1, 4, 2}
	var emitted []uint16
	for _, seq := range order {
		got, err := r.Push(mkPacket(ssrc, seq, uint32(seq*160)))
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range got {
			emitted = append(emitted, op.Seq)
		}
	}
	// seq 4 stays in the reorder window until Flush closes the stream.
	rest, gaps := r.Flush()
	for _, op := range rest {
		emitted = append(emitted, op.Seq)
	}
	wantEmitted := []uint16{0, 1, 2, 4}
	if len(emitted) != len(wantEmitted) {
		t.Fatalf("emitted %v want %v", emitted, wantEmitted)
	}
	for i := range wantEmitted {
		if emitted[i] != wantEmitted[i] {
			t.Fatalf("emitted %v want %v", emitted, wantEmitted)
		}
	}
	// Re-pushing seq 4 on a fresh reorderer must be a no-op duplicate.
	r2 := NewReorderer(8)
	_, _ = r2.Push(mkPacket(ssrc, 4, 640))
	if got, err := r2.Push(mkPacket(ssrc, 4, 640)); err != nil || len(got) != 0 {
		t.Fatalf("duplicate not idempotent: %d %v", len(got), err)
	}
	if r2.Duplicates() != 1 {
		t.Fatalf("duplicate count: %d", r2.Duplicates())
	}
	if len(gaps) != 1 {
		t.Fatalf("gaps: %+v", gaps)
	}
	g := gaps[0]
	if g.FirstSeq != 2 || g.LastSeq != 3 || g.Count != 2 || g.SSRC != ssrc {
		t.Fatalf("gap fields: %+v", g)
	}
	if !g.Recovered {
		t.Fatal("retransmitted gap must be marked recovered")
	}
}

func TestUnrecoveredGap(t *testing.T) {
	r := NewReorderer(8)
	var pushed int
	for _, seq := range []uint16{0, 1, 5} {
		got, err := r.Push(mkPacket(9, seq, 0))
		if err != nil {
			t.Fatal(err)
		}
		pushed += len(got)
	}
	out, gaps := r.Flush()
	if pushed+len(out) != 3 || len(gaps) != 1 || gaps[0].Recovered {
		t.Fatalf("pushed=%d flushed=%d gaps=%+v", pushed, len(out), gaps)
	}
	if gaps[0].FirstSeq != 2 || gaps[0].LastSeq != 4 {
		t.Fatalf("gap range: %+v", gaps[0])
	}
}

func TestPermanentLossAdvancesWindow(t *testing.T) {
	// A gap wider than the window becomes permanent loss; later in-order
	// packets must keep flowing instead of stalling forever.
	r := NewReorderer(4)
	var emitted []uint16
	for _, seq := range []uint16{0, 10, 11} {
		got, err := r.Push(mkPacket(5, seq, 0))
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range got {
			emitted = append(emitted, op.Seq)
		}
	}
	want := []uint16{0, 10, 11}
	if len(emitted) != len(want) {
		t.Fatalf("emitted %v want %v", emitted, want)
	}
	_, gaps := r.Flush()
	if len(gaps) != 1 || gaps[0].Recovered || gaps[0].Count != 9 {
		t.Fatalf("gaps: %+v", gaps)
	}
}
