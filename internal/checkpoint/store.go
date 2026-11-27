// Package checkpoint persists per-segment pipeline progress atomically so an
// interrupted run can resume without redoing finished work. Each segment is
// recorded only after its output is fully written and hash-verified; an
// orphaned output left behind by a crash is reused only when its digest still
// matches, otherwise it is rebuilt.
package checkpoint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/fashionweek/mediad/internal/cache"
	"github.com/fashionweek/mediad/internal/media"
)

// SegmentState records one finished segment across all variants.
type SegmentState struct {
	Index   int               `json:"index"`
	Outputs map[string]string `json:"outputs"` // variant -> sha256 of segment
	// CutAt is the shared timeline cut time in rational Num/Den seconds.
	CutAtNum int64 `json:"cut_at_num"`
	CutAtDen int64 `json:"cut_at_den"`
}

// Store is the durable checkpoint for one job.
type Store struct {
	root  string
	state *State
}

// State is the full checkpoint document.
type State struct {
	JobKey   string          `json:"job_key"`
	Segments []SegmentState  `json:"segments"`
	Finished map[int]bool    `json:"-"`
}

// Open loads an existing checkpoint or initialises a fresh one.
func Open(root, jobKey string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, media.New(media.PhaseCheckpoint, media.KindTool, root, -1,
			"cannot create checkpoint dir").Wrap(err)
	}
	s := &Store{root: root, state: &State{JobKey: jobKey, Finished: map[int]bool{}}}
	path := s.path()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, media.New(media.PhaseCheckpoint, media.KindTool, path, -1,
			"cannot read checkpoint").Wrap(err)
	}
	if err := json.Unmarshal(data, s.state); err != nil {
		// A corrupt checkpoint must not silently corrupt the timeline.
		return nil, media.New(media.PhaseCheckpoint, media.KindDigest, path, -1,
			"checkpoint document unreadable").Wrap(err)
	}
	if s.state.Finished == nil {
		s.state.Finished = map[int]bool{}
	}
	for _, seg := range s.state.Segments {
		s.state.Finished[seg.Index] = true
	}
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.root, "checkpoint.json") }

// IsDone reports whether a segment was durably recorded.
func (s *Store) IsDone(index int) bool { return s.state.Finished[index] }

// Segment returns the recorded state for a finished segment.
func (s *Store) Segment(index int) (SegmentState, bool) {
	for _, seg := range s.state.Segments {
		if seg.Index == index {
			return seg, true
		}
	}
	return SegmentState{}, false
}

// Record atomically appends a finished segment and persists the document.
func (s *Store) Record(seg SegmentState) error {
	replaced := false
	for i, existing := range s.state.Segments {
		if existing.Index == seg.Index {
			s.state.Segments[i] = seg
			replaced = true
			break
		}
	}
	if !replaced {
		s.state.Segments = append(s.state.Segments, seg)
		sort.Slice(s.state.Segments, func(a, b int) bool {
			return s.state.Segments[a].Index < s.state.Segments[b].Index
		})
	}
	s.state.Finished[seg.Index] = true
	return s.flush()
}

// CompletedCount is the number of durably finished segments.
func (s *Store) CompletedCount() int { return len(s.state.Segments) }

// AdoptOrphan validates an output file left behind by a crash against the
// expected hash and returns true when it can be reused as-is.
func (s *Store) AdoptOrphan(path, wantHash string) bool {
	got, err := cache.HashFile(path)
	if err != nil {
		return false
	}
	return got == wantHash
}

func (s *Store) flush() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return media.New(media.PhaseCheckpoint, media.KindTool, s.path(), -1,
			"cannot marshal checkpoint").Wrap(err)
	}
	tmp, err := os.CreateTemp(s.root, ".cp-*")
	if err != nil {
		return media.New(media.PhaseCheckpoint, media.KindTool, s.path(), -1,
			"cannot create checkpoint temp").Wrap(err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhaseCheckpoint, media.KindTool, s.path(), -1,
			"checkpoint write failed").Wrap(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhaseCheckpoint, media.KindTool, s.path(), -1,
			"checkpoint fsync failed").Wrap(err)
	}
	tmp.Close()
	if err := os.Rename(tmpName, s.path()); err != nil {
		os.Remove(tmpName)
		return media.New(media.PhaseCheckpoint, media.KindTool, s.path(), -1,
			"checkpoint atomic rename failed").Wrap(err)
	}
	return nil
}
