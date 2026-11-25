package recovery

import (
	"mediad/internal/fault"
)

func (e *exec) stageManifest() error {
	if len(e.dash.Representations) < 2 {
		return fault.New(fault.InvalidManifest, e.dash.Path, 0,
			"DASH must contain multiple representations, got %d", len(e.dash.Representations))
	}
	want := len(e.segs)
	for _, rep := range e.dash.Representations {
		if len(rep.Segments) != want {
			return fault.New(fault.Boundary, e.dash.Path, 0,
				"representation %s has %d timeline segments, want %d", rep.ID, len(rep.Segments), want)
		}
		for i, s := range rep.Segments {
			if s.Index != i {
				return fault.New(fault.Boundary, e.dash.Path, s.LineOffset,
					"representation %s segment index mismatch at %d", rep.ID, i)
			}
			if s.InitURI == "" {
				return fault.New(fault.InitMissing, e.dash.Path, 0, "representation %s missing init", rep.ID)
			}
		}
	}

	unique := map[string]struct{}{}
	for _, s := range e.segs {
		unique[s.hash] = struct{}{}
	}
	e.sum.Segments = len(e.segs)
	e.sum.SegmentSHA256 = len(unique)
	return e.finishStage("manifest")
}
