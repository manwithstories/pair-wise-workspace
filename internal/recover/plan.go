// Package recover turns scanned media into a recovery plan.
package recover

import (
	"fmt"

	"archivearchivist/internal/graph"
	"archivearchivist/internal/model"
)

// Run builds the recovery plan.
func Run(in Input, cfg Config) Result {
	if cfg.KeyframeSearch <= 0 || cfg.MaxErrorUS <= 0 {
		cfg = DefaultConfig()
	}
	res := Result{Tracks: []TrackState{}}
	if in.Timeline == nil || in.Graph == nil {
		res.Faults.Add(model.Fault{Code: model.CodeInvalidInput, Severity: model.SevFatal,
			Detail: "recovery requires a timeline and dependency graph"})
		return res
	}

	// Phase 1: per-track readiness. Clock normalization and keyframe
	// verification happen before any boundary work.
	states := map[string]*TrackState{}
	for _, t := range in.Timeline.Tracks {
		st, faults := verifyTrack(t, in)
		states[t.ID] = &st
		res.Faults.Add(faults...)
	}
	// Nodes caught in a cycle can never be ordered, so they are marked failed
	// up front. Their successors are blocked below; everything outside the
	// cycle continues normally.
	if _, stuck := in.Graph.TopoOrder(); len(stuck) > 0 {
		for _, id := range stuck {
			tid, isTrack := trimTrack(id)
			if !isTrack {
				continue
			}
			if st := states[tid]; st != nil && st.OK {
				st.OK = false
				st.Reason = "track participates in a dependency cycle"
				res.Faults.Add(model.Fault{
					Code: model.CodeDependencyCycle, Severity: model.SevFatal, Track: tid,
					Detail: st.Reason,
				})
			}
		}
	}

	// A blocked dependency poisons only its own successors.
	for _, id := range topoOrder(in.Graph) {
		tid, isTrack := trimTrack(id)
		if !isTrack {
			continue
		}
		st := states[tid]
		if st == nil || !st.OK {
			continue
		}
		owner := in.Timeline.TrackByID(tid)
		if owner == nil {
			continue
		}
		for _, dep := range owner.DependsOn {
			if ds := states[dep]; ds != nil && !ds.OK {
				st.OK = false
				st.BlockedBy = dep
				st.Reason = "upstream track " + dep + " failed: " + ds.Reason
				res.Faults.Add(model.Fault{
					Code: model.CodeBlockedDependency, Severity: model.SevError, Track: tid,
					Detail: st.Reason,
				})
				break
			}
		}
	}

	// Phase 2: damaged ranges, only for ready tracks. An unrecoverable range
	// is fatal for the track and blocks its successors in phase 3.
	trackReady := map[string]bool{}
	for _, t := range in.Timeline.Tracks {
		trackReady[t.ID] = states[t.ID].OK
	}
	for _, t := range in.Timeline.Tracks {
		sc := in.Scans[t.ID]
		if sc == nil {
			continue
		}
		ranges := findRanges(t.ID, sc, cfg)
		res.Ranges = append(res.Ranges, ranges...)
		for _, r := range ranges {
			if r.Status == "unrecoverable" {
				trackReady[t.ID] = false
				res.Faults.Add(model.Fault{
					Code: model.CodeUnrecoverable, Severity: model.SevFatal, Track: t.ID,
					ByteStart: r.CorruptStart, ByteEnd: r.CorruptEnd,
					Detail: r.Detail,
				})
			}
		}
	}
	// Re-propagate blocking after range verdicts.
	for _, id := range topoOrder(in.Graph) {
		tid, isTrack := trimTrack(id)
		if !isTrack || !trackReady[tid] {
			continue
		}
		owner := in.Timeline.TrackByID(tid)
		if owner == nil {
			continue
		}
		for _, dep := range owner.DependsOn {
			if !trackReady[dep] {
				trackReady[tid] = false
				states[tid].Reason = "upstream track " + dep + " has an unrecoverable range"
				states[tid].BlockedBy = dep
				res.Faults.Add(model.Fault{
					Code: model.CodeBlockedDependency, Severity: model.SevError, Track: tid,
					Detail: states[tid].Reason,
				})
				break
			}
		}
	}
	for _, t := range in.Timeline.Tracks {
		st := states[t.ID]
		if st.OK && !trackReady[t.ID] {
			st.OK = false
		}
		res.Tracks = append(res.Tracks, *st)
	}

	// Phase 3: clip edges in dependency order. A single pass over the clip
	// list builds the lookup, so a timeline with many clips stays linear.
	clipByID := make(map[string]*model.Clip, len(in.Timeline.Clips))
	for i := range in.Timeline.Clips {
		clipByID[in.Timeline.Clips[i].ID] = &in.Timeline.Clips[i]
	}
	for _, id := range topoOrder(in.Graph) {
		cid, isClip := trimClip(id)
		if !isClip {
			continue
		}
		clipPtr, ok := clipByID[cid]
		if !ok {
			continue
		}
		clip := *clipPtr
		st := states[clip.Track]
		if st == nil || !trackReady[clip.Track] {
			reason := "owning track unavailable"
			if st != nil && st.Reason != "" {
				reason = st.Reason
			}
			res.Boundaries = append(res.Boundaries,
				Boundary{Clip: cid, Track: clip.Track, Edge: "in", Intended: clip.Start, Status: "blocked", Keyframe: -1, ErrorUS: -1},
				Boundary{Clip: cid, Track: clip.Track, Edge: "out", Intended: clip.Start.Add(clip.Duration), Status: "blocked", Keyframe: -1, ErrorUS: -1},
			)
			res.Faults.Add(model.Fault{
				Code: model.CodeBlockedDependency, Severity: model.SevError,
				Clip: cid, Track: clip.Track, Detail: "boundary recovery blocked: " + reason,
			})
			continue
		}
		bs := recoverClip(clip, in, cfg)
		res.Boundaries = append(res.Boundaries, bs...)
		for _, b := range bs {
			if b.Status == "recovered" && b.Snapped && b.ErrorUS > cfg.MaxErrorUS {
				res.Faults.Add(model.Fault{
					Code: model.CodeKeyframeSnap, Severity: model.SevWarn, Clip: b.Clip, Track: b.Track,
					Value: b.ErrorUS,
					Detail: fmt.Sprintf("%s edge snapped %.3f ms from the intended cut",
						b.Edge, float64(b.ErrorUS)/1000),
				})
			}
		}
	}

	// Phase 4: crossfade synchronization between recovered sides.
	for _, x := range in.Timeline.Transitions {
		res.Transitions = append(res.Transitions, recoverTransition(x, res.Boundaries))
	}

	res.Faults.Sort()
	res.Recovered = !res.Faults.Unrecoverable()
	return res
}

func trimTrack(id string) (string, bool) {
	if len(id) > 2 && id[:2] == "T:" {
		return id[2:], true
	}
	return "", false
}

func trimClip(id string) (string, bool) {
	if len(id) > 2 && id[:2] == "C:" {
		return id[2:], true
	}
	return "", false
}

func topoOrder(g *graph.Graph) []string {
	order, stuck := g.TopoOrder()
	// Cycle members are still evaluated so their faults stay locatable, but
	// they are appended last and cannot unblock anything.
	return append(order, stuck...)
}
