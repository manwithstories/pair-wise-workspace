package normalize

import (
	"fmt"

	"archivecheck/internal/isobmff"
)

// ColorPolicy selects how conflicting colour metadata is resolved.
type ColorPolicy string

const (
	// PolicyKeepSource keeps each representation's own colour metadata.
	PolicyKeepSource ColorPolicy = "keep-source"
	// PolicyUnifyTarget rewrites every representation to the target value.
	PolicyUnifyTarget ColorPolicy = "unify-target"
)

// RepColor records one representation's source metadata and the decision.
type RepColor struct {
	Rep       string             `json:"rep"`
	Source    *isobmff.ColorInfo `json:"source"`
	Effective *isobmff.ColorInfo `json:"effective"`
	Decision  string             `json:"decision"` // keep-source | unified-to-target | assumed-target
}

// ColorDecision is the manifest record of a colour resolution.
type ColorDecision struct {
	Policy   ColorPolicy        `json:"policy"`
	Target   *isobmff.ColorInfo `json:"target,omitempty"`
	Conflict bool               `json:"conflict"`
	Reps     []RepColor         `json:"reps"`
}

func sameColor(a, b *isobmff.ColorInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ResolveColor applies policy to the per-representation source metadata.
// Every representation's source and the chosen value are recorded; a
// conflict never silently picks a winner.
func ResolveColor(reps []string, sources []*isobmff.ColorInfo, policy ColorPolicy, target *isobmff.ColorInfo) (*ColorDecision, error) {
	if len(reps) != len(sources) {
		return nil, fmt.Errorf("normalize: %d names for %d sources", len(reps), len(sources))
	}
	if policy == PolicyUnifyTarget && target == nil {
		return nil, fmt.Errorf("normalize: unify-target policy requires a target")
	}
	d := &ColorDecision{Policy: policy, Target: target}
	var first *isobmff.ColorInfo
	for i, src := range sources {
		if i == 0 {
			first = src
		} else if !sameColor(first, src) {
			d.Conflict = true
		}
		rc := RepColor{Rep: reps[i], Source: src}
		switch {
		case policy == PolicyUnifyTarget:
			rc.Effective = target
			if sameColor(src, target) {
				rc.Decision = "keep-source"
			} else {
				rc.Decision = "unified-to-target"
			}
		case src == nil:
			rc.Effective = target // may still be nil: recorded as missing
			rc.Decision = "assumed-target"
		default:
			rc.Effective = src
			rc.Decision = "keep-source"
		}
		d.Reps = append(d.Reps, rc)
	}
	return d, nil
}
