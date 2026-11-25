// Package model defines timelines, tracks, clips and transitions.
package model

import (
	"encoding/json"
	"sort"

	"bcast/internal/fault"
	"bcast/internal/timebase"
)

// Rational is the shared exact rational time representation.
type Rational = timebase.Rational

// Timeline is the canonical JSON edit description.
type Timeline struct {
	Schema        string       `json:"schema"`
	ID            string       `json:"id"`
	FrameDuration Rational     `json:"frame_duration"`
	Duration      Rational     `json:"duration"`
	Tracks        []Track      `json:"tracks"`
	Transitions   []Transition `json:"transitions,omitempty"`
}

// Track is one video, audio or data track.
type Track struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // video | audio
	Gain  *Ratio `json:"gain,omitempty"`
	Clips []Clip `json:"clips"`
}

// Ratio allows gain to be represented exactly as numerator/denominator.
type Ratio struct {
	N int64 `json:"n"`
	D int64 `json:"d"`
}

// R converts a gain ratio to timebase.Rational.
func (r Ratio) R() Rational { return Rational{N: r.N, D: r.D}.Normalize() }

// Clip references a bounded interval in a source asset.
type Clip struct {
	ID    string   `json:"id"`
	Asset string   `json:"asset"`
	Start Rational `json:"start"`
	In    Rational `json:"in"`
	Out   Rational `json:"out"`
	Gain  *Ratio   `json:"gain,omitempty"`
}

// Transition is a dissolve between two clips.
type Transition struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"` // dissolve
	FromClip string   `json:"from_clip"`
	ToClip   string   `json:"to_clip"`
	At       Rational `json:"at"`
	Duration Rational `json:"duration"`
}

// SourceInterval is a provenance-bearing interval in one source.
type SourceInterval struct {
	Asset string   `json:"asset"`
	Start Rational `json:"start"`
	End   Rational `json:"end"`
	PTS0  uint64   `json:"pts0,omitempty"`
	PTS1  uint64   `json:"pts1,omitempty"`
}

// Segment is an executable unit planned from the timeline.
type Segment struct {
	Index        int              `json:"index"`
	Name         string           `json:"name"`
	Start        Rational         `json:"start"`
	End          Rational         `json:"end"`
	Mode         string           `json:"mode"` // copy | composite
	Sources      []SourceInterval `json:"sources"`
	Transition   *Transition      `json:"transition,omitempty"`
	KeyframeNeed []uint32         `json:"keyframe_need,omitempty"`
}

// ParseTimeline decodes and normalizes JSON timeline bytes.
func ParseTimeline(data []byte) (*Timeline, error) {
	var tl Timeline
	if err := json.Unmarshal(data, &tl); err != nil {
		return nil, fault.E(fault.TimelineInvalid, "model", "ParseTimeline", err)
	}
	if err := tl.Normalize(); err != nil {
		return nil, err
	}
	return &tl, nil
}

// Normalize reduces rational fields and gives audio tracks neutral gain.
// It does not reorder clips because clip order is editorial semantics.
func (t *Timeline) Normalize() error {
	if t.Schema == "" {
		t.Schema = "bcast.timeline/v1"
	}
	if t.ID == "" {
		return fault.E(fault.TimelineInvalid, "model", "Normalize", errString("timeline id required"))
	}
	t.FrameDuration = t.FrameDuration.Normalize()
	t.Duration = t.Duration.Normalize()
	for i := range t.Tracks {
		tr := &t.Tracks[i]
		if tr.ID == "" || tr.Kind == "" {
			return fault.E(fault.TimelineInvalid, "model", "Normalize", errString("track id and kind required"))
		}
		if tr.Kind != "video" && tr.Kind != "audio" {
			return fault.E(fault.TimelineInvalid, "model", "Normalize", errString("unsupported track kind: "+tr.Kind))
		}
		if tr.Gain != nil {
			r := tr.Gain.R()
			if r.D == 0 || r.N < 0 {
				return fault.E(fault.TimelineInvalid, "model", "Normalize", errString("invalid track gain"))
			}
			tr.Gain = &Ratio{N: r.N, D: r.D}
		}
		for j := range tr.Clips {
			c := &tr.Clips[j]
			if c.ID == "" || c.Asset == "" {
				return fault.E(fault.TimelineInvalid, "model", "Normalize", errString("clip id and asset required"))
			}
			c.Start = c.Start.Normalize()
			c.In = c.In.Normalize()
			c.Out = c.Out.Normalize()
			if timebase.Cmp(c.Out, c.In) <= 0 {
				return fault.E(fault.TimelineInvalid, "model", "Normalize", errString("clip out must exceed in: "+c.ID))
			}
			if c.Gain != nil {
				r := c.Gain.R()
				if r.D == 0 || r.N < 0 {
					return fault.E(fault.TimelineInvalid, "model", "Normalize", errString("invalid clip gain"))
				}
				c.Gain = &Ratio{N: r.N, D: r.D}
			}
		}
	}
	for i := range t.Transitions {
		tr := &t.Transitions[i]
		tr.At = tr.At.Normalize()
		tr.Duration = tr.Duration.Normalize()
		if tr.Type == "" {
			tr.Type = "dissolve"
		}
		if tr.FromClip == "" || tr.ToClip == "" || tr.Duration.N <= 0 {
			return fault.E(fault.TimelineInvalid, "model", "Normalize", errString("invalid transition"))
		}
	}
	return nil
}

// CanonicalJSON returns deterministic JSON used as part of the index key.
func (t *Timeline) CanonicalJSON() ([]byte, error) {
	cp := *t
	cp.Normalize() //nolint:errcheck // already validated by callers and ParseTimeline
	// Transitions are a set for identity purposes; sort by stable ID.
	sort.SliceStable(cp.Transitions, func(i, j int) bool { return cp.Transitions[i].ID < cp.Transitions[j].ID })
	b, err := json.Marshal(cp)
	if err != nil {
		return nil, fault.E(fault.TimelineInvalid, "model", "CanonicalJSON", err)
	}
	return b, nil
}

type stringError string

func (e stringError) Error() string { return string(e) }
func errString(s string) error      { return stringError(s) }
