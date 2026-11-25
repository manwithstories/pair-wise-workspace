package fieldpost_test

import (
	"context"
	"testing"

	"github.com/pion/rtp"

	"fieldpost/fixture"
	"fieldpost/transport"
)

func TestRootTransportReassemblesFixtureAcrossWrap(t *testing.T) {
	seg := fixture.MediaSegment(42, "roving", 3)
	// A small deterministic container stand-in is enough for transport.
	wire := make([]byte, 4096)
	for i := range wire {
		wire[i] = byte(i * 7)
	}
	packets := fixture.RTPPacketize(seg.ID, 65530, wire, 191)
	if len(packets) < 20 {
		t.Fatalf("packets=%d", len(packets))
	}
	bySeq := map[uint16]bool{}
	var in []*rtp.Packet
	for i, p := range packets {
		bySeq[p.SequenceNumber] = true
		if i == len(packets)/2 {
			continue // recovered by local retry
		}
		in = append(in, p)
		if i == 2 {
			in = append(in, p)
		}
	}
	fetch := func(_ context.Context, seq uint16) (*rtp.Packet, error) {
		for _, p := range packets {
			if p.SequenceNumber == seq {
				return p, nil
			}
		}
		return nil, context.DeadlineExceeded
	}
	chunks, rep, err := transport.RecoverSegment(context.Background(), seg.ID, 65530,
		len(packets), in, fetch, 128)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Duplicates == 0 || rep.Retries != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if got := fixture.ReassembleRTP(chunks); string(got) != string(wire) {
		t.Fatal("reassembled wire mismatch")
	}
}
