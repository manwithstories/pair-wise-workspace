package model

import "fmt"

// Kind classifies a timeline track.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
	KindProxy Kind = "proxy"
	KindData  Kind = "data"
)

// Modulus33 is the default extended transport clock modulus (2^33 ticks).
const Modulus33 int64 = 1 << 33

// Capture describes the RTP capture that accompanies a timeline.
type Capture struct {
	Path      string `json:"path"`
	ClockRate uint32 `json:"clock_rate"`
	Window    int    `json:"window"`
}

// Track is one media track of the archive timeline.
type Track struct {
	ID           string   `json:"id"`
	Kind         Kind     `json:"kind"`
	Codec        string   `json:"codec"`
	Source       string   `json:"source"`
	Timescale    uint32   `json:"timescale"`
	ClockOrigin  int64    `json:"clock_origin"`
	ClockModulus int64    `json:"clock_modulus,omitempty"`
	NominalRate  Rational `json:"nominal_rate,omitempty"`
	DependsOn    []string `json:"depends_on,omitempty"`
}

// Modulus returns the configured wrap modulus, defaulting to 2^33.
func (t Track) Modulus() int64 {
	if t.ClockModulus > 1 {
		return t.ClockModulus
	}
	return Modulus33
}

// Clip is a cut boundary on one track.
type Clip struct {
	ID            string   `json:"id"`
	Track         string   `json:"track"`
	Start         Rational `json:"start"`
	Duration      Rational `json:"duration"`
	SourceIn      Rational `json:"source_in"`
	ExpectedStart Rational `json:"expected_start"`
	Parent        string   `json:"parent,omitempty"`
}

// Transition joins two clips with a crossfade.
type Transition struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Duration Rational `json:"duration"`
	Center   Rational `json:"center"`
}

// Timeline is the fixture/input document.
type Timeline struct {
	Version     int          `json:"version"`
	Seed        int64        `json:"seed"`
	Description string       `json:"description,omitempty"`
	Capture     Capture      `json:"capture"`
	Tracks      []Track      `json:"tracks"`
	Clips       []Clip       `json:"clips"`
	Transitions []Transition `json:"transitions"`
}

// FindTrack returns the track with the given ID.
func (tl *Timeline) FindTrack(id string) (Track, bool) {
	for _, t := range tl.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return Track{}, false
}

// FindClip returns the clip with the given ID.
func (tl *Timeline) FindClip(id string) (Clip, bool) {
	for _, c := range tl.Clips {
		if c.ID == id {
			return c, true
		}
	}
	return Clip{}, false
}

// FindTransition returns the transition with the given ID.
func (tl *Timeline) FindTransition(id string) (Transition, bool) {
	for _, x := range tl.Transitions {
		if x.ID == id {
			return x, true
		}
	}
	return Transition{}, false
}

// Validate rejects malformed input before any scanning starts.
func (tl *Timeline) Validate() []error {
	var errs []error
	if tl.Version != 1 {
		errs = append(errs, fmt.Errorf("unsupported timeline version %d", tl.Version))
	}
	if len(tl.Tracks) == 0 {
		errs = append(errs, fmt.Errorf("timeline has no tracks"))
	}
	trackIndex := make(map[string]bool, len(tl.Tracks))
	for _, t := range tl.Tracks {
		trackIndex[t.ID] = true
	}
	clipIndex := make(map[string]bool, len(tl.Clips))
	for _, c := range tl.Clips {
		clipIndex[c.ID] = true
	}
	seen := map[string]bool{}
	for _, t := range tl.Tracks {
		if t.ID == "" || seen[t.ID] {
			errs = append(errs, fmt.Errorf("track id %q is empty or duplicated", t.ID))
		}
		seen[t.ID] = true
		if t.Timescale == 0 {
			errs = append(errs, fmt.Errorf("track %s has zero timescale", t.ID))
		}
		if t.Source == "" {
			errs = append(errs, fmt.Errorf("track %s has no source", t.ID))
		}
		if t.ClockOrigin < 0 {
			errs = append(errs, fmt.Errorf("track %s has negative clock origin", t.ID))
		}
		for _, dep := range t.DependsOn {
			if !trackIndex[dep] {
				errs = append(errs, fmt.Errorf("track %s depends on unknown track %s", t.ID, dep))
			}
		}
	}
	clipSeen := map[string]bool{}
	for _, c := range tl.Clips {
		if c.ID == "" || clipSeen[c.ID] {
			errs = append(errs, fmt.Errorf("clip id %q is empty or duplicated", c.ID))
		}
		clipSeen[c.ID] = true
		if !trackIndex[c.Track] {
			errs = append(errs, fmt.Errorf("clip %s references unknown track %s", c.ID, c.Track))
		}
		if c.Duration.Sign() <= 0 {
			errs = append(errs, fmt.Errorf("clip %s has non-positive duration", c.ID))
		}
		if c.SourceIn.Sign() < 0 {
			errs = append(errs, fmt.Errorf("clip %s has negative source in-point", c.ID))
		}
	}
	for _, c := range tl.Clips {
		if c.Parent != "" {
			if !clipIndex[c.Parent] {
				errs = append(errs, fmt.Errorf("clip %s has missing parent %s", c.ID, c.Parent))
			}
		}
	}
	for _, x := range tl.Transitions {
		if x.ID == "" {
			errs = append(errs, fmt.Errorf("transition with empty id"))
		}
		if !clipIndex[x.From] {
			errs = append(errs, fmt.Errorf("transition %s references missing clip %s", x.ID, x.From))
		}
		if !clipIndex[x.To] {
			errs = append(errs, fmt.Errorf("transition %s references missing clip %s", x.ID, x.To))
		}
		if x.Duration.Sign() <= 0 {
			errs = append(errs, fmt.Errorf("transition %s has non-positive duration", x.ID))
		}
	}
	return errs
}
