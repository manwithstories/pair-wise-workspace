package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"avrepair/internal/media"
)

func firstSync(seg *media.Segment) *media.Sample {
	for i := range seg.Samples {
		if seg.Samples[i].Sync && !seg.Samples[i].Duplicate {
			return &seg.Samples[i]
		}
	}
	return nil
}

// Seek returns the entry at or before t on a track, or false when t is
// earlier than the first indexed point.
func (idx *Index) Seek(track string, t media.Rational) (Entry, bool) {
	var best *Entry
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.Track != track {
			continue
		}
		if e.PTS.Cmp(t) <= 0 {
			best = e
			continue
		}
		break
	}
	if best == nil {
		return Entry{}, false
	}
	return *best, true
}

// Encode serializes the index deterministically.
func (idx *Index) Encode() ([]byte, error) {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Decode parses a committed index and validates its invariants.
func Decode(data []byte) (*Index, error) {
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("index: decode: %w", err)
	}
	if idx.Version != Version {
		return nil, fmt.Errorf("index: unsupported version %d", idx.Version)
	}
	if err := idx.Validate(""); err != nil {
		return nil, err
	}
	return &idx, nil
}

// Validate checks monotonicity, uniqueness and (optionally) that every
// referenced output exists and is a regular file.
func (idx *Index) Validate(outDir string) error {
	seen := map[string]bool{}
	last := map[string]media.Rational{}
	for _, e := range idx.Entries {
		if e.Segment == "" || e.Output == "" {
			return fmt.Errorf("index: entry with empty segment or output")
		}
		if seen[e.Output] {
			return fmt.Errorf("index: duplicate output %q", e.Output)
		}
		seen[e.Output] = true
		if prev, ok := last[e.Track]; ok && e.PTS.Cmp(prev) <= 0 {
			return fmt.Errorf("index: %s PTS not increasing (%s <= %s)", e.Track, e.PTS, prev)
		}
		last[e.Track] = e.PTS
		if outDir != "" {
			p := filepath.Join(outDir, e.Output)
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("index: output %q missing or empty", e.Output)
			}
		}
	}
	return nil
}
