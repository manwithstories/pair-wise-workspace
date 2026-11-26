package pairflow_test

import (
	"testing"

	"pairflow/internal/transport"
)

func TestJitterFileContract(t *testing.T) {
	jb := transport.NewJitterBuffer(transport.JitterConfig{BaseSequence: 10, PacketDurationNS: 10_000_000})
	late := transport.BuildPacket(11, 100, 1, 96, []byte("x"), false)
	if _, err := jb.Push(transport.ObservedPacket{Packet: late}); err != nil {
		t.Fatal(err)
	}
	out := jb.DrainUntil(100_000_000)
	if len(out) != 1 {
		t.Fatalf("released=%d want buffered packet after gap deadline", len(out))
	}

	seqs := []uint16{20, 21}
	shards := [][]byte{[]byte("aa"), []byte("bb")}
	block, err := transport.NewFECBlock(0, seqs, shards, 22)
	if err != nil {
		t.Fatal(err)
	}
	got, merr := block.RecoverXOR(map[uint16][]byte{20: shards[0], 22: block.Parity})
	if merr != nil || string(got) != "bb" {
		t.Fatalf("FEC got=%q err=%v", got, merr)
	}
}
