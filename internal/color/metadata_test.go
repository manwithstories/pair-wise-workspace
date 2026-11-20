package color

import (
	"testing"

	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
)

func meta(p, tr, m uint16, full bool) *ingest.ColorMeta {
	return &ingest.ColorMeta{Primaries: p, Transfer: tr, Matrix: m, FullRange: full}
}

func projectDecl() *Descriptor {
	return &Descriptor{Primaries: 1, Transfer: 1, Matrix: 1, FullRange: false, Source: "v1"}
}

func TestInheritFromProject(t *testing.T) {
	frames := []*ingest.Frame{
		{TrackID: "v1", Seq: 0, Color: nil},
		{TrackID: "v1", Seq: 1, Color: meta(1, 1, 1, false)},
	}
	out, faults := Reconcile("v1", "f", frames, projectDecl())
	if model.HasFatal(faults) {
		t.Fatalf("inheritance should be recoverable: %v", faults)
	}
	if !out[0].Inferred || out[0].Source != "project:v1" {
		t.Fatalf("not inherited: %+v", out[0])
	}
	if out[1].Inferred {
		t.Fatal("matching frame must not be marked inferred")
	}
}

func TestMissingWithoutProjectFatal(t *testing.T) {
	frames := []*ingest.Frame{{TrackID: "v1", Seq: 7, Color: nil, Offset: 700}}
	_, faults := Reconcile("v1", "f", frames, nil)
	if len(faults) != 1 || faults[0].Class != model.FaultColorMissing || faults[0].Severity != model.SeverityFatal {
		t.Fatalf("want fatal missing, got %v", faults)
	}
	if faults[0].Location.Seq != 7 || faults[0].Location.Offset != 700 {
		t.Fatalf("location wrong: %+v", faults[0].Location)
	}
}

func TestConflictRetainsSourceValue(t *testing.T) {
	frames := []*ingest.Frame{{TrackID: "v1", Seq: 3, Color: meta(9, 16, 9, true)}}
	out, faults := Reconcile("v1", "f", frames, projectDecl())
	var conflict *model.Fault
	for i := range faults {
		if faults[i].Class == model.FaultColorConflict {
			conflict = &faults[i]
		}
	}
	if conflict == nil {
		t.Fatalf("conflict not reported: %v", faults)
	}
	// Source value retained, never overwritten by the project declaration.
	if out[0].Primaries != 9 || out[0].FullRange != true || out[0].Inferred {
		t.Fatalf("source value not retained: %+v", out[0])
	}
	if conflict.Expected == "" || conflict.Actual == "" {
		t.Fatal("conflict must record both basis values")
	}
}

func TestTrackSummaryDedupes(t *testing.T) {
	resolved := []Descriptor{
		{Primaries: 1, Source: "frame:0"},
		{Primaries: 1, Source: "frame:1"},
		{Primaries: 9, Source: "frame:2"},
	}
	sum := TrackSummary(resolved)
	if len(sum) != 2 {
		t.Fatalf("summary=%d want 2 distinct", len(sum))
	}
}
