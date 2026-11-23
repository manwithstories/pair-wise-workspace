package demo_test

import (
	"bytes"
	"testing"

	"media-pipeline/internal/demo"
)

func TestLCGDeterministic(t *testing.T) {
	a := demo.NewLCG(42)
	b := demo.NewLCG(42)
	for i := 0; i < 1000; i++ {
		if a.Uint64() != b.Uint64() {
			t.Fatalf("LCG diverged at iteration %d", i)
		}
	}
	c := demo.NewLCG(43)
	if demo.NewLCG(42).Uint64() == c.Uint64() {
		t.Fatal("different seeds produced identical first value")
	}
}

func TestScenarioDeterminism(t *testing.T) {
	s1 := demo.BuildScenarios(42)
	s2 := demo.BuildScenarios(42)
	if len(s1) != len(s2) || len(s1) != 6 {
		t.Fatalf("scenario count = %d, want 6", len(s1))
	}
	for i := range s1 {
		if s1[i].Name != s2[i].Name {
			t.Fatalf("scenario %d name mismatch", i)
		}
		if len(s1[i].Datagrams) != len(s2[i].Datagrams) {
			t.Fatalf("scenario %s datagram count differs", s1[i].Name)
		}
		for j := range s1[i].Datagrams {
			if !bytes.Equal(s1[i].Datagrams[j].Bytes, s2[i].Datagrams[j].Bytes) {
				t.Fatalf("scenario %s pkt %d not deterministic", s1[i].Name, j)
			}
		}
	}
}

func TestBurstLossHasHoles(t *testing.T) {
	scen := demo.BuildScenarios(42)
	var burst *demo.Scenario
	for i := range scen {
		if scen[i].Name == demo.ScenBurstLoss {
			burst = &scen[i]
		}
	}
	seqs := map[uint16]bool{}
	for _, d := range burst.Datagrams {
		if seqs[d.Seq] {
			t.Fatal("unexpected duplicate in burst scenario")
		}
		seqs[d.Seq] = true
	}
	for _, missing := range []uint16{10, 11, 12} {
		if seqs[missing] {
			t.Fatalf("seq %d should be missing in burst loss", missing)
		}
	}
}

func TestReorderAndDuplicate(t *testing.T) {
	scen := demo.BuildScenarios(42)
	var ro *demo.Scenario
	for i := range scen {
		if scen[i].Name == demo.ScenReorder {
			ro = &scen[i]
		}
	}
	// 额外追加的重复包使数据报数比序号数多 1，且相邻序号被交换。
	if ro.Datagrams[4].Seq != 5 || ro.Datagrams[5].Seq != 4 {
		t.Fatal("expected swapped sequence numbers at index 4/5")
	}
	counts := map[uint16]int{}
	for _, d := range ro.Datagrams {
		counts[d.Seq]++
	}
	var dupes int
	for _, n := range counts {
		if n > 1 {
			dupes++
		}
	}
	if dupes != 1 {
		t.Fatalf("expected exactly one duplicated seq, got %d", dupes)
	}
}

func TestCorruptByteFlipsTarget(t *testing.T) {
	raw := []byte{0, 0, 0, 0, 0}
	off := demo.CorruptByte(raw, 2)
	if off != 2 || raw[2] != 0x01 {
		t.Fatalf("corruption at wrong offset/value: off=%d byte=%d", off, raw[2])
	}
}
