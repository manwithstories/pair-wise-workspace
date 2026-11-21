package fixtures

import (
	"testing"

	"github.com/pion/rtp"
)

func TestAllScenariosBuild(t *testing.T) {
	for _, name := range AllScenarios() {
		s, err := BuildStream(name, 30, 42)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(s.Packets) == 0 {
			t.Fatalf("%s produced no packets", name)
		}
		for i, raw := range s.Packets {
			var p rtp.Packet
			if err := p.Unmarshal(raw); err != nil {
				t.Fatalf("%s packet %d: %v", name, i, err)
			}
			if p.Version != 2 {
				t.Fatalf("%s packet %d version %d", name, i, p.Version)
			}
		}
	}
}

func TestWrapScenarioCrossesBoundary(t *testing.T) {
	plan, err := ScenarioPlan(ScenarioWrap, 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if int(plan.StartSeq)+plan.Count <= 65535 {
		t.Fatalf("wrap scenario does not cross 65535: start=%d count=%d", plan.StartSeq, plan.Count)
	}
	packets, _ := BuildPackets(plan)
	if packets[0].Seq != 65530 || packets[6].Seq != 0 {
		t.Fatalf("sequences = %d ... %d", packets[0].Seq, packets[6].Seq)
	}
}

func TestBurstScenarioHasExplicitLoss(t *testing.T) {
	s, err := BuildStream(ScenarioBurst, 30, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.LostIndices) != 4 {
		t.Fatalf("lost = %v", s.LostIndices)
	}
	for i := 1; i < len(s.LostIndices); i++ {
		if s.LostIndices[i] != s.LostIndices[i-1]+1 {
			t.Fatal("loss is not a contiguous burst")
		}
	}
}

func TestDuplicateScenarioEmitsExtras(t *testing.T) {
	s, err := BuildStream(ScenarioDuplicate, 30, 42)
	if err != nil {
		t.Fatal(err)
	}
	if s.DuplicateCount == 0 || len(s.Packets) != 30+s.DuplicateCount {
		t.Fatalf("duplicates=%d packets=%d", s.DuplicateCount, len(s.Packets))
	}
}

func TestReorderScenarioSwapsArrival(t *testing.T) {
	s, err := BuildStream(ScenarioReorder, 30, 42)
	if err != nil {
		t.Fatal(err)
	}
	swapped := 0
	var prev rtp.Packet
	havePrev := false
	for _, raw := range s.Packets {
		var p rtp.Packet
		_ = p.Unmarshal(raw)
		if havePrev && p.SequenceNumber < prev.SequenceNumber {
			swapped++
		}
		prev, havePrev = p, true
	}
	if swapped == 0 {
		t.Fatal("no out-of-order arrival in reorder scenario")
	}
}

func TestUnknownScenarioIsUsageError(t *testing.T) {
	_, err := ScenarioPlan("nope", 10, 0)
	if err == nil {
		t.Fatal("unknown scenario accepted")
	}
}
