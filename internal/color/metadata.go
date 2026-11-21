// Package color normalizes colour metadata carried by different sources and
// records disagreements.  Normalization is deterministic and total: unknown
// enumerations become "unspecified" rather than an error, while conflicting
// known values are never silently overwritten.  The caller must choose a
// policy that either rejects the conflict or picks a declared priority.
package color

import (
	"fmt"
	"sort"
	"strings"

	"mediapack/internal/fault"
)

// Triplet is the three CICP values plus the full-range flag.
type Triplet struct {
	Primaries uint8 `json:"primaries"`
	Transfer  uint8 `json:"transfer"`
	Matrix    uint8 `json:"matrix"`
	FullRange bool  `json:"full_range"`
	// Unknown is set when the source used a non-standard signal type that we
	// normalized to unspecified values.  The original token is preserved.
	Unknown      bool   `json:"unknown"`
	OriginalType string `json:"original_type,omitempty"`
}

// Source is one declaration of colour metadata.
type Source struct {
	// ID names the declaring track or file; always preserved in reports.
	ID      string
	Triplet Triplet
	// Tag names the codepoint used by the container (nclx, colr, SEI, ...).
	Tag string
}

// Policy selects conflict handling.
type Policy string

const (
	PolicyBlock  Policy = "block"
	PolicyPrefer Policy = "prefer"
)

// Config is the operator's explicit decision.
type Config struct {
	Policy       Policy
	PreferSource string
}

// Conflict keeps both declarations.
type Conflict struct {
	Field    string
	Values   []Value
	Resolved Value
	Chosen   bool
}

// Value is one source's declaration of a field.
type Value struct {
	Source string
	Tag    string
	Value  string
}

// Result is the normalized, policy-resolved metadata.
type Result struct {
	Triplet   Triplet
	Source    string
	Conflicts []Conflict
	NormNotes []string
}

// Limits bounds the number of metadata sources.
type Limits struct{ MaxSources int }

// DefaultLimits is deliberately modest.
func DefaultLimits() Limits { return Limits{MaxSources: 16} }

var primariesNames = map[uint8]string{
	1: "bt709", 2: "unspecified", 4: "bt470m/bt470bg", 5: "bt470bg",
	6: "smpte170m", 7: "smpte240m", 8: "film", 9: "bt2020", 10: "smpte428",
	11: "smpte431", 12: "smpte432", 22: "ebu3213",
}

var transferNames = map[uint8]string{
	1: "bt709", 2: "unspecified", 4: "gamma22", 5: "gamma28", 6: "smpte170m",
	7: "smpte240m", 8: "linear", 9: "log100", 10: "log316", 11: "iec61966-2-4",
	12: "bt1361", 13: "iec61966-2-1", 14: "bt2020-10", 15: "bt2020-12",
	16: "pq", 17: "smpte2084", 18: "hlg",
}

var matrixNames = map[uint8]string{
	0: "rgb", 1: "bt709", 2: "unspecified", 4: "fcc", 5: "bt470bg",
	6: "smpte170m", 7: "smpte240m", 8: "ycgco", 9: "bt2020nc", 10: "bt2020c",
	11: "smpte2085", 12: "chroma-nc", 13: "chroma-c",
}

// NameOf returns a stable human-readable name for each CICP value.
func NameOf(field string, v uint8) string {
	table := matrixNames
	switch field {
	case "primaries":
		table = primariesNames
	case "transfer":
		table = transferNames
	}
	if name, ok := table[v]; ok {
		return name
	}
	return fmt.Sprintf("cicp-%d", v)
}

// Normalize converts a source declaration into canonical CICP values.
// Recognized container tokens are mapped; anything else becomes unspecified
// with Unknown set and the original token preserved.
func Normalize(src Source) (Triplet, string) {
	t := src.Triplet
	note := ""
	if t.OriginalType != "" {
		canon, ok := namedTriplets[strings.ToLower(t.OriginalType)]
		if !ok {
			t = Triplet{Primaries: 2, Transfer: 2, Matrix: 2, FullRange: false,
				Unknown: true, OriginalType: src.Triplet.OriginalType}
			note = fmt.Sprintf("%s: unknown colour type %q normalized to unspecified", src.ID, src.Triplet.OriginalType)
		} else {
			t = canon
		}
	}
	// Unrecognized CICP enumerations collapse to "unspecified" (2), which is
	// the standard's own escape value; the declared value is kept in notes.
	if !t.Unknown {
		if _, ok := primariesNames[t.Primaries]; !ok {
			note = join(note, fmt.Sprintf("%s: primaries %d has no known meaning, using unspecified", src.ID, t.Primaries))
			t.Primaries = 2
		}
		if _, ok := transferNames[t.Transfer]; !ok {
			note = join(note, fmt.Sprintf("%s: transfer %d has no known meaning, using unspecified", src.ID, t.Transfer))
			t.Transfer = 2
		}
		if _, ok := matrixNames[t.Matrix]; !ok {
			note = join(note, fmt.Sprintf("%s: matrix %d has no known meaning, using unspecified", src.ID, t.Matrix))
			t.Matrix = 2
		}
	}
	return t, note
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

var namedTriplets = map[string]Triplet{
	"bt709":     {Primaries: 1, Transfer: 1, Matrix: 1},
	"bt2020":    {Primaries: 9, Transfer: 14, Matrix: 9},
	"bt2020pq":  {Primaries: 9, Transfer: 16, Matrix: 9},
	"bt2020hlg": {Primaries: 9, Transfer: 18, Matrix: 9},
	"srgb":      {Primaries: 1, Transfer: 13, Matrix: 0},
	"p3d65":     {Primaries: 12, Transfer: 13, Matrix: 0},
}

// Resolve normalizes sources and applies cfg to any conflict.
func Resolve(sources []Source, cfg Config, limits Limits) (*Result, error) {
	if len(sources) == 0 {
		return nil, fault.New(fault.ClassInput, "color", "*", "no-color-metadata", "no colour metadata supplied")
	}
	if len(sources) > limits.MaxSources {
		return nil, fault.New(fault.ClassLimit, "color", "*", "too-many-sources",
			fmt.Sprintf("%d colour sources exceeds limit %d", len(sources), limits.MaxSources))
	}
	switch cfg.Policy {
	case PolicyBlock, PolicyPrefer, "":
	default:
		return nil, fault.New(fault.ClassUsage, "color", "*", "unknown-policy",
			fmt.Sprintf("unknown colour policy %q", cfg.Policy))
	}

	ordered := append([]Source(nil), sources...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	norm := make([]Triplet, len(ordered))
	res := &Result{}
	for i, s := range ordered {
		if s.ID == "" {
			return nil, fault.New(fault.ClassInput, "color", "*", "invalid-source", "colour source has empty id")
		}
		t, note := Normalize(s)
		norm[i] = t
		if note != "" {
			res.NormNotes = append(res.NormNotes, note)
		}
	}

	base := norm[0]
	res.Triplet = base
	res.Source = ordered[0].ID

	fields := []struct {
		name string
		get  func(Triplet) string
	}{
		{"primaries", func(t Triplet) string { return NameOf("primaries", t.Primaries) }},
		{"transfer", func(t Triplet) string { return NameOf("transfer", t.Transfer) }},
		{"matrix", func(t Triplet) string { return NameOf("matrix", t.Matrix) }},
		{"full_range", func(t Triplet) string { return fmt.Sprintf("%t", t.FullRange) }},
	}

	var conflicts []Conflict
	for _, f := range fields {
		var vals []Value
		for i := range norm {
			v := f.get(norm[i])
			if v == f.get(base) {
				continue
			}
			vals = append(vals, Value{Source: ordered[i].ID, Tag: ordered[i].Tag, Value: v})
		}
		if len(vals) == 0 {
			continue
		}
		vals = append([]Value{{Source: ordered[0].ID, Tag: ordered[0].Tag, Value: f.get(base)}}, vals...)
		conflicts = append(conflicts, Conflict{Field: f.name, Values: vals})
	}

	if len(conflicts) == 0 {
		return res, nil
	}

	switch cfg.Policy {
	case PolicyBlock, "":
		c := conflicts[0]
		return nil, fault.New(fault.ClassDependency, "color", c.Values[1].Source, "color-conflict",
			fmt.Sprintf("%s: %s=%s conflicts with %s=%s", c.Field,
				c.Values[0].Source, c.Values[0].Value, c.Values[1].Source, c.Values[1].Value)).
			WithBlockedBy("color:" + c.Values[1].Source)
	case PolicyPrefer:
		idx := -1
		for i := range ordered {
			if ordered[i].ID == cfg.PreferSource {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, fault.New(fault.ClassUsage, "color", cfg.PreferSource, "prefer-source-missing",
				"preferred colour source does not name a supplied source")
		}
		res.Triplet = norm[idx]
		res.Source = ordered[idx].ID
		for i := range conflicts {
			c := &conflicts[i]
			for _, v := range c.Values {
				if v.Source == ordered[idx].ID {
					c.Resolved = v
					c.Chosen = true
					break
				}
			}
		}
		res.Conflicts = conflicts
		return res, nil
	}
	return res, nil
}

// String renders a normalized triplet for manifests.
func (t Triplet) String() string {
	if t.Unknown {
		return fmt.Sprintf("unknown(%s)", t.OriginalType)
	}
	rng := "limited"
	if t.FullRange {
		rng = "full"
	}
	return fmt.Sprintf("%s/%s/%s/%s", NameOf("primaries", t.Primaries), NameOf("transfer", t.Transfer), NameOf("matrix", t.Matrix), rng)
}
