// Package render exports an off-screen PNG frame sequence at chosen master
// ticks. Frames are deterministic self-describing RGBA samples: every pixel
// is a pure function of (seed, stream, master tick, keyframe landing), so the
// same seed always yields byte-identical PNGs and hashes with no GUI or
// display dependency. Sparse keyframes are located by seeking backward and
// the actual landing tick is verified against the timeline.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"bq/internal/clock"
	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/timeline"
)

// FrameRequest names one frame to export, expressed in master ticks.
type FrameRequest struct {
	AtMaster int64
	Label    string
}

// FrameResult is one exported frame and its verification data.
type FrameResult struct {
	StreamID       int    `json:"stream_id"`
	Label          string `json:"label"`
	AtMaster       int64  `json:"at_master"`
	KeyframeMaster int64  `json:"keyframe_master"`
	KeyframeSeq    int64  `json:"keyframe_seq"`
	SeekDeltaMs    int64  `json:"seek_delta_ms"`
	PNGPath        string `json:"png_path"`
	SHA256         string `json:"sha256"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
}

// Exporter renders frames for one video stream into outDir/frames.
type Exporter struct {
	stream  manifest.Stream
	items   []timeline.Item
	reports []timeline.DriftReport
	outDir  string
	seed    uint64
}

// NewExporter builds a renderer. Drift reports let frames reflect the same
// read-only sync correction the verifier uses.
func NewExporter(s manifest.Stream, items []timeline.Item,
	reports []timeline.DriftReport, outDir string, seed uint64) *Exporter {
	return &Exporter{stream: s, items: items, reports: reports, outDir: outDir, seed: seed}
}

// Export renders every request. A frame whose keyframe cannot be located is
// recorded as a KEYFRAME_SEEK error and the remaining frames still render.
func (e *Exporter) Export(reqs []FrameRequest) ([]FrameResult, []model.Error) {
	frameDir := filepath.Join(e.outDir, "frames", fmt.Sprintf("stream%d", e.stream.ID))
	if err := os.MkdirAll(frameDir, 0o755); err != nil {
		return nil, []model.Error{model.NewError(model.StageRender, e.stream.ID,
			model.ErrKeyframeSeek, model.SeverityFatal,
			"cannot create frame dir: "+err.Error(), -1, 0, false)}
	}
	var results []FrameResult
	var errs []model.Error

	for _, req := range reqs {
		kf, ok := timeline.NextKeyframeAtOrAfter(e.items, req.AtMaster)
		if !ok {
			errs = append(errs, model.NewError(model.StageRender, e.stream.ID,
				model.ErrKeyframeSeek, model.SeverityError,
				fmt.Sprintf("no keyframe at or before tick %d", req.AtMaster),
				-1, req.AtMaster, true))
			continue
		}
		syncedTick := e.applyDrift(req.AtMaster)
		img := DrawFrame(DrawInput{
			Seed: e.seed, StreamID: e.stream.ID, Width: e.stream.Width,
			Height: e.stream.Height, AtMaster: syncedTick,
			KeyframeMaster: kf.MasterPTS, KeyframeSeq: kf.Seq,
		})
		name := fmt.Sprintf("%s-s%d.png", req.Label, e.stream.ID)
		pngPath := filepath.Join(frameDir, name)
		if err := writePNG(pngPath, img); err != nil {
			errs = append(errs, model.NewError(model.StageRender, e.stream.ID,
				model.ErrKeyframeSeek, model.SeverityError, err.Error(),
				kf.SourceOffset, req.AtMaster, true))
			continue
		}
		sum := sha256.Sum256(pngBytes(img))
		results = append(results, FrameResult{
			StreamID: e.stream.ID, Label: req.Label, AtMaster: req.AtMaster,
			KeyframeMaster: kf.MasterPTS, KeyframeSeq: kf.Seq,
			SeekDeltaMs: clock.MasterToMs(req.AtMaster - kf.MasterPTS),
			PNGPath: pngPath, SHA256: hex.EncodeToString(sum[:]),
			Width: e.stream.Width, Height: e.stream.Height,
		})
	}
	return results, errs
}

// applyDrift shifts the render tick by the read-only segment sync offset.
// Unrecoverable segments return the uncorrected tick and are flagged later
// by the verifier rather than silently moving the image.
func (e *Exporter) applyDrift(masterTick int64) int64 {
	for _, rep := range e.reports {
		if rep.StreamID != e.stream.ID {
			continue
		}
		if off, ok := rep.SyncOffsetAt(masterTick); ok {
			return masterTick + off
		}
	}
	return masterTick
}

// DefaultRequests picks sparse checkpoints spread across the run: the first
// frame, 25%, 50%, 75% and the last available second.
func DefaultRequests(durationS int64) []FrameRequest {
	points := []int64{0, durationS / 4, durationS / 2, 3 * durationS / 4, durationS - 1}
	labels := []string{"head", "q1", "mid", "q3", "tail"}
	out := make([]FrameRequest, 0, len(points))
	for i, sec := range points {
		if sec < 0 {
			sec = 0
		}
		out = append(out, FrameRequest{
			AtMaster: sec * clock.MasterHz,
			Label:    labels[i],
		})
	}
	return out
}
