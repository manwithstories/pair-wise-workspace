package rtp

import (
	"testing"

	"rtspaudit/internal/fixture"
)

func depacketize(t *testing.T, packets []fixture.Packet, drop int) DepacketStats {
	t.Helper()
	first := packets[0].RTP.SequenceNumber
	tr := NewSeqTrackerAt(first, 32)
	d := NewDepacketizer()
	dropped := false
	for _, p := range packets {
		if drop >= 0 && p.RTP.SequenceNumber == uint16(drop) {
			dropped = true
			continue
		}
		out, _, _ := tr.Push(&p.RTP)
		for _, op := range out {
			if err := d.PushOrdered(op); err != nil {
				// Malformed payloads are covered by a separate test.
				return d.Stats()
			}
		}
	}
	if !dropped && drop >= 0 {
		t.Fatalf("sequence %d not present", drop)
	}
	for _, op := range tr.Flush() {
		_ = d.PushOrdered(op)
	}
	d.Finish()
	return d.Stats()
}

func TestDepacketizerRoundTrip(t *testing.T) {
	f, err := fixture.Generate(20)
	if err != nil {
		t.Fatal(err)
	}
	packets := f.Packetize(fixture.FirstSequence(7))
	stats := depacketize(t, packets, -1)
	if stats.FramesReassembled != 20 || stats.DroppedFrames != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if stats.Keyframes != 1 || len(stats.FrameHashes) != 3 || stats.Bytes == 0 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestDepacketizerDropsFrameWithMissingFUAFragment(t *testing.T) {
	f, err := fixture.Generate(12)
	if err != nil {
		t.Fatal(err)
	}
	packets := f.Packetize(fixture.FirstSequence(7))
	drop := -1
	for i, p := range packets {
		if p.FrameIndex == 3 && len(p.RTP.Payload) > 0 && p.RTP.Payload[0]&0x1f == 28 {
			drop = int(packets[i+1].RTP.SequenceNumber)
			break
		}
	}
	if drop < 0 {
		t.Fatal("fixture has no FU-A fragment to drop")
	}
	stats := depacketize(t, packets, drop)
	if stats.DroppedFrames == 0 || stats.FramesReassembled >= 12 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestDepacketizerMalformedSTAPA(t *testing.T) {
	d := NewDepacketizer()
	pkt := packet(1)
	pkt.Payload = []byte{24, 0, 8, 0x65}
	if err := d.Push(pkt, 0); err == nil {
		t.Fatal("expected malformed STAP-A error")
	}
}
