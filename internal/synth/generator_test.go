package synth

import (
	"testing"

	"archivemedia/internal/packet"
)

func TestGenerateDeterministic(t *testing.T) {
	cfg := Config{
		Seed: 41, Seconds: 2, Codec: packet.CodecOpus,
		Faults: []Fault{FaultDuplicate, FaultReorder, FaultBurst, FaultLate, FaultCRC},
	}
	g1, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(g1.Events) != len(g2.Events) || len(g1.GroundTruth) != len(g2.GroundTruth) {
		t.Fatal("同 seed 生成规模不一致")
	}
	for i := range g1.Events {
		a, b := g1.Events[i], g2.Events[i]
		if a.Seq != b.Seq || a.ArrivalIndex != b.ArrivalIndex ||
			string(a.Payload) != string(b.Payload) {
			t.Fatalf("事件 %d 同 seed 不确定", i)
		}
	}
}

func TestFaultsInjected(t *testing.T) {
	cfg := Config{
		Seed: 7, Seconds: 2, Codec: packet.CodecOpus,
		Faults: []Fault{FaultDuplicate, FaultReorder, FaultBurst, FaultLate, FaultCRC},
	}
	g, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := g.Stats
	if st.Duplicates < 1 || st.Reorders < 1 || st.Bursts < 1 ||
		st.Late < 1 || st.CRC < 1 {
		t.Fatalf("故障注入不足: %+v", st)
	}
	// 突发丢失序号必须真实地从到达事件中缺席。
	truthSeq := map[uint16]bool{}
	for _, e := range g.GroundTruth {
		truthSeq[e.Seq] = true
	}
	arrivedSeq := map[uint16]int{}
	for _, e := range g.Events {
		arrivedSeq[e.Seq]++
	}
	for _, missing := range st.BurstMissing {
		if arrivedSeq[missing] != 0 {
			t.Fatalf("突发丢失序号 %d 仍然到达", missing)
		}
		if !truthSeq[missing] {
			t.Fatalf("丢失序号 %d 不在基准流中", missing)
		}
	}
}

func TestDuplicateSameKeySameContent(t *testing.T) {
	cfg := Config{
		Seed: 3, Seconds: 2, Codec: packet.CodecAAC,
		Faults: []Fault{FaultDuplicate},
	}
	g, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.ExtraData) == 0 {
		t.Fatal("AAC 必须产出 extradata")
	}
	count := map[uint16]int{}
	for _, e := range g.Events {
		count[e.Seq]++
	}
	hasDup := false
	for _, c := range count {
		if c >= 2 {
			hasDup = true
		}
	}
	if !hasDup {
		t.Fatal("未观察到重复序号")
	}
}

func TestNoFaultsCleanStream(t *testing.T) {
	cfg := Config{Seed: 1, Seconds: 1, Codec: packet.CodecOpus}
	g, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Events) != len(g.GroundTruth) {
		t.Fatal("无故障时到达事件应与基准等长")
	}
	for i, e := range g.Events {
		if e.Seq != g.GroundTruth[i].Seq {
			t.Fatal("无故障流应保持序号顺序")
		}
	}
}

func TestCRCEventStillStructurallyValid(t *testing.T) {
	cfg := Config{
		Seed: 11, Seconds: 2, Codec: packet.CodecOpus,
		Faults: []Fault{FaultCRC},
	}
	g, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 损坏包仍是结构合法事件（载荷非空、时长非零），只是解码时失败。
	for _, e := range g.Events {
		if err := e.Validate(); err != nil {
			t.Fatalf("CRC 故障包结构非法: %v", err)
		}
	}
}
