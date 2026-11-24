package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"mediaflow/internal/clock"
	"mediaflow/internal/repair"
)

// Summary is the single structured JSON document emitted by the demo. Every
// acceptance field named in the specification lives here under a stable name.
type Summary struct {
	Seed            int64   `json:"seed"`
	Representations int     `json:"representations"`
	Loss            float64 `json:"loss"`
	TruncateEvery   int     `json:"truncate_every"`
	GeneratedBytes  int     `json:"generated_bytes"`
	TotalSegments   int     `json:"total_segments"`
	TotalFragments  int     `json:"total_fragments"`
	TotalSamples    int     `json:"total_samples"`

	Layers []LayerReport `json:"layers"`

	DamagedRanges         []inspectRange   `json:"damaged_ranges"`
	DamagedCount          int              `json:"damaged_range_count"`
	GOPIntervalsMS        []float64        `json:"gop_interval_ms"`
	BoundaryRef           string           `json:"boundary_reference"`
	BoundaryChecks        []repairBoundary `json:"boundary_checks"`
	BoundaryMismatchCount int              `json:"boundary_mismatch_count"`
	SyncErrorMS           float64          `json:"sync_error_ms"`
	BoundaryMismatch      bool             `json:"boundary_mismatch"`

	LossRecoveryRate         float64 `json:"loss_recovery_rate"`
	PacketsSent              int     `json:"packets_sent"`
	PacketsDelivered         int     `json:"packets_delivered"`
	PacketsLost              int     `json:"packets_lost"`
	Unrecoverable            int     `json:"unrecoverable_packets"`
	Duplicates               int     `json:"duplicate_packets"`
	Reordered                int     `json:"reordered_packets"`
	Recovered                int     `json:"recovered_packets"`
	UnrecoverableOutOfWindow int     `json:"unrecoverable_out_of_window"`

	LatencyP50MS float64 `json:"p50_latency_ms"`
	LatencyP95MS float64 `json:"p95_latency_ms"`
	LatencyMaxMS float64 `json:"max_latency_ms"`
	LatencyScope string  `json:"latency_scope"`
	ElapsedMS    float64 `json:"elapsed_ms"`

	DeterministicDigest string     `json:"recovery_digest"`
	LayerDigests        []string   `json:"layer_digests"`
	Thresholds          Thresholds `json:"thresholds"`
	Pass                bool       `json:"pass"`
	Failures            []string   `json:"failures,omitempty"`

	Truth []TruthEntry `json:"truth"`
}

// Thresholds records the acceptance gates actually applied.
type Thresholds struct {
	MaxSyncErrorMS   float64 `json:"sync_error_ms_max"`
	MinRecoveryRate  float64 `json:"loss_recovery_rate_min"`
	MaxRecoveryRate  float64 `json:"loss_recovery_rate_max"`
	MaxP95LatencyMS  float64 `json:"p95_latency_ms_max"`
	MaxBoundaryDelta float64 `json:"boundary_tolerance_ms"`
}

// TruthEntry is the injected damage summary for one layer, so a reader can
// compare what was planted with what was reported.
type TruthEntry struct {
	Layer      string      `json:"layer"`
	Injections []Injection `json:"injections"`
}

// inspectRange mirrors inspect.DamageRange for the top-level array.
type inspectRange struct {
	Layer         string `json:"layer"`
	StartUS       int64  `json:"start_us"`
	EndUS         int64  `json:"end_us"`
	TrackID       uint32 `json:"track_id"`
	Kind          string `json:"kind"`
	Segment       int    `json:"segment"`
	Offset        uint64 `json:"offset"`
	PositionKnown bool   `json:"position_known"`
}

// repairBoundary mirrors repair.BoundaryCheck for JSON.
type repairBoundary struct {
	Layer          string  `json:"layer"`
	ReferenceLayer string  `json:"reference_layer"`
	StartDeltaMS   float64 `json:"start_delta_ms"`
	EndDeltaMS     float64 `json:"end_delta_ms"`
	MaxDeltaMS     float64 `json:"max_delta_ms"`
	Mismatch       bool    `json:"mismatch"`
}

// Run executes the full fan-out/fan-in for cfg and returns the summary. The
// layers are processed sequentially in a fixed order so the output is exactly
// reproducible; the work inside each layer is already cheap enough to meet the
// latency budget, and determinism is worth more than wall-clock parallelism
// here. (Concurrent execution is available via RunParallel.)
func Run(cfg Config) (Summary, error) {
	start := time.Now()
	if err := cfg.Validate(); err != nil {
		return Summary{}, err
	}
	sources, err := BuildSources(cfg)
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{
		Seed:            cfg.Seed,
		Representations: cfg.Representations,
		Loss:            cfg.Loss,
		TruncateEvery:   cfg.TruncateEvery,
		LatencyScope:    "per-representation wall time, scan+inspect+recover+transport, macOS local run",
		Thresholds: Thresholds{
			MaxSyncErrorMS:   cfg.SyncTolMS,
			MinRecoveryRate:  0.90,
			MaxRecoveryRate:  1.00,
			MaxP95LatencyMS:  100,
			MaxBoundaryDelta: cfg.BoundaryTolMS,
		},
	}
	latencies := make([]float64, 0, len(sources))
	for i, src := range sources {
		summary.GeneratedBytes += len(src.Init)
		for _, s := range src.Segments {
			summary.GeneratedBytes += len(s.Bytes)
		}
		rep := ProcessRepresentation(src, cfg)
		summary.TotalSegments += rep.Segments
		summary.TotalFragments += rep.Fragments
		summary.TotalSamples += rep.Samples
		latencies = append(latencies, float64(rep.ProcessUs)/1000)
		summary.Layers = append(summary.Layers, rep)
		summary.Truth = append(summary.Truth, TruthEntry{Layer: src.Name, Injections: src.Truth})
		summary.LayerDigests = append(summary.LayerDigests, fmt.Sprintf("%s:%s", src.Name, rep.Layer.Digest))
		_ = i
	}

	layerResults := make([]repair.LayerResult, len(summary.Layers))
	for i, l := range summary.Layers {
		layerResults[i] = l.Layer
	}
	ref, checks, mismatches := repair.CrossCheck(layerResults, cfg.MismatchTolMS)
	summary.BoundaryRef = ref
	summary.BoundaryMismatchCount = mismatches
	summary.SyncErrorMS = repair.CrossCheckMaxMS(checks)
	for _, c := range checks {
		summary.BoundaryChecks = append(summary.BoundaryChecks, repairBoundary{
			Layer: c.Layer, ReferenceLayer: c.ReferenceLayer,
			StartDeltaMS: c.StartDeltaMS, EndDeltaMS: c.EndDeltaMS,
			MaxDeltaMS: c.MaxDeltaMS, Mismatch: c.Mismatch,
		})
	}
	summary.BoundaryMismatch = summary.SyncErrorMS > cfg.SyncTolMS

	summary.aggregateDamage()
	summary.aggregateGOP()
	summary.aggregateTransport()

	summary.LatencyP50MS = round3(clock.Percentile(latencies, 0.50))
	summary.LatencyP95MS = round3(clock.Percentile(latencies, 0.95))
	summary.LatencyMaxMS = round3(clock.Percentile(latencies, 1.0))
	summary.ElapsedMS = round3(float64(time.Since(start).Microseconds()) / 1000)
	summary.DeterministicDigest = summaryDigest(summary)
	summary.evaluate()
	return summary, nil
}

func (s *Summary) aggregateDamage() {
	for _, l := range s.Layers {
		for _, d := range l.DamagedRanges {
			s.DamagedRanges = append(s.DamagedRanges, inspectRange{
				Layer: l.Name, StartUS: d.StartUs, EndUS: d.EndUs, TrackID: d.TrackID,
				Kind: string(d.Kind), Segment: d.Segment, Offset: d.Offset, PositionKnown: d.PositionKnown,
			})
		}
	}
	s.DamagedCount = len(s.DamagedRanges)
}

func (s *Summary) aggregateGOP() {
	for _, l := range s.Layers {
		for _, g := range l.GOP {
			s.GOPIntervalsMS = append(s.GOPIntervalsMS, g.Intervals...)
		}
	}
}

func (s *Summary) aggregateTransport() {
	sent, delivered, lost, unrecoverable, dups, reordered, recovered, outOfWindow := 0, 0, 0, 0, 0, 0, 0, 0
	for _, l := range s.Layers {
		sent += l.Transport.PacketsSent
		delivered += l.Transport.Delivered
		lost += l.Transport.Lost
		unrecoverable += l.Transport.Unrecoverable
		dups += l.Transport.Duplicates
		reordered += l.Transport.Reordered
		recovered += l.Transport.Recovered
		outOfWindow += l.Transport.UnrecoverableOutOfWindow
	}
	s.PacketsSent, s.PacketsDelivered = sent, delivered
	s.PacketsLost, s.Unrecoverable = lost, unrecoverable
	s.Duplicates, s.Reordered = dups, reordered
	s.Recovered, s.UnrecoverableOutOfWindow = recovered, outOfWindow
	if sent > 0 {
		s.LossRecoveryRate = round4(float64(delivered) / float64(sent))
	} else {
		s.LossRecoveryRate = 1
	}
}

// evaluate applies the acceptance thresholds and records every failure.
func (s *Summary) evaluate() {
	s.Failures = nil
	if s.SyncErrorMS > s.Thresholds.MaxSyncErrorMS {
		s.Failures = append(s.Failures, fmt.Sprintf("sync_error_ms %.3f > %.1f", s.SyncErrorMS, s.Thresholds.MaxSyncErrorMS))
	}
	if s.LossRecoveryRate < s.Thresholds.MinRecoveryRate || s.LossRecoveryRate > s.Thresholds.MaxRecoveryRate {
		s.Failures = append(s.Failures, fmt.Sprintf("loss_recovery_rate %.4f outside [%.2f, %.2f]",
			s.LossRecoveryRate, s.Thresholds.MinRecoveryRate, s.Thresholds.MaxRecoveryRate))
	}
	if s.LatencyP95MS > s.Thresholds.MaxP95LatencyMS {
		s.Failures = append(s.Failures, fmt.Sprintf("p95_latency_ms %.3f > %.1f", s.LatencyP95MS, s.Thresholds.MaxP95LatencyMS))
	}
	for _, l := range s.Layers {
		if l.Failed {
			s.Failures = append(s.Failures, fmt.Sprintf("layer %s failed: %s", l.Name, l.Failure))
		}
	}
	s.Pass = len(s.Failures) == 0
}

// summaryDigest hashes the deterministic parts of the summary (layer digests,
// damage ranges, boundaries), excluding timing so a rerun with the same seed
// matches.
func summaryDigest(s Summary) string {
	h := sha256.New()
	digests := append([]string(nil), s.LayerDigests...)
	sort.Strings(digests)
	for _, d := range digests {
		fmt.Fprintf(h, "digest|%s\n", d)
	}
	for _, d := range s.DamagedRanges {
		fmt.Fprintf(h, "damage|%s|%d|%d|%s|%d\n", d.Layer, d.StartUS, d.EndUS, d.Kind, d.Segment)
	}
	for _, b := range s.BoundaryChecks {
		fmt.Fprintf(h, "boundary|%s|%.3f|%.3f\n", b.Layer, b.StartDeltaMS, b.EndDeltaMS)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func round3(v float64) float64 { return float64(int64(v*1000+0.5)) / 1000 }
func round4(v float64) float64 { return float64(int64(v*10000+0.5)) / 10000 }
