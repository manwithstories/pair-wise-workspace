// Package pipeline orchestrates a per-track run with bounded memory.
//
// The track is processed in one forward pass: the cursor parses and validates
// each frame, the index builder turns it into a jump point, the manifest writer
// records its row, and the cache verifies its content. Nothing accumulates
// frames in memory; the durable artifacts are the only state carried forward.
//
// After the pass the pipeline re-derives its report from the files it wrote:
// the index is re-read and checked, the manifest is aligned to an independent
// second pass over the source, and the metrics are recomputed from measured
// bytes and timestamps.
package pipeline

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/cache"
	"mediad/internal/checkpoint"
	"mediad/internal/index"
	"mediad/internal/ingest"
	"mediad/internal/metrics"
	"mediad/internal/model"
)

// Options configures one run.
type Options struct {
	WorkDir        string
	TrackID        uint32
	TargetFrames   int
	InterruptAfter int  // stop after this many frames of this run (0 = never)
	Resume         bool // continue from a verified checkpoint
	SkipCache      bool // exercise the recompute path
	RTPLoss        int  // preview packets lost; their frames become gaps
}

// TrackResult is one track's report plus the artifacts that prove it.
type TrackResult struct {
	Metrics       metrics.TrackMetrics
	FrameCount    int
	CacheHits     int
	CacheMiss     int
	CacheInvalid  int
	CacheVerified int
	RTPGaps       int
	Duplicates    int // identical repeated frame numbers, isolated from index
	Conflicts     int // conflicting repeated/rewound frame numbers, counted bad
	Resumed       bool
	Interrupted   bool
	ManifestSum   [32]byte
	IndexSum      [32]byte
	Paths         Paths
}

// Paths derives the artifact names for a track under a work directory.
type Paths struct {
	Dir   string
	Track uint32
}

func (p Paths) Source() string { return filepath.Join(p.Dir, fmt.Sprintf("track-%d.mediad", p.Track)) }
func (p Paths) Index() string  { return filepath.Join(p.Dir, fmt.Sprintf("track-%d.idx", p.Track)) }
func (p Paths) FramesLog() string {
	return filepath.Join(p.Dir, fmt.Sprintf("track-%d.frames", p.Track))
}
func (p Paths) Manifest() string {
	return filepath.Join(p.Dir, fmt.Sprintf("track-%d.manifest.csv", p.Track))
}
func (p Paths) Preview() string {
	return filepath.Join(p.Dir, fmt.Sprintf("track-%d.preview.txt", p.Track))
}
func (p Paths) State() string    { return filepath.Join(p.Dir, fmt.Sprintf("track-%d.ckpt", p.Track)) }
func (p Paths) CacheDir() string { return filepath.Join(p.Dir, "cache") }

// ErrInterrupted is re-exported so callers need only this package.
var ErrInterrupted = model.ErrInterrupted

// Run executes one track end to end. Fatal errors (class 2) mean unusable
// input; verify errors (class 3) mean a check or threshold failed. When
// InterruptAfter fires, the partial result is returned with ErrInterrupted.
func Run(opt Options) (*TrackResult, error) {
	if opt.WorkDir == "" {
		return nil, model.Fatal("pipeline", errors.New("empty work dir"))
	}
	if err := os.MkdirAll(opt.WorkDir, 0o755); err != nil {
		return nil, model.Fatal("pipeline", err)
	}
	paths := Paths{Dir: opt.WorkDir, Track: opt.TrackID}
	res := &TrackResult{Paths: paths}

	resume, priorState, err := prepareResume(paths, opt.Resume)
	if err != nil {
		return nil, err
	}
	res.Resumed = resume != nil

	// Refuse to overwrite output that no verified checkpoint backs. A previous
	// run that died before committing a checkpoint leaves an index or manifest
	// whose contents cannot be trusted; silently replacing them could hide a
	// half-written result an operator still needs. A verified checkpoint proves
	// the previous run's prefix is durable and safe to extend.
	if resume == nil {
		if err := guardUnfinished(paths); err != nil {
			return nil, err
		}
		// The guard proved any existing output is backed by a verified
		// checkpoint. A non-resume run is a deliberate clean rebuild, so the
		// old checkpoint is cleared to allow counters to restart at one.
		if err := checkpoint.Clear(paths.State()); err != nil {
			return nil, model.Fatal("pipeline.guard", err)
		}
	}

	src, err := os.Open(paths.Source())
	if err != nil {
		return nil, model.Fatal("pipeline.source", err)
	}
	defer src.Close()

	// Stage 1: single forward pass. Every frame is verified, indexed and
	// recorded through streaming writers. If interruption fires, we stop after
	// a durable checkpoint and report the partial state.
	interrupted, err := pass(src, paths, opt, resume, priorState, res)
	if err != nil {
		return nil, err
	}
	res.Interrupted = interrupted
	if interrupted {
		// The run stopped on purpose after a committed checkpoint. The index and
		// manifest prefix are published and cover exactly the frames ingested so
		// far; verifying them against the full source would be wrong until the
		// resume run completes. Report the partial state and return.
		res.Metrics.IndexEntries = countIndex(paths.Index())
		res.Metrics.Track = opt.TrackID
		res.Metrics.RTPLost = opt.RTPLoss
		return res, model.ErrInterrupted
	}

	// Stage 2: re-read and verify the index we wrote. This is independent of
	// the values used to write it: entries are re-resolved against the source.
	idx, err := verifyIndex(src, paths)
	if err != nil {
		return nil, err
	}
	res.Metrics.IndexEntries = idx.Entries

	// Stage 3: write the manifest from the verified index, then align it to a
	// second sequential pass over the source. A lost preview packet is a gap
	// in the preview, not a hole in the source: the frame remains and stays
	// indexed. The gap list is therefore advisory and not enforced as a
	// prohibition on entries.
	rtp, err := reassemblePreview(opt, paths)
	if err != nil {
		return nil, err
	}
	res.RTPGaps = len(rtp.Missing)
	res.Metrics.RTPLost = rtp.Lost
	digest, rows, err := writeManifest(paths)
	if err != nil {
		return nil, err
	}
	res.ManifestSum = digest
	if rows != idx.Entries {
		return nil, model.Verify("pipeline.manifest", fmt.Errorf("%d rows for %d index entries", rows, idx.Entries))
	}

	skew, err := alignManifest(src, paths)
	if err != nil {
		return nil, err
	}

	// Stage 4: metrics recomputed from the written source and artifacts.
	m, err := recomputeMetrics(paths, opt, res, idx.Entries, skew)
	if err != nil {
		return nil, err
	}
	res.Metrics = m

	// The manifest and index are durable; only now may the final checkpoint
	// commit, recording the manifest digest.
	if err := saveFinal(paths, opt.TrackID, res); err != nil {
		return nil, err
	}
	if err := metrics.CheckThresholds(res.Metrics); err != nil {
		return res, model.Verify("pipeline.thresholds", err)
	}
	return res, nil
}

// pass performs the single forward scan with streaming writers.
//
// On a fresh run it truncates the artifacts and starts from the first frame. On
// a resume it preserves the durable prefix — the index entries and frame facts
// already covered by the checkpoint — and continues from the checkpoint's
// offset, so the resumed pass never re-adds or regresses earlier frames.
func pass(src *os.File, paths Paths, opt Options, resume *ingest.Resume, prior checkpoint.State, res *TrackResult) (bool, error) {
	cur, err := ingest.NewCursor(src, resume)
	if err != nil {
		return false, err
	}
	store, err := cache.Open(paths.CacheDir())
	if err != nil {
		return false, err
	}

	// Trim the frames log to the resume point, then stream the durable prefix
	// straight into the new index writer. Nothing is materialized, so a resumed
	// run of any length uses the same memory as a fresh one.
	prefixEntries, prefixErr := trimToResume(paths, resume)
	if prefixErr != nil {
		return false, model.Fatal("pipeline.prefix", prefixErr)
	}

	idxTmp, idxCommit, err := tempFile(paths.Index())
	if err != nil {
		return false, model.Fatal("pipeline.index", err)
	}
	defer idxCommit(false)
	iw, err := index.NewWriter(idxTmp)
	if err != nil {
		return false, model.Fatal("pipeline.index", err)
	}
	pfx, err := rebuildPrefixReader(paths, resume)
	if err != nil {
		return false, model.Fatal("pipeline.prefix", err)
	}
	for {
		e, more, err := pfx()
		if err != nil {
			return false, model.Fatal("pipeline.prefix", err)
		}
		if !more {
			break
		}
		if err := iw.Add(e); err != nil {
			return false, model.Fatal("pipeline.index", err)
		}
	}

	logf, err := os.OpenFile(paths.FramesLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, model.Fatal("pipeline.frameslog", err)
	}
	defer logf.Close()

	var st streamState
	st.frames = int(prior.NextFrame)
	st.ok = int(prior.FramesOK)
	st.bad = int(prior.FramesBad)
	entries := prefixEntries
	count := 0
	interrupted := false

	for {
		fr, err := cur.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
		st.frames++
		count++
		if fr.Status.Usable() {
			st.ok++
		} else if fr.Status != model.FrameDuplicate {
			st.bad++
		}

		verifyCache(store, src, fr, &st, opt.SkipCache)

		if _, err := logf.Write(encodeFrameFact(fr)); err != nil {
			return false, model.Fatal("pipeline.frameslog", err)
		}
		if fr.Status.Usable() {
			e := index.Entry{
				FrameNo: fr.Meta.FrameNo, TrackID: fr.Meta.TrackID, PTS: fr.PTS,
				Offset: fr.Offset, Size: fr.Size,
				Digest: index.EntryDigest(fr.Meta.TrackID, fr.Meta.FrameNo, fr.PTS, fr.Offset, fr.Size, fr.Digest),
			}
			if err := iw.Add(e); err != nil {
				return false, model.Fatal("pipeline.index", err)
			}
			entries++
		}

		// Commit a checkpoint at this frame boundary before either continuing or
		// stopping. The frame's fact and index entry are already written, so the
		// checkpoint covers exactly what is durable; an interruption therefore
		// leaves a consistent prefix with nothing to roll back.
		if err := commitIngestCheckpoint(cur, paths, opt.TrackID, uint64(st.frames), &st, idxTmp, logf, entries); err != nil {
			return false, err
		}
		if opt.InterruptAfter > 0 && count >= opt.InterruptAfter {
			interrupted = true
			break
		}
	}

	// Publish the index prefix (complete or interrupted). The manifest is
	// written later from the verified index so its rows are derived from the
	// same jump points that survived validation.
	if err := iw.Finish(seekPatcher(idxTmp)); err != nil {
		return false, model.Fatal("pipeline.index", err)
	}
	res.IndexSum = iw.HeaderDigest()
	if err := idxCommit(true); err != nil {
		return false, model.Fatal("pipeline.index", err)
	}
	res.FrameCount = st.frames
	res.CacheHits, res.CacheMiss, res.CacheInvalid = st.cache.Hits, st.cache.Misses, st.cache.Invalid
	res.CacheVerified = st.cache.Verified
	if entries == 0 && !interrupted {
		return false, model.Verify("pipeline", errors.New("no usable frames"))
	}
	return interrupted, nil
}

// verifyCache runs the content-addressed check for one usable frame: a hit must
// prove provenance; a miss or rejected entry is recomputed and stored, then
// read back so cache_verified counts entries a reader can truly retrieve.
func verifyCache(store *cache.Store, src *os.File, fr model.Frame, st *streamState, skip bool) {
	if !fr.Status.Usable() || skip {
		return
	}
	prov := cache.Provenance{
		TrackID: fr.Meta.TrackID, FrameNo: fr.Meta.FrameNo,
		MetaDigest: fr.Meta.Digest(), ContentDigest: fr.Digest,
	}
	_, gerr := store.Get(prov)
	switch {
	case gerr == nil:
		// A hit already passed the full provenance and integrity check inside
		// Get, so it counts as verified.
		st.cache.Hit()
		st.cache.Verified++
	case errors.Is(gerr, cache.ErrMiss):
		st.cache.Miss()
		storeAndVerify(store, src, prov, fr, st)
	default:
		st.cache.Rejected()
		storeAndVerify(store, src, prov, fr, st)
	}
}

func storeAndVerify(store *cache.Store, src *os.File, prov cache.Provenance, fr model.Frame, st *streamState) {
	payload := mustPayload(src, fr)
	if payload == nil {
		return
	}
	if err := store.Put(prov, payload); err != nil {
		return
	}
	if _, err := store.Get(prov); err == nil {
		st.cache.Verified++
	}
}

func mustPayload(src *os.File, fr model.Frame) []byte {
	fb, err := ingest.ReadFrameAt(src, fr.Offset, uint64(fr.Size))
	if err != nil {
		return nil
	}
	return fb.Payload
}

// specFPS is the capture cadence of generated fixtures, used only to size a
// single-frame timeline.

// tempFile creates a sibling temp file and a commit function that either
// renames it into place or removes it.
func tempFile(dest string) (*os.File, func(bool) error, error) {
	dir := filepath.Dir(dest)
	f, err := os.CreateTemp(dir, ".pipeline-*.tmp")
	if err != nil {
		return nil, nil, err
	}
	name := f.Name()
	committed := false
	commit := func(keep bool) error {
		if committed {
			return nil
		}
		committed = true
		if !keep {
			f.Close()
			os.Remove(name)
			return nil
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(name, dest)
	}
	return f, commit, nil
}

func seekPatcher(f *os.File) func([16]byte) error {
	return func(h [16]byte) error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err := f.Write(h[:])
		return err
	}
}
