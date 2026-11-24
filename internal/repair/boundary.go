package repair

import (
	"fmt"
	"sort"

	"mediaflow/internal/clock"
)

// LayerResult is one representation's independent recovery. Layers never
// share mutable state, so a failure here cannot rewrite another layer's
// numbers.
type LayerResult struct {
	Name       string  `json:"name"`
	Index      int     `json:"index"`
	TrackID    uint32  `json:"track_id"`
	StartUs    int64   `json:"start_us"`
	EndUs      int64   `json:"end_us"`
	Digest     string  `json:"digest"`
	Recovered  int     `json:"recovered_fragments"`
	Partial    int     `json:"partial_fragments"`
	Missing    int     `json:"missing_fragments"`
	Duplicates int     `json:"duplicate_fragments"`
	Untrusted  int     `json:"untrusted_fragments"`
	MissingMS  float64 `json:"missing_ms"`
	Error      string  `json:"error,omitempty"`
	Failed     bool    `json:"failed"`
}

// LayerSummary converts a recovery into a compact layer result.
func LayerSummary(name string, idx int, rec Recovery) LayerResult {
	start, end, ok := rec.Boundary()
	if !ok {
		return LayerResult{
			Name: name, Index: idx, TrackID: rec.TrackID, Digest: rec.Digest,
			Recovered: rec.Recovered, Partial: rec.Partial, Missing: rec.MissingCount,
			Duplicates: rec.Duplicates, Untrusted: rec.Untrusted,
			MissingMS: rec.Missing.TotalMS(), Failed: true, Error: "no recoverable fragment",
		}
	}
	return LayerResult{
		Name: name, Index: idx, TrackID: rec.TrackID, StartUs: start, EndUs: end,
		Digest: rec.Digest, Recovered: rec.Recovered, Partial: rec.Partial,
		Missing: rec.MissingCount, Duplicates: rec.Duplicates, Untrusted: rec.Untrusted,
		MissingMS: rec.Missing.TotalMS(),
	}
}

// BoundaryCheck compares one layer against the shared reference boundary on
// the common clock.
type BoundaryCheck struct {
	Layer            string  `json:"layer"`
	ReferenceLayer   string  `json:"reference_layer"`
	StartDeltaMS     float64 `json:"start_delta_ms"`
	EndDeltaMS       float64 `json:"end_delta_ms"`
	MaxDeltaMS       float64 `json:"max_delta_ms"`
	Mismatch         bool    `json:"mismatch"`
	ReferenceStartUs int64   `json:"reference_start_us"`
	ReferenceEndUs   int64   `json:"reference_end_us"`
	LayerStartUs     int64   `json:"layer_start_us"`
	LayerEndUs       int64   `json:"layer_end_us"`
	ToleranceMS      float64 `json:"tolerance_ms"`
}

// CrossCheck compares every layer's start/end against the reference layer.
// The reference is chosen deterministically: the layer with the lowest index
// whose recovery succeeded and whose digest is the majority digest; ties
// break by index. A layer with mismatched boundaries is flagged, never
// "corrected" to match the reference.
func CrossCheck(layers []LayerResult, toleranceMS float64) (ref string, checks []BoundaryCheck, mismatches int) {
	var okLayers []LayerResult
	for _, l := range layers {
		if l.Failed || l.EndUs <= l.StartUs {
			continue
		}
		okLayers = append(okLayers, l)
	}
	if len(okLayers) == 0 {
		return "", nil, 0
	}
	// Vote by digest; the most common digest wins, ties go to the lowest index.
	counts := make(map[string]int)
	for _, l := range okLayers {
		counts[l.Digest]++
	}
	bestCount := -1
	ref = okLayers[0].Name
	refStart, refEnd := okLayers[0].StartUs, okLayers[0].EndUs
	for _, l := range okLayers {
		if counts[l.Digest] > bestCount {
			bestCount = counts[l.Digest]
			ref, refStart, refEnd = l.Name, l.StartUs, l.EndUs
		}
	}
	for _, l := range okLayers {
		startDelta := float64(l.StartUs-refStart) / float64(clock.UsPerMS)
		endDelta := float64(l.EndUs-refEnd) / float64(clock.UsPerMS)
		maxDelta := max(abs(startDelta), abs(endDelta))
		mismatch := maxDelta > toleranceMS
		checks = append(checks, BoundaryCheck{
			Layer: l.Name, ReferenceLayer: ref,
			StartDeltaMS: startDelta, EndDeltaMS: endDelta, MaxDeltaMS: maxDelta, Mismatch: mismatch, ReferenceStartUs: refStart, ReferenceEndUs: refEnd,
			LayerStartUs: l.StartUs, LayerEndUs: l.EndUs, ToleranceMS: toleranceMS,
		})
		if mismatch {
			mismatches++
		}
	}
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].Layer < checks[j].Layer })
	return ref, checks, mismatches
}

// SyncErrorMS is the maximum boundary disagreement across successful layers:
// the number reported as sync_error_ms in the summary.
func CrossCheckMaxMS(checks []BoundaryCheck) float64 {
	var max float64
	for _, c := range checks {
		if c.MaxDeltaMS > max {
			max = c.MaxDeltaMS
		}
	}
	return max
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// CompareLayers is a convenience wrapper returning a formatted reference
// description for terminal output.
func CompareLayers(layers []LayerResult, toleranceMS float64) string {
	ref, checks, n := CrossCheck(layers, toleranceMS)
	if ref == "" {
		return "no comparable layer"
	}
	return fmt.Sprintf("reference=%s layers=%d mismatches=%d max_delta=%.3fms",
		ref, len(checks), n, CrossCheckMaxMS(checks))
}
