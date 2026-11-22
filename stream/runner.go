package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"

	"mediaslice/align"
	"mediaslice/media"
	"mediaslice/scan"
)

// Default memory budgets: far below the 256 MiB process ceiling.
const (
	DefaultMaxBufferedBytes int64 = 32 * 1024 * 1024
	DefaultMaxBufferedSamples     = 8192
	ioCopyChunk                   = 64 * 1024
)

// Config controls one streaming run.
type Config struct {
	MaxBufferedBytes   int64
	MaxBufferedSamples int
	// CrashAfterSequence, when >= 0, simulates a process interruption after
	// the segment with this sequence is durably complete.
	CrashAfterSequence int
	// CheckpointEvery is the segment cadence for durable checkpoints.
	CheckpointEvery int
}

// DefaultConfig returns production-safe runner settings.
func DefaultConfig() Config {
	return Config{
		MaxBufferedBytes:   DefaultMaxBufferedBytes,
		MaxBufferedSamples: DefaultMaxBufferedSamples,
		CrashAfterSequence: -1,
		CheckpointEvery:    10,
	}
}

// ErrCrashInjected is returned at the simulated interruption point.
type ErrCrashInjected struct{ Sequence int }

func (e *ErrCrashInjected) Error() string {
	return fmt.Sprintf("simulated crash after segment %d", e.Sequence)
}

// RunResult summarizes a run (or a partial run ending in a crash).
type RunResult struct {
	Manifest       *Manifest
	Segments       []SegmentEntry
	ReusedSegments int
	Crashed        bool
	OutputSHA256   string
}

// repInput pairs one representation's validated index and cutting boundary.
type repInput struct {
	id     string
	idx    *scan.FileIndex
	track  *scan.TrackIndex
	bound  *align.TrackBoundary
}

// Runner executes a ladder plan against source files under memory budgets.
type Runner struct {
	workDir string
	cfg     Config
	inputs  map[string]*scan.FileIndex
	plan    *align.LadderPlan
	reps    []repInput
}

// NewRunner validates inputs and prepares the executor. Rep order is derived
// from the plan so all output ordering is deterministic.
func NewRunner(workDir string, plan *align.LadderPlan,
	indexes map[string]*scan.FileIndex, bounds []*align.TrackBoundary, cfg Config) (*Runner, error) {
	if cfg.MaxBufferedBytes <= 0 {
		cfg.MaxBufferedBytes = DefaultMaxBufferedBytes
	}
	if cfg.MaxBufferedSamples <= 0 {
		cfg.MaxBufferedSamples = DefaultMaxBufferedSamples
	}
	boundByID := map[string]*align.TrackBoundary{}
	for _, b := range bounds {
		boundByID[b.RepID] = b
	}
	repSet := map[string]bool{}
	for _, seg := range plan.Segments {
		for rep := range seg.Ranges {
			repSet[rep] = true
		}
	}
	ids := make([]string, 0, len(repSet))
	for id := range repSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	reps := make([]repInput, 0, len(ids))
	for _, id := range ids {
		idx, ok := indexes[id]
		if !ok {
			return nil, media.NewError(media.KindMissingTrack, id, "no scan index")
		}
		track := idx.VideoTrack()
		if track == nil {
			return nil, media.NewError(media.KindMissingTrack, id, "no video track")
		}
		reps = append(reps, repInput{id: id, idx: idx, track: track, bound: boundByID[id]})
	}
	return &Runner{workDir: workDir, cfg: cfg, inputs: indexes, plan: plan, reps: reps}, nil
}

// Run executes every segment. When a checkpoint exists and verifies,
// completed segments are reused instead of re-emitted.
func (r *Runner) Run() (*RunResult, error) {
	cp, err := LoadCheckpoint(r.workDir)
	if err != nil {
		return nil, err
	}
	startSeq := 0
	res := &RunResult{}
	if cp != nil {
		// Boundary tick of the resume point equals the start of startSeq.
		var boundary media.Ticks
		if cp.NextSequence < len(r.plan.Segments) {
			boundary = r.plan.Segments[cp.NextSequence].Ranges[r.reps[0].id].FromTick
		}
		if mm := VerifyResume(r.workDir, cp, r.inputs, boundary); mm != nil {
			return nil, media.NewError(media.KindCheckpoint, r.workDir, mm.Error())
		}
		startSeq = cp.NextSequence
		res.ReusedSegments = len(cp.Completed) / len(r.reps)
	}

	entries := make([]SegmentEntry, 0, len(r.plan.Segments)*len(r.reps))
	if cp != nil {
		// Re-materialize entries in the canonical (sequence, repID) order so
		// a resumed run emits byte-identical output to a clean run.
		entries = append(entries, r.canonicalEntries(cp)...)
	}

	for seq := startSeq; seq < len(r.plan.Segments); seq++ {
		seg := r.plan.Segments[seq]
		for _, rep := range r.reps {
			rng, ok := seg.Ranges[rep.id]
			if !ok {
				return nil, media.NewError(media.KindMissingTrack, rep.id,
					fmt.Sprintf("no range for segment %d", seq))
			}
			e, err := r.emitSegment(rep, seq, rng)
			if err != nil {
				return nil, err
			}
			entries = append(entries, e)
		}
		// Checkpoint only after every rep of the segment is durably on disk.
		if r.cfg.CheckpointEvery > 0 && (seq+1)%r.cfg.CheckpointEvery == 0 {
			if err := r.persistCheckpoint(entries, seq+1); err != nil {
				return nil, err
			}
		}
		if r.cfg.CrashAfterSequence >= 0 && seq == r.cfg.CrashAfterSequence {
			if err := r.persistCheckpoint(entries, seq+1); err != nil {
				return nil, err
			}
			res.Crashed = true
			res.Segments = entries
			return res, &ErrCrashInjected{Sequence: seq}
		}
	}
	if err := r.persistCheckpoint(entries, len(r.plan.Segments)); err != nil {
		return nil, err
	}
	m := r.buildManifest(entries)
	if _, err := WriteManifest(r.workDir, m); err != nil {
		return nil, err
	}
	res.Manifest = m
	res.Segments = entries
	res.OutputSHA256 = hashEntries(entries)
	return res, nil
}

// emitSegment streams one representation's samples for one segment into a
// single file with a bounded in-memory buffer, hashing as it writes.
func (r *Runner) emitSegment(rep repInput, seq int, rng align.SegmentRange) (SegmentEntry, error) {
	fileName := segmentFileName(rep.id, seq)
	path, err := scan.SafeJoin(r.workDir, fileName)
	if err != nil {
		return SegmentEntry{}, err
	}
	if err := os.MkdirAll(filepathDir(path), 0o755); err != nil {
		return SegmentEntry{}, media.NewError(media.KindIO, path, err.Error())
	}
	src, err := os.Open(rep.idx.SourcePath)
	if err != nil {
		return SegmentEntry{}, media.NewError(media.KindIO, rep.idx.SourcePath, err.Error())
	}
	defer src.Close()

	out, err := os.Create(path)
	if err != nil {
		return SegmentEntry{}, media.NewError(media.KindIO, path, err.Error())
	}
	h := sha256.New()
	var totalBytes int64
	sampleCount := 0
	var bufferedBytes int64
	buf := make([]byte, ioCopyChunk)

	n := rep.track.SampleCount()
	for i := 0; i < n; i++ {
		smp := rep.track.Sample(i)
		if smp.StartTick < rng.FromTick || smp.StartTick >= rng.ToTick {
			continue
		}
		sampleCount++
		if sampleCount > r.cfg.MaxBufferedSamples {
			_ = out.Close()
			return SegmentEntry{}, media.NewError(media.KindIO, rep.id,
				"sample budget exceeded")
		}
		remaining := int64(smp.Size)
		if bufferedBytes+remaining > r.cfg.MaxBufferedBytes {
			_ = out.Close()
			return SegmentEntry{}, media.NewError(media.KindIO, rep.id,
				"byte budget exceeded")
		}
		bufferedBytes += remaining
		if _, err := src.Seek(smp.Offset, io.SeekStart); err != nil {
			_ = out.Close()
			return SegmentEntry{}, media.NewError(media.KindIO, rep.idx.SourcePath, err.Error())
		}
		for remaining > 0 {
			n := int64(len(buf))
			if remaining < n {
				n = remaining
			}
			if _, err := io.ReadFull(src, buf[:n]); err != nil {
				_ = out.Close()
				return SegmentEntry{}, media.NewError(media.KindIO, rep.idx.SourcePath, err.Error())
			}
			if _, err := out.Write(buf[:n]); err != nil {
				_ = out.Close()
				return SegmentEntry{}, media.NewError(media.KindIO, path, err.Error())
			}
			h.Write(buf[:n])
			totalBytes += n
			remaining -= n
		}
	}
	if err := out.Close(); err != nil {
		return SegmentEntry{}, media.NewError(media.KindIO, path, err.Error())
	}
	return SegmentEntry{
		RepID: rep.id, Sequence: seq, FileName: fileName,
		FromTick: rng.FromTick, ToTick: rng.ToTick,
		Bytes: totalBytes, SHA256: hex.EncodeToString(h.Sum(nil)),
		SampleCount: sampleCount,
	}, nil
}

func (r *Runner) persistCheckpoint(entries []SegmentEntry, nextSeq int) error {
	completed := make(map[string]SegmentEntry, len(entries))
	var boundary media.Ticks
	for _, e := range entries {
		completed[fmt.Sprintf("%s/%d", e.RepID, e.Sequence)] = e
		if e.Sequence == nextSeq-1 {
			boundary = e.ToTick
		}
	}
	hashes := map[string]string{}
	for id, idx := range r.inputs {
		hashes[id] = idx.InputSHA256
	}
	cp := &Checkpoint{
		Version:        CheckpointVersion,
		ParserVersion:  scan.ParserVersion,
		NextSequence:   nextSeq,
		BoundaryTick:   boundary,
		Completed:      completed,
		InputHashes:    hashes,
		ManifestDigest: hashEntries(entries),
	}
	return SaveCheckpoint(r.workDir, cp)
}

func (r *Runner) buildManifest(entries []SegmentEntry) *Manifest {
	ids := make([]string, 0, len(r.reps))
	hashes := map[string]string{}
	for _, rep := range r.reps {
		ids = append(ids, rep.id)
		hashes[rep.id] = rep.idx.InputSHA256
	}
	sort.Strings(ids)
	return &Manifest{
		Version:         ManifestVersion,
		ParserVersion:   scan.ParserVersion,
		SegmentTicks:    r.plan.SegmentTicks,
		Representations: ids,
		Segments:        entries,
		SegmentCount:    len(r.plan.Segments),
		InputHashes:     hashes,
	}
}

// canonicalEntries returns checkpointed segments in the same order a clean
// run produces (outer sequence, inner representation id), which is the order
// hashed into the manifest and output digest.
func (r *Runner) canonicalEntries(cp *Checkpoint) []SegmentEntry {
	out := make([]SegmentEntry, 0, len(cp.Completed))
	for seq := 0; seq < cp.NextSequence; seq++ {
		for _, rep := range r.reps {
			key := fmt.Sprintf("%s/%d", rep.id, seq)
			if e, ok := cp.Completed[key]; ok {
				out = append(out, e)
			}
		}
	}
	return out
}
