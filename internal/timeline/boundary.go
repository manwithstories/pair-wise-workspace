package timeline

import (
	"fmt"
	"sort"

	"mediasync/internal/model"
)

// Fragment is one contiguous representation segment expressed on the shared
// clock. Ranges are half-open: [FirstTick, EndTick).
type Fragment struct {
	TrackID   string
	Rep       string
	FirstTick int64
	EndTick   int64
	Frames    int
}

// BoundaryMismatch describes a contradiction between representation fragments
// and the merged timeline. It propagates into the export manifest.
type BoundaryMismatch struct {
	TrackID  string
	AtTick   int64
	Kind     model.Kind
	RepA     string
	RepB     string
	Message  string
}

// Fragmentize builds per-track fragments from timeline frames. A representation
// change, a tick gap larger than one slot, or an incomplete placeholder starts a
// new fragment. Output is deterministic (sorted by track, then first tick).
func Fragmentize(tl *Timeline) []Fragment {
	type acc struct {
		track  string
		rep    string
		first  int64
		end    int64
		frames int
	}
	groups := map[string][]acc{}
	for _, slot := range tl.Slots {
		for _, af := range slot.Frames {
			track := af.TrackID
			fr := af.Frame
			n := len(groups[track])
			if n == 0 || groups[track][n-1].rep != fr.Rep ||
				groups[track][n-1].end != af.SharedTick || fr.Incomplete {
				groups[track] = append(groups[track], acc{
					track: track, rep: fr.Rep, first: af.SharedTick,
					end: af.SharedTick, frames: 0,
				})
				n = len(groups[track])
			}
			groups[track][n-1].end = nextEnd(tl, af.SharedTick)
			groups[track][n-1].frames++
		}
	}
	var out []Fragment
	for track, list := range groups {
		for _, a := range list {
			out = append(out, Fragment{TrackID: track, Rep: a.rep, FirstTick: a.first,
				EndTick: a.end, Frames: a.frames})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TrackID != out[j].TrackID {
			return out[i].TrackID < out[j].TrackID
		}
		return out[i].FirstTick < out[j].FirstTick
	})
	return out
}

// nextEnd returns the timeline tick strictly after tick, or tick+1 at the tail.
func nextEnd(tl *Timeline, tick int64) int64 {
	for _, s := range tl.Slots {
		if s.Tick > tick {
			return s.Tick
		}
	}
	return tick + 1
}

// VerifyBoundaries cross-checks fragments against each other and the timeline:
// adjacent fragments must meet at one shared boundary without overlap, and every
// fragment must cover ticks the timeline actually contains.
func VerifyBoundaries(tl *Timeline, fragments []Fragment) ([]BoundaryMismatch, model.Bag) {
	var diag model.Bag
	var mismatches []BoundaryMismatch
	knownTicks := map[int64]bool{}
	for _, s := range tl.Slots {
		knownTicks[s.Tick] = true
	}
	byTrack := map[string][]Fragment{}
	for _, f := range fragments {
		byTrack[f.TrackID] = append(byTrack[f.TrackID], f)
	}
	tracks := make([]string, 0, len(byTrack))
	for k := range byTrack {
		tracks = append(tracks, k)
	}
	sort.Strings(tracks)
	for _, track := range tracks {
		list := byTrack[track]
		sort.Slice(list, func(i, j int) bool { return list[i].FirstTick < list[j].FirstTick })
		for i, f := range list {
			if f.EndTick <= f.FirstTick {
				m := record(&diag, track, f.FirstTick, model.KindBoundaryMismatch, f.Rep, "",
					fmt.Sprintf("fragment has empty/negative range [%d,%d)", f.FirstTick, f.EndTick))
				mismatches = append(mismatches, m)
			}
			if !knownTicks[f.FirstTick] {
				m := record(&diag, track, f.FirstTick, model.KindBoundaryMismatch, f.Rep, "",
					"fragment starts at a tick absent from the timeline")
				mismatches = append(mismatches, m)
			}
			if i+1 >= len(list) {
				continue
			}
			next := list[i+1]
			switch {
			case next.FirstTick < f.EndTick:
				m := record(&diag, track, next.FirstTick, model.KindBoundaryMismatch, f.Rep, next.Rep,
					fmt.Sprintf("fragments overlap: [%d,%d) vs [%d,%d)", f.FirstTick, f.EndTick,
						next.FirstTick, next.EndTick))
				mismatches = append(mismatches, m)
			case next.FirstTick > f.EndTick:
				m := record(&diag, track, f.EndTick, model.KindBoundaryMismatch, f.Rep, next.Rep,
					fmt.Sprintf("boundary gap between reps %s and %s: missing ticks [%d,%d)",
						f.Rep, next.Rep, f.EndTick, next.FirstTick))
				mismatches = append(mismatches, m)
			}
		}
	}
	return mismatches, diag
}

func record(diag *model.Bag, track string, tick int64, kind model.Kind, repA, repB, msg string) BoundaryMismatch {
	m := BoundaryMismatch{TrackID: track, AtTick: tick, Kind: kind, RepA: repA, RepB: repB, Message: msg}
	diag.Add(model.Diagnostic{Kind: kind, Severity: model.SeverityError, Track: track,
		AtTicks: tick, Message: msg})
	return m
}

// MergeDiag folds boundary diagnostics into the timeline's diagnostic bag so
// export consumers see one ordered trail.
func MergeDiag(tl *Timeline, extra model.Bag) {
	for _, d := range extra.All() {
		tl.Diag.Add(d)
	}
}
