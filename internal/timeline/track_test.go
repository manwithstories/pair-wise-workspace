package timeline

import "testing"

func TestTopoOrder(t *testing.T) {
	g := NewGraph()
	// Added in reverse dependency order on purpose.
	if err := g.Add(&Track{SSRC: 2, Kind: "audio", ClockRate: 48000, DependsOn: []uint32{1}}); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(&Track{SSRC: 1, Kind: "video", ClockRate: 90000}); err != nil {
		t.Fatal(err)
	}
	order, err := g.TopoOrder()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0].SSRC != 1 || order[1].SSRC != 2 {
		t.Fatalf("order=%v", order)
	}
	if order[0].TrackID != 1 || order[1].TrackID != 2 {
		t.Fatalf("track IDs: %d %d", order[0].TrackID, order[1].TrackID)
	}
}

func TestCycleRejected(t *testing.T) {
	g := NewGraph()
	_ = g.Add(&Track{SSRC: 1, DependsOn: []uint32{2}})
	_ = g.Add(&Track{SSRC: 2, DependsOn: []uint32{1}})
	if _, err := g.TopoOrder(); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestMissingDependencyRejected(t *testing.T) {
	g := NewGraph()
	_ = g.Add(&Track{SSRC: 1, DependsOn: []uint32{99}})
	if _, err := g.TopoOrder(); err == nil {
		t.Fatal("expected missing-dependency error")
	}
}

func TestDuplicateRejected(t *testing.T) {
	g := NewGraph()
	_ = g.Add(&Track{SSRC: 1})
	if err := g.Add(&Track{SSRC: 1}); err == nil {
		t.Fatal("expected duplicate error")
	}
}
