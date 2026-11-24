// Package metrics recomputes the acceptance numbers from the files that were
// actually written. Nothing here trusts an in-memory counter: bitrate comes
// from the source container's byte count and duration, quality from the
// frame payloads, and manifest skew from the manifest and timeline.
package metrics

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	"mediad/internal/model"
)

// TrackMetrics is the per-track report. Field names match the terminal
// contract: track, frames_ok, frames_bad, index_entries, cache_verified,
// target_kbps, actual_kbps, bitrate_delta_pct, quality_mean,
// manifest_skew_ms, rtp_lost.
type TrackMetrics struct {
	Track           uint32
	FramesOK        int
	FramesBad       int
	IndexEntries    int
	CacheVerified   int
	TargetKbps      float64
	ActualKbps      float64
	BitrateDeltaPct float64
	QualityMean     float64
	ManifestSkewMS  float64
	RTPLost         int
}

// SourceBytes is the on-disk container size and its media duration in seconds.
// Both are measured from the written file, never from generation parameters.
type SourceBytes struct {
	Bytes       int64
	DurationSec float64
}

// FromSource computes the actual bitrate: total container bytes (including
// headers) over the timeline duration measured from first to last frame PTS.
// Target is the declared track bitrate. Delta is signed, in percent.
func FromSource(src SourceBytes, targetKbps float64) (actual, deltaPct float64) {
	if src.DurationSec <= 0 || src.Bytes <= 0 {
		return 0, 0
	}
	actual = float64(src.Bytes) * 8 / 1000 / src.DurationSec
	if targetKbps <= 0 {
		return actual, 0
	}
	deltaPct = (actual - targetKbps) / targetKbps * 100
	return actual, deltaPct
}

// QualitySamples summarizes a track's frame-quality distribution.
type QualitySamples struct {
	Count int
	Mean  float64
	Min   float64
	Max   float64
	Bad   int
}

// QualityStats computes the mean of per-frame scores. Scores are supplied per
// usable frame by the caller (for example from decoded luma variance) so this
// package stays independent of the image codec.
func QualityStats(scores []float64, bad int) QualitySamples {
	q := QualitySamples{Bad: bad, Count: len(scores)}
	if len(scores) == 0 {
		return q
	}
	sum := 0.0
	q.Min, q.Max = math.Inf(1), math.Inf(-1)
	for _, s := range scores {
		sum += s
		if s < q.Min {
			q.Min = s
		}
		if s > q.Max {
			q.Max = s
		}
	}
	q.Mean = sum / float64(len(scores))
	return q
}

// ManifestRow is one parsed manifest line.
type ManifestRow struct {
	Track uint32
	Frame uint64
	PTS   int64
}

// ReadManifest parses a manifest CSV, skipping the header. It is used to
// recompute skew from the file as written rather than from the rows the
// writer intended.
func ReadManifest(r io.Reader) ([]ManifestRow, error) {
	cr := csv.NewReader(bufio.NewReader(r))
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("manifest: read header: %w", err)
	}
	if len(header) < 3 {
		return nil, fmt.Errorf("manifest: too few columns")
	}
	var rows []ManifestRow
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		t, err := strconv.ParseUint(rec[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("manifest: bad track %q: %w", rec[0], err)
		}
		f, err := strconv.ParseUint(rec[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("manifest: bad frame %q: %w", rec[1], err)
		}
		p, err := strconv.ParseInt(rec[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("manifest: bad pts %q: %w", rec[2], err)
		}
		rows = append(rows, ManifestRow{Track: uint32(t), Frame: f, PTS: p})
	}
	return rows, nil
}

// SkewFromManifest recomputes the manifest alignment error in milliseconds:
// the largest deviation between a row's PTS and the PTS implied by its frame
// number at the manifest's own cadence.
//
// The cadence is the median consecutive PTS delta, and the reference is the
// last row, so a single nudge does not bias the baseline it is measured
// against. A legitimate gap shifts later rows once; the reference row is
// chosen at the end of the file where a gap has already been absorbed.
func SkewFromManifest(rows []ManifestRow, timescale uint32) (float64, error) {
	if len(rows) < 2 || timescale == 0 {
		return 0, nil
	}
	steps := make([]int64, 0, len(rows)-1)
	for i := 1; i < len(rows); i++ {
		d := rows[i].PTS - rows[i-1].PTS
		if d > 0 {
			steps = append(steps, d)
		}
	}
	if len(steps) == 0 {
		return 0, fmt.Errorf("manifest: no monotonic frames")
	}
	step := median(steps)
	ref := rows[len(rows)-1]
	maxSkew := int64(0)
	for _, row := range rows {
		want := ref.PTS + int64(row.Frame-ref.Frame)*step
		d := row.PTS - want
		if d < 0 {
			d = -d
		}
		if d > maxSkew {
			maxSkew = d
		}
	}
	return float64(maxSkew) * 1000 / float64(timescale), nil
}

func median(v []int64) int64 {
	c := append([]int64(nil), v...)
	// Insertion sort: step counts are small and this avoids importing sort
	// semantics that could reorder equal values differently.
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j] < c[j-1]; j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
	return c[len(c)/2]
}

// FileSize returns the size of a written artifact, so a metric can never
// disagree with the file it describes.
func FileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// FrameQualityScores derives a per-frame quality score in [0,1] from the
// payload's decoded luminance variance. It is deterministic and cheap, and it
// reads the payload from the written frame rather than the source parameters.
func FrameQualityScores(payloads [][]byte, decode func([]byte) (float64, error)) ([]float64, int) {
	scores := make([]float64, 0, len(payloads))
	bad := 0
	for _, p := range payloads {
		s, err := decode(p)
		if err != nil {
			bad++
			continue
		}
		scores = append(scores, s)
	}
	return scores, bad
}

// CheckThresholds compares a track report to the acceptance contract and
// returns a descriptive error when any bound is exceeded.
func CheckThresholds(m TrackMetrics) error {
	if m.FramesBad > 3 {
		return fmt.Errorf("track %d: frames_bad=%d exceeds 3", m.Track, m.FramesBad)
	}
	if m.BitrateDeltaPct > 5 || m.BitrateDeltaPct < -5 {
		return fmt.Errorf("track %d: bitrate_delta_pct=%.2f exceeds 5%%", m.Track, m.BitrateDeltaPct)
	}
	if m.ManifestSkewMS > 1.0 {
		return fmt.Errorf("track %d: manifest_skew_ms=%.3f exceeds 1ms", m.Track, m.ManifestSkewMS)
	}
	return nil
}

// IsVerifyFailure reports whether err comes from a threshold check; used to
// map exit codes without importing the caller's error wrappers.
func IsVerifyFailure(err error) bool { return err != nil && model.ClassOf(err) == model.ClassVerify }

// Counter tallies cache outcomes for the report. The pipeline classifies each
// lookup and calls one method, so metrics needs no knowledge of cache errors.
type Counter struct {
	Hits     int
	Misses   int
	Invalid  int
	Verified int // entries proven retrievable through a full provenance check
}

// Hit records a verified cache hit.
func (c *Counter) Hit() { c.Hits++ }

// Miss records an absent entry that had to be recomputed.
func (c *Counter) Miss() { c.Misses++ }

// Rejected records an entry rejected by a provenance or integrity check.
func (c *Counter) Rejected() { c.Invalid++ }
