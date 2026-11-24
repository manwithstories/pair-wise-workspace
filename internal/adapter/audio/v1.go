// Package audio implements the versioned audio metadata adapter (v1).
// It normalises channel layouts and sample rates across segments with an
// explicit policy: losslessly mappable layouts are normalised with a
// recorded decision; unmappable ones are isolated with original metadata
// preserved. Nothing is silently overwritten.
package audio

import (
	"fmt"

	"media-check/internal/model"
)

const (
	AdapterID = "audio-meta"
	Version   = "v1"
)

// Adapter is the audio metadata adapter v1.
type Adapter struct{}

func (Adapter) ID() string      { return AdapterID }
func (Adapter) Version() string { return Version }

// Meta describes the audio essence of one segment.
type Meta struct {
	Channels   int
	Layout     string // supplier-declared layout tag, "" = undeclared
	SampleRate int
}

// Decision is one traceable normalisation action.
type Decision struct {
	Segment  int
	Action   string // "normalise-layout", "fill-layout", "isolate"
	Reason   string
	Original Meta
	Result   Meta // zero value when isolated
}

// Result of normalising a batch of segment metadata.
type Result struct {
	Normalized []Meta       // per segment; zero value when isolated
	Isolated   map[int]bool // segments excluded from the normalised stream
	Decisions  []Decision
	Faults     model.Faults
}

// canonicalLayouts maps accepted aliases to a canonical layout tag.
// Only true aliases are listed: mapping never drops or remixes channels.
var canonicalLayouts = map[string]string{
	"mono": "mono", "1.0": "mono", "1ch": "mono",
	"stereo": "stereo", "2.0": "stereo", "2ch": "stereo",
}

// layoutForChannels fills an undeclared layout from the channel count
// when the mapping is unambiguous.
func layoutForChannels(ch int) (string, bool) {
	switch ch {
	case 1:
		return "mono", true
	case 2:
		return "stereo", true
	}
	return "", false
}

// Normalize applies the batch policy: the reference is the first
// non-isolated segment's canonical layout and sample rate. A segment
// whose layout or rate cannot be mapped losslessly onto the reference is
// isolated; its original metadata is kept in the decision log.
func (a Adapter) Normalize(metas []Meta) Result {
	res := Result{
		Normalized: make([]Meta, len(metas)),
		Isolated:   map[int]bool{},
	}
	var ref Meta
	var haveRef bool

	for i, m := range metas {
		orig := m
		// Step 1: resolve the layout tag without altering channels.
		if m.Layout == "" {
			if lay, ok := layoutForChannels(m.Channels); ok {
				m.Layout = lay
				res.Decisions = append(res.Decisions, Decision{
					Segment: i, Action: "fill-layout",
					Reason:   fmt.Sprintf("layout undeclared, filled from %d channels", m.Channels),
					Original: orig, Result: m,
				})
			}
		} else if canon, ok := canonicalLayouts[m.Layout]; ok {
			if canon != m.Layout {
				m.Layout = canon
				res.Decisions = append(res.Decisions, Decision{
					Segment: i, Action: "normalise-layout",
					Reason:   "layout alias mapped to canonical tag",
					Original: orig, Result: m,
				})
			}
		}
		// Step 2: consistency between layout and channel count.
		if expect, ok := layoutForChannels(m.Channels); ok && m.Layout != "" && m.Layout != expect {
			res.isolate(a, i, orig, fmt.Sprintf(
				"layout %q conflicts with %d channels", m.Layout, m.Channels))
			continue
		}
		// Step 3: reference match. Unmappable rate/layout → isolate.
		if !haveRef {
			ref = m
			haveRef = true
		} else if m.Layout != ref.Layout || m.SampleRate != ref.SampleRate {
			res.isolate(a, i, orig, fmt.Sprintf(
				"layout/rate %s@%d cannot losslessly map to reference %s@%d",
				m.Layout, m.SampleRate, ref.Layout, ref.SampleRate))
			continue
		}
		res.Normalized[i] = m
	}
	return res
}

func (r *Result) isolate(a Adapter, seg int, orig Meta, reason string) {
	r.Isolated[seg] = true
	r.Decisions = append(r.Decisions, Decision{
		Segment: seg, Action: "isolate", Reason: reason, Original: orig,
	})
	r.Faults = append(r.Faults, model.Fault{
		Code: model.FaultAudioIsolated, Adapter: a.ID() + "@" + a.Version(),
		Segment: seg, Detail: reason,
	})
}
