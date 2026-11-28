package demux

import (
	"fmt"
	"sort"
	"strings"

	"github.com/asticode/go-astiav"

	"avpipe/internal/media"
)

// ErrKind identifies a mapping failure class.
type ErrKind string

const (
	ErrMissing   ErrKind = "missing"
	ErrDuplicate ErrKind = "duplicate"
	ErrUnknown   ErrKind = "unknown"
	ErrInvalid   ErrKind = "invalid"
)

// MappingError describes a mapping problem with enough detail to point at the
// track and the source index.
type MappingError struct {
	Kind    ErrKind
	TrackID string
	Source  int
	Message string
	Origin  media.Origin
}

func (e *MappingError) Error() string { return "mapping " + string(e.Kind) + ": " + e.Message }

// MediaKind classifies a mapping failure for the terminal exit code. A
// mapping rule may be malformed (policy) or it may name a track the demuxer
// disabled because its display metadata was illegal (corrupt input).
func (e *MappingError) MediaKind() media.ErrorKind {
	if e.Kind == ErrInvalid {
		return media.KindCorrupt
	}
	return media.KindPolicy
}

// MapSpec is one operator-supplied mapping rule: the stable output track ID
// and the source stream index it comes from.
type MapSpec struct {
	TrackID string
	Source  int
}

// Mapped pairs a stable track ID with the actual track it resolved to.
type Mapped struct {
	TrackID string `json:"track_id"`
	Source  int    `json:"source"`
	Kind    media.TrackKind `json:"kind"`
	Codec   string `json:"codec"`
	TimeBase media.Rational `json:"time_base"`
}

// Mapping is the resolved, validated mapping for one input.
type Mapping struct {
	Input        string   `json:"input"`
	Container    string   `json:"container"`
	Tracks       []Mapped `json:"tracks"`
	Disabled     []string `json:"disabled,omitempty"` // tracks with invalid metadata
	ByID         map[string]Mapped
	BySource     map[int]Mapped
}

// Resolve validates operator mapping rules against the tracks an input
// actually offers. Every rule must name an existing source index; every
// enabled video/audio track must appear exactly once unless the caller opts
// to ignore unmatched tracks. Duplicate output IDs or duplicate source indices
// are errors, and a track whose display metadata is invalid is disabled
// automatically.
//
// A nil specs slice means "map every usable track by its stable ID", which is
// the deterministic default the selfcheck and tests rely on.
func (in *Input) Mapping(specs []MapSpec, allowUnmatched bool) (*Mapping, error) {
	m := &Mapping{
		Input: in.Path, Container: string(in.Container),
		ByID: map[string]Mapped{}, BySource: map[int]Mapped{},
	}
	for _, t := range in.Tracks {
		if t.Display.Invalid {
			m.Disabled = append(m.Disabled, t.ID)
		}
	}
	// Build the effective rules.
	rules := specs
	if rules == nil {
		rules = make([]MapSpec, 0, len(in.Tracks))
		for _, t := range in.Tracks {
			if t.Display.Invalid {
				continue
			}
			rules = append(rules, MapSpec{TrackID: t.ID, Source: t.Source})
		}
	}
	if len(rules) == 0 {
		// If every candidate was disabled by illegal display metadata, the
		// input itself is corrupt: report the reason, not a generic "nothing
		// to map" that hides it.
		if len(m.Disabled) > 0 {
			for _, t := range in.Tracks {
				if t.Display.Invalid {
					return nil, &MappingError{Kind: ErrInvalid, TrackID: t.ID, Source: t.Source,
						Message: fmt.Sprintf("track %s disabled: %s", t.ID, t.Display.InvalidReason),
						Origin:  media.Origin{File: in.Path, TrackID: t.ID, Stage: "demux"}}
				}
			}
		}
		return nil, &MappingError{Kind: ErrMissing, Message: "no mapping rules and no usable tracks",
			Origin: media.Origin{File: in.Path, Stage: "demux"}}
	}
	for _, r := range rules {
		if strings.TrimSpace(r.TrackID) == "" {
			return nil, &MappingError{Kind: ErrInvalid, Source: r.Source,
				Message: "empty output track id",
				Origin:  media.Origin{File: in.Path, Stage: "demux"}}
		}
		if _, dup := m.ByID[r.TrackID]; dup {
			return nil, &MappingError{Kind: ErrDuplicate, TrackID: r.TrackID,
				Message: fmt.Sprintf("duplicate output track id %q", r.TrackID),
				Origin:  media.Origin{File: in.Path, TrackID: r.TrackID, Stage: "demux"}}
		}
		if _, dup := m.BySource[r.Source]; dup {
			return nil, &MappingError{Kind: ErrDuplicate, Source: r.Source,
				Message: fmt.Sprintf("source index %d mapped twice", r.Source),
				Origin:  media.Origin{File: in.Path, Offset: 0, Stage: "demux"}}
		}
		tr, ok := in.trackBySource(r.Source)
		if !ok {
			return nil, &MappingError{Kind: ErrUnknown, TrackID: r.TrackID, Source: r.Source,
				Message: fmt.Sprintf("source index %d is not a usable track", r.Source),
				Origin:  media.Origin{File: in.Path, TrackID: r.TrackID, Stage: "demux"}}
		}
		if tr.Display.Invalid {
			return nil, &MappingError{Kind: ErrInvalid, TrackID: r.TrackID, Source: r.Source,
				Message: fmt.Sprintf("track %s disabled: %s", tr.ID, tr.Display.InvalidReason),
				Origin:  media.Origin{File: in.Path, TrackID: tr.ID, Stage: "demux"}}
		}
		mp := Mapped{TrackID: r.TrackID, Source: tr.Source, Kind: tr.Kind, Codec: tr.Codec, TimeBase: tr.TimeBase}
		m.ByID[r.TrackID] = mp
		m.BySource[r.Source] = mp
		m.Tracks = append(m.Tracks, mp)
	}
	// Every mapped source must be a real track; every usable track must be
	// mapped unless unmatched tracks are explicitly allowed.
	if !allowUnmatched {
		if err := in.requireAllMapped(m, rules); err != nil {
			return nil, err
		}
	}
	sort.Slice(m.Tracks, func(i, j int) bool { return m.Tracks[i].TrackID < m.Tracks[j].TrackID })
	return m, nil
}

func (in *Input) requireAllMapped(m *Mapping, rules []MapSpec) error {
	mapped := map[int]bool{}
	for _, r := range rules {
		mapped[r.Source] = true
	}
	var missing []string
	for _, t := range in.Tracks {
		if t.Display.Invalid {
			continue
		}
		if !mapped[t.Source] {
			missing = append(missing, t.ID)
		}
	}
	if len(missing) > 0 {
		return &MappingError{Kind: ErrMissing,
			Message: fmt.Sprintf("tracks not mapped: %s", strings.Join(missing, ", ")),
			Origin:  media.Origin{File: in.Path, Stage: "demux"}}
	}
	return nil
}

func (in *Input) trackBySource(idx int) (media.Track, bool) {
	for _, t := range in.Tracks {
		if t.Source == idx {
			return t, true
		}
	}
	return media.Track{}, false
}

// Track looks up a track by its stable ID.
func (in *Input) Track(id string) (media.Track, bool) {
	for _, t := range in.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return media.Track{}, false
}

// CodecParameters returns the source codec parameters for a track so the
// remux stage can copy them verbatim. The pointer is owned by the underlying
// format context and remains valid until Close.
func (in *Input) CodecParameters(trackID string) (*astiav.CodecParameters, bool) {
	for _, s := range in.fc.Streams() {
		st, ok := in.scratch[s.Index()]
		if !ok {
			continue
		}
		if st.track.ID == trackID {
			return s.CodecParameters(), true
		}
	}
	return nil, false
}
