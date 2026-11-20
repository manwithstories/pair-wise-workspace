package pipeline

import (
	"sort"
	"sync"

	"github.com/mediamend/mediamend/internal/audio"
	"github.com/mediamend/mediamend/internal/color"
	"github.com/mediamend/mediamend/internal/graph"
	"github.com/mediamend/mediamend/internal/index"
	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
	"github.com/mediamend/mediamend/internal/timeline"
)

// Result is the aggregated pipeline outcome, serialized as the JSON manifest.
type Result struct {
	Root              string                  `json:"-"`
	InputDigest       string                  `json:"input_digest"`
	Resumed           bool                    `json:"resumed"`
	FramesTotal       int                     `json:"frames_total"`
	CorruptLocated    int                     `json:"corrupt_located"`
	LocationPrecision float64                 `json:"location_precision"`
	ResumeMS          int64                   `json:"resume_ms"`
	SkippedStages     []string                `json:"resume_skipped_stages"`
	SeekChecked       int                     `json:"seek_checked"`
	OutputSHA256      string                  `json:"output_sha256"`
	CompleteTracks    int                     `json:"complete_tracks"`
	IncompleteTracks  int                     `json:"incomplete_tracks"`
	FaultCount        int                     `json:"fault_count"`
	FatalCount        int                     `json:"fatal_count"`
	Tracks            map[string]*TrackResult `json:"tracks"`
	Faults            []model.Fault           `json:"faults"`

	// frameStore holds ingested frames between stages; not serialized.
	frameStore  map[string][]*ingest.Frame
	fileByTrack map[string]string
	layers      [][]string
	mu          sync.Mutex
}

// TrackResult is the per-track outcome, always present even when blocked.
type TrackResult struct {
	TrackID      string             `json:"track_id"`
	Kind         graph.Kind         `json:"kind"`
	Complete     bool               `json:"complete"`
	BlockedBy    []string           `json:"blocked_by,omitempty"`
	FrameCount   int                `json:"frame_count"`
	IndexFile    string             `json:"index_file,omitempty"`
	Faults       []model.Fault      `json:"faults,omitempty"`
	Audio        *audio.Spec        `json:"audio,omitempty"`
	ColorSummary []color.Descriptor `json:"color_summary,omitempty"`
	Repaired     *timeline.Report   `json:"-"`
	Index        *index.Index       `json:"-"`
}

func newResult() *Result {
	return &Result{
		Tracks:        map[string]*TrackResult{},
		SkippedStages: []string{},
		Faults:        []model.Fault{},
		frameStore:    map[string][]*ingest.Frame{},
		fileByTrack:   map[string]string{},
	}
}

// addFaults appends many faults.
func (r *Result) addFaults(fs []model.Fault) { r.Faults = append(r.Faults, fs...) }

// sortedFaults returns faults ordered by file/track/seq for stable output.
func (r *Result) sortedFaults() []model.Fault {
	out := append([]model.Fault(nil), r.Faults...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Location, out[j].Location
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.Offset < b.Offset
	})
	return out
}

// hasFatal reports whether any fatal fault was recorded.
func (r *Result) hasFatal() bool { return model.HasFatal(r.Faults) }

// ensureTrack lazily creates the per-track result slot.
func (r *Result) ensureTrack(id string, kind graph.Kind) *TrackResult {
	tr, ok := r.Tracks[id]
	if !ok {
		tr = &TrackResult{TrackID: id, Kind: kind, Complete: true}
		r.Tracks[id] = tr
	}
	return tr
}

// sortedTrackIDs yields deterministic processing order.
func (r *Result) sortedTrackIDs() []string {
	ids := make([]string, 0, len(r.Tracks))
	for id := range r.Tracks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
