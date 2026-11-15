// Package packager reuses the shared manifest model to write per-
// representation segment index files next to the manifest.
package packager

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"dashkit/internal/model"
)

// IndexEntry is one segment row in the packager index.
type IndexEntry struct {
	Number uint64 `json:"n"`
	T      uint64 `json:"t"`
	D      uint64 `json:"d"`
	URL    string `json:"url"`
}

// Index is the per-representation, per-period segment index.
type Index struct {
	RepresentationID string       `json:"representation_id"`
	Period           int          `json:"period"`
	Timescale        uint64       `json:"timescale"`
	Segments         []IndexEntry `json:"segments"`
}

// WriteIndexes writes one JSON index per (period, representation). urls is
// keyed "period/repID" and must match the expanded segment counts.
func WriteIndexes(m *model.MPD, urls map[string][]string, dir string) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	total := 0
	for pi, p := range m.Periods {
		for _, as := range p.Sets {
			for _, rep := range as.Reps {
				// Timeline problems are reported by the verify path; the
				// packager still writes a partial index for the good rungs.
				segs, _ := model.ExpandTimeline(rep.Template.Timeline, rep.Template.StartNumber)
				key := fmt.Sprintf("%d/%s", pi, rep.ID)
				us := urls[key]
				if len(us) != len(segs) {
					return total, fmt.Errorf("rep %s period %d: %d urls for %d segments",
						rep.ID, pi, len(us), len(segs))
				}
				idx := Index{RepresentationID: rep.ID, Period: pi, Timescale: rep.Template.Timescale}
				for i, s := range segs {
					idx.Segments = append(idx.Segments, IndexEntry{Number: s.Number, T: s.T, D: s.D, URL: us[i]})
				}
				data, err := json.MarshalIndent(idx, "", "  ")
				if err != nil {
					return total, err
				}
				name := fmt.Sprintf("p%d_%s.json", pi, rep.ID)
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
					return total, err
				}
				total += len(segs)
			}
		}
	}
	return total, nil
}

// SortedKeys returns map keys in stable order for deterministic output.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
