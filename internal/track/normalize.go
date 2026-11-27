package track

import (
	"fmt"

	"avrepair/internal/clock"
)

// TargetSampleRate is the single normalized audio rate (Hz).
const TargetSampleRate = 48_000

// layoutChannels maps a declared layout label to its canonical channel count.
// Unknown labels are deliberately absent so they are isolated, not guessed.
var layoutChannels = map[string]int{
	"mono":   1,
	"stereo": 2,
	"3.0":    3,
	"5.1":    6,
	"7.1":    8,
}

// Normalized is the output of the normalization pass for one node.
type Normalized struct {
	ID           string
	Layout       string
	Channels     int
	SampleRate   int
	Resampled    bool
	Color        ColorMeta
	ColorConflict bool
	Isolated     bool
	Errors       []*clock.TrackError
}

// Normalize walks nodes in topological order. It mutates a defensive copy of
// each node, leaving the graph input untouched, and records every conflict as a
// structured error carrying the track ID and repair action.
func Normalize(g *Graph, order []string) ([]*Normalized, []*clock.TrackError) {
	var (
		out   []*Normalized
		errs  []*clock.TrackError
	)
	for _, id := range order {
		node, _ := g.NodeByID(id)
		nz := &Normalized{ID: id, SampleRate: node.SampleRate, Color: node.Color}
		switch node.Kind {
		case KindAudio:
			normalizeAudio(node, nz)
		case KindVideo:
			normalizeVideo(g, node, nz)
		default:
			nz.Layout = node.Layout
		}
		errs = append(errs, nz.Errors...)
		out = append(out, nz)
	}
	return out, errs
}

// normalizeAudio resolves channel layout then sample rate. An unknown layout
// isolates the whole node so downstream consumers never see an ambiguous mix.
func normalizeAudio(node *Node, nz *Normalized) {
	want, known := layoutChannels[node.Layout]
	if !known {
		nz.Isolated = true
		nz.Layout = node.Layout
		nz.Errors = append(nz.Errors, clock.NewError(clock.CodeUnknownLayout,
			node.ID, clock.Interval{},
			fmt.Sprintf("unknown channel layout %q; track isolated", node.Layout)).
			WithAction(clock.ActionIsolated))
		return
	}
	nz.Layout = node.Layout
	nz.Channels = want
	if node.Channels != 0 && node.Channels != want {
		// Declared layout wins; the channel count is remapped to it.
		nz.Errors = append(nz.Errors, clock.NewError(clock.CodeUnknownLayout,
			node.ID, clock.Interval{},
			fmt.Sprintf("channel count %d conflicts with layout %q (%d); remapped",
				node.Channels, node.Layout, want)).
			WithAction(clock.ActionMapped))
	}
	if node.SampleRate != TargetSampleRate {
		nz.Errors = append(nz.Errors, clock.NewError(clock.CodeSampleRate,
			node.ID, clock.Interval{},
			fmt.Sprintf("sample rate %d Hz normalized to %d Hz",
				node.SampleRate, TargetSampleRate)).
			WithAction(clock.ActionResampled))
		nz.Resampled = true
		nz.SampleRate = TargetSampleRate
	}
}

// normalizeVideo preserves the source color label and flags conflicts against
// already-normalized upstream dependencies in the same graph.
func normalizeVideo(g *Graph, node *Node, nz *Normalized) {
	nz.Color = node.Color
	for _, depID := range node.DependsOn {
		dep, ok := g.NodeByID(depID)
		if !ok || dep.Kind != KindVideo {
			continue
		}
		if dep.Color.Source != "" && node.Color.Source != "" &&
			dep.Color.Source != node.Color.Source {
			nz.ColorConflict = true
			nz.Errors = append(nz.Errors, clock.NewError(clock.CodeColorConflict,
				node.ID, clock.Interval{},
				fmt.Sprintf("color source %q conflicts with upstream %q (%q)",
					node.Color.Source, depID, dep.Color.Source)).
				WithAction(clock.ActionNudged))
		}
		if dep.Color.Space != "" && node.Color.Space != "" &&
			dep.Color.Space != node.Color.Space {
			nz.ColorConflict = true
			nz.Errors = append(nz.Errors, clock.NewError(clock.CodeColorConflict,
				node.ID, clock.Interval{},
				fmt.Sprintf("color space %q conflicts with upstream %q (%q)",
					node.Color.Space, depID, dep.Color.Space)).
				WithAction(clock.ActionNudged))
		}
	}
}
