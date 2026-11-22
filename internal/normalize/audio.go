// Package normalize compares channel layouts and sample rates across
// representations against the configured target. Differences are
// recorded and normalized per configuration; unknown channel layouts
// are marked not auto-fixable instead of being silently remapped.
package normalize

import "fmt"

// Diff records one normalized (or normalizable) difference.
type Diff struct {
	Representation string `json:"representation"`
	Field          string `json:"field"`
	From           string `json:"from"`
	To             string `json:"to"`
}

// Report is the serializable outcome of a normalization pass.
type Report struct {
	Diffs     []Diff   `json:"diffs"`
	Unfixable []string `json:"unfixable,omitempty"`
}

// Fixable reports whether every difference can be auto-repaired.
func (r *Report) Fixable() bool { return len(r.Unfixable) == 0 }

func (r *Report) add(rep, field, from, to string) {
	r.Diffs = append(r.Diffs, Diff{Representation: rep, Field: field, From: from, To: to})
}

// knownLayouts maps channel counts to layout names.
var knownLayouts = map[int]string{1: "mono", 2: "stereo", 6: "5.1", 8: "7.1"}

// LayoutName returns the layout name and whether it is known.
func LayoutName(channels int) (string, bool) {
	n, ok := knownLayouts[channels]
	return n, ok
}

// RepAudio is the audio view of one representation.
type RepAudio struct {
	ID         string
	Channels   int
	SampleRate int
}

// AudioTarget is the configured audio normalization target.
type AudioTarget struct {
	Channels   int `json:"channels"`
	SampleRate int `json:"sample_rate"`
}

// NormalizeAudio compares each representation with the target.
func NormalizeAudio(reps []RepAudio, target AudioTarget) Report {
	var r Report
	tLayout, tok := LayoutName(target.Channels)
	if !tok {
		r.Unfixable = append(r.Unfixable,
			fmt.Sprintf("configured target layout %d channels is unknown", target.Channels))
		return r
	}
	for _, rep := range reps {
		layout, ok := LayoutName(rep.Channels)
		if !ok {
			r.Unfixable = append(r.Unfixable,
				fmt.Sprintf("%s: unknown channel layout (%d channels), cannot auto-fix",
					rep.ID, rep.Channels))
			continue
		}
		if rep.Channels != target.Channels {
			r.add(rep.ID, "channel_layout", layout, tLayout)
		}
		if rep.SampleRate != target.SampleRate {
			r.add(rep.ID, "sample_rate",
			fmt.Sprintf("%d", rep.SampleRate), fmt.Sprintf("%d", target.SampleRate))
		}
	}
	return r
}
