// Package color reconciles per-frame color metadata against the project-level
// track declaration. Missing metadata is inherited from the project
// declaration; conflicting metadata keeps the source value and records why.
// No value is ever overwritten without an explicit, reported basis.
package color

import (
	"fmt"

	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
)

// Descriptor is a resolved color description plus its provenance.
type Descriptor struct {
	Primaries uint16 `json:"primaries"`
	Transfer  uint16 `json:"transfer"`
	Matrix    uint16 `json:"matrix"`
	FullRange bool   `json:"full_range"`
	// Source records where the value came from, e.g. "frame:12" or
	// "project:v1". An inherited value is also marked Inferred.
	Source   string `json:"source"`
	Inferred bool   `json:"inferred"`
}

func fromFrame(m *ingest.ColorMeta, seq uint64) Descriptor {
	return Descriptor{
		Primaries: m.Primaries, Transfer: m.Transfer, Matrix: m.Matrix,
		FullRange: m.FullRange, Source: fmt.Sprintf("frame:%d", seq),
	}
}

func fromProject(d Descriptor) Descriptor {
	d.Source = "project:" + d.Source
	d.Inferred = true
	return d
}

// equal compares the technical fields, ignoring provenance.
func (a Descriptor) equal(b Descriptor) bool {
	return a.Primaries == b.Primaries && a.Transfer == b.Transfer &&
		a.Matrix == b.Matrix && a.FullRange == b.FullRange
}

func (d Descriptor) String() string {
	return fmt.Sprintf("prim=%d xfer=%d matrix=%d full=%t", d.Primaries, d.Transfer, d.Matrix, d.FullRange)
}

// Reconcile walks one track's frame metadata. project may be nil when the
// project declares nothing for this track. The returned slice aligns 1:1 with
// frames. Missing metadata with no project declaration is fatal; a conflict
// between frame and project values is recoverable and always resolved in
// favour of the frame source.
func Reconcile(trackID, file string, frames []*ingest.Frame, project *Descriptor) ([]Descriptor, []model.Fault) {
	out := make([]Descriptor, len(frames))
	var faults []model.Fault

	for i, fr := range frames {
		m := fr.Color
		seq := fr.Seq
		loc := model.SourceLocation{File: file, Track: trackID, Seq: seq, Offset: fr.Offset}

		if m == nil {
			if project == nil {
				faults = append(faults, model.Fault{
					Class: model.FaultColorMissing, Severity: model.SeverityFatal,
					Location: loc,
					Message:  "color metadata missing and no project declaration available",
				})
				continue
			}
			desc := fromProject(*project)
			out[i] = desc
			faults = append(faults, model.Fault{
				Class: model.FaultColorMissing, Severity: model.SeverityRecoverable,
				Location: loc, Inferred: true,
				Message:  fmt.Sprintf("color metadata missing; inherited project declaration (%s)", desc.String()),
				Expected: project.String(),
			})
			continue
		}

		frame := fromFrame(m, seq)
		if project != nil && !frame.equal(*project) {
			// Conflict: retain the frame's own value. The project declaration
			// is never force-applied; the selection basis is reported.
			out[i] = frame
			faults = append(faults, model.Fault{
				Class: model.FaultColorConflict, Severity: model.SeverityRecoverable,
				Location: loc,
				Message: fmt.Sprintf("frame color conflicts with project declaration; source value retained (%s vs %s)",
					frame.String(), project.String()),
				Expected: project.String(), Actual: frame.String(),
			})
			continue
		}
		out[i] = frame
	}
	return out, faults
}

// TrackSummary reports the distinct descriptors observed on a track so the
// manifest can show whether a track is internally consistent.
func TrackSummary(resolved []Descriptor) []Descriptor {
	seen := map[string]Descriptor{}
	var order []string
	for _, d := range resolved {
		key := d.String()
		if _, ok := seen[key]; !ok {
			seen[key] = d
			order = append(order, key)
		}
	}
	out := make([]Descriptor, 0, len(order))
	for _, k := range order {
		out = append(out, seen[k])
	}
	return out
}
