package transport

import (
	"errors"
	"testing"

	"pairflow/internal/model"
)

func TestJitterBufferReordersBeforeDeadline(t *testing.T) {
	jb := NewJitterBuffer(JitterConfig{BaseSequence: 100, StartNS: 0, PacketDurationNS: 10_000_000})
	p100 := BuildPacket(100, 9000, 1, 96, []byte("a"), false)
	p101 := BuildPacket(101, 9900, 1, 96, []byte("b"), false)
	if r, err := jb.Push(ObservedPacket{Packet: p101}); err != nil || r.Disposition != PushBuffered {
		t.Fatalf("101 disposition=%v err=%v", r, err)
	}
	if r, err := jb.Push(ObservedPacket{Packet: p100}); err != nil || r.Disposition != PushAccepted {
		t.Fatalf("100 disposition=%v err=%v", r, err)
	}
	out := jb.DrainUntil(5_000_000)
	if len(out) != 2 || out[0].Sequence() != 100 || out[1].Sequence() != 101 {
		t.Fatalf("order=%v", seqList(out))
	}
}

func TestJitterBufferDuplicateAndLate(t *testing.T) {
	jb := NewJitterBuffer(JitterConfig{BaseSequence: 200, StartNS: 0, PacketDurationNS: 10_000_000})
	p200 := BuildPacket(200, 1, 1, 96, []byte("x"), false)
	if _, err := jb.Push(ObservedPacket{Packet: p200}); err != nil {
		t.Fatal(err)
	}
	if r, _ := jb.Push(ObservedPacket{Packet: p200}); r.Disposition != PushDuplicate {
		t.Fatalf("duplicate disposition=%s", r.Disposition)
	}
	jb.DrainUntil(1_000_000_000) // deadlines for 201 and onward pass
	late := BuildPacket(201, 2, 1, 96, []byte("late"), false)
	r, err := jb.Push(ObservedPacket{Packet: late})
	if err == nil {
		t.Fatal("expected late diagnostic")
	}
	if r.Disposition != PushLate {
		t.Fatalf("disposition=%s want late", r.Disposition)
	}
	if len(jb.Late) == 0 {
		t.Fatal("late sequence not recorded")
	}
}

func TestSameSequenceDifferentContentIsConflict(t *testing.T) {
	jb := NewJitterBuffer(JitterConfig{BaseSequence: 300, PacketDurationNS: 10_000_000})
	p1 := BuildPacket(300, 1, 1, 96, []byte("one"), false)
	p2 := BuildPacket(300, 1, 1, 96, []byte("two"), false)
	if _, err := jb.Push(ObservedPacket{Packet: p1}); err != nil {
		t.Fatal(err)
	}
	_, err := jb.Push(ObservedPacket{Packet: p2})
	var me *model.MediaError
	if !errors.As(err, &me) || me.Code != model.CodeSeqConflict {
		t.Fatalf("err=%v want sequence conflict", err)
	}
	if len(jb.Conflicts) != 1 {
		t.Fatalf("conflicts=%d want 1", len(jb.Conflicts))
	}
}

func TestXORFECSingleRecoveryAndBurstFailure(t *testing.T) {
	seqs := []uint16{1, 2, 3}
	shards := [][]byte{[]byte("aaaa"), []byte("bbbb"), []byte("cccc")}
	block, err := NewFECBlock(0, seqs, shards, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, merr := block.RecoverXOR(map[uint16][]byte{1: shards[0], 3: shards[2], 4: block.Parity})
	if merr != nil {
		t.Fatal(merr)
	}
	if string(got) != "bbbb" {
		t.Fatalf("recovered=%q", got)
	}
	_, merr = block.RecoverXOR(map[uint16][]byte{1: shards[0], 4: block.Parity})
	if merr == nil || merr.Code != model.CodeBurstLoss {
		t.Fatalf("merr=%v want burst loss", merr)
	}
}

func seqList(in []ObservedPacket) []uint16 {
	out := make([]uint16, len(in))
	for i, p := range in {
		out[i] = p.Sequence()
	}
	return out
}
