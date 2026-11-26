package fanout

import (
	"testing"

	"medcheck/fault"
	"medcheck/ingest"
)

func TestFanoutCopiesToEveryRepresentation(t *testing.T) {
	reps := Standard(1_000_000)
	var got []string
	err := Fanout([]ingest.Packet{{Header: ingest.Header{Camera: "c", Stream: "src", Seq: 1}}}, reps,
		func(rep Representation, p ingest.Packet) error {
			got = append(got, rep.ID+":"+p.Stream)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1x:1x", "2x:2x", "4x:4x"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestSuddenBandwidthDropDowngradesLayers(t *testing.T) {
	fm := fault.NewModel()
	c := NewController(Standard(1_000_000), fm)
	if drop, allowed := c.Observe(0, 5_000_000); drop || len(allowed) != 3 {
		t.Fatalf("initial bandwidth should allow all: drop=%v layers=%v", drop, allowed)
	}
	drop, allowed := c.Observe(1000, 800_000)
	if !drop {
		t.Fatal("expected sudden drop event")
	}
	if len(allowed) != 0 {
		t.Fatalf("800 kbps should not fit 1 Mbps base layer: %v", allowed)
	}
	if fm.Count(fault.BandwidthDrop) != 1 {
		t.Fatalf("bandwidth fault not recorded")
	}
	drop, allowed = c.Observe(2000, 2_000_000)
	if drop || len(allowed) != 2 || allowed[0].ID != "1x" || allowed[1].ID != "2x" {
		t.Fatalf("recovery bandwidth selection wrong: drop=%v allowed=%v", drop, allowed)
	}
}
