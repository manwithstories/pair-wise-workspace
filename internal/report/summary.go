package report

import (
	"encoding/json"

	"archivetl/internal/diag"
	"archivetl/internal/pipeline"
)

// ErrorStat is the JSON representation of one structured error.
type ErrorStat struct {
	Phase       string `json:"phase"`
	Source      string `json:"source"`
	Offset      int64  `json:"offset"`
	Operation   string `json:"operation"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

// SegmentStat is one segment's entry in the summary.
type SegmentStat struct {
	ID            int     `json:"id"`
	StartTick     int64   `json:"start_tick"`
	EndTick       int64   `json:"end_tick"`
	Events        int     `json:"events"`
	KeyframeHits  int     `json:"keyframe_hits"`
	Skipped       bool    `json:"skipped"`
	SHA256        string  `json:"segment_sha256"`
	FailedOffsets []int64 `json:"failed_offsets,omitempty"`
}

// BoundaryStat reports validation failures on boundary/rejected records.
type BoundaryStat struct {
	RejectedRecords int `json:"rejected_records"`
	ZeroDuration    int `json:"zero_duration"`
	Overflow        int `json:"overflow"`
}

// Summary is the full terminal JSON document.
type Summary struct {
	Segments        []SegmentStat     `json:"segments"`
	SegmentSHA256   map[string]string `json:"segment_sha256"`
	Boundary        BoundaryStat      `json:"boundary"`
	Errors          []ErrorStat       `json:"errors"`
	KeyframeHitRate float64           `json:"keyframe_hit_rate"`
	RoundtripRate   float64           `json:"roundtrip_rate"`
	RejectedCount   int               `json:"rejected_count"`
	Stats           map[string]int    `json:"stats"`
}

// Builder accumulates the summary from pipeline results and the error
// collector shared by the parse stages.
type Builder struct {
	store *FileStore
}

// NewBuilder binds a builder to the run's file store.
func NewBuilder(store *FileStore) *Builder { return &Builder{store: store} }

// Build assembles the JSON-ready summary. eventProbeTotal/Matched describe
// keyframe lookup coverage; roundtripOK/Total describe fixture round-trips.
func (b *Builder) Build(results []pipeline.Result, coll *diag.Collector,
	hitMatched, hitTotal, roundtripOK, roundtripTotal int) *Summary {
	sum := &Summary{
		SegmentSHA256: map[string]string{},
		Errors:        []ErrorStat{},
		Stats:         map[string]int{},
	}
	manifest := b.store.Manifest()
	for _, r := range results {
		seg := r.Segment
		key := entryKey(seg.ID)
		entry := manifest.Entries[key]
		stat := SegmentStat{
			ID: seg.ID, StartTick: int64(seg.Start), EndTick: int64(seg.End),
			Events: seg.Summary.Accepted, KeyframeHits: seg.KeyHits,
			Skipped: seg.Skipped, SHA256: entry.OutputHash,
			FailedOffsets: append([]int64(nil), seg.FailedOffsets...),
		}
		if stat.SHA256 == "" {
			stat.SHA256 = seg.OutputHash
		}
		sum.Segments = append(sum.Segments, stat)
		sum.SegmentSHA256[key] = stat.SHA256
		sum.Boundary.RejectedRecords += seg.Summary.RejectedCount
		sum.Boundary.ZeroDuration += seg.Summary.ZeroDuration
		sum.Boundary.Overflow += seg.Summary.Overflow
	}
	sum.RejectedCount = sum.Boundary.RejectedRecords

	for _, e := range coll.All() {
		sum.Errors = append(sum.Errors, ErrorStat{
			Phase: string(e.Phase), Source: e.Source, Offset: e.Offset,
			Operation: e.Op, Message: e.Message, Recoverable: e.Recoverable,
		})
	}
	if hitTotal > 0 {
		sum.KeyframeHitRate = float64(hitMatched) / float64(hitTotal)
	}
	if roundtripTotal > 0 {
		sum.RoundtripRate = float64(roundtripOK) / float64(roundtripTotal)
	}
	sum.Stats["segments_committed"] = len(results)
	sum.Stats["errors_total"] = coll.Len()
	sum.Stats["errors_recoverable"] = coll.Recoverable()
	return sum
}

// Marshal renders the summary as indented JSON with a trailing newline.
func (s *Summary) Marshal() ([]byte, error) {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
