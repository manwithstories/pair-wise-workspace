// Package manifest builds the segment manifest, keyframe index and
// per-segment hashes from aligned boundaries and projected events.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediaroute/internal/adapter"
)

// Segment is one entry of the output manifest.
type Segment struct {
	Index    int    `json:"index"`
	StartNs  int64  `json:"start_ns"`
	EndNs    int64  `json:"end_ns"`
	Keyframe bool   `json:"keyframe"`
	Events   int    `json:"events"`
	SHA256   string `json:"sha256"`
}

// Manifest is the published segment listing plus keyframe index.
type Manifest struct {
	Version  int       `json:"version"`
	Segments []Segment `json:"segments"`
	KeyIndex []int64   `json:"keyframe_index_ns"`
}

// FileName is the manifest file name inside an output directory.
const FileName = "manifest.json"

// Build assembles the manifest from aligned boundaries, per-segment events
// and the primary rendition's keyframe index. Segments are hashed over
// their canonical event serialization.
func Build(bounds []int64, segEvents [][]adapter.Event, keyIndex []int64) *Manifest {
	m := &Manifest{Version: 1, KeyIndex: keyIndex}
	kf := make(map[int64]bool, len(keyIndex))
	for _, k := range keyIndex {
		kf[k] = true
	}
	for i := 0; i+1 < len(bounds); i++ {
		seg := Segment{
			Index:    i,
			StartNs:  bounds[i],
			EndNs:    bounds[i+1],
			Keyframe: kf[bounds[i]],
			Events:   len(segEvents[i]),
		}
		seg.SHA256 = HashSegment(seg, segEvents[i])
		m.Segments = append(m.Segments, seg)
	}
	return m
}

// KeyframeHitRate returns the fraction of segments starting on a keyframe.
func (m *Manifest) KeyframeHitRate() float64 {
	if len(m.Segments) == 0 {
		return 0
	}
	hit := 0
	for _, s := range m.Segments {
		if s.Keyframe {
			hit++
		}
	}
	return float64(hit) / float64(len(m.Segments))
}

// Save writes the manifest as indented JSON.
func (m *Manifest) Save(dir string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, FileName), append(data, '\n'), 0o644)
}

// Load reads a manifest from a directory.
func Load(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// SortEvents orders events canonically: track, then timestamp, then seq.
func SortEvents(events []adapter.Event) {
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		if a.TimestampNs != b.TimestampNs {
			return a.TimestampNs < b.TimestampNs
		}
		return a.Seq < b.Seq
	})
}
