// Package manifest reconciles a delivery manifest with the media that was
// actually produced. All comparisons happen on the shared rational time base
// and errors are reported in integer microseconds, so repeated runs agree
// exactly.
package manifest

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"mediad/internal/clock"
	"mediad/internal/model"
)

// Version is the manifest schema version.
const Version = 1

// Timebase is the shared tick rate used to express manifest times. 90000 is
// the MPEG-2 system clock and an integer multiple of every rate mediad
// handles (24/25/30/60 and 1000/1001 families).
const Timebase = 90000

// TrackTarget is the independently authored expectation for one track.
type TrackTarget struct {
	TrackID uint32         `json:"track_id"`
	Start   clock.Rational `json:"start"`
	End     clock.Rational `json:"end"`
	// TargetBitrateKbps is the bitrate the delivery spec requires.
	TargetBitrateKbps float64 `json:"target_bitrate_kbps"`
}

// TrackResult is one track's alignment against its target.
type TrackResult struct {
	TrackID uint32 `json:"track_id"`
	// TargetStartUS, ActualStartUS and the End pair are in microseconds on
	// the shared time base.
	TargetStartUS int64 `json:"target_start_us"`
	ActualStartUS int64 `json:"actual_start_us"`
	TargetEndUS   int64 `json:"target_end_us"`
	ActualEndUS   int64 `json:"actual_end_us"`
	StartErrorUS  int64 `json:"start_error_us"`
	EndErrorUS    int64 `json:"end_error_us"`

	TargetBitrateKbps float64 `json:"target_bitrate_kbps"`
	ActualBitrateKbps float64 `json:"actual_bitrate_kbps"`
	BitrateDeltaPct   float64 `json:"bitrate_delta_pct"`

	SampleCount int `json:"sample_count"`
	// DamagedSamples and SynthesisedSamples are carried through so a
	// delivery can be rejected for quality without re-deriving them.
	DamagedSamples     int `json:"damaged_samples"`
	SynthesisedSamples int `json:"synthesised_samples"`
}

// Manifest is the machine-readable delivery record. Field order is fixed and
// there are no maps, so JSON encoding is canonical and its SHA-256 is stable
// for identical inputs.
type Manifest struct {
	Version  int           `json:"version"`
	Seed     int64         `json:"seed"`
	Timebase int64         `json:"timebase"`
	Format   string        `json:"format"`
	Tracks   []TrackResult `json:"tracks"`
	// AlignmentUS is the worst start/end error across all tracks.
	AlignmentUS int64 `json:"alignment_us"`
	// TotalSamples and TotalBytes describe the delivered media.
	TotalSamples int   `json:"total_samples"`
	TotalBytes   int64 `json:"total_bytes"`
}

// Build reconciles targets against actual tracks.
func Build(seed int64, format string, targets []TrackTarget, tracks []model.Track) (Manifest, error) {
	m := Manifest{Version: Version, Seed: seed, Timebase: Timebase, Format: format}
	targetsByID := make(map[uint32]TrackTarget, len(targets))
	for _, t := range targets {
		targetsByID[t.TrackID] = t
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].ID < tracks[j].ID })
	seen := make(map[uint32]bool, len(tracks))
	for _, tr := range tracks {
		tgt, ok := targetsByID[tr.ID]
		if !ok {
			return m, model.New(model.KindInvalidMedia, "manifest.align",
				fmt.Sprintf("track %d appears in the media but not in the manifest", tr.ID)).WithTrack(tr.ID)
		}
		if seen[tr.ID] {
			return m, model.New(model.KindInvalidMedia, "manifest.align",
				fmt.Sprintf("track %d appears twice", tr.ID)).WithTrack(tr.ID)
		}
		seen[tr.ID] = true
		res, err := alignOne(tgt, tr)
		if err != nil {
			return m, err
		}
		m.Tracks = append(m.Tracks, res)
		m.TotalSamples += res.SampleCount
	}
	for _, t := range targets {
		if !seen[t.TrackID] {
			return m, model.New(model.KindInvalidMedia, "manifest.align",
				fmt.Sprintf("manifest expects track %d but the media has no such track", t.TrackID)).WithTrack(t.TrackID)
		}
	}
	for _, r := range m.Tracks {
		if abs64(r.StartErrorUS) > abs64(m.AlignmentUS) {
			m.AlignmentUS = abs64(r.StartErrorUS)
		}
		if abs64(r.EndErrorUS) > abs64(m.AlignmentUS) {
			m.AlignmentUS = abs64(r.EndErrorUS)
		}
	}
	return m, nil
}

func alignOne(t TrackTarget, tr model.Track) (TrackResult, error) {
	var res TrackResult
	res.TrackID = tr.ID
	if len(tr.Samples) == 0 {
		return res, model.New(model.KindInvalidMedia, "manifest.align",
			"track has no samples to align").WithTrack(tr.ID)
	}
	start := tr.Samples[0].DTS
	last := tr.Samples[len(tr.Samples)-1]
	end, err := last.End()
	if err != nil {
		return res, model.Wrap(model.KindInvalidMedia, "manifest.align", "track end", err).WithTrack(tr.ID)
	}
	duration, err := end.Sub(start)
	if err != nil {
		return res, model.Wrap(model.KindInvalidMedia, "manifest.align", "duration", err).WithTrack(tr.ID)
	}
	if res.TargetStartUS, err = t.Start.Rescale(1_000_000); err != nil {
		return res, model.Wrap(model.KindInvalidMedia, "manifest.align", "target start", err).WithTrack(tr.ID)
	}
	if res.TargetEndUS, err = t.End.Rescale(1_000_000); err != nil {
		return res, model.Wrap(model.KindInvalidMedia, "manifest.align", "target end", err).WithTrack(tr.ID)
	}
	if res.ActualStartUS, err = start.Rescale(1_000_000); err != nil {
		return res, model.Wrap(model.KindInvalidMedia, "manifest.align", "actual start", err).WithTrack(tr.ID)
	}
	if res.ActualEndUS, err = end.Rescale(1_000_000); err != nil {
		return res, model.Wrap(model.KindInvalidMedia, "manifest.align", "actual end", err).WithTrack(tr.ID)
	}
	res.StartErrorUS = res.ActualStartUS - res.TargetStartUS
	res.EndErrorUS = res.ActualEndUS - res.TargetEndUS
	res.TargetBitrateKbps = t.TargetBitrateKbps

	var bytes int64
	for _, s := range tr.Samples {
		bytes += int64(s.Size)
		if s.Damaged {
			res.DamagedSamples++
		}
		if s.Synthesised {
			res.SynthesisedSamples++
		}
	}
	res.SampleCount = len(tr.Samples)
	if duration.Sign() > 0 {
		res.ActualBitrateKbps = float64(bytes*8) / 1000 / duration.Float64()
		if t.TargetBitrateKbps > 0 {
			res.BitrateDeltaPct = (res.ActualBitrateKbps - t.TargetBitrateKbps) / t.TargetBitrateKbps * 100
		}
	}
	return res, nil
}

// AlignedMS returns the worst alignment error in milliseconds.
func (m Manifest) AlignedMS() float64 { return float64(m.AlignmentUS) / 1000 }

// WorstBitrateDeltaPct returns the largest absolute bitrate deviation.
func (m Manifest) WorstBitrateDeltaPct() float64 {
	var worst float64
	for _, t := range m.Tracks {
		v := t.BitrateDeltaPct
		if v < 0 {
			v = -v
		}
		if v > worst {
			worst = v
		}
	}
	return worst
}

// Canonical returns the canonical JSON encoding used for the digest.
func (m Manifest) Canonical() ([]byte, error) {
	return json.Marshal(m)
}

// Digest returns the hex SHA-256 of the canonical encoding.
func (m Manifest) Digest() (string, error) {
	b, err := m.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum), nil
}

// Gate applies the delivery thresholds: alignment at most maxAlignMS and
// bitrate deviation at most maxDeltaPct. A failure is a threshold error so
// the CLI exits 4.
func (m Manifest) Gate(maxAlignMS, maxDeltaPct float64) error {
	if got := m.AlignedMS(); got > maxAlignMS {
		return model.New(model.KindThreshold, "manifest.gate",
			fmt.Sprintf("manifest alignment %.3f ms exceeds %.3f ms", got, maxAlignMS))
	}
	if got := m.WorstBitrateDeltaPct(); got > maxDeltaPct {
		return model.New(model.KindThreshold, "manifest.gate",
			fmt.Sprintf("bitrate deviation %.2f%% exceeds %.2f%%", got, maxDeltaPct))
	}
	return nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
