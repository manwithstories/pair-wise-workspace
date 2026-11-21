package rtp

import (
	"testing"

	"mediacheck/internal/model"
)

func TestCleanStreamReassemblesFully(t *testing.T) {
	s := NewStream(1234, 90000, 3000)
	pkts := s.BuildPackets(50)
	out, st, err := Reassemble(ApplyDamage(pkts, LossConfig{}), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 50 || st.Unrecoverable != 0 || st.RecoveryPercent() != 100 {
		t.Fatalf("clean stream stats wrong: out=%d stats=%+v", len(out), st)
	}
	for i, p := range out {
		if p.Seq != uint16(i) {
			t.Fatalf("not in sequence order at %d: %d", i, p.Seq)
		}
	}
}

func TestReorderAndDuplicateRecovered(t *testing.T) {
	s := NewStream(1, 90000, 3000)
	pkts := s.BuildPackets(30)
	cfg := LossConfig{ReorderAt: 10, DuplicateAt: 20}
	delivered := ApplyDamage(pkts, cfg)
	out, st, err := Reassemble(delivered, 30)
	if err != nil {
		t.Fatal(err)
	}
	if st.Reordered == 0 {
		t.Fatal("reorder not detected")
	}
	if st.Duplicate != 1 {
		t.Fatalf("duplicate count = %d, want 1", st.Duplicate)
	}
	if st.Unrecoverable != 0 || len(out) != 30 {
		t.Fatalf("reorder/duplicate must be fully recoverable: %+v", st)
	}
}

func TestBurstLossRecoveryRate(t *testing.T) {
	s := NewStream(7, 90000, 3000)
	const total = 100
	pkts := s.BuildPackets(total)
	cfg := LossConfig{BurstAt: 40, BurstLen: 12, ReorderAt: 5, DuplicateAt: 8}
	delivered := ApplyDamage(pkts, cfg)
	out, st, err := Reassemble(delivered, total)
	if err != nil {
		t.Fatal(err)
	}
	if st.Unrecoverable != 12 {
		t.Fatalf("unrecoverable = %d, want 12", st.Unrecoverable)
	}
	if len(out) != total-12 {
		t.Fatalf("reassembled = %d, want %d", len(out), total-12)
	}
	// Acceptance gate: burst of 12 among 100 must stay at/above 85%.
	if st.RecoveryPercent() < 85 {
		t.Fatalf("recovery = %.2f%%, want >= 85", st.RecoveryPercent())
	}
	if len(st.UnrecoverableRanges) != 1 {
		t.Fatalf("ranges = %d, want one coalesced burst range", len(st.UnrecoverableRanges))
	}
	r := st.UnrecoverableRanges[0]
	if r.Start != 40 || r.End != 52 {
		t.Fatalf("range = [%d,%d), want [40,52)", r.Start, r.End)
	}
}

func TestUnrecoverableSharesModelCode(t *testing.T) {
	s := NewStream(9, 90000, 3000)
	pkts := s.BuildPackets(20)
	delivered := ApplyDamage(pkts, LossConfig{BurstAt: 0, BurstLen: 3})
	_, st, err := Reassemble(delivered, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range st.UnrecoverableRanges {
		me := model.NewError(model.CodeRTPUnrecoverable, "rtp", r.Start, r.End, "burst")
		if !me.Code.Recoverable() {
			t.Fatal("rtp unrecoverable code must be in the shared recoverable vocabulary")
		}
	}
	if st.Unrecoverable != 3 {
		t.Fatalf("unrecoverable = %d, want 3", st.Unrecoverable)
	}
}

func TestPionMarshalRoundTrip(t *testing.T) {
	s := NewStream(55, 90000, 3000)
	pkts := s.BuildPackets(8)
	wire, err := Marshal(pkts)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != 8 {
		t.Fatalf("wire packets = %d", len(wire))
	}
	parsed, err := Unmarshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range parsed {
		if p.SequenceNumber != uint16(i) || p.SSRC != 55 {
			t.Fatalf("header mismatch at %d: seq=%d ssrc=%d", i, p.SequenceNumber, p.SSRC)
		}
		if string(p.Payload) != string(pkts[i].Payload) {
			t.Fatalf("payload mismatch at %d", i)
		}
	}
}

func TestCorruptPayloadRejected(t *testing.T) {
	s := NewStream(3, 90000, 3000)
	pkts := s.BuildPackets(4)
	pkts[2].Payload[3] ^= 0xFF
	if _, _, err := Reassemble(pkts, 4); err == nil {
		t.Fatal("corrupted payload must fail independent verification")
	}
}
