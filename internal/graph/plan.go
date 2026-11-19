package graph

import (
	"fmt"
	"strings"

	"archivearchivist/internal/model"
)

// Build constructs the graph for a timeline and reports structural faults:
// cycles, missing parents and audio tracks that do not cover their dependent
// clips. The graph is still returned so independent branches can be planned.
func Build(tl *model.Timeline) (*Graph, model.FaultList) {
	g := New()
	var faults model.FaultList

	// Index the timeline once; the per-clip lookups below would otherwise be
	// quadratic on a timeline with many clips.
	trackIndex := make(map[string]bool, len(tl.Tracks))
	for _, t := range tl.Tracks {
		trackIndex[t.ID] = true
	}
	clipIndex := make(map[string]bool, len(tl.Clips))
	for _, c := range tl.Clips {
		clipIndex[c.ID] = true
	}

	for _, t := range tl.Tracks {
		label := string(t.Kind) + ":" + t.Codec
		g.AddNode(Node{ID: "T:" + t.ID, Kind: NodeTrack, Track: t.ID, Label: label})
	}
	for _, c := range tl.Clips {
		kind := NodeClip
		if !trackIndex[c.Track] {
			faults.Add(model.Fault{
				Code: model.CodeMissingParent, Severity: model.SevError, Clip: c.ID,
				Detail: "clip belongs to unknown track " + c.Track,
			})
		}
		g.AddNode(Node{
			ID: "C:" + c.ID, Kind: kind, Track: c.Track,
			Label: c.ID + "@" + c.Track,
		})
		g.AddEdge(Edge{From: "T:" + c.Track, To: "C:" + c.ID, Reason: "track media"})
	}

	for _, t := range tl.Tracks {
		for _, dep := range t.DependsOn {
			g.AddEdge(Edge{From: "T:" + dep, To: "T:" + t.ID, Reason: "track dependency"})
		}
	}

	for _, c := range tl.Clips {
		if c.Parent == "" {
			continue
		}
		if !clipIndex[c.Parent] {
			faults.Add(model.Fault{
				Code: model.CodeMissingParent, Severity: model.SevError, Clip: c.ID,
				Detail: "parent clip " + c.Parent + " does not exist",
			})
			continue
		}
		g.AddEdge(Edge{From: "C:" + c.Parent, To: "C:" + c.ID, Reason: "clip parent"})
	}

	for _, x := range tl.Transitions {
		if !clipIndex[x.From] || !clipIndex[x.To] {
			continue // Validate already reported this
		}
		// A crossfade cannot be resolved until both sides are recoverable.
		g.AddNode(Node{ID: "X:" + x.ID, Kind: NodeClip, Label: x.ID})
		g.AddEdge(Edge{From: "C:" + x.From, To: "X:" + x.ID, Reason: "crossfade source"})
		g.AddEdge(Edge{From: "C:" + x.To, To: "X:" + x.ID, Reason: "crossfade target"})
	}

	// Audio coverage: an audio clip on a dependent track must cover the span
	// of the video clip it accompanies, otherwise downstream cuts would run
	// past the end of the audio bed.
	faults.Add(audioCoverage(tl)...)

	for _, cycle := range g.Cycles() {
		faults.Add(model.Fault{
			Code: model.CodeDependencyCycle, Severity: model.SevFatal,
			Detail: "dependency cycle: " + strings.Join(cycle, " -> "),
		})
	}
	faults.Sort()
	return g, faults
}

// audioCoverage verifies that a dependent audio track's clips are long enough
// to cover the clips on the track they follow. Clips are indexed by track once
// so the comparison stays linear in the number of clips.
func audioCoverage(tl *model.Timeline) model.FaultList {
	var faults model.FaultList
	byTrack := map[string][]model.Clip{}
	for _, c := range tl.Clips {
		byTrack[c.Track] = append(byTrack[c.Track], c)
	}
	for _, t := range tl.Tracks {
		if t.Kind != model.KindAudio {
			continue
		}
		for _, dep := range t.DependsOn {
			for _, vc := range byTrack[dep] {
				end := vc.Start.Add(vc.Duration)
				covered := false
				for _, ac := range byTrack[t.ID] {
					if ac.Start.Cmp(vc.Start) <= 0 && ac.Start.Add(ac.Duration).Cmp(end) >= 0 {
						covered = true
						break
					}
				}
				if !covered {
					faults.Add(model.Fault{
						Code: model.CodeAudioUnderCover, Severity: model.SevError,
						Track: t.ID, Clip: vc.ID,
						Detail: fmt.Sprintf("audio track %s does not cover clip %s [%s,%s)",
							t.ID, vc.ID, vc.Start, end),
					})
				}
			}
		}
	}
	return faults
}
