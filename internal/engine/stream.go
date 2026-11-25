package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"bcast/internal/fault"
	"bcast/internal/index"
	"bcast/internal/model"
	"bcast/internal/provenance"
	"bcast/internal/safe"
)

// Config runs the streaming engine.
type Config struct {
	Root           *safe.Root
	Timeline       *model.Timeline
	Key            index.Key
	Index          *index.Store
	Params         Params
	InterruptAfter int
}

// Report is the run result used by the CLI and verification command.
type Report struct {
	Planned     int                           `json:"planned"`
	Resumed     int                           `json:"resumed"`
	Processed   int                           `json:"processed"`
	Completed   int                           `json:"completed"`
	Interrupted bool                          `json:"interrupted"`
	Hashes      []string                      `json:"segment_sha256"`
	Manifests   []*provenance.SegmentManifest `json:"-"`
}

// Run executes pending segments with a bounded in-flight segment channel and
// atomic per-segment checkpoints.
func Run(ctx context.Context, cfg Config) (*Report, *Checkpoint, error) {
	if cfg.Root == nil || cfg.Timeline == nil {
		return nil, nil, fault.E(fault.InvalidArgument, "engine", "Run", errString("root and timeline required"))
	}
	plans, err := PlanTimeline(cfg.Timeline)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Params.Width == 0 {
		cfg.Params = DefaultParams()
	}
	keyHash := cfg.Key.Hash()

	cp, exists, err := LoadCheckpoint(cfg.Root)
	if err != nil {
		return nil, nil, err
	}
	if !exists || cp.KeyHash != keyHash {
		cp = NewCheckpoint(keyHash, cfg.Key.Inputs)
	}

	rep := &Report{Planned: len(plans), Manifests: make([]*provenance.SegmentManifest, len(plans))}
	for i := 0; i >= 0 && i <= cp.LastCompleted && i < len(plans); i++ {
		m, lerr := LoadManifest(cfg.Root, i)
		if lerr != nil {
			return rep, cp, lerr
		}
		if want := cp.SegmentSHA256[i]; want != "" && want != m.SHA256 {
			return rep, cp, fault.E(fault.EvidenceMismatch, "engine", "Run.checkpoint",
				fmt.Errorf("segment %d hash differs from checkpoint", i)).WithSegment(i)
		}
		rep.Manifests[i] = m
		rep.Resumed++
	}

	start := cp.LastCompleted + 1
	if start < 0 {
		start = 0
	}
	buffer := cfg.Params.BufferSegments
	if buffer < 1 {
		buffer = 1
	}
	segCh := make(chan indexedSegment, buffer)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer close(segCh)
		for i := start; i < len(plans); i++ {
			select {
			case segCh <- indexedSegment{index: i, segment: plans[i]}:
			case <-runCtx.Done():
				return
			}
		}
	}()

	exec := &execConfig{root: cfg.Root, key: cfg.Key, indexStore: cfg.Index, timeline: cfg.Timeline, params: cfg.Params}
	newCount := 0
	for item := range segCh {
		if err := ctx.Err(); err != nil {
			return rep, cp, fault.E(fault.Interrupted, "engine", "Run.context", err)
		}
		m, err := exec.executeSegment(runCtx, item.segment)
		if err != nil {
			cancel()
			return rep, cp, err
		}
		if err := cp.MarkCompleted(item.index, m.SHA256, cfg.Key.Inputs, keyHash); err != nil {
			cancel()
			return rep, cp, err
		}
		if err := cp.Save(cfg.Root); err != nil {
			cancel()
			return rep, cp, err
		}
		rep.Manifests[item.index] = m
		rep.Processed++
		newCount++
		if cfg.InterruptAfter > 0 && newCount >= cfg.InterruptAfter {
			rep.Interrupted = true
			cancel()
			return rep, cp, fault.E(fault.Interrupted, "engine", "Run",
				fmt.Errorf("simulated interrupt after %d committed segment(s)", newCount)).
				WithSegment(item.index).WithRetryable(true)
		}
	}

	rep.Completed = countManifests(rep.Manifests)
	rep.Hashes = make([]string, 0, len(plans))
	for _, m := range rep.Manifests {
		if m != nil {
			rep.Hashes = append(rep.Hashes, m.SHA256)
		}
	}
	return rep, cp, nil
}

type indexedSegment struct {
	index   int
	segment model.Segment
}

// LoadManifest reads and verifies one segment manifest by index.
func LoadManifest(root *safe.Root, index int) (*provenance.SegmentManifest, error) {
	rel := fmt.Sprintf("manifests/seg-%03d.json", index)
	b, err := root.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	var m provenance.SegmentManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fault.E(fault.EvidenceMismatch, "engine", "LoadManifest.json", err).WithPath(rel).WithSegment(index)
	}
	ok, err := m.Verify()
	if err != nil {
		return nil, fault.E(fault.EvidenceMismatch, "engine", "LoadManifest.verify", err).WithPath(rel).WithSegment(index)
	}
	if !ok {
		return nil, fault.E(fault.EvidenceMismatch, "engine", "LoadManifest", fmt.Errorf("segment hash mismatch")).WithPath(rel).WithSegment(index)
	}
	return &m, nil
}

func countManifests(ms []*provenance.SegmentManifest) int {
	n := 0
	for _, m := range ms {
		if m != nil {
			n++
		}
	}
	return n
}
