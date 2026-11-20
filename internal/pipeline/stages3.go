package pipeline

import (
	"github.com/mediamend/mediamend/internal/graph"
	"github.com/mediamend/mediamend/internal/model"
)

// graphBlocked rebuilds the project graph and returns the tracks transitively
// blocked by the given failed track ids.
func graphBlocked(res *Result, failed []string) []string {
	g := graph.New()
	for id, tr := range res.Tracks {
		_ = g.AddTrack(id, tr.Kind)
	}
	// Re-derive the video->audio dependency edges from the project spec via
	// track kinds using the same deterministic pairing as runGraph.
	var audios, videos []string
	for id, tr := range res.Tracks {
		switch tr.Kind {
		case graph.KindAudio:
			audios = append(audios, id)
		case graph.KindVideo:
			videos = append(videos, id)
		}
	}
	for _, a := range audios {
		_ = g.AddTrack(a, graph.KindAudio)
	}
	for i, v := range videos {
		if len(audios) > 0 {
			_ = g.AddDependency(v, audios[i%len(audios)])
		}
	}
	return g.BlockedBy(failed)
}

// finalizeMetrics computes the corruption-location precision and fault totals.
// Precision is located_corrupt_faults / expected_corrupt_frames; when the
// fixture injected no corruption it is 1.0 (nothing to miss).
func finalizeMetrics(cfg Config, res *Result) {
	corrupt := 0
	for _, f := range res.Faults {
		if f.Class == model.FaultChecksum {
			corrupt++
		}
	}
	res.CorruptLocated = corrupt
	expected := len(cfg.ExpectedCorrupt) * len(cfg.Project.Tracks)
	if expected == 0 {
		res.LocationPrecision = 1.0
	} else {
		res.LocationPrecision = float64(corrupt) / float64(expected)
	}
	res.FaultCount = len(res.Faults)
	for _, f := range res.Faults {
		if f.Severity == model.SeverityFatal {
			res.FatalCount++
		}
	}
	finalizeTrackStatus(res)
}
