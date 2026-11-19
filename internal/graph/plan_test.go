package graph

import (
	"reflect"
	"testing"

	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/model"
)

func node(id uint32, kind string, deps ...uint32) Node {
	return Node{ID: id, Kind: kind, Deps: deps}
}

func TestSortRespectsDependencies(t *testing.T) {
	g, err := New([]Node{
		node(3, model.KindVideo, 1),
		node(1, model.KindIndex),
		node(2, model.KindAudio),
	}, "m")
	if err != nil {
		t.Fatal(err)
	}
	p := g.Sort()
	if len(p.Stopped) != 0 {
		t.Fatalf("unexpected stops: %+v", p.Stopped)
	}
	pos := map[uint32]int{}
	for i, id := range p.Order {
		pos[id] = i
	}
	if pos[1] > pos[3] {
		t.Fatalf("index track ordered after its dependant: %v", p.Order)
	}
	if len(p.Order) != 3 {
		t.Fatalf("order=%v", p.Order)
	}
}

func TestSortStopsMissingDependencyBranchOnly(t *testing.T) {
	g, err := New([]Node{
		node(1, model.KindVideo, 99), // 99 does not exist
		node(2, model.KindAudio),     // independent
		node(3, model.KindVideo, 1),  // depends on stopped 1
	}, "m")
	if err != nil {
		t.Fatal(err)
	}
	p := g.Sort()
	if !p.Recoverable(2) {
		t.Fatalf("independent track 2 was stopped: %+v", p)
	}
	if p.Recoverable(1) || p.Recoverable(3) {
		t.Fatalf("dependent branch not stopped: %v", p.Order)
	}
	var sawMissing, sawStopped bool
	for _, s := range p.Stopped {
		switch s.Code {
		case errs.CodeDependencyMissing:
			sawMissing = true
			if s.TrackID != 1 {
				t.Fatalf("missing dependency attributed to wrong track: %+v", s)
			}
		case errs.CodeDependencyStopped:
			sawStopped = true
		}
	}
	if !sawMissing || !sawStopped {
		t.Fatalf("stops not classified: %+v", p.Stopped)
	}
	if !p.Issues.HasFatal() {
		t.Fatal("missing dependency should be fatal for the branch")
	}
}

func TestSortDetectsCycleAndNamesTracks(t *testing.T) {
	g, err := New([]Node{
		node(1, model.KindVideo, 2),
		node(2, model.KindAudio, 3),
		node(3, model.KindIndex, 1),
		node(4, model.KindVideo),
	}, "m")
	if err != nil {
		t.Fatal(err)
	}
	p := g.Sort()
	if !p.Issues.Has(errs.CodeDependencyCycle) {
		t.Fatalf("cycle not reported: %v", p.Issues)
	}
	if !p.Recoverable(4) {
		t.Fatalf("independent track stopped by unrelated cycle")
	}
	var related []uint32
	for _, s := range p.Stopped {
		if s.Code == errs.CodeDependencyCycle {
			related = s.Related
			if len(s.Related) != 3 {
				t.Fatalf("cycle members not fully named: %+v", s)
			}
		}
	}
	if !reflect.DeepEqual(related, []uint32{1, 2, 3}) {
		t.Fatalf("related=%v", related)
	}
	if p.Recoverable(1) || p.Recoverable(2) || p.Recoverable(3) {
		t.Fatalf("cycle members recovered: %v", p.Order)
	}
}

func TestSortFatalNodeStopsDependants(t *testing.T) {
	n := node(1, model.KindVideo)
	n.Fatal = true
	g, err := New([]Node{n, node(2, model.KindAudio, 1)}, "m")
	if err != nil {
		t.Fatal(err)
	}
	p := g.Sort()
	if p.Recoverable(1) || p.Recoverable(2) {
		t.Fatalf("fatal branch not stopped: %v", p.Order)
	}
	if len(p.Stopped) != 2 {
		t.Fatalf("stopped=%+v", p.Stopped)
	}
}

func TestSortDeterministicTieBreak(t *testing.T) {
	g, _ := New([]Node{node(5, model.KindVideo), node(3, model.KindAudio), node(9, model.KindIndex)}, "m")
	want := []uint32{3, 5, 9}
	for i := 0; i < 20; i++ {
		if got := g.Sort().Order; !reflect.DeepEqual(got, want) {
			t.Fatalf("order not stable: %v", got)
		}
	}
}

func TestNewRejectsDuplicateIDs(t *testing.T) {
	if _, err := New([]Node{node(1, model.KindVideo), node(1, model.KindAudio)}, "m"); err == nil {
		t.Fatal("duplicate ID accepted")
	}
}
