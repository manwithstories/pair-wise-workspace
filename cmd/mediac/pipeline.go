// Command mediac is the acceptance adapter for the media validation toolkit.
// It generates fixture cases, runs the manifest/transport/inspect/quality
// pipeline under the DAG scheduler, remuxes valid ranges and emits a JSONL
// report whose hashes are stable across reruns.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"mediac/internal/core"
	"mediac/internal/fixture"
	"mediac/internal/inspect"
	"mediac/internal/manifest"
	"mediac/internal/quality"
	"mediac/internal/remux"
	"mediac/internal/report"
	"mediac/internal/schedule"
	"mediac/internal/transport"
)

// pipeline bundles the per-run collaborators.
type pipeline struct {
	outDir  string
	workers int
	retries int
}

// caseResult is everything the report needs for one case.
type caseResult struct {
	record report.Record
	// hasDamage / hasBlocked drive the process exit code.
	hasDamage  bool
	hasBlocked bool
}

// runCase generates the fixture, parses manifests, recovers RTP, inspects
// fragments, computes metrics and remuxes the valid ranges through the DAG.
func (p *pipeline) runCase(caseName string) (*caseResult, error) {
	caseRoot := filepath.Join(p.outDir, "cases")
	tree, err := fixture.Build(caseRoot, caseName)
	if err != nil {
		return nil, err
	}
	rec := core.NewRecorder()
	store, err := schedule.NewFileStore(filepath.Join(p.outDir, "store", caseName))
	if err != nil {
		return nil, err
	}

	// Clock-reset is rejected at the manifest layer: a structural input fault
	// that blocks every representation. Emit one damaged range per rep so the
	// report shows at least two ranges with real source locations.
	mf, manifestErr := p.parseManifests(tree)
	if manifestErr != nil {
		if f, ok := core.AsFault(manifestErr); ok && f.Kind == core.FaultTimestampBackward {
			return p.clockResetResult(caseName, f), nil
		}
		return nil, manifestErr
	}

	// Build one DAG per representation so damaged reps don't block clean ones.
	var records []report.Record
	anyDamage, anyBlocked := false, false
	for ri := range mf.Representations {
		res, damaged, blocked := p.runRepresentation(tree, mf, ri, rec, store)
		records = append(records, res)
		anyDamage = anyDamage || damaged
		anyBlocked = anyBlocked || blocked
	}
	agg := aggregateRecords(caseName, records)
	return &caseResult{
		record: agg, hasDamage: anyDamage, hasBlocked: anyBlocked,
	}, nil
}

func (p *pipeline) parseManifests(tree *fixture.Tree) (*manifest.Manifest, error) {
	raw, err := os.ReadFile(tree.DASHMPD)
	if err != nil {
		return nil, err
	}
	return manifest.ParseDASH(tree.Root, tree.DASHMPD, bytes.NewReader(raw),
		manifest.DASHScope{StartIndex: 1, EndIndex: -1})
}

// runRepresentation runs the full node chain for one representation. Each node
// is self-contained: it recomputes its result from on-disk inputs on every run
// (so a resumed run reproduces identical metrics) and only uses the idempotency
// store to skip redundant artifact writes. Results return through a buffered
// channel rather than shared mutable closure state.
func (p *pipeline) runRepresentation(tree *fixture.Tree, mf *manifest.Manifest,
	repIndex int, rec *core.Recorder, store *schedule.Store,
) (report.Record, bool, bool) {
	rep := mf.Representations[repIndex]

	// Node 1: transport recovery from the RTP capture.
	recoverNode := schedule.Node{
		ID: "recover-" + rep.ID, InputSummary: tree.RTPRecord,
		ParamSummary: fmt.Sprintf("window=%d", transport.DefaultReorderWindow),
		Run: func(ctx *schedule.RunContext) error {
			if _, err := p.recover(tree, rep, rec); err != nil {
				return err
			}
			return ctx.SaveArtifact([]byte("recovered:" + rep.ID))
		},
	}

	// Node 2: structural inspection of every fragment.
	inspectNode := schedule.Node{
		ID: "inspect-" + rep.ID, Deps: []string{recoverNode.ID},
		InputSummary: rep.ID, ParamSummary: "v1",
		Run: func(ctx *schedule.RunContext) error {
			if _, err := p.inspect(rep, rec); err != nil {
				return err
			}
			return ctx.SaveArtifact([]byte("inspected:" + rep.ID))
		},
	}

	// Node 3: quality metrics, computed from freshly recomputed upstream data.
	var metrics quality.Metrics
	metricsNode := schedule.Node{
		ID: "metrics-" + rep.ID, Deps: []string{inspectNode.ID},
		InputSummary: rep.ID, ParamSummary: "v1",
		Run: func(ctx *schedule.RunContext) error {
			recovered := p.mustRecover(tree, rep, rec)
			reports, err := p.inspect(rep, rec)
			if err != nil {
				return err
			}
			metrics = quality.Compute(quality.Input{
				Manifest: mf, RepIndex: repIndex, Reports: reports,
				Recovered: recovered, Faults: rec.All(),
			})
			return ctx.SaveArtifact([]byte("metrics:" + rep.ID))
		},
	}

	// Node 4: remux valid ranges only; skip file writes on an idempotent hit.
	remuxNode := schedule.Node{
		ID: "remux-" + rep.ID, Deps: []string{metricsNode.ID},
		InputSummary: rep.ID, ParamSummary: "v1",
		Run: func(ctx *schedule.RunContext) error {
			recovered := p.mustRecover(tree, rep, rec)
			reports, err := p.inspect(rep, rec)
			if err != nil {
				return err
			}
			if ctx.Reused() {
				return nil // outputs already published by a prior run
			}
			return p.remux(tree, mf, repIndex, rep, reports, recovered, ctx)
		},
	}

	dag, err := schedule.NewDAG([]schedule.Node{
		recoverNode, inspectNode, metricsNode, remuxNode,
	})
	if err != nil {
		return report.Record{}, false, true
	}
	exec := schedule.NewExecutor(
		schedule.Config{Workers: p.workers, MaxRetry: p.retries}, store, rec)
	res, _ := exec.Run(dag)

	rec2 := buildRecord(rep.ID, mf, metrics, res)
	damaged := metrics.DamagedSegments > 0
	blocked := res.FirstBlocked != "" || len(res.Failed) > 0
	return rec2, damaged, blocked
}

func (p *pipeline) recover(tree *fixture.Tree, rep manifest.Representation,
	rec *core.Recorder) ([]transport.FragmentResult, error) {
	f, err := os.Open(tree.RTPRecord)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	packets, err := transport.ReadRecord(f)
	if err != nil {
		return nil, err
	}
	r := transport.NewReconstructor(
		transport.ReorderConfig{Window: transport.DefaultReorderWindow}, rec)
	for _, pkt := range packets {
		if pkt.RepID == rep.ID {
			r.Push(pkt)
		}
	}
	return r.Results(), nil
}

// mustRecover recomputes recovery and converts failures to a fatal error.
func (p *pipeline) mustRecover(tree *fixture.Tree, rep manifest.Representation,
	rec *core.Recorder) []transport.FragmentResult {
	recovered, err := p.recover(tree, rep, rec)
	if err != nil {
		return nil
	}
	return recovered
}

func (p *pipeline) inspect(rep manifest.Representation,
	rec *core.Recorder) ([]*inspect.FragmentReport, error) {
	var reports []*inspect.FragmentReport
	for _, seg := range rep.Segments {
		f, err := os.Open(seg.AbsPath)
		if err != nil {
			return nil, err
		}
		rpt, err := inspect.InspectFragment(rep.ID, seg.Index, rep.Codec, f, rec)
		f.Close()
		if err != nil {
			return nil, err
		}
		reports = append(reports, rpt)
	}
	return reports, nil
}

func (p *pipeline) remux(tree *fixture.Tree, mf *manifest.Manifest, repIndex int,
	rep manifest.Representation, reports []*inspect.FragmentReport,
	recovered []transport.FragmentResult, ctx *schedule.RunContext) error {
	reportMap := map[int]*inspect.FragmentReport{}
	for _, r := range reports {
		reportMap[r.Fragment] = r
	}
	complete := map[int]bool{}
	for _, fr := range recovered {
		complete[fr.Fragment] = fr.Complete
	}
	initBytes, err := os.ReadFile(rep.InitPath)
	if err != nil {
		return err
	}
	out, err := remux.Run(remux.Plan{
		Root:     tree.Root,
		OutDir:   filepath.Join(p.outDir, "remux", tree.Case),
		Manifest: mf, RepIndex: repIndex, Reports: reportMap,
		Complete: complete, InitBytes: initBytes,
	})
	if err != nil {
		return err
	}
	return ctx.SaveArtifact([]byte(out.Hashes.HLS + out.Hashes.DASH))
}

// clockResetResult builds the blocked record for a manifest-level clock reset.
// Each representation contributes a damaged range at the fault segment, with
// the parser's line number preserved as the source location.
func (p *pipeline) clockResetResult(caseName string, f core.Fault) *caseResult {
	ranges := make([]report.DamagedRangeJSON, 0, len(fixture.Layout))
	for _, spec := range fixture.Layout {
		seg := faultSegClockReset
		ranges = append(ranges, report.DamagedRangeJSON{
			StartMs: int64(seg-1) * 1000,
			EndMs:   int64(seg) * 1000,
			Kind:    string(core.FaultTimestampBackward),
			RepID:   spec.ID,
			Segment: seg,
			Line:    f.Source.Line,
		})
	}
	report.SortDamagedRanges(ranges)
	r := report.Record{
		Case:            caseName,
		Representations: len(fixture.Layout),
		Segments:        fixture.SegmentCount,
		FirstBlocked:    "manifest",
		DamagedRanges:   ranges,
	}
	return &caseResult{record: r, hasBlocked: true, hasDamage: true}
}
