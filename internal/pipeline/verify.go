package pipeline

import (
	"errors"
	"fmt"
	"sort"

	"mediapack/internal/clock"
	"mediapack/internal/demux"
	"mediapack/internal/fault"
	"mediapack/internal/ladder"
	"mediapack/internal/manifest"
	"mediapack/internal/metrics"
	"mediapack/internal/segment"
)

// ErrManifestMismatch indicates the manifest does not match a fresh
// recomputation from the input media.
var ErrManifestMismatch = errors.New("manifest does not match input media")

// Verify re-demuxes the input, recomputes the segment plan with the
// configuration stored in the manifest and compares the deterministic
// digest. It returns the expected manifest (recomputed) for reporting.
func Verify(input, manifestPath string) (*manifest.Manifest, *fault.List, error) {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return nil, nil, err
	}
	c, err := demux.Open(input)
	if err != nil {
		return nil, nil, err
	}
	if c.Faults.Blocking(fault.PolicyStrict) && c.Faults.MaxSeverity() >= fault.Fatal {
		return nil, c.Faults, nil
	}
	if c.Fingerprint != m.Fingerprint {
		return nil, c.Faults, fmt.Errorf("%w: fingerprint differs", ErrManifestMismatch)
	}
	reps := c.VideoTracks()
	if len(reps) == 0 {
		return nil, c.Faults, fmt.Errorf("no video track in input")
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].ID < reps[j].ID })
	master := reps[len(reps)-1]
	bounds := segment.Cut(master, clock.TicksFromMillis(m.SegmentTargetMs))
	align := ladder.Align(reps, bounds, clock.TicksFromMillis(m.ToleranceMs))
	var targets []uint64
	for _, r := range m.Representations {
		targets = append(targets, r.TargetBitrate)
	}
	expected, err := metrics.Assemble(metrics.Input{
		Container: c, Bounds: bounds, Alignment: align,
		TargetBitrates: targets, SegmentTargetMs: m.SegmentTargetMs,
		ToleranceMs: m.ToleranceMs,
	})
	if err != nil {
		return nil, c.Faults, err
	}
	expected.Digest = expected.ComputeDigest()
	if expected.Digest != m.Digest {
		return nil, c.Faults, fmt.Errorf("%w: digest %s != recomputed %s",
			ErrManifestMismatch, m.Digest, expected.Digest)
	}
	return m, c.Faults, nil
}

// IsManifestMismatch reports whether err is a manifest/media mismatch.
func IsManifestMismatch(err error) bool {
	return errors.Is(err, ErrManifestMismatch)
}
