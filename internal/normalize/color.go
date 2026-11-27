package normalize

import (
	"fmt"

	"mediad/internal/media"
)

// ColorInfo is the per-track color description: primaries, transfer
// function, matrix and range.
type ColorInfo struct {
	Primaries string
	Transfer  string
	Matrix    string
	Range     string
}

func (c ColorInfo) fields() [][2]string {
	return [][2]string{
		{"primaries", c.Primaries},
		{"transfer", c.Transfer},
		{"matrix", c.Matrix},
		{"range", c.Range},
	}
}

func (c *ColorInfo) set(name, value string) {
	switch name {
	case "primaries":
		c.Primaries = value
	case "transfer":
		c.Transfer = value
	case "matrix":
		c.Matrix = value
	case "range":
		c.Range = value
	}
}

// ColorRegistry keeps the confirmed color description of every track.
// Conflicting or missing values are written to the diagnostics summary;
// confirmed values are never overwritten by unconfirmed defaults.
type ColorRegistry struct {
	tracks map[uint32]*ColorInfo
	diag   []string
}

func NewColorRegistry() *ColorRegistry {
	return &ColorRegistry{tracks: make(map[uint32]*ColorInfo)}
}

// Observe merges a newly signaled description for ssrc.
func (r *ColorRegistry) Observe(ssrc uint32, c ColorInfo) *media.Fault {
	cur, ok := r.tracks[ssrc]
	if !ok {
		cp := c
		r.tracks[ssrc] = &cp
		for _, f := range c.fields() {
			if f[1] == "" {
				r.diag = append(r.diag,
					fmt.Sprintf("track %d: color field %s missing, left unset", ssrc, f[0]))
			}
		}
		return nil
	}
	conflict := false
	for _, f := range c.fields() {
		name, v := f[0], f[1]
		var curV string
		switch name {
		case "primaries":
			curV = cur.Primaries
		case "transfer":
			curV = cur.Transfer
		case "matrix":
			curV = cur.Matrix
		case "range":
			curV = cur.Range
		}
		switch {
		case v == "":
			r.diag = append(r.diag,
				fmt.Sprintf("track %d: color field %s missing in update, keeping %q", ssrc, name, curV))
		case curV == "":
			cur.set(name, v)
		case curV != v:
			conflict = true
			r.diag = append(r.diag,
				fmt.Sprintf("track %d: color field %s conflict %q vs %q, keeping confirmed %q",
					ssrc, name, curV, v, curV))
		}
	}
	if conflict {
		return &media.Fault{
			Kind: media.FaultColorConflict, Op: "normalize.color", Track: ssrc,
			Msg: "conflicting color metadata, confirmed values kept",
		}
	}
	return nil
}

// Confirmed returns the confirmed description for ssrc.
func (r *ColorRegistry) Confirmed(ssrc uint32) ColorInfo {
	if c, ok := r.tracks[ssrc]; ok {
		return *c
	}
	return ColorInfo{}
}

// Diagnostics returns the diagnostic summary lines.
func (r *ColorRegistry) Diagnostics() []string {
	return append([]string(nil), r.diag...)
}

// Snapshot returns the confirmed descriptions and diagnostics for
// checkpointing.
func (r *ColorRegistry) Snapshot() (map[uint32]ColorInfo, []string) {
	m := make(map[uint32]ColorInfo, len(r.tracks))
	for k, v := range r.tracks {
		m[k] = *v
	}
	return m, r.Diagnostics()
}

// Restore replaces the registry state, e.g. after a checkpoint load.
func (r *ColorRegistry) Restore(confirmed map[uint32]ColorInfo, diag []string) {
	r.tracks = make(map[uint32]*ColorInfo, len(confirmed))
	for k, v := range confirmed {
		cp := v
		r.tracks[k] = &cp
	}
	r.diag = append([]string(nil), diag...)
}
