package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pairwise/mediapack/internal/audio"
	"github.com/pairwise/mediapack/internal/cut"
	"github.com/pairwise/mediapack/internal/fingerprint"
	"github.com/pairwise/mediapack/internal/fixture"
	"github.com/pairwise/mediapack/internal/index"
	"github.com/pairwise/mediapack/internal/ingest"
	"github.com/pairwise/mediapack/internal/pipeline"
	"github.com/pairwise/mediapack/internal/txn"
)

// Run executes generation, round trip, uninterrupted commit, crash/recovery,
// resume rerun, and hash comparison.
func Run(parent context.Context, cfg Config) (*Result, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	sampler, stopSampler, heapDone := runHeapSampler(ctx)

	res, err := runWorkflow(ctx, cfg)
	stopSampler()
	<-heapDone
	if res != nil {
		res.PeakHeapMiB = MiB(sampler.peak)
		if res.PeakHeapMiB > 32 {
			return res, invariant("peak heap %.2f MiB exceeds 32 MiB", res.PeakHeapMiB)
		}
	}
	return res, err
}

func runWorkflow(ctx context.Context, cfg Config) (*Result, error) {
	baseDir := cfg.WorkDir
	if baseDir == "" {
		baseDir = ".mediapack"
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, invariant("create work dir: %v", err)
	}
	workDir, err := os.MkdirTemp(baseDir, "verify-*")
	if err != nil {
		return nil, invariant("create run dir: %v", err)
	}
	res := &Result{WorkDir: workDir}

	healthy, err := fixture.Generate(fixture.Config{Seed: cfg.Seed, Faults: nil})
	if err != nil {
		return nil, unrecoverable("generate healthy sample: %v", err)
	}
	faulty, err := fixture.Generate(fixture.Config{Seed: cfg.Seed, Faults: cfg.Faults})
	if err != nil {
		return nil, unrecoverable("generate fault sample: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "normal.ts"), healthy.TS, 0o644); err != nil {
		return nil, invariant("write normal sample: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "faulty.ts"), faulty.TS, 0o644); err != nil {
		return nil, invariant("write faulty sample: %v", err)
	}

	rawRTP, stats, err := ingest.Depacketize(faulty.RTPDatagrams)
	if err != nil {
		return nil, unrecoverable("RTP depacketize: %v", err)
	}
	if !bytes.Equal(rawRTP, faulty.CompleteTS) || stats.TSPackets != faulty.PacketCount {
		return nil, invariant("RTP/TS round trip mismatch: packets=%d want %d equal=%v",
			stats.TSPackets, faulty.PacketCount, bytes.Equal(rawRTP, faulty.CompleteTS))
	}
	res.Roundtrip = float64(stats.TSPackets) * 100 / float64(faulty.PacketCount)

	opts := pipeline.Options{
		BufferPackets: cfg.BufferPackets,
		ConsumerDelay: cfg.ConsumerDelay,
		Silence: audio.Config{
			SampleRate: fixture.SampleRate, Channels: 1, WindowMS: 20,
			MinSilenceMS: cfg.SilenceMS, EnergyThreshold: 1,
		},
	}

	normalReport, err := pipeline.Run(ctx, bytes.NewReader(faulty.TS), opts)
	if err != nil {
		return res, unrecoverable("uninterrupted pipeline: %v", err)
	}
	res.QueueHighWater = normalReport.QueueHighWater
	normalDir := filepath.Join(workDir, "normal-state")
	normalMgr, err := txn.NewManager(normalDir, 0)
	if err != nil {
		return res, invariant("normal WAL: %v", err)
	}
	normalHashes, committed, err := commitSegments(normalMgr, normalReport.Cut.Segments)
	if err != nil {
		_ = normalMgr.Close()
		return res, err
	}
	if err := commitIndex(normalMgr, cfg.Seed, normalReport.Cut.Segments, committed); err != nil {
		_ = normalMgr.Close()
		return res, err
	}
	if err := normalMgr.Close(); err != nil {
		return res, invariant("close normal WAL: %v", err)
	}
	res.NormalHashes = normalHashes

	recoveryDir := filepath.Join(workDir, "recovery-state")
	crashMgr, err := txn.NewManager(recoveryDir, int64(cfg.CrashAfter))
	if err != nil {
		return res, invariant("crash WAL: %v", err)
	}
	var crashed bool
	for _, seg := range normalReport.Cut.Segments {
		_, err := crashMgr.Commit(seg.ID, blobName(seg), seg.Blob)
		if errors.Is(err, txn.ErrCrashInjected) {
			crashed = true
			break
		}
		if err != nil {
			_ = crashMgr.Close()
			return res, invariant("crash-phase commit: %v", err)
		}
	}
	if cfg.CrashAfter > 0 && !crashed {
		_ = crashMgr.Close()
		return res, invariant("crash-after=%d was not injected", cfg.CrashAfter)
	}
	_ = crashMgr.Close()
	orphan := filepath.Join(recoveryDir, "tmp", "orphan-outside-commit.tmp")
	if err := os.WriteFile(orphan, []byte("partial temp write"), 0o644); err != nil {
		return res, invariant("write orphan temp: %v", err)
	}

	if !cfg.Resume {
		_ = crashMgr.Close()
		return res, invariant("injected crash after %d WAL records; rerun with --resume", cfg.CrashAfter)
	}

	resumeMgr, err := txn.NewManager(recoveryDir, 0)
	if err != nil {
		return res, invariant("resume WAL: %v", err)
	}
	recovery, err := resumeMgr.Recover()
	if err != nil {
		_ = resumeMgr.Close()
		return res, invariant("WAL recovery: %v", err)
	}
	res.WALReplayed = len(recovery.Committed)
	if cfg.CrashAfter == 2 && res.WALReplayed != 1 {
		return res, invariant("wal_replayed=%d, want 1", res.WALReplayed)
	}

	resumeReport, err := pipeline.Run(ctx, bytes.NewReader(faulty.TS), opts)
	if err != nil {
		_ = resumeMgr.Close()
		return res, unrecoverable("resume pipeline: %v", err)
	}
	if resumeReport.Truncated == nil && containsFault(cfg.Faults, "truncated") {
		return res, unrecoverable("truncated fault was not detected")
	}
	res.BadCRC = resumeReport.BadCRC
	res.QueueHighWater = max(res.QueueHighWater, resumeReport.QueueHighWater)

	resumeHashes, committedResume, err := commitSegments(resumeMgr, resumeReport.Cut.Segments)
	if err != nil {
		_ = resumeMgr.Close()
		return res, err
	}
	if err := recordProvenance(resumeMgr, resumeReport.Cut.Segments); err != nil {
		_ = resumeMgr.Close()
		return res, err
	}
	hit, err := commitIndexWithHit(resumeMgr, cfg.Seed, resumeReport.Cut.Segments, committedResume)
	if err != nil {
		_ = resumeMgr.Close()
		return res, err
	}
	res.KeyframeHit = hit
	res.ResumeHashes = resumeHashes
	res.TruncatedCommitted = 0
	res.Clips = clipResults(resumeReport.Cut.Segments)

	if len(normalHashes) != len(resumeHashes) {
		return res, invariant("hash count changed: %d vs %d", len(normalHashes), len(resumeHashes))
	}
	matches := 0
	for i := range normalHashes {
		if normalHashes[i] == resumeHashes[i] {
			matches++
		}
	}
	res.HashRepeat = float64(matches) * 100 / float64(len(normalHashes))
	if matches != len(normalHashes) {
		return res, invariant("normal/recovery clip hashes differ")
	}
	if err := resumeMgr.Close(); err != nil {
		return res, invariant("close resume WAL: %v", err)
	}
	return res, nil
}

func blobName(s *cut.Segment) string { return fmt.Sprintf("blobs/%s.clip", s.ClipSHA256) }

func commitSegments(m *txn.Manager, segs []*cut.Segment) ([]string, map[string]string, error) {
	hashes := make([]string, 0, len(segs))
	committed := make(map[string]string, len(segs))
	for _, s := range segs {
		if !s.Committable {
			return nil, nil, invariant("refusing to commit invalid segment %s: %s", s.ID, s.RejectReason)
		}
		if _, err := m.Commit(s.ID, blobName(s), s.Blob); err != nil {
			return nil, nil, invariant("commit %s: %v", s.ID, err)
		}
		hashes = append(hashes, s.ClipSHA256)
		committed[s.ID] = s.ClipSHA256
	}
	return hashes, committed, nil
}

func commitIndex(m *txn.Manager, seed int64, segs []*cut.Segment, committed map[string]string) error {
	_, err := commitIndexWithHit(m, seed, segs, committed)
	return err
}

func commitIndexWithHit(m *txn.Manager, seed int64, segs []*cut.Segment, committed map[string]string) (float64, error) {
	sc := index.FromSegments(seed, segs)
	b, err := index.MarshalSidecar(sc)
	if err != nil {
		return 0, invariant("marshal index: %v", err)
	}
	if _, err := m.Commit("index", "index/keyframes.mp4box", b); err != nil {
		return 0, invariant("commit index: %v", err)
	}
	p, err := m.ObjectPath("index/keyframes.mp4box")
	if err != nil {
		return 0, invariant("index path: %v", err)
	}
	readSC, err := index.ReadSidecarFile(p)
	if err != nil {
		return 0, invariant("read back go-mp4 index: %v", err)
	}
	return index.KeyframeHitRate(readSC, committed), nil
}

func recordProvenance(m *txn.Manager, segs []*cut.Segment) error {
	d := fingerprint.NewDedupIndex()
	for _, s := range segs {
		_, _ = d.Observe(s.ContentFP, fingerprint.Source{ClipID: s.ID, Source: "generated-multicam"}, blobName(s))
	}
	b, err := json.MarshalIndent(d.Records(), "", "  ")
	if err != nil {
		return invariant("marshal provenance: %v", err)
	}
	if _, err := m.Commit("provenance", "fingerprints/provenance.json", b); err != nil {
		return invariant("commit provenance: %v", err)
	}
	return nil
}

func clipResults(segs []*cut.Segment) []ClipResult {
	out := make([]ClipResult, 0, len(segs))
	for _, s := range segs {
		out = append(out, ClipResult{
			ID: s.ID, SHA256: s.ClipSHA256, FP: s.ContentFP,
			Silence: len(s.Silence), Markers: len(s.Markers),
		})
	}
	return out
}

func containsFault(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
