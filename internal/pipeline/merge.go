package pipeline

import (
	"context"
	"path/filepath"
	"sort"

	"mediascan/internal/analyze"
	"mediascan/internal/media"
	"mediascan/internal/report"
	"mediascan/internal/store"
)

// MergeConfig controls deterministic report assembly.
type MergeConfig struct {
	Parameters    store.Params
	CheckpointDir string
	CheckpointRel string
}

// OrderedMerge deduplicates completed outcomes by task key, orders files by
// index and tracks by ID, joins stream/keyframe fragments, and produces the
// canonical provenance report. Only outcomes carrying a completed Result are
// merged; canceled/in-flight work is absent.
func OrderedMerge(ctx context.Context, outcomes []Outcome, cfg MergeConfig) (*report.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	byKey := map[string]Outcome{}
	for _, o := range outcomes {
		if o.Result == nil {
			continue
		}
		key := o.Result.Task.Key
		if key == "" {
			key = o.Job.Task.Key
			o.Result.Task = o.Job.Task
		}
		// Identical retries have identical results; the first completed outcome
		// is enough and prevents duplicate report rows.
		byKey[key] = o
	}

	completed := make([]Outcome, 0, len(byKey))
	for _, o := range byKey {
		completed = append(completed, o)
	}
	sort.SliceStable(completed, func(i, j int) bool {
		a, b := completed[i].Result, completed[j].Result
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		return a.Task.AssetName < b.Task.AssetName
	})

	relCheckpoint := cfg.CheckpointRel
	if relCheckpoint == "" {
		relCheckpoint = "checkpoints"
	}
	r := &report.Report{
		SchemaVersion:   report.SchemaVersion,
		AnalyzerVersion: store.AnalyzerVersion,
		Parameters: report.Parameters{
			Workers: cfg.Parameters.Workers,
			Seed:    cfg.Parameters.Seed,
		},
		Inputs: []report.Input{},
		Tracks: []report.TrackReport{},
		Faults: []media.Fault{},
	}

	skipped := 0
	for _, o := range completed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res := o.Result
		state := "scanned"
		if o.Skipped {
			state = "resumed"
			skipped++
		}
		r.Inputs = append(r.Inputs, report.Input{
			Index:          res.Index,
			File:           res.Task.AssetName,
			SHA256:         res.SHA256,
			Size:           res.Size,
			TaskKey:        res.Task.Key,
			CheckpointPath: filepath.ToSlash(filepath.Join(relCheckpoint, res.Task.Key+".json")),
			State:          state,
		})

		kfByTrack := map[uint32]analyze.KeyframeReport{}
		for _, kf := range res.Keyframes {
			kfByTrack[kf.TrackID] = kf
		}
		streams := append([]analyze.Stream(nil), res.Streams...)
		sort.SliceStable(streams, func(i, j int) bool { return streams[i].TrackID < streams[j].TrackID })
		for _, st := range streams {
			tr := report.TrackReport{Stream: st, Keyframes: []analyze.Keyframe{}, Intervals: []analyze.KeyframeInterval{}}
			if kf, ok := kfByTrack[st.TrackID]; ok {
				tr.Keyframes = append(tr.Keyframes, kf.Keyframes...)
				tr.Intervals = append(tr.Intervals, kf.Intervals...)
			}
			if tr.Keyframes == nil {
				tr.Keyframes = []analyze.Keyframe{}
			}
			if tr.Intervals == nil {
				tr.Intervals = []analyze.KeyframeInterval{}
			}
			r.Tracks = append(r.Tracks, tr)
		}
		r.Faults = append(r.Faults, res.Faults...)
	}

	sortFaults(r.Faults)
	r.Metrics = report.Metrics{
		Files:              len(r.Inputs),
		Tracks:             len(r.Tracks),
		Faults:             len(r.Faults),
		CorruptRegions:     countFaults(r.Faults, media.IsCorruptRegion),
		UnknownBoxes:       countFaultCode(r.Faults, media.CodeUnknownBoxSkipped),
		ZeroTimescaleCount: countFaultCode(r.Faults, media.CodeZeroTimescale),
		ResumeSkipped:      skipped,
	}
	return r, nil
}
