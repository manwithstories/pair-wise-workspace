package index

import (
	"encoding/json"
	"fmt"
	"os"

	"mediarun/internal/clock"
	"mediarun/internal/model"
	"mediarun/internal/parse"
)

// Save writes the index as deterministic JSON.
func (idx *Index) Save(path string) error {
	if err := idx.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// Load reads an index and validates it. A corrupt file is reported as such
// so the caller can rebuild instead of aborting.
func Load(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", model.ErrCorruptIndex, path, err)
	}
	if err := idx.Validate(); err != nil {
		return nil, err
	}
	return &idx, nil
}

// LookupFromTrack adapts a parsed track to SampleLookup, resolving sample
// numbers against the original table (not the index).
func LookupFromTrack(t parse.Track) SampleLookup {
	byNumber := make(map[int]parse.Sample, len(t.Samples))
	for _, s := range t.Samples {
		byNumber[s.Index] = s
	}
	return func(number int) (clock.Tick, clock.Tick, int64, uint32, bool) {
		s, ok := byNumber[number]
		if !ok || t.Timescale == 0 {
			return 0, 0, 0, 0, false
		}
		start, err := clock.ScaleToTicks(int64(s.DTS), t.Timescale)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		dur, err := clock.ScaleToTicks(int64(s.Duration), t.Timescale)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		return start, dur, s.Offset, s.Size, true
	}
}

// GenerateQueries derives one query tick per keyframe plus a mid-GOP probe,
// giving a deterministic query set for hit-rate measurement.
func GenerateQueries(idx *Index) []clock.Tick {
	var out []clock.Tick
	for i, e := range idx.Entries {
		out = append(out, e.Start)
		if i+1 < len(idx.Entries) && idx.Entries[i+1].Start > e.Start+1 {
			out = append(out, e.Start+(idx.Entries[i+1].Start-e.Start)/2)
		}
	}
	return out
}
