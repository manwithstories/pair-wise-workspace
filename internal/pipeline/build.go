// Package pipeline orchestrates the build and verify flows: demux onto the
// unified timeline, cut segments, align representations, checkpoint
// incrementally and emit the manifest.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediapack/internal/clock"
	"mediapack/internal/demux"
	"mediapack/internal/fault"
	"mediapack/internal/index"
	"mediapack/internal/ladder"
	"mediapack/internal/manifest"
	"mediapack/internal/metrics"
	"mediapack/internal/segment"
)

// BuildOptions configures one build run.
type BuildOptions struct {
	Input             string
	Out               string
	SegmentMs         int64
	ToleranceMs       int64
	CheckpointSamples uint64
	TargetBitrates    []uint64
	Resume            bool
	Policy            fault.Policy
}

// Result carries the build outcome.
type Result struct {
	Manifest *manifest.Manifest
	Faults   *fault.List
	Blocking bool // faults require a media-error exit under the policy
}

// ErrCheckpointMismatch wraps index.ErrMismatch; callers map it to exit 4.
var ErrCheckpointMismatch = index.ErrMismatch

func configHash(o BuildOptions) string {
	h := sha256.New()
	fmt.Fprintf(h, "seg=%d|tol=%d|targets=%v|policy=%d",
		o.SegmentMs, o.ToleranceMs, o.TargetBitrates, o.Policy)
	return hex.EncodeToString(h.Sum(nil))
}

// validatePaths guards against overwriting the source material.
func validatePaths(input, out string) error {
	inAbs, err := filepath.Abs(input)
	if err != nil {
		return err
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	if inAbs == outAbs {
		return fmt.Errorf("output path must differ from input")
	}
	if filepath.Dir(inAbs) == outAbs {
		return fmt.Errorf("output directory must not be the input directory")
	}
	return nil
}

// Build runs the full segmentation pipeline.
func Build(o BuildOptions) (*Result, error) {
	if o.SegmentMs <= 0 {
		return nil, fmt.Errorf("segment duration must be positive")
	}
	if o.CheckpointSamples == 0 {
		return nil, fmt.Errorf("checkpoint-samples must be positive")
	}
	if err := validatePaths(o.Input, o.Out); err != nil {
		return nil, err
	}
	c, err := demux.Open(o.Input)
	if err != nil {
		return nil, err
	}
	res := &Result{Faults: c.Faults}
	if c.Faults.Blocking(o.Policy) {
		res.Blocking = true
		return res, nil
	}
	if err := manifest.Clean(o.Out); err != nil {
		return nil, err
	}
	cleanIndexTmp(o.Out)
	reps := c.VideoTracks()
	if len(reps) == 0 {
		return nil, fmt.Errorf("no video track in input")
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].ID < reps[j].ID })
	master := reps[len(reps)-1]
	target := clock.TicksFromMillis(o.SegmentMs)
	bounds := segment.Cut(master, target)
	align := ladder.Align(reps, bounds, clock.TicksFromMillis(o.ToleranceMs))
	writer, err := index.NewWriter(o.Out)
	if err != nil {
		return nil, err
	}
	cfgHash := configHash(o)
	committed, stats, processed, startSeg, recovered, err := resumeState(o, c, cfgHash)
	if err != nil {
		return nil, err
	}
	end := clock.Ticks(master.Duration(), master.Timescale)
	nextCp := processed + o.CheckpointSamples
	for i := startSeg; i < len(bounds); i++ {
		segEnd := end
		if i+1 < len(bounds) {
			segEnd = bounds[i+1].Ticks
		}
		committed = append(committed, manifest.SegmentEntry{
			Index: i, StartMs: clock.Millis(bounds[i].Ticks), EndMs: clock.Millis(segEnd),
			KeyframeSample: bounds[i].SampleIndex,
		})
		for _, t := range c.Tracks {
			st := trackStat(&stats, t.ID)
			for _, s := range t.Samples {
				pts := clock.Ticks(s.PTS, t.Timescale)
				if pts < bounds[i].Ticks || pts >= segEnd {
					continue
				}
				st.Samples++
				processed++
				if s.Available {
					st.Bytes += uint64(s.Size)
				} else {
					st.Skipped++
				}
			}
		}
		if processed >= nextCp {
			if err := writer.Save(&index.Checkpoint{
				InputFingerprint: c.Fingerprint, ConfigHash: cfgHash,
				SamplesProcessed: processed, Segments: committed, TrackStats: stats,
			}); err != nil {
				return nil, err
			}
			nextCp = processed + o.CheckpointSamples
		}
	}
	if err := writer.Save(&index.Checkpoint{
		InputFingerprint: c.Fingerprint, ConfigHash: cfgHash,
		SamplesProcessed: processed, Segments: committed, TrackStats: stats,
	}); err != nil {
		return nil, err
	}
	m, err := metrics.Assemble(metrics.Input{
		Container: c, Bounds: bounds, Alignment: align,
		TargetBitrates: o.TargetBitrates, SegmentTargetMs: o.SegmentMs,
		ToleranceMs: o.ToleranceMs, CheckpointRecovered: recovered,
	})
	if err != nil {
		return nil, err
	}
	m.Segments = committed
	if err := manifest.Write(o.Out, m); err != nil {
		return nil, err
	}
	res.Manifest = m
	return res, nil
}

// resumeState loads and validates the checkpoint when resuming.
func resumeState(o BuildOptions, c *demux.Container, cfgHash string) (
	segments []manifest.SegmentEntry, stats []index.TrackStat, processed uint64,
	startSeg int, recovered bool, err error) {
	if !o.Resume {
		return nil, nil, 0, 0, false, nil
	}
	cp, lerr := index.Load(o.Out)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return nil, nil, 0, 0, false, nil
		}
		return nil, nil, 0, 0, false, lerr
	}
	if verr := cp.Validate(c.Fingerprint, cfgHash); verr != nil {
		return nil, nil, 0, 0, false, fmt.Errorf("%w", verr)
	}
	return cp.Segments, cp.TrackStats, cp.SamplesProcessed, cp.CommittedBoundaries(), true, nil
}

func trackStat(stats *[]index.TrackStat, id uint32) *index.TrackStat {
	for i := range *stats {
		if (*stats)[i].TrackID == id {
			return &(*stats)[i]
		}
	}
	*stats = append(*stats, index.TrackStat{TrackID: id})
	return &(*stats)[len(*stats)-1]
}

func cleanIndexTmp(out string) {
	entries, err := os.ReadDir(filepath.Join(out, "index"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			os.Remove(filepath.Join(out, "index", e.Name()))
		}
	}
}

// IsCheckpointMismatch reports whether err is a checkpoint/input mismatch.
func IsCheckpointMismatch(err error) bool {
	return errors.Is(err, index.ErrMismatch)
}
