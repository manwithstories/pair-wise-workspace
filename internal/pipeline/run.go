package pipeline

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"mediad/internal/audio"
	"mediad/internal/dag"
	"mediad/internal/fingerprint"
	"mediad/internal/model"
	"mediad/internal/segment"
	"mediad/internal/verify"
)

// aggregateArtifact is the durable product of the convergence node.
type aggregateArtifact struct {
	Cuts       []segment.Cut                `json:"cuts"`
	Silence    []audio.Interval             `json:"silence"`
	Duplicates []fingerprint.DuplicateGroup `json:"duplicates"`
	Bad        []verify.BadOffset           `json:"bad"`
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// Run executes the pipeline. interruptAfter > 0 stops scheduling new nodes
// after that many leaves complete, demonstrating resumable interruption.
func (e *Engine) Run(interruptAfter int) (*Outcome, error) {
	segs, partial, err := e.loadTimelines()
	if err != nil {
		return nil, err
	}
	nodes, err := e.buildNodes(segs)
	if err != nil {
		return nil, err
	}
	g, err := dag.NewGraph(nodes)
	if err != nil {
		return nil, err
	}
	opts := dag.Options{Concurrency: 4, MaxAttempts: 2}
	if interruptAfter > 0 {
		opts.Interrupt = func(done int) bool { return done >= interruptAfter }
	}
	results, err := dag.NewRunner(g, e.store).Run(opts)
	if err != nil {
		return nil, err
	}
	blockedID, _ := dag.FirstBlocked(results)
	if blockedID != "" {
		return e.partialOutcome(results, blockedID, partial)
	}
	agg, err := e.readAggregate()
	if err != nil {
		return nil, err
	}
	in := verify.Inputs{
		Cuts: agg.Cuts, Silence: agg.Silence,
		Duplicates: agg.Duplicates, Bad: agg.Bad,
	}
	rep := verify.Build(in)
	if _, err := rep.Finalize(); err != nil {
		return nil, err
	}
	if err := rep.WriteTo(filepath.Join(e.outDir, "report.json")); err != nil {
		return nil, err
	}
	exit := verify.ExitOK
	if partial || len(agg.Bad) > 0 {
		exit = verify.ExitRecoverable
	}
	return &Outcome{Report: rep, Results: results, Exit: exit}, nil
}

func (e *Engine) buildNodes(segs []model.SegmentRef) ([]dag.Node, error) {
	leafIDs := make([]string, 0, len(segs))
	nodes := make([]dag.Node, 0, len(segs)+1)
	leafDigests := make([]string, 0, len(segs))
	for _, seg := range segs {
		seg := seg
		id := nodeID(seg)
		path := e.segmentPath(seg)
		digest, err := fileDigest(path)
		if err != nil {
			return nil, err
		}
		leafIDs = append(leafIDs, id)
		leafDigests = append(leafDigests, digest)
		nodes = append(nodes, dag.Node{
			ID: id, InputDigest: digest,
			Run: func(*dag.RunContext) error { return e.runLeaf(seg) },
		})
	}
	aggDigest := digestStrings(leafDigests)
	nodes = append(nodes, dag.Node{
		ID: "aggregate", Inputs: leafIDs, InputDigest: aggDigest,
		Run: func(*dag.RunContext) error { return e.runAggregate() },
	})
	return nodes, nil
}

// runLeaf validates one fragment and persists its full evidence artifact.
// Recoverable media defects are recorded, not propagated, so a partial but
// usable result is still produced; unrecoverable failures fail the node.
func (e *Engine) runLeaf(seg model.SegmentRef) error {
	id := nodeID(seg)
	path := e.segmentPath(seg)
	a := Artifact{Segment: seg}
	ev, verr := segment.Validate(path, seg)
	if verr != nil {
		var me *model.MediaError
		if !errors.As(verr, &me) || !me.Recoverable {
			return verr // unrecoverable: node fails, pipeline aborts
		}
		a.Bad = &verify.BadOffset{
			Track: string(seg.Track), Segment: seg.Index, Offset: me.Offset,
			Recoverable: me.Recoverable, Op: me.Op, Reason: me.Err.Error(),
		}
		// Fingerprint available bytes but never merge with healthy fragments.
		fp, ferr := fingerprintFile(path, true)
		if ferr != nil {
			return ferr
		}
		a.FP = fp
		return e.writeArtifact(id, a)
	}
	a.Walk = ev.Report
	if seg.Sync && ev.Report.Sync() {
		cut := segment.Cut{
			Track: seg.Track, Index: seg.Index, URI: seg.URI,
			ByteStart: seg.ByteStart, ByteSize: seg.ByteSize,
			StartMS: seg.StartMS, EndMS: seg.StartMS + seg.DurationMS, Keyframe: true,
		}
		a.Cut = &cut
	}
	pcm, err := e.extractPCM(path, ev.Report)
	if err != nil {
		return err
	}
	ivs, err := e.detector.Detect(bytes.NewReader(pcm), seg.Start)
	if err != nil {
		return err
	}
	a.Silence = ivs
	fp, err := fingerprintFile(path, false)
	if err != nil {
		return err
	}
	a.FP = fp
	return e.writeArtifact(id, a)
}

func (e *Engine) extractPCM(path string, rep *segment.WalkReport) ([]byte, error) {
	var mdat *segment.BoxRecord
	for i := range rep.Boxes {
		if rep.Boxes[i].Type == "mdat" {
			mdat = &rep.Boxes[i]
			break
		}
	}
	if mdat == nil {
		return nil, errors.New("pipeline: fragment has no mdat box")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	payloadLen := mdat.Size - mdat.HeaderSize
	buf := make([]byte, payloadLen)
	if _, err := f.ReadAt(buf, mdat.Offset+mdat.HeaderSize); err != nil {
		return nil, err
	}
	return buf, nil
}

func (e *Engine) partialOutcome(results []dag.Result, blockedID string, partial bool) (*Outcome, error) {
	rep := verify.Build(verify.Inputs{Blocked: blockedID})
	if _, err := rep.Finalize(); err != nil {
		return nil, err
	}
	if err := rep.WriteTo(filepath.Join(e.outDir, "report.json")); err != nil {
		return nil, err
	}
	_ = partial
	return &Outcome{Report: rep, Results: results, Exit: verify.ExitRecoverable}, nil
}
