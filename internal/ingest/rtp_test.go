package ingest

import (
	"errors"
	"testing"

	"github.com/pion/rtp"

	"github.com/pairwise/mediad/internal/media"
)

func mkPacket(seq uint16, ts uint32, payload string) Packet {
	return Packet{Sequence: seq, Timestamp: ts, Payload: []byte(payload)}
}

func TestDeduplicateSameSequenceSamePayload(t *testing.T) {
	s := NewStream(16)
	p := mkPacket(7, 7000, "frame-a")
	if err := s.Accept(p); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(p); err != nil {
		t.Fatal(err)
	}
	r := s.Flush()
	if r.Duplicates != 1 || len(r.Packets) != 1 {
		t.Fatalf("dup=%d packets=%d", r.Duplicates, len(r.Packets))
	}
}

func TestConflictSameSequenceDifferentPayload(t *testing.T) {
	s := NewStream(16)
	if err := s.Accept(mkPacket(8, 8000, "same-seq")); err != nil {
		t.Fatal(err)
	}
	err := s.Accept(mkPacket(8, 8000, "different"))
	var me *media.Error
	if !errors.As(err, &me) || me.Code != media.CodePacketConflict {
		t.Fatalf("err=%v", err)
	}
	r := s.Flush()
	if len(r.Conflicts) != 1 || len(r.Packets) != 1 || string(r.Packets[0].Payload) != "same-seq" {
		t.Fatalf("report=%+v", r)
	}
}

func TestReorderSequenceWrapAndBurstGap(t *testing.T) {
	s := NewStream(64)
	// Start near wrap and deliberately arrive out of order.
	for _, seq := range []uint16{65534, 1, 0, 65535, 4} {
		if err := s.Accept(mkPacket(seq, uint32(seq)*10, string(rune(seq)))); err != nil {
			t.Fatal(err)
		}
	}
	r := s.Flush()
	wantSeqs := []uint64{65534, 65535, 65536, 65537, 65540}
	if len(r.Packets) != len(wantSeqs) {
		t.Fatalf("packets=%d", len(r.Packets))
	}
	for i, want := range wantSeqs {
		if r.Packets[i].UnwrappedSeq != want {
			t.Fatalf("packet %d seq=%d want %d", i, r.Packets[i].UnwrappedSeq, want)
		}
	}
	// Missing 65537,65538 form one two-packet burst; 65540? no because last=65539.
	if len(r.Gaps) != 1 || r.Gaps[0].Count != 2 || r.BurstLosses != 1 {
		t.Fatalf("gaps=%+v burst=%d", r.Gaps, r.BurstLosses)
	}
}

func TestRetransmissionFillsGap(t *testing.T) {
	s := NewStream(16)
	_ = s.Accept(mkPacket(1, 1000, "a"))
	_ = s.Accept(mkPacket(3, 3000, "c"))
	missing := mkPacket(2, 2000, "b")
	missing.UnwrappedSeq = 2
	if err := s.Retransmit(missing); err != nil {
		t.Fatal(err)
	}
	r := s.Flush()
	if len(r.Missing) != 0 || r.Recovered != 1 {
		t.Fatalf("missing=%v recovered=%d", r.Missing, r.Recovered)
	}
}

func TestPionRoundTripAndMalformedRaw(t *testing.T) {
	src := (&rtp.Packet{Header: rtp.Header{SequenceNumber: 99, Timestamp: 9000, SSRC: 42, Marker: true}, Payload: []byte("fmp4")})
	raw, err := src.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRTP(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStream(8)
	if err := s.AcceptPion(parsed); err != nil {
		t.Fatal(err)
	}
	out, err := MarshalRTP(s.Flush().Packets[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("empty RTP output")
	}
	if _, err := ParseRTP([]byte{0x80, 0x01}); media.ExitCode(err) != 2 {
		t.Fatalf("malformed exit=%d err=%v", media.ExitCode(err), err)
	}
}
