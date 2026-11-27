// Package audit quantifies delivery conformance and writes traceable JSONL
// audit records. It computes per-variant target/actual bitrate deltas, mean
// PSNR/SSIM, layer timing spans and the cross-rendition manifest alignment
// error, then emits one JSON object per line so records stay append-only and
// stream-friendly for long assets.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"

	"avcore/internal/clock"
	"avcore/internal/state"
)

// VariantMetrics summarizes one rendition against its targets.
type VariantMetrics struct {
	Name             string  `json:"name"`
	TargetBPS        int     `json:"target_bps"`
	ActualBPS        int     `json:"actual_bps"`
	BitrateDeltaPct  float64 `json:"bitrate_delta_pct"`
	PSNRMeanDB       float64 `json:"psnr_mean_db"`
	SSIMMean         float64 `json:"ssim_mean"`
	StartTicks       int64   `json:"start_ticks"`
	EndTicks         int64   `json:"end_ticks"`
	SegmentCount     int     `json:"segment_count"`
}

// Summary is the full conformance report for a job.
type Summary struct {
	Case             string           `json:"case"`
	GeneratedAt      string           `json:"generated_at"`
	Variants         []VariantMetrics `json:"variants"`
	MaxAlignmentMS   float64          `json:"max_alignment_ms"`
	BadRecords       int              `json:"bad_records"`
	ReusedSegments   int              `json:"reused_segments"`
	ManifestSHAEqual bool             `json:"manifest_sha256_equal"`
	ManifestSHA256   string           `json:"manifest_sha256"`
}

// ComputeVariant aggregates committed segment records into one variant's
// metrics. Actual bitrate is total bits over the summed segment duration.
func ComputeVariant(name string, targetBPS int, recs []state.SegmentRecord) VariantMetrics {
	m := VariantMetrics{Name: name, TargetBPS: targetBPS, SegmentCount: len(recs)}
	if len(recs) == 0 {
		return m
	}
	var totalBytes int64
	var totalTicks int64
	var psnrSum, ssimSum float64
	m.StartTicks = recs[0].StartTicks
	m.EndTicks = recs[len(recs)-1].EndTicks
	for _, r := range recs {
		totalBytes += r.Bytes
		totalTicks += r.EndTicks - r.StartTicks
		psnrSum += r.PSNRMeanDB
		ssimSum += r.SSIMMean
	}
	n := float64(len(recs))
	m.PSNRMeanDB = round(psnrSum/n, 4)
	m.SSIMMean = round(ssimSum/n, 6)
	if totalTicks > 0 {
		secs := float64(totalTicks) / float64(clock.TicksPerSecond)
		m.ActualBPS = int(float64(totalBytes*8) / secs)
	}
	if targetBPS > 0 {
		m.BitrateDeltaPct = round(float64(m.ActualBPS-targetBPS)/float64(targetBPS)*100, 3)
	}
	m.StartTicks = recs[0].StartTicks
	m.EndTicks = recs[len(recs)-1].EndTicks
	return m
}

// AlignmentMS returns the largest boundary discrepancy in milliseconds across
// the supplied per-variant cut-start lists. Equal-length, identically-snapped
// plans yield zero.
func AlignmentMS(variantStarts [][]int64) float64 {
	if len(variantStarts) < 2 {
		return 0
	}
	base := variantStarts[0]
	var maxTicks int64
	for vi := 1; vi < len(variantStarts); vi++ {
		other := variantStarts[vi]
		limit := len(base)
		if len(other) < limit {
			limit = len(other)
		}
		for i := 0; i < limit; i++ {
			d := base[i] - other[i]
			if d < 0 {
				d = -d
			}
			if d > maxTicks {
				maxTicks = d
			}
		}
	}
	return round(float64(maxTicks)*1000/float64(clock.TicksPerSecond), 3)
}

// Logger appends JSONL audit records safely across goroutines.
type Logger struct {
	mu sync.Mutex
	w  *bufio.Writer
	f  *os.File
}

// NewLogger creates (or truncates) an audit JSONL file.
func NewLogger(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{w: bufio.NewWriterSize(f, 64*1024), f: f}, nil
}

// Record is one JSONL entry.
type Record map[string]any

// Emit writes one record as a single JSON line.
func (l *Logger) Emit(rec Record) error {
	if _, ok := rec["ts"]; !ok {
		rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(append(raw, '\n')); err != nil {
		return err
	}
	return l.w.Flush()
}

// Close flushes and releases the file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Flush(); err != nil {
		return err
	}
	return l.f.Close()
}

func round(v float64, digits int) float64 {
	pow := 1.0
	for i := 0; i < digits; i++ {
		pow *= 10
	}
	return float64(int64(v*pow+sign(v)*0.5)) / pow
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
