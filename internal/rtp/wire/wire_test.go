package wire

import (
	"testing"

	"github.com/pion/rtp"
)

func TestExtenderHandlesWrap(t *testing.T) {
	e := &Extender{}
	got := []uint64{
		e.Extend(65530), e.Extend(65535), e.Extend(0), e.Extend(5),
	}
	want := []uint64{65530, 65535, 65536, 65541}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %d want %d", i, got[i], want[i])
		}
	}
	if e.WrapCount != 1 {
		t.Fatalf("wrap count = %d", e.WrapCount)
	}
}

func TestExtenderKeepsReorderInEpoch(t *testing.T) {
	e := &Extender{}
	e.Extend(10)
	if got := e.Extend(8); got != 8 {
		t.Fatalf("reordered packet changed epoch: %d", got)
	}
	if got := e.Extend(9); got != 9 {
		t.Fatalf("got %d", got)
	}
}

func TestParsePacketUsesWireHeader(t *testing.T) {
	in := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, SequenceNumber: 321, Timestamp: 9000, SSRC: 0xdeadbeef,
		},
		Payload: []byte{1, 2, 3},
	}
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	seq, ts, ssrc, err := ParsePacket(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if seq != 321 || ts != 9000 || ssrc != 0xdeadbeef {
		t.Fatalf("header decoded wrong: %d %d %x", seq, ts, ssrc)
	}
}

func TestParsePacketRejectsGarbage(t *testing.T) {
	if _, _, _, err := ParsePacket([]byte{0x00}); err == nil {
		t.Fatalf("garbage datagram accepted")
	}
}

func TestStampToNS(t *testing.T) {
	ns, err := StampToNS(90_000, 90_000)
	if err != nil || ns != 1_000_000_000 {
		t.Fatalf("ns=%d err=%v", ns, err)
	}
	if _, err := StampToNS(1, 0); err == nil {
		t.Fatalf("zero clock rate accepted")
	}
}
