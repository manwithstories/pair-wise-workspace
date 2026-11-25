// Package pipeline wires manifests, ingestion, repair, indexing, hashing, and
// the resumable DAG executor into the terminal demo use cases.
package pipeline

import (
	"encoding/json"
	"sort"

	"github.com/pairwise/mediad/internal/fixture"
	"github.com/pairwise/mediad/internal/flow"
	"github.com/pairwise/mediad/internal/index"
	"github.com/pairwise/mediad/internal/manifest"
	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/metric"
	"github.com/pairwise/mediad/internal/repair"
	"github.com/pairwise/mediad/internal/store"
)

// Options are the validated demo inputs.
type Options struct {
	Scenario string
	Seed     int64
	Out      string
}

// Result contains terminal metrics and stable per-segment hashes.
type Result struct {
	Options          Options
	Bundle           *fixture.Bundle
	Repair           *repair.Report
	Index            *index.Index
	DAG              *flow.DAG
	Executor         *flow.Executor
	Store            *store.LocalStore
	RoundTripRate    float64
	RoundTripMatched int
	RoundTripTotal   int
	KeyframeHits     int
	KeyframeTotal    int
	KeyframeRate     float64
	HashMatched      int
	HashTotal        int
	SegmentHashes    []SegmentHash
	FirstBlocked     string
	DamageRejected   string
}

// SegmentHash is one deterministic per-artifact hash row.
type SegmentHash struct {
	GlobalIndex int
	RelPath     string
	SHA256      string
}

// Run executes a demo scenario. A non-nil Result may accompany a structured
// rejection/blocking error so the CLI can print evidence before the exit code.
func Run(opts Options) (*Result, error) {
	if opts.Scenario == "" {
		opts.Scenario = fixture.ScenarioClean
	}
	if opts.Seed == 0 {
		opts.Seed = 73
	}
	if opts.Out == "" {
		return nil, media.NewError(media.CodeInvalidArgument, "cli", "out", "--out is required")
	}
	switch opts.Scenario {
	case fixture.ScenarioClean, fixture.ScenarioDamaged, fixture.ScenarioMissingInit:
	default:
		return nil, media.NewError(media.CodeInvalidArgument, "cli", "scenario", "unknown scenario: "+opts.Scenario)
	}

	bundle := fixture.Generate(opts.Seed, opts.Scenario)
	res := &Result{Options: opts, Bundle: bundle}

	// Manifest parse -> normalize -> serialize -> reparse evidence.
	_, dashData, err := bundle.DASHManifest()
	if err != nil {
		return res, err
	}
	canonicalDASH, dashOriginal, dashReparsed, dashMismatch, err := manifest.RoundTripDASH(dashData)
	if err != nil {
		return res, err
	}
	res.RoundTripTotal++
	if dashMismatch == nil && manifest.CompareDASH(manifest.NormalizeDASH(dashOriginal), manifest.NormalizeDASH(dashReparsed)) == nil {
		res.RoundTripMatched++
	}

	_, hlsData := bundle.HLSManifest()
	canonicalHLS, hlsOriginal, hlsReparsed, hlsMismatch, err := manifest.RoundTripHLS(hlsData)
	if err != nil {
		return res, err
	}
	res.RoundTripTotal++
	if hlsMismatch == nil && manifest.CompareHLS(manifest.NormalizeHLS(hlsOriginal), manifest.NormalizeHLS(hlsReparsed)) == nil {
		res.RoundTripMatched++
	}
	res.RoundTripRate = 100
	if res.RoundTripTotal > 0 {
		res.RoundTripRate = float64(res.RoundTripMatched) * 100 / float64(res.RoundTripTotal)
	}

	report, err := repair.Align(bundle.Tracks, bundle.Steps)
	res.Repair = report
	if err != nil && opts.Scenario != fixture.ScenarioDamaged {
		return res, err
	}
	if err != nil && opts.Scenario == fixture.ScenarioDamaged {
		// The generated main timeline stays conflict-free; unexpected conflicts
		// here are still fatal. The explicit damage gate runs after metrics.
		if me, ok := media.AsError(err); !ok || me.Code != media.CodePacketConflict {
			return res, err
		}
	}

	st, err := store.New(opts.Out)
	if err != nil {
		return res, err
	}
	res.Store = st
	if err := st.WriteAtomic("manifest/normalized.mpd", canonicalDASH); err != nil {
		return res, err
	}
	if err := st.WriteAtomic("manifest/normalized.m3u8", canonicalHLS); err != nil {
		return res, err
	}
	exec, err := flow.NewExecutor(st, "dag-state.json")
	if err != nil {
		return res, err
	}
	res.Executor = exec
	dag, err := buildDAG(bundle, report)
	if err != nil {
		return res, err
	}
	res.DAG = dag

	runErr := exec.Run(dag)
	if blocked, berr := dag.FirstBlocked(); berr == nil && blocked != "" {
		res.FirstBlocked = blocked
	}
	if runErr != nil {
		if media.ExitCode(runErr) == 3 {
			return res, runErr
		}
		return res, runErr
	}

	hashes, matched, total, err := verifySegmentHashes(st, dag, report)
	res.SegmentHashes = hashes
	res.HashMatched = matched
	res.HashTotal = total
	if err != nil {
		return res, err
	}

	idx, err := buildIndex(report, dag)
	if err != nil {
		return res, err
	}
	res.Index = idx
	indexJSON, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return res, media.Wrap(err, media.CodeInternal, "pipeline.index", "random-access.json", "index serialization failed")
	}
	if err := st.WriteAtomic("index/random-access.json", indexJSON); err != nil {
		return res, err
	}
	queries := make([]int64, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		queries = append(queries, e.StartTicks)
	}
	hits, qtotal, rate, err := idx.KeyframeHitRate(queries)
	res.KeyframeHits, res.KeyframeTotal, res.KeyframeRate = hits, qtotal, rate
	if err != nil {
		return res, err
	}

	if opts.Scenario == fixture.ScenarioDamaged {
		if err := rejectDamageSamples(bundle); err != nil {
			res.DamageRejected = err.Error()
			return res, err
		}
	}
	return res, nil
}

func verifySegmentHashes(st *store.LocalStore, dag *flow.DAG, report *repair.Report) ([]SegmentHash, int, int, error) {
	var rows []SegmentHash
	matched := 0
	total := len(report.Segments)
	for _, seg := range report.Segments {
		nodeID := segmentNodeID(seg.TrackID, seg.SegmentInTrack)
		node := dag.Get(nodeID)
		if node == nil || node.State != flow.StateDone {
			continue
		}
		data, err := st.Read(seg.OutputRel)
		if err != nil {
			return rows, matched, total, err
		}
		got := metric.BytesSHA256(data)
		want := metric.BytesSHA256(seg.Data)
		if got == want && got == node.SHA256 {
			matched++
		}
		rows = append(rows, SegmentHash{GlobalIndex: seg.GlobalIndex, RelPath: seg.OutputRel, SHA256: got})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].GlobalIndex < rows[j].GlobalIndex })
	return rows, matched, total, nil
}

func buildIndex(report *repair.Report, dag *flow.DAG) (*index.Index, error) {
	var entries []index.Entry
	for _, seg := range report.Segments {
		node := dag.Get(segmentNodeID(seg.TrackID, seg.SegmentInTrack))
		hash := ""
		size := 0
		if node != nil {
			hash = node.SHA256
		}
		if seg.Complete {
			size = len(seg.Data)
		}
		entries = append(entries, index.Entry{
			Index:      seg.GlobalIndex,
			SegmentID:  segmentNodeID(seg.TrackID, seg.SegmentInTrack),
			Path:       seg.OutputRel,
			StartTicks: seg.StartTicks,
			EndTicks:   seg.EndTicks,
			Keyframe:   seg.Keyframe,
			Size:       size,
			SHA256:     hash,
			Complete:   seg.Complete,
		})
	}
	return index.New(entries)
}
