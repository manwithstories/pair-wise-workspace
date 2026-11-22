package normalize

import "fmt"

// ColorMeta is the nclx colour view of one representation.
type ColorMeta struct {
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
}

// RepColor is the colour view of one representation.
type RepColor struct {
	ID   string
	Meta *ColorMeta // nil: no colr box present
}

// ColorTarget is the configured colour normalization target.
type ColorTarget struct {
	Primaries, Transfer, Matrix uint16
	FullRange                   bool
}

func (c ColorMeta) String() string {
	return fmt.Sprintf("nclx(%d/%d/%d,full=%t)", c.Primaries, c.Transfer, c.Matrix, c.FullRange)
}

func (c ColorMeta) equals(o ColorTarget) bool {
	return c.Primaries == o.Primaries && c.Transfer == o.Transfer &&
		c.Matrix == o.Matrix && c.FullRange == o.FullRange
}

// NormalizeColor checks colour metadata against the configured target.
// Representations conflicting with each other while no target is
// configured are marked not auto-fixable.
func NormalizeColor(reps []RepColor, target *ColorTarget) Report {
	var r Report
	if target == nil {
		// No configuration: only consistency is verifiable.
		var ref *ColorMeta
		for _, rep := range reps {
			if rep.Meta == nil {
				continue
			}
			if ref == nil {
				ref = rep.Meta
				continue
			}
			if *rep.Meta != *ref {
				r.Unfixable = append(r.Unfixable, fmt.Sprintf(
					"%s: conflicting color metadata %s vs %s with no configured target",
					rep.ID, rep.Meta, ref))
			}
		}
		return r
	}
	t := ColorMeta{target.Primaries, target.Transfer, target.Matrix, target.FullRange}
	for _, rep := range reps {
		if rep.Meta == nil {
			r.add(rep.ID, "color_metadata", "absent", t.String())
			continue
		}
		if !rep.Meta.equals(*target) {
			r.add(rep.ID, "color_metadata", rep.Meta.String(), t.String())
		}
	}
	return r
}
