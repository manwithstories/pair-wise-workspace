package pipeline

import (
	"os"
	"path/filepath"

	"relaycheck/internal/media"
	"relaycheck/internal/meta"
	"relaycheck/internal/segment"
	"relaycheck/internal/txn"
	"relaycheck/internal/verify"
)

// Result carries everything the CLI needs to render the report.
type Result struct {
	Cfg           Config
	Plan          *Plan
	Report        *verify.Report
	WALReplay     int
	ResumeMatched int
	ResumeTotal   int
}

// Run executes the case check (if any), then the deterministic pipeline with
// transactional commit, crash injection, resume and independent verification.
func Run(cfg Config) (*Result, *media.Fault) {
	if cfg.Case != "" {
		if f := runCase(cfg.Case); f != nil {
			return nil, f
		}
	}

	plan, f := BuildPlan(cfg)
	if f != nil {
		return nil, f
	}

	c, f := txn.NewCommitter(cfg.Out)
	if f != nil {
		return nil, f
	}
	defer c.Close()

	walReplay := 0
	if cfg.Resume {
		pending, pf := c.RecoverPending()
		if pf != nil {
			return nil, pf
		}
		for _, p := range pending {
			if p.Segment < 0 || p.Segment >= len(plan.Segments) {
				return nil, media.NewFault(media.CodeWALCorrupt, "wal references segment %d out of range", p.Segment)
			}
			if rf := c.ResolvePending(p, plan.Segments[p.Segment].Bytes); rf != nil {
				return nil, rf
			}
			walReplay++
		}
	}

	for i := 0; i < SegmentCount; i++ {
		if cfg.Resume && committedMatches(c, i, plan.RefHashes[i]) {
			continue
		}
		if !cfg.Resume && cfg.CrashAfterSegment == i {
			if _, pf := c.PrepareSegment(i, plan.Segments[i].Bytes); pf != nil {
				return nil, pf
			}
			return nil, media.NewFault(media.CodeInterrupted, "injected interrupt after durable prepare of segment %d", i).
				WithTrack(1)
		}
		if _, cf := c.CommitSegment(i, plan.Segments[i].Bytes); cf != nil {
			return nil, cf
		}
	}

	entries := make([]txn.ManifestEntry, SegmentCount)
	for i := 0; i < SegmentCount; i++ {
		entries[i] = txn.ManifestEntry{
			Index: i, File: txn.SegmentFileName(i),
			Size: int64(len(plan.Segments[i].Bytes)), SHA256: plan.RefHashes[i],
			KeyframeID: plan.Segments[i].KeyframeID,
		}
	}
	if wf := c.WriteManifest(&txn.Manifest{
		Seed: cfg.Seed, TargetRate: cfg.TargetRate, Layout: cfg.Layout,
		IntegratedLUFS: cfg.IntegratedLUFS, SegmentCount: SegmentCount, Entries: entries,
	}); wf != nil {
		return nil, wf
	}

	rep, vf := verify.VerifyDirectory(cfg.Out, cfg.Seed)
	if vf != nil {
		return nil, vf
	}
	resumeMatched, resumeTotal := verify.CompareHashes(plan.RefHashes, rep.Hashes)
	return &Result{
		Cfg: cfg, Plan: plan, Report: rep,
		WALReplay: walReplay, ResumeMatched: resumeMatched, ResumeTotal: resumeTotal,
	}, nil
}

func committedMatches(c *txn.Committer, index int, wantSHA string) bool {
	b, err := os.ReadFile(filepath.Join(c.Dir, txn.SegmentFileName(index)))
	if err != nil {
		return false
	}
	return segment.HashBytes(b) == wantSHA
}

// runCase exercises a named fixture through parse + reconcile. It returns the
// fault (already carrying track/time range) for invalid/corrupt cases.
func runCase(name string) *media.Fault {
	if isMXFCase(name) {
		if !contains(meta.MXFCases, name) {
			return media.NewFault(media.CodeInvalidInput, "unknown mxf case %q", name)
		}
		m, f := meta.ReadMXF(meta.BuildMXFCase(name))
		if f != nil {
			return f
		}
		_, f = m.Reconcile(meta.Layer{})
		return f
	}

	if !contains(meta.MP4Cases, name) {
		return media.NewFault(media.CodeInvalidInput, "unknown case %q", name)
	}
	m, f := meta.ReadMP4(meta.BuildMP4Case(name))
	if f != nil {
		return f
	}
	_, f = m.Reconcile(meta.Layer{}, meta.Layer{})
	return f
}

func isMXFCase(name string) bool {
	return len(name) >= 4 && name[:4] == "mxf-" || name == "truncated-mxf"
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
