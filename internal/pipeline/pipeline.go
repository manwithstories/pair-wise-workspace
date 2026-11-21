// Package pipeline wires manifest parsing, scanning, stream analysis, RTP
// verification, recovery and provenance into the commands the CLI exposes.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"mediascan/internal/analyze"
	"mediascan/internal/manifest"
	"mediascan/internal/model"
	"mediascan/internal/recover"
	"mediascan/internal/stream"
)

// Options are the tunables bound into every checkpoint.
type Options struct {
	Work       string
	Manifest   string // root-relative playlist path
	LossEvery  int
	RTPPackets int
	AVTolMS    int64
	MaxGOPMS   int64
}

// Defaults fills unset options.
func (o *Options) Defaults() {
	if o.Manifest == "" {
		o.Manifest = "hls/media.m3u8"
	}
	if o.LossEvery <= 1 {
		o.LossEvery = 17
	}
	if o.RTPPackets <= 0 {
		o.RTPPackets = 240
	}
	if o.AVTolMS <= 0 {
		o.AVTolMS = 40
	}
	if o.MaxGOPMS <= 0 {
		o.MaxGOPMS = 2000
	}
}

// Params converts CLI options into checkpoint-bound parameters.
func (o Options) Params() recover.Params {
	return recover.Params{
		LossEvery:     o.LossEvery,
		AVToleranceMS: o.AVTolMS,
		MaxGOPMS:      o.MaxGOPMS,
		ReorderWindow: stream.ReorderWindow,
	}
}

// StreamResult is the per-track outcome of the streaming phase.
type StreamResult struct {
	Segments  int                    `json:"segments"`
	Corrupt   []model.Fault          `json:"corrupt_ranges"`
	FirstBad  string                 `json:"first_undecodable_segment,omitempty"`
	Sync      analyze.SyncResult     `json:"sync"`
	Keyframes analyze.KeyframeReport `json:"keyframes"`
	Faults    []model.Fault          `json:"faults,omitempty"`
}

// Result is the complete, digestible outcome of one run.
type Result struct {
	InputSHA     string                 `json:"input_sha256"`
	ResultSHA    string                 `json:"result_sha256"`
	Manifest     string                 `json:"manifest"`
	Format       string                 `json:"format"`
	Segments     int                    `json:"segments"`
	Missing      []string               `json:"missing,omitempty"`
	FirstMiss    string                 `json:"first_missing,omitempty"`
	CorruptRange []model.Fault          `json:"corrupt_ranges,omitempty"`
	AVSync       analyze.SyncResult     `json:"av_sync"`
	Keyframes    analyze.KeyframeReport `json:"keyframes"`
	RTP          stream.RTPStats        `json:"rtp"`
	P95LatencyMS float64                `json:"p95_latency_ms"`
	IndexVersion int                    `json:"index_version"`
	Checkpoint   int                    `json:"checkpoint_next_segment"`
	ResumeReused int                    `json:"resume_reused_segments"`
	Interrupted  bool                   `json:"interrupted,omitempty"`
	Faults       []model.Fault          `json:"faults,omitempty"`
}

// Load parses the HLS playlist (primary) and the DASH manifest when present,
// applying presence checks to both. The HLS timeline drives segment order and
// resume; DASH is validated and contributes its own missing-media faults.
func Load(o Options) (*manifest.Resolver, *manifest.Timeline, *manifest.Timeline, error) {
	res, err := manifest.NewResolver(o.Work)
	if err != nil {
		return nil, nil, nil, err
	}
	primary, err := manifest.Load(res, o.Manifest)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := manifest.MarkPresence(res, primary); err != nil {
		return nil, nil, nil, err
	}
	secondaryPath := "dash/manifest.mpd"
	if _, statErr := os.Stat(filepath.Join(o.Work, secondaryPath)); statErr == nil {
		secondary, err := manifest.Load(res, secondaryPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("dash: %w", err)
		}
		if err := manifest.MarkPresence(res, secondary); err != nil {
			return nil, nil, nil, err
		}
		return res, primary, secondary, nil
	}
	return res, primary, nil, nil
}

// manifestFaults converts absent manifest references into faults so the
// missing dependency is visible even before any media is read.
func manifestFaults(tl *manifest.Timeline) []model.Fault {
	var fs []model.Fault
	if tl == nil {
		return fs
	}
	for _, st := range tl.Streams {
		for _, seg := range st.Segments {
			if seg.Present {
				continue
			}
			end, err := seg.End()
			if err != nil {
				end = seg.Start
			}
			fs = append(fs, model.Fault{
				Kind: model.FaultMissingSegment, Severity: model.SeverityCritical,
				Stream: st.ID, Segment: seg.Name,
				Message:   fmt.Sprintf("%s manifest references absent media", tl.Format),
				TimeStart: seg.Start, TimeEnd: end,
			})
		}
	}
	return fs
}
