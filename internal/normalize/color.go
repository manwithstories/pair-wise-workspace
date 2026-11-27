package normalize

import (
	"sort"
	"sync"

	"github.com/asticode/go-astiav"
)

// ColorObservation is one colour-metadata reading with its provenance.
type ColorObservation struct {
	Source    string `json:"source"` // camera/representation/segment
	Primaries string `json:"primaries"`
	Transfer  string `json:"transfer"`
	Space     string `json:"space"`
	Range     string `json:"range"`
}

// key collapses an observation to the fields that define colour handling.
func (o ColorObservation) key() string {
	return o.Primaries + "|" + o.Transfer + "|" + o.Space + "|" + o.Range
}

// ColorConflict captures divergent colour metadata for the same timeline.
type ColorConflict struct {
	// Field is the conflicting colour dimension, or "composite".
	Field string `json:"field"`
	// Ambiguous is true when no dominant value can be chosen confidently.
	Ambiguous bool `json:"ambiguous"`
	// Dominant is the value chosen for downstream handling (the most common).
	Dominant string `json:"dominant"`
	// Sources lists every distinct reading with its provenance.
	Sources []ColorObservation `json:"sources"`
}

// ColorRegistry accumulates colour observations across representations and
// detects conflicts. It is safe for concurrent use because the pipeline probes
// several representations in parallel.
type ColorRegistry struct {
	mu          sync.Mutex
	observations []ColorObservation
}

// NewColorRegistry returns an empty registry.
func NewColorRegistry() *ColorRegistry { return &ColorRegistry{} }

// Record stores one reading. Unspecified fields are kept but never counted as
// an authoritative value when deciding the dominant colour description.
func (r *ColorRegistry) Record(o ColorObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations = append(r.observations, o)
}

// Analyse returns one conflict per colour dimension that has more than one
// distinct explicit value, plus a composite entry when full descriptions
// disagree. When there is no conflict the returned slice is empty.
func (r *ColorRegistry) Analyse() []ColorConflict {
	r.mu.Lock()
	obs := make([]ColorObservation, len(r.observations))
	copy(obs, r.observations)
	r.mu.Unlock()

	if len(obs) == 0 {
		return nil
	}
	var conflicts []ColorConflict
	dimensions := []struct {
		field string
		get   func(ColorObservation) string
	}{
		{"primaries", func(o ColorObservation) string { return o.Primaries }},
		{"transfer", func(o ColorObservation) string { return o.Transfer }},
		{"space", func(o ColorObservation) string { return o.Space }},
		{"range", func(o ColorObservation) string { return o.Range }},
	}
	for _, d := range dimensions {
		counts := map[string]int{}
		var order []string // first-encounter order of distinct values
		var sources []ColorObservation
		for _, o := range obs {
			v := d.get(o)
			if v == "" || v == "unspecified" || v == astiav.ColorSpaceUnspecified.String() {
				continue
			}
			if _, seen := counts[v]; !seen {
				order = append(order, v)
			}
			counts[v]++
			sources = append(sources, o)
		}
		if len(counts) > 1 {
			dominant, topCount := dominantValue(counts, order)
			// Ambiguous when no value strictly dominates.
			ambiguous := 2*topCount <= len(sources)
			conflicts = append(conflicts, ColorConflict{
				Field:     d.field,
				Ambiguous: ambiguous,
				Dominant:  dominant,
				Sources:   dedupSources(sources),
			})
		}
	}
	// Composite conflict: full colour descriptions disagree.
	keys := map[string]int{}
	for _, o := range obs {
		if o.key() == "unspecified|unspecified|unspecified|unspecified" {
			continue
		}
		keys[o.key()]++
	}
	if len(keys) > 1 {
		conflicts = append(conflicts, ColorConflict{
			Field:     "composite",
			Ambiguous: true,
			Dominant:  "",
			Sources:   dedupSources(obs),
		})
	}
	return conflicts
}

// Observations returns a copy of all recorded readings.
func (r *ColorRegistry) Observations() []ColorObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ColorObservation, len(r.observations))
	copy(out, r.observations)
	return out
}

func dominantValue(counts map[string]int, order []string) (string, int) {
	best := ""
	bestCount := -1
	// Walk in first-encounter order; a strictly higher count wins, while ties
	// keep the earlier value so the chosen dominant is deterministic and
	// reflects the primary representation observed first.
	for _, k := range order {
		v := counts[k]
		if v > bestCount {
			best = k
			bestCount = v
		}
	}
	return best, bestCount
}

func dedupSources(in []ColorObservation) []ColorObservation {
	seen := map[string]bool{}
	var out []ColorObservation
	for _, o := range in {
		if seen[o.Source] {
			continue
		}
		seen[o.Source] = true
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}
