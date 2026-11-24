package testkit

import (
	"errors"
	"math/rand"
	"testing"

	"mediarun/internal/model"
	"mediarun/internal/parse"
)

func TestGeneratePacketsScenarios(t *testing.T) {
	for _, s := range []Scenario{ScenarioClean, ScenarioReorder, ScenarioDuplicate, ScenarioBurst, ScenarioWrap} {
		packets, err := GeneratePackets(3, s, 1, 40, 0)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if len(packets) == 0 {
			t.Fatalf("%s produced nothing", s)
		}
		for _, p := range packets {
			if _, err := EncodeWire(p); err != nil {
				t.Fatalf("%s wire: %v", s, err)
			}
		}
	}
	clean, _ := GeneratePackets(3, ScenarioClean, 1, 40, 0)
	dup, _ := GeneratePackets(3, ScenarioDuplicate, 1, 40, 0)
	if len(dup) <= len(clean) {
		t.Fatalf("duplicate scenario did not add packets: %d vs %d", len(dup), len(clean))
	}
	burst, _ := GeneratePackets(3, ScenarioBurst, 1, 40, 0)
	if len(burst) >= len(clean) {
		t.Fatalf("burst scenario did not drop packets: %d vs %d", len(burst), len(clean))
	}
	if drops := BurstDropRanges(40); len(drops) == 0 {
		t.Fatal("burst ranges empty")
	}
}

func TestWrapScenarioWraps(t *testing.T) {
	packets, err := GeneratePackets(5, ScenarioWrap, 1, 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := false
	for i := 1; i < len(packets); i++ {
		if packets[i].Timestamp < packets[i-1].Timestamp {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("wrap scenario never wraps")
	}
}

func TestReorderStreamStable(t *testing.T) {
	in := make([]RTPPacket, 30)
	for i := range in {
		in[i] = RTPPacket{Sequence: uint16(i), Payload: []byte{byte(i)}}
	}
	rng := rand.New(rand.NewSource(1))
	out, err := ReorderStream(in, ScenarioReorder, rng)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("reorder changed length")
	}
	if out[0].Sequence == in[0].Sequence && out[2].Sequence == in[2].Sequence {
		t.Fatal("reorder did not move anything")
	}
	if _, err := ReorderStream(in, Scenario("nope"), rng); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("bad scenario: %v", err)
	}
}

func TestCorruptBoxes(t *testing.T) {
	media, err := GenerateMedia(11)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := CorruptBoxes(media.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 4 {
		t.Fatalf("corrupt cases %d", len(cases))
	}
	for i, c := range cases {
		if _, err := parse.ParseMovie(c); err == nil {
			t.Fatalf("case %d accepted by parser", i)
		} else if model.ClassOf(err) != model.ClassCorrupt {
			t.Fatalf("case %d class %v", i, model.ClassOf(err))
		}
	}
	if _, err := CorruptBoxes([]byte{1, 2, 3}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("short input: %v", err)
	}
}

func TestEncodeWireRejectsOversize(t *testing.T) {
	if _, err := EncodeWire(RTPPacket{Payload: make([]byte, 3000)}); !errors.Is(err, model.ErrLimit) {
		t.Fatalf("oversize: %v", err)
	}
}
