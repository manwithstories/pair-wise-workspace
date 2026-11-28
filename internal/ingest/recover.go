// Package ingest turns adapter probe output into a per-camera delivery
// plan: damaged inputs keep their valid prefix, time gaps are marked, and
// every recovery is recorded with original offset, reason and content hash.
// Cameras that cannot be recovered fail individually without stopping the
// batch.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"presspack/internal/adapter"
	"presspack/internal/clock"
	"presspack/internal/core"
)

// Gap is a span of the declared timeline with no decodable media.
type Gap struct {
	Start, End core.Time
	Reason     string
}

// RecoveryRecord documents one recovered span for the audit trail.
type RecoveryRecord struct {
	Camera         string
	OriginalOffset int64  // byte offset in the source where recovery acted
	Reason         string // fault kind that triggered recovery
	Hash           string // sha256 of the recovered source region
	Start, End     core.Time
}

// CameraPlan is the recover-layer output consumed by the segment layer.
type CameraPlan struct {
	Camera   string
	Source   string
	Format   string
	FPS      clock.FPS
	Duration core.Time // deliverable duration (valid prefix)
	Samples  []adapter.MediaSample
	Gaps     []Gap
	Records  []RecoveryRecord
	Faults   core.Faults
}

// ErrUnrecoverable marks a camera that cannot be delivered at all.
var ErrUnrecoverable = errors.New("camera unrecoverable")

// BuildPlan probes one camera source and derives the delivery plan. The
// sidecar index (srcPath+".idx") is consulted when present; a missing index
// is recoverable because the adapter scans the container directly. expect is
// the duration the camera should deliver (from the production manifest);
// when the decodable prefix ends more than one frame early the tail is
// marked as a time gap and a recovery record is written.
func BuildPlan(ctx context.Context, ad adapter.Adapter, camera, srcPath string, expect core.Time) (*CameraPlan, error) {
	plan := &CameraPlan{Camera: camera, Source: srcPath}
	if _, err := os.Stat(srcPath + ".idx"); err != nil {
		plan.Faults = append(plan.Faults, core.NewFault(core.FaultMissingIndex, -1, true,
			"sidecar index absent; scanning recoverable region"))
	}
	res, err := ad.Probe(ctx, srcPath)
	if err != nil {
		plan.Faults = append(plan.Faults, core.NewFault(core.FaultUnrecoverable, -1, false, err.Error()))
		return plan, fmt.Errorf("%s: %w: %v", camera, ErrUnrecoverable, err)
	}
	plan.Format = res.Format
	plan.FPS = res.FPS
	plan.Samples = normalizePTS(res.Samples)
	plan.Faults = append(plan.Faults, res.Faults...)
	plan.Duration = samplesDuration(plan.Samples)

	if missing := plan.Faults.OfKind(core.FaultMissingIndex); len(missing) > 0 {
		// Index absent: the container scan itself is the recovery act.
		hash, herr := hashPrefix(srcPath, -1)
		if herr != nil {
			hash = "unavailable: " + herr.Error()
		}
		plan.Records = append(plan.Records, RecoveryRecord{
			Camera:         camera,
			OriginalOffset: firstByteOffset(res.Samples),
			Reason:         string(core.FaultMissingIndex),
			Hash:           hash,
			Start:          core.Time{},
			End:            plan.Duration,
		})
	}

	frame := plan.FPS.FrameDuration()
	if expect.Ticks > 0 && expect.Sub(plan.Duration).Ticks > frame.Ticks {
		// Decodable prefix ends before the expected timeline: tail loss.
		plan.Faults = append(plan.Faults, core.NewFault(core.FaultCorruptMedia,
			lastByteOffset(res.Samples), true,
			"tail truncated; valid prefix kept, gap marked"))
		plan.Gaps = append(plan.Gaps, Gap{Start: plan.Duration, End: expect,
			Reason: string(core.FaultCorruptMedia)})
		hash, herr := hashPrefix(srcPath, lastByteOffset(res.Samples))
		if herr != nil {
			hash = "unavailable: " + herr.Error()
		}
		plan.Records = append(plan.Records, RecoveryRecord{
			Camera:         camera,
			OriginalOffset: lastByteOffset(res.Samples),
			Reason:         string(core.FaultCorruptMedia),
			Hash:           hash,
			Start:          core.Time{},
			End:            plan.Duration,
		})
	}
	return plan, nil
}

// normalizePTS shifts samples so the earliest presentation time is zero,
// absorbing container start offsets (e.g. the MPEG-TS 1.4 s lead-in).
func normalizePTS(samples []adapter.MediaSample) []adapter.MediaSample {
	if len(samples) == 0 {
		return samples
	}
	min := samples[0].PTS
	for _, s := range samples[1:] {
		if s.PTS.Cmp(min) < 0 {
			min = s.PTS
		}
	}
	out := make([]adapter.MediaSample, len(samples))
	for i, s := range samples {
		s.PTS = s.PTS.Sub(min)
		out[i] = s
	}
	return out
}

// samplesDuration is the end of the last sample in presentation order.
func samplesDuration(samples []adapter.MediaSample) core.Time {
	var max core.Time
	for _, s := range samples {
		if e := s.PTS.Add(s.Duration); e.Cmp(max) > 0 {
			max = e
		}
	}
	return max
}

func lastByteOffset(samples []adapter.MediaSample) int64 {
	var max int64 = -1
	for _, s := range samples {
		if s.Offset > max {
			max = s.Offset
		}
	}
	return max
}

func firstByteOffset(samples []adapter.MediaSample) int64 {
	min := int64(-1)
	for _, s := range samples {
		if s.Offset >= 0 && (min < 0 || s.Offset < min) {
			min = s.Offset
		}
	}
	return min
}

// hashPrefix streams the first n bytes of path through sha256 (n<0 hashes
// the whole file). Reads are chunked so memory stays bounded.
func hashPrefix(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	r := io.Reader(f)
	if n >= 0 {
		r = io.LimitReader(f, n)
	}
	if _, err := io.CopyBuffer(h, r, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
