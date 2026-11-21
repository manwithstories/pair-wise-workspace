package timeline

import (
	"fmt"
	"sort"

	"mediasync/internal/model"
)

// gapReservedBase mirrors merge.gapBase: ticks at or above it belong to
// incomplete gap placeholders rather than real media content.
const gapReservedBase int64 = 1_000_000_000

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
	TrackID string
	AtTick  int64
	Kind    model.Kind
	RepA    string
	RepB    string
	Message string
}

// Fragmentize builds per-track fragments from timeline frames. A new fragment
// starts when the representation changes or when an incomplete placeholder is
// reached. Frames sharing a timestamp (retransmitted recovery traffic) stay in
// the same fragment, so healthy ABR switches never manufacture overlaps.
// Output is deterministic (sorted by track, then first tick).
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
			list := groups[track]
			n := len(list)
			switch {
			case n == 0 || list[n-1].rep != fr.Rep || fr.Incomplete:
				groups[track] = append(list, acc{
					track: track, rep: fr.Rep, first: af.SharedTick,
					end: nextEnd(tl, af.SharedTick), frames: 1,
				})
			default:
				groups[track][n-1].end = nextEnd(tl, af.SharedTick)
				groups[track][n-1].frames++
			}
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

// VerifyBoundaries cross-checks fragments against the timeline per track.
// Adjacent same-track fragments must not overlap and must not skip a tick that
// the track actually contributed. Sparse tracks (e.g. audio blocks) naturally
// have empty ticks between their own frames and are not flagged.
func VerifyBoundaries(tl *Timeline, fragments []Fragment) ([]BoundaryMismatch, model.Bag) {
	var diag model.Bag
	var mismatches []BoundaryMismatch
	trackTicks := map[string]map[int64]bool{}
	for _, slot := range tl.Slots {
		for _, af := range slot.Frames {
			if trackTicks[af.TrackID] == nil {
				trackTicks[af.TrackID] = map[int64]bool{}
			}
			trackTicks[af.TrackID][af.SharedTick] = true
		}
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
		// Gap placeholders live on reserved synthetic ticks and are reported via
		// the incomplete-tracks/manifest trail, not as boundary contradictions.
		list := filterRealFragments(byTrack[track])
		sort.Slice(list, func(i, j int) bool { return list[i].FirstTick < list[j].FirstTick })
		known := trackTicks[track]
		for i, f := range list {
			if f.EndTick <= f.FirstTick {
				m := record(&diag, track, f.FirstTick, model.KindBoundaryMismatch, f.Rep, "",
					fmt.Sprintf("fragment has empty/negative range [%d,%d)", f.FirstTick, f.EndTick))
				mismatches = append(mismatches, m)
			}
			if !known[f.FirstTick] {
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
			default:
				// Hole only when this track genuinely had a frame between the two
				// fragments that no fragment now covers.
				for tick := range known {
					if tick > f.EndTick && tick < next.FirstTick {
						m := record(&diag, track, tick, model.KindBoundaryMismatch, f.Rep, next.Rep,
							fmt.Sprintf("boundary gap between reps %s and %s at tick %d", f.Rep, next.Rep, tick))
						mismatches = append(mismatches, m)
						break
					}
				}
			}
		}
	}
	return mismatches, diag
}

// filterRealFragments drops fragments that cover only reserved gap-placeholder
// ticks; those gaps are represented by incomplete-track diagnostics instead.
func filterRealFragments(in []Fragment) []Fragment {
	var out []Fragment
	for _, f := range in {
		if f.FirstTick >= gapReservedBase {
			continue
		}
		out = append(out, f)
	}
	return out
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
