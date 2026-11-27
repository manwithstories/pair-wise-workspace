package packet

import (
	"bytes"
	"math/rand"
	"testing"

	"review/internal/fault"
)

func payloads(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = bytes.Repeat([]byte{byte(i + 1)}, 8+i*3) // 不等长
	}
	return out
}

func shuffled(pkts []Packet, seed int64) []Packet {
	out := make([]Packet, len(pkts))
	copy(out, pkts)
	rand.New(rand.NewSource(seed)).Shuffle(len(out), func(i, j int) {
		out[i], out[j] = out[j], out[i]
	})
	return out
}

func TestReorder(t *testing.T) {
	pkts := shuffled(MakeGroup(0, payloads(4)), 42)
	got := Reorder(pkts)
	for i, p := range got {
		if p.Seq != i {
			t.Fatalf("pos %d has seq %d", i, p.Seq)
		}
	}
}

func TestRecoverNoLoss(t *testing.T) {
	src := payloads(4)
	res, errs := Recover(shuffled(MakeGroup(0, src), 7))
	if len(errs) != 0 {
		t.Fatalf("faults: %v", errs)
	}
	if res.Lost != 0 || res.Recovered != 0 {
		t.Fatalf("lost=%d recovered=%d", res.Lost, res.Recovered)
	}
	for i, p := range res.Payloads {
		if !bytes.Equal(p, src[i]) {
			t.Fatalf("payload %d mismatch", i)
		}
	}
}

func TestRecoverSingleLoss(t *testing.T) {
	src := payloads(4)
	pkts := MakeGroup(0, src)
	// 丢弃 seq=1 的数据包并打乱
	kept := append([]Packet{}, pkts[:1]...)
	kept = append(kept, pkts[2:]...)
	res, errs := Recover(shuffled(kept, 1))
	if len(errs) != 0 {
		t.Fatalf("faults: %v", errs)
	}
	if res.Lost != 1 || res.Recovered != 1 {
		t.Fatalf("lost=%d recovered=%d", res.Lost, res.Recovered)
	}
	for i, p := range res.Payloads {
		if !bytes.Equal(p, src[i]) {
			t.Fatalf("payload %d mismatch after recovery", i)
		}
	}
}

func TestNoFalseRecoveryOnCorruptParity(t *testing.T) {
	src := payloads(4)
	pkts := MakeGroup(0, src)
	pkts[GroupSize].Payload[0] ^= 0xFF // 损坏校验包
	kept := append([]Packet{}, pkts[:1]...)
	kept = append(kept, pkts[2:]...)
	res, errs := Recover(kept)
	if res.Recovered != 0 {
		t.Fatal("corrupt parity must not be reported as recovered")
	}
	f := fault.First(errs)
	if f == nil || f.Kind != fault.KindCRC || f.Recoverable {
		t.Fatalf("want unrecoverable crc fault, got %v", errs)
	}
}

func TestDoubleLossUnrecoverable(t *testing.T) {
	pkts := MakeGroup(0, payloads(4))
	res, errs := Recover(pkts[2:]) // 丢两个数据包
	if res.Recovered != 0 {
		t.Fatal("double loss must not recover")
	}
	f := fault.First(errs)
	if f == nil || f.Kind != fault.KindPacketLoss || f.Recoverable {
		t.Fatalf("got %v", errs)
	}
}

func TestMultiGroup(t *testing.T) {
	var all []Packet
	var want [][]byte
	for g := 0; g < 3; g++ {
		src := payloads(4)
		all = append(all, MakeGroup(g, src)...)
		want = append(want, src...)
	}
	// 每组各丢 1 包（seq=g%4）
	var kept []Packet
	for _, p := range all {
		if !p.Parity && p.Seq == p.Group%4 {
			continue
		}
		kept = append(kept, p)
	}
	res, errs := Recover(shuffled(kept, 9))
	if len(errs) != 0 {
		t.Fatalf("faults: %v", errs)
	}
	if res.Lost != 3 || res.Recovered != 3 {
		t.Fatalf("lost=%d recovered=%d", res.Lost, res.Recovered)
	}
	for i, p := range res.Payloads {
		if !bytes.Equal(p, want[i]) {
			t.Fatalf("payload %d mismatch", i)
		}
	}
}
