package pipeline

import (
	"sort"

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
	// Sorting is required: res.Tracks is a map, so without this the pairing
	// below would be nondeterministic across runs.
	sort.Strings(audios)
	sort.Strings(videos)
	for i, v := range videos {
		if len(audios) > 0 {
			_ = g.AddDependency(v, audios[i%len(audios)])
		}
	}
	return g.BlockedBy(failed)
}

// countCorrupt counts located checksum faults and derives location precision.
// Idempotent, so it can run at both the index checkpoint and finalization. On
// a resume that skips ingest, checksum faults are not re-collected, so a value
// already backfilled from the checkpoint is preserved rather than zeroed.
func countCorrupt(cfg Config, res *Result) {
	corrupt := 0
	for _, f := range res.Faults {
		if f.Class == model.FaultChecksum {
			corrupt++
		}
	}
	if res.Resumed && corrupt == 0 && res.CorruptLocated > 0 {
		// Keep the checkpoint-backfilled count and its stored precision.
		if res.LocationPrecision == 0 {
			res.LocationPrecision = 1.0
		}
		return
	}
	res.CorruptLocated = corrupt
	expected := len(cfg.ExpectedCorrupt) * len(cfg.Project.Tracks)
	if expected == 0 {
		res.LocationPrecision = 1.0
	} else {
		res.LocationPrecision = float64(corrupt) / float64(expected)
	}
}

// finalizeMetrics computes the corruption-location precision and fault totals.
// Precision is located_corrupt_faults / expected_corrupt_frames; when the
// fixture injected no corruption it is 1.0 (nothing to miss).
func finalizeMetrics(cfg Config, res *Result) {
	countCorrupt(cfg, res)
	res.FaultCount = len(res.Faults)
	res.FatalCount = 0
	for _, f := range res.Faults {
		if f.Severity == model.SeverityFatal {
			res.FatalCount++
		}
	}
	finalizeTrackStatus(res)
}
