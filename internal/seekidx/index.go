// Package seekidx builds a keyframe/GOP index over a parsed track and locates
// the nearest sync sample at or before a presentation-time target.
package seekidx

import (
	"encoding/json"
	"os"
	"sort"

	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
)

// Key is one sync sample entry in the index.
type Key struct {
	SampleIndex int    `json:"sample_index"`
	PtsMs       int64  `json:"pts_ms"`
	Offset      int64  `json:"offset"`
	Size        uint32 `json:"size"`
}

// Index is the serializable keyframe index for one track.
type Index struct {
	Handler     string `json:"handler"`
	Timescale   uint32 `json:"timescale"`
	SampleCount int    `json:"sample_count"`
	Keys        []Key  `json:"keys"`
	// PtsMs of every sample, used to resolve the target frame and bounds.
	PtsMs []int64 `json:"pts_ms"`
}

// Build constructs an index from a parsed track.
func Build(tr *mp4atom.Track) *Index {
	idx := &Index{
		Handler:     tr.Handler(),
		Timescale:   tr.Table.Timescale,
		SampleCount: len(tr.Samples),
		PtsMs:       tr.Timeline.PresentMs,
	}
	for i, s := range tr.Samples {
		if s.Sync {
			idx.Keys = append(idx.Keys, Key{
				SampleIndex: i,
				PtsMs:       tr.Timeline.PresentMs[i],
				Offset:      s.Offset,
				Size:        s.Size,
			})
		}
	}
	return idx
}

// Save writes the index as JSON for reuse.
func (idx *Index) Save(path string) error {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return report.NewError(report.ExitArgs, "index", err.Error())
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return report.NewError(report.ExitFileRead, "index", err.Error())
	}
	return nil
}

// Load reads a previously saved index.
func Load(path string) (*Index, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, report.NewError(report.ExitFileRead, "index", err.Error())
	}
	idx := &Index{}
	if err := json.Unmarshal(b, idx); err != nil {
		return nil, report.NewError(report.ExitBoxInvalid, "index", err.Error())
	}
	return idx, nil
}

// Landing is the resolved seek result.
type Landing struct {
	Key           Key
	TargetIndex   int   // sample whose pts is the greatest <= requested
	GopDistanceMs int64 // requested minus keyframe pts
	FrameType     string
}

// Seek finds the nearest sync sample at or before the requested millisecond.
// It returns a coded error when the target is outside the presentation range.
func (idx *Index) Locate(requestedMs int64) (*Landing, error) {
	if len(idx.PtsMs) == 0 {
		return nil, report.NewError(report.ExitSeekOOB, "seek", "no samples")
	}
	first, last := idx.PtsMs[0], idx.PtsMs[len(idx.PtsMs)-1]
	if requestedMs < first {
		return nil, report.NewError(report.ExitSeekOOB, "seek",
			"target earlier than first frame")
	}
	if requestedMs > last {
		return nil, report.NewError(report.ExitSeekOOB, "seek",
			"target later than last frame")
	}
	// greatest sample with pts <= requested
	target := sort.Search(len(idx.PtsMs), func(i int) bool {
		return idx.PtsMs[i] > requestedMs
	}) - 1
	// greatest keyframe with pts <= requested
	k := sort.Search(len(idx.Keys), func(i int) bool {
		return idx.Keys[i].PtsMs > requestedMs
	}) - 1
	if k < 0 {
		return nil, report.NewError(report.ExitSeekOOB, "seek",
			"no keyframe at or before target")
	}
	key := idx.Keys[k]
	return &Landing{
		Key:           key,
		TargetIndex:   target,
		GopDistanceMs: requestedMs - key.PtsMs,
		FrameType:     "I",
	}, nil
}
