package ebml

import (
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
)

func TestResolveUnknownDeclaredSize(t *testing.T) {
	e := Element{ID: IDCluster, Size: 10, End: 20, DataOffset: 10}
	res, err := ResolveUnknown(Context{Limit: 100}, e, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.End != 20 {
		t.Fatalf("end=%d", res.End)
	}
}

func TestResolveUnknownClusterSibling(t *testing.T) {
	e := Element{ID: IDCluster, Offset: 10, DataOffset: 14, UnknownSize: true}
	res, err := ResolveUnknown(Context{Limit: 100}, e, IDTracks, 50, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.End != 50 || res.SiblingID != IDTracks {
		t.Fatalf("res=%+v", res)
	}
}

func TestResolveUnknownAmbiguousSiblingFails(t *testing.T) {
	e := Element{ID: IDSegment, Offset: 0, DataOffset: 4, UnknownSize: true}
	// A child element that cannot terminate a Segment, with data still after
	// it, has no provable boundary.
	_, err := ResolveUnknown(Context{Limit: 100}, e, IDCluster, 50, 100)
	if !errors.Is(err, ErrBoundary) {
		t.Fatalf("got %v", err)
	}
	if fault.StageOf(err) != fault.StageParse {
		t.Fatalf("stage=%q", fault.StageOf(err))
	}
}

func TestResolveUnknownSegmentGlobal(t *testing.T) {
	e := Element{ID: IDSegment, Offset: 0, DataOffset: 4, UnknownSize: true}
	res, err := ResolveUnknown(Context{Limit: 100}, e, IDEBML, 60, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.End != 60 {
		t.Fatalf("end=%d", res.End)
	}
}

func TestResolveUnknownRunsToSourceEnd(t *testing.T) {
	e := Element{ID: IDSegment, Offset: 0, DataOffset: 4, UnknownSize: true}
	res, err := ResolveUnknown(Context{Limit: 1000}, e, 0, 200, 200)
	if err != nil {
		t.Fatal(err)
	}
	if res.End != 200 {
		t.Fatalf("end=%d", res.End)
	}
}
