package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"

	"segmanifest/internal/manifest"
	"segmanifest/internal/verify"
)

type Drift struct {
	Samples      int
	MaxDriftMS   float64
	MeanDriftMS  float64
	P95DriftMS   float64
	ResyncPoints int
}

type StatsJSON struct {
	Representations []RepStats `json:"representations"`
	Overall         RepStats   `json:"overall"`
}

type RepStats struct {
	RepresentationID string  `json:"representation_id"`
	Bandwidth        uint64  `json:"bandwidth"`
	Segments         int     `json:"segments"`
	MaxDriftMS       float64 `json:"max_drift_ms"`
	MeanDriftMS      float64 `json:"mean_drift_ms"`
	P95DriftMS       float64 `json:"p95_drift_ms"`
	ResyncPoints     int     `json:"resync_points"`
}

func Overall(m *manifest.Manifest) Drift {
	var samples []int64
	for _, rep := range m.Representations {
		for _, seg := range rep.Segments {
			samples = append(samples, abs(seg.DriftUS))
		}
	}
	return driftFromSamples(samples, len(m.ResyncPoints))
}

func PerRepresentation(m *manifest.Manifest) []RepStats {
	stats := make([]RepStats, 0, len(m.Representations))
	for _, rep := range m.Representations {
		var samples []int64
		resyncs := 0
		for _, seg := range rep.Segments {
			samples = append(samples, abs(seg.DriftUS))
		}
		for _, point := range m.ResyncPoints {
			if point.RepresentationID == rep.ID {
				resyncs++
			}
		}
		d := driftFromSamples(samples, resyncs)
		stats = append(stats, RepStats{
			RepresentationID: rep.ID,
			Bandwidth:        rep.Bandwidth,
			Segments:         len(rep.Segments),
			MaxDriftMS:       d.MaxDriftMS,
			MeanDriftMS:      d.MeanDriftMS,
			P95DriftMS:       d.P95DriftMS,
			ResyncPoints:     d.ResyncPoints,
		})
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Bandwidth < stats[j].Bandwidth })
	return stats
}

func driftFromSamples(samples []int64, resyncPoints int) Drift {
	d := Drift{Samples: len(samples), ResyncPoints: resyncPoints}
	if len(samples) == 0 {
		return d
	}
	var total int64
	var maxUS int64
	ordered := append([]int64(nil), samples...)
	for _, value := range samples {
		total += value
		if value > maxUS {
			maxUS = value
		}
	}
	d.MaxDriftMS = ms2(maxUS)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	rank := (95*len(ordered) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	d.MeanDriftMS = ms2f(float64(total) / float64(len(samples)))
	d.P95DriftMS = ms2(ordered[rank-1])
	return d
}

func PrintPlan(w io.Writer, m *manifest.Manifest, gridPoints int) {
	d := Overall(m)
	fmt.Fprintf(w, "representations=%d segments=%d grid_points=%d max_drift_ms=%s p95_drift_ms=%s resync_points=%d discontinuities=%d\n",
		len(m.Representations), segmentCount(m), gridPoints, decimal2(d.MaxDriftMS), decimal2(d.P95DriftMS),
		len(m.ResyncPoints), len(m.Discontinuities))
}

func PrintStats(w io.Writer, m *manifest.Manifest, asJSON bool) error {
	stats := PerRepresentation(m)
	overall := Overall(m)
	if asJSON {
		payload := StatsJSON{Representations: stats}
		payload.Overall = RepStats{
			RepresentationID: "overall",
			Segments:         segmentCount(m),
			MaxDriftMS:       overall.MaxDriftMS,
			MeanDriftMS:      overall.MeanDriftMS,
			P95DriftMS:       overall.P95DriftMS,
			ResyncPoints:     overall.ResyncPoints,
		}
		return encodeJSON(w, payload)
	}
	for _, stat := range stats {
		fmt.Fprintf(w, "representation=%s bandwidth=%d segments=%d max_drift_ms=%s mean_drift_ms=%s p95_drift_ms=%s resync_points=%d\n",
			stat.RepresentationID, stat.Bandwidth, stat.Segments, decimal2(stat.MaxDriftMS), decimal2(stat.MeanDriftMS), decimal2(stat.P95DriftMS), stat.ResyncPoints)
	}
	fmt.Fprintf(w, "overall segments=%d max_drift_ms=%s mean_drift_ms=%s p95_drift_ms=%s resync_points=%d\n",
		overall.Samples, decimal2(overall.MaxDriftMS), decimal2(overall.MeanDriftMS), decimal2(overall.P95DriftMS), overall.ResyncPoints)
	return nil
}

func PrintVerify(w io.Writer, result verify.Result, asJSON bool) error {
	if asJSON {
		return encodeJSON(w, result)
	}
	fmt.Fprintf(w, "expanded_paths=%d duplicate_paths=%d holes=%d overlap_ms=%s roundtrip_ok=%t\n",
		result.ExpandedPaths, result.DuplicatePaths, result.Holes, number(result.OverlapMS), result.RoundtripOK)
	return nil
}

func encodeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func segmentCount(m *manifest.Manifest) int {
	count := 0
	for _, rep := range m.Representations {
		count += len(rep.Segments)
	}
	return count
}

func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func ms2(us int64) float64 {
	return math.Round(float64(us)/10) / 100
}

func ms2f(ms float64) float64 {
	return math.Round(ms*100) / 100
}

func decimal2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
