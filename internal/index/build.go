// Package index builds the random-access keyframe index from repaired,
// independently re-parsed fragments. Only sync samples (trun sample flags
// with the depends-on-others bit clear) become index entries, so every entry
// is a real random-access point on the global timeline.
package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/timeline"
)

// Entry points at one random-access sample.
type Entry struct {
	TrackID       uint32 `json:"track_id"`
	Sequence      uint32 `json:"sequence"` // repaired contiguous sequence
	SampleIndex   uint32 `json:"sample_index"`
	GlobalDTS     uint64 `json:"global_dts"`
	FragmentPath  string `json:"fragment_path"`
	SampleOffset  int64  `json:"sample_offset"` // source-level byte offset
	Sync          bool   `json:"sync"`
}

// Index is the sorted keyframe catalog for one pipeline run.
type Index struct {
	Entries []Entry `json:"entries"`
}

// Builder accumulates entries only from published, repaired fragments.
type Builder struct {
	idx Index
}

// AddFragment re-parses the on-disk output fragment (never trusting in-memory
// pipeline counters) and records every sync sample it contains.
func (b *Builder) AddFragment(rf *timeline.RepairedFragment, outDir string) error {
	path := filepath.Join(outDir, "frags", filepath.Base(rf.Key()))
	raw, err := os.ReadFile(path)
	if err != nil {
		return media.NewFault(media.FaultCheckpoint, rf.TrackID, rf.NewSequence,
			rf.MoofOffset, "read output for index: "+err.Error())
	}
	parsed, err := ingest.ParseFragment(path, 0, raw)
	if err != nil {
		return media.NewFault(media.FaultStructure, rf.TrackID, rf.NewSequence,
			0, "index re-parse failed: "+err.Error())
	}
	for _, s := range parsed.Samples {
		if !s.Sync {
			continue
		}
		b.idx.Entries = append(b.idx.Entries, Entry{
			TrackID:      rf.TrackID,
			Sequence:     rf.NewSequence,
			SampleIndex:  s.Index,
			GlobalDTS:    rf.GlobalDTS,
			FragmentPath: filepath.ToSlash(filepath.Join("frags", filepath.Base(rf.Key()))),
			SampleOffset: s.DataOffset,
			Sync:         true,
		})
	}
	return nil
}

// Build returns the index sorted for deterministic, binary-searchable access.
func (b *Builder) Build() *Index {
	sort.SliceStable(b.idx.Entries, func(i, j int) bool {
		if b.idx.Entries[i].TrackID != b.idx.Entries[j].TrackID {
			return b.idx.Entries[i].TrackID < b.idx.Entries[j].TrackID
		}
		if b.idx.Entries[i].GlobalDTS != b.idx.Entries[j].GlobalDTS {
			return b.idx.Entries[i].GlobalDTS < b.idx.Entries[j].GlobalDTS
		}
		return b.idx.Entries[i].SampleIndex < b.idx.Entries[j].SampleIndex
	})
	return &b.idx
}

// Write serializes the index as canonical JSON (no map iteration, fixed key
// order via struct fields) so repeated runs hash identically.
func Write(idx *Index, path string) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return media.NewFault(media.FaultCheckpoint, 0, 0, 0,
			"encode index: "+err.Error())
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return media.NewFault(media.FaultCheckpoint, 0, 0, 0,
			"write index: "+err.Error())
	}
	return nil
}

// Load reads an index back from disk for independent verification.
func Load(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, media.NewFault(media.FaultCheckpoint, 0, 0, 0,
			"read index: "+err.Error())
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, media.NewFault(media.FaultCheckpoint, 0, 0, 0,
			"decode index: "+err.Error())
	}
	return &idx, nil
}

// Lookup reports whether a (track, sequence, sample) entry exists and is sync.
func (x *Index) Lookup(trackID, sequence, sampleIndex uint32) (Entry, bool) {
	for _, e := range x.Entries {
		if e.TrackID == trackID && e.Sequence == sequence &&
			e.SampleIndex == sampleIndex && e.Sync {
			return e, true
		}
	}
	return Entry{}, false
}
