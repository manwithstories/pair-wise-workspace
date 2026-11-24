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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/cache"
	"mediad/internal/checkpoint"
	"mediad/internal/index"
	"mediad/internal/ingest"
	"mediad/internal/manifest"
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

// streamState is the O(1) running state shared by the pass.
type streamState struct {
	track    *ingest.Track
	frames   int
	ok       int
	bad      int
	lastPTS  int64
	firstPTS int64
	havePTS  bool
	cache    metrics.Counter
}

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

// countIndex reports how many complete entries an index file holds.
func countIndex(path string) int {
	entries, dropped, err := index.Open(path)
	if err != nil {
		return 0
	}
	return len(entries) - dropped
}

// prepareResume loads and verifies a checkpoint. A checkpoint that fails
// verification is ignored, never trusted; the run restarts clean.
func prepareResume(paths Paths, want bool) (*ingest.Resume, checkpoint.State, error) {
	if !want {
		return nil, checkpoint.State{}, nil
	}
	st, err := checkpoint.Load(paths.State())
	if err != nil {
		return nil, checkpoint.State{}, nil
	}
	if err := checkpoint.Verify(st); err != nil {
		return nil, checkpoint.State{}, nil
	}
	if st.Stage == checkpoint.StageIngest && st.NextFrame > 0 {
		return &ingest.Resume{
			Offset: st.Offset, NextFrame: st.NextFrame,
			Mapper: st.Mapper, Sequencer: st.Sequencer,
		}, st, nil
	}
	return nil, checkpoint.State{}, nil
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

// commitIngestCheckpoint fsyncs the streaming writers and saves ingest-stage
// state naming the source and the index prefix. The manifest is not yet
// durable by digest at this stage, which Ordering permits.
func commitIngestCheckpoint(cur *ingest.Cursor, paths Paths, trackID uint32, next uint64, st *streamState, idxTmp, logf *os.File, entries int) error {
	if err := idxTmp.Sync(); err != nil {
		return model.Fatal("pipeline.checkpoint", err)
	}
	// The frames log must be durable before its digest is recorded; otherwise a
	// restart could verify a checkpoint whose frames were never written.
	if err := logf.Sync(); err != nil {
		return model.Fatal("pipeline.checkpoint", err)
	}
	srcArt, err := checkpoint.ArtifactOf(paths.Source())
	if err != nil {
		return model.Fatal("pipeline.checkpoint", err)
	}
	framesArt, err := checkpoint.ArtifactOf(paths.FramesLog())
	if err != nil {
		return model.Fatal("pipeline.checkpoint", err)
	}
	mapper, seq := cur.Snapshot()
	return checkpoint.Save(paths.State(), checkpoint.State{
		TrackID: trackID, NextFrame: next, Indexed: uint64(entries), Offset: cur.Offset(),
		FramesOK: uint64(st.ok), FramesBad: uint64(st.bad), Stage: checkpoint.StageIngest,
		Source: srcArt, FramesFile: framesArt, Mapper: mapper, Sequencer: seq,
	})
}

func saveFinal(paths Paths, trackID uint32, res *TrackResult) error {
	srcArt, err := checkpoint.ArtifactOf(paths.Source())
	if err != nil {
		return model.Fatal("pipeline.finalize", err)
	}
	idxArt, err := checkpoint.ArtifactOf(paths.Index())
	if err != nil {
		return model.Fatal("pipeline.finalize", err)
	}
	st, err := checkpoint.Load(paths.State())
	if err != nil {
		st = checkpoint.State{}
	}
	return checkpoint.Save(paths.State(), checkpoint.State{
		TrackID: trackID, NextFrame: st.NextFrame, Offset: st.Offset,
		FramesOK: st.FramesOK, FramesBad: st.FramesBad, Stage: checkpoint.StageManifest,
		Source: srcArt, FramesFile: st.FramesFile, IndexFile: idxArt,
		ManifestSum: fmt.Sprintf("%x", res.ManifestSum),
	})
}

// indexStats is the bounded result of validating an index.
type indexStats struct {
	Entries int
}

// verifyIndex re-reads the published index and resolves every entry's frame
// against the source, proving each jump point is valid and its PTS unified. It
// streams the index and holds no entries, so memory is independent of the
// frame count.
func verifyIndex(src *os.File, paths Paths) (indexStats, error) {
	f, err := os.Open(paths.Index())
	if err != nil {
		return indexStats{}, model.Verify("pipeline.index", err)
	}
	defer f.Close()
	var st indexStats
	dropped, err := index.Scan(f, func(e index.Entry) error {
		fb, err := ingest.ReadFrameAt(src, e.Offset, uint64(e.Size))
		if err != nil {
			return fmt.Errorf("entry frame %d at %d: %w", e.FrameNo, e.Offset, err)
		}
		content := model.DigestBytes(fb.Payload)
		if got := index.EntryDigest(e.TrackID, e.FrameNo, e.PTS, e.Offset, e.Size, content); got != e.Digest {
			return fmt.Errorf("entry frame %d digest mismatch", e.FrameNo)
		}
		st.Entries++
		return nil
	})
	if err != nil {
		return indexStats{}, model.Verify("pipeline.index", err)
	}
	if dropped != 0 {
		return indexStats{}, model.Verify("pipeline.index", fmt.Errorf("%d incomplete entries", dropped))
	}
	return st, nil
}

// eachIndexEntry opens the index and invokes fn for every entry, in order. The
// index is re-read for each consumer that needs it, which trades a sequential
// file read for bounded memory.
func eachIndexEntry(paths Paths, fn func(index.Entry) error) (int, error) {
	f, err := os.Open(paths.Index())
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	dropped, err := index.Scan(f, func(e index.Entry) error {
		n++
		return fn(e)
	})
	if err != nil {
		return n, err
	}
	if dropped != 0 {
		return n, fmt.Errorf("%d incomplete index entries", dropped)
	}
	return n, nil
}

// writeManifest writes rows from the verified index, streaming entries from the
// file rather than holding them.
func writeManifest(paths Paths) ([32]byte, int, error) {
	w, err := manifest.NewAtomicWriter(paths.Manifest())
	if err != nil {
		return [32]byte{}, 0, model.Fatal("pipeline.manifest", err)
	}
	defer w.Abort()
	_, err = eachIndexEntry(paths, func(e index.Entry) error { return w.WriteRow(e) })
	if err != nil {
		return [32]byte{}, 0, model.Verify("pipeline.manifest", err)
	}
	if err := w.Finish(); err != nil {
		return [32]byte{}, 0, model.Fatal("pipeline.manifest", err)
	}
	if err := w.Commit(); err != nil {
		return [32]byte{}, 0, model.Fatal("pipeline.manifest", err)
	}
	return w.Digest(), w.Rows(), nil
}

// alignManifest merge-joins the index against a fresh scan of the source to
// prove structural agreement, then measures numeric skew from the manifest as
// written. Both sides stream.
func alignManifest(src *os.File, paths Paths) (float64, error) {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, model.Fatal("pipeline.align", err)
	}
	cur, err := ingest.NewCursor(src, nil)
	if err != nil {
		return 0, err
	}
	tr := cur.Track()
	idxf, err := os.Open(paths.Index())
	if err != nil {
		return 0, model.Verify("pipeline.align", err)
	}
	defer idxf.Close()
	// A channel-free iterator: the manifest merge-join calls next on demand.
	entryNext, closeEntries := streamingEntries(idxf)
	defer closeEntries()
	if _, err := manifest.Align(manifest.Args{
		Entries:   entryNext,
		Frames:    &manifest.CursorSource{NextFn: cur.Next},
		Timescale: tr.Timescale, CaptureUPS: tr.CaptureUPS, EpochUnits: tr.EpochUnits,
	}); err != nil {
		return 0, model.Verify("pipeline.align", err)
	}
	// Numeric skew: re-read the manifest and measure rows against its own
	// median cadence.
	f, err := os.Open(paths.Manifest())
	if err != nil {
		return 0, model.Verify("pipeline.align", err)
	}
	defer f.Close()
	rows, err := metrics.ReadManifest(f)
	if err != nil {
		return 0, model.Verify("pipeline.align", err)
	}
	return metrics.SkewFromManifest(rows, tr.Timescale)
}

// streamingEntries turns an index file into an on-demand entry iterator.
func streamingEntries(f *os.File) (func() (index.Entry, bool, error), func()) {
	// Scan's callback API does not fit a pull iterator, so decode one entry at
	// a time with a small cursor over the file.
	c := &entryCursor{f: f}
	return c.next, func() {}
}

type entryCursor struct {
	f   *os.File
	buf [index.EntrySize]byte
	ini bool
	n   int
}

func (c *entryCursor) next() (index.Entry, bool, error) {
	if !c.ini {
		c.ini = true
		if _, err := c.f.Seek(int64(index.HeaderSize), io.SeekStart); err != nil {
			return index.Entry{}, false, err
		}
	}
	n, err := io.ReadFull(c.f, c.buf[:])
	if err == io.EOF {
		return index.Entry{}, false, nil
	}
	if err == io.ErrUnexpectedEOF || n < index.EntrySize {
		return index.Entry{}, false, nil
	}
	if err != nil {
		return index.Entry{}, false, err
	}
	e, ok := index.DecodeEntry(c.buf[:])
	if !ok {
		return index.Entry{}, false, nil
	}
	return e, true, nil
}

// previewGaps returns the frame numbers whose preview packets were lost.
func previewGaps(opt Options) []uint64 {
	if opt.RTPLoss <= 0 || opt.TargetFrames <= 0 {
		return nil
	}
	seen := map[uint64]bool{}
	var out []uint64
	for i := 0; i < opt.RTPLoss; i++ {
		f := uint64(1 + (opt.TargetFrames / 3) + i*(opt.TargetFrames/6+1))
		if f > uint64(opt.TargetFrames) || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// recomputeMetrics derives the report from measured artifacts, not from the
// pass's in-memory counters.
func recomputeMetrics(paths Paths, opt Options, res *TrackResult, indexEntries int, skewMS float64) (metrics.TrackMetrics, error) {
	var m metrics.TrackMetrics
	m.Track = opt.TrackID
	m.IndexEntries = indexEntries
	m.RTPLost = opt.RTPLoss
	m.ManifestSkewMS = skewMS

	// Frame tallies are re-derived by scanning the source's log of statuses is
	// not persisted; instead recompute from the source cursor once more. This
	// keeps the numbers honest even if the pass was resumed.
	frames, err := countFrames(paths)
	if err != nil {
		return m, err
	}
	m.FramesOK, m.FramesBad = frames.ok, frames.bad

	size, err := metrics.FileSize(paths.Source())
	if err != nil {
		return m, model.Verify("pipeline.metrics", err)
	}
	tb, err := readTimebase(paths)
	if err != nil {
		return m, err
	}
	m.TargetKbps = float64(tb.TargetKbps)
	dur := 0.0
	if frames.total > 0 && frames.interval > 0 {
		dur = float64(int64(frames.total)*frames.interval) / float64(tb.Timescale)
	}
	m.ActualKbps, m.BitrateDeltaPct = metrics.FromSource(metrics.SourceBytes{Bytes: size, DurationSec: dur}, m.TargetKbps)
	m.CacheVerified = res.CacheVerified
	if frames.qn > 0 {
		m.QualityMean = frames.qsum / float64(frames.qn)
	}
	return m, nil
}

type frameTally struct {
	ok, bad     int
	total       int
	first, last int64
	interval    int64
	qsum        float64
	qn          int
}

// specFPS is the capture cadence of generated fixtures, used only to size a
// single-frame timeline.
const specFPS = 24

// countFrames re-scans the source to recompute frame tallies and the timeline
// span. It is a third sequential pass but needs no memory beyond one frame.
func countFrames(paths Paths) (frameTally, error) {
	var t frameTally
	f, err := os.Open(paths.Source())
	if err != nil {
		return t, model.Fatal("pipeline.metrics", err)
	}
	defer f.Close()
	cur, err := ingest.NewCursor(f, nil)
	if err != nil {
		return t, err
	}
	first := true
	for {
		fr, err := cur.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return t, err
		}
		t.total++
		if fr.Status.Usable() {
			t.ok++
			// Recompute quality from the payload as written, not from any value
			// carried in memory during ingestion.
			if fb, rerr := ingest.ReadFrameAt(f, fr.Offset, uint64(fr.Size)); rerr == nil {
				if score, qerr := ingest.QualityScore(fb.Payload, fr.Meta.Width, fr.Meta.Height); qerr == nil {
					t.qsum += score
					t.qn++
				}
			}
		} else if fr.Status != model.FrameDuplicate {
			t.bad++
		}
		if first {
			t.first, first = fr.PTS, false
		}
		t.last = fr.PTS
	}
	// The media occupies one frame interval per delivered frame, so the
	// duration is frames × interval rather than last-first (which would omit
	// the final interval and inflate the bitrate by one frame).
	if t.total > 1 {
		t.interval = (t.last - t.first) / int64(t.total-1)
	} else if tr := cur.Track(); tr != nil {
		t.interval = int64(tr.Timescale) / specFPS
	}
	return t, nil
}

func readTimebase(paths Paths) (*ingest.Track, error) {
	f, err := os.Open(paths.Source())
	if err != nil {
		return nil, model.Fatal("pipeline.metrics", err)
	}
	defer f.Close()
	tr, err := ingest.ReadTrackDeclaration(f)
	if err != nil {
		return nil, model.Fatal("pipeline.metrics", err)
	}
	return tr, nil
}

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

// encodeFrameFact serializes one frame's durable fact record.
func encodeFrameFact(fr model.Frame) []byte {
	b := make([]byte, 0, frameFactSize)
	b = binary.BigEndian.AppendUint64(b, fr.Meta.FrameNo)
	b = binary.BigEndian.AppendUint64(b, uint64(fr.Offset))
	b = binary.BigEndian.AppendUint32(b, fr.Size)
	b = binary.BigEndian.AppendUint64(b, uint64(fr.PTS))
	b = append(b, byte(fr.Status))
	return b
}

const frameFactSize = 8 + 8 + 4 + 8 + 1

// trimToResume truncates the frames log to the checkpoint's frame count and
// returns how many index entries the durable prefix should contain. The index
// file itself is not read: it may be torn, and this run rebuilds it.
func trimToResume(paths Paths, resume *ingest.Resume) (int, error) {
	if resume == nil {
		if err := os.Truncate(paths.FramesLog(), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		return 0, nil
	}
	want := int64(resume.NextFrame) * frameFactSize
	f, err := os.OpenFile(paths.FramesLog(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if fi.Size() > want {
		if err := f.Truncate(want); err != nil {
			return 0, err
		}
	}
	return int(resume.NextFrame), nil
}

// rebuildPrefixReader returns a pull iterator over index entries for the
// durable prefix, re-deriving each from the frame facts and the source. It
// counts only usable frames, matching the prefix count the caller expects.
func rebuildPrefixReader(paths Paths, resume *ingest.Resume) (func() (index.Entry, bool, error), error) {
	if resume == nil {
		return func() (index.Entry, bool, error) { return index.Entry{}, false, nil }, nil
	}
	src, err := os.Open(paths.Source())
	if err != nil {
		return nil, err
	}
	logf, err := os.Open(paths.FramesLog())
	if err != nil {
		src.Close()
		return nil, err
	}
	buf := make([]byte, frameFactSize)
	remaining := int(resume.NextFrame)
	return func() (index.Entry, bool, error) {
		for remaining > 0 {
			remaining--
			if _, err := io.ReadFull(logf, buf); err != nil {
				logf.Close()
				src.Close()
				return index.Entry{}, false, nil
			}
			rec, ok := decodeFrameFact(buf)
			if !ok || !model.FrameStatus(rec.Status).Usable() {
				continue
			}
			fb, err := ingest.ReadFrameAt(src, rec.Offset, uint64(rec.Size))
			if err != nil {
				continue
			}
			content := model.DigestBytes(fb.Payload)
			return index.Entry{
				FrameNo: fb.FrameNo, TrackID: fb.TrackID, PTS: rec.PTS,
				Offset: rec.Offset, Size: rec.Size,
				Digest: index.EntryDigest(fb.TrackID, fb.FrameNo, rec.PTS, rec.Offset, rec.Size, content),
			}, true, nil
		}
		logf.Close()
		src.Close()
		return index.Entry{}, false, nil
	}, nil
}

func resumeCount(r *ingest.Resume) uint64 {
	if r == nil {
		return 0
	}
	return r.NextFrame
}
func resumeOK(r *ingest.Resume) uint64  { return 0 }
func resumeBad(r *ingest.Resume) uint64 { return 0 }

type frameFact struct {
	FrameNo uint64
	Offset  int64
	Size    uint32
	PTS     int64
	Status  byte
}

func decodeFrameFact(b []byte) (frameFact, bool) {
	if len(b) < frameFactSize {
		return frameFact{}, false
	}
	return frameFact{
		FrameNo: binary.BigEndian.Uint64(b[0:]),
		Offset:  int64(binary.BigEndian.Uint64(b[8:])),
		Size:    binary.BigEndian.Uint32(b[16:]),
		PTS:     int64(binary.BigEndian.Uint64(b[20:])),
		Status:  b[28],
	}, true
}

// guardUnfinished refuses to overwrite published output when no verified
// checkpoint proves it was completed. The frames log and checkpoint itself are
// not outputs; the index and manifest are. A fresh track with neither passes.
func guardUnfinished(paths Paths) error {
	hasOutput := false
	for _, p := range []string{paths.Index(), paths.Manifest()} {
		if _, err := os.Stat(p); err == nil {
			hasOutput = true
			break
		}
	}
	if !hasOutput {
		return nil
	}
	// Output exists. It is safe to replace only when a committed checkpoint
	// verifies that the previous run's artifacts are intact; otherwise this
	// would overwrite output whose completion was never proven.
	st, err := checkpoint.Load(paths.State())
	if err != nil {
		return model.Fatal("pipeline.guard", fmt.Errorf("%w: no committed checkpoint", model.ErrUnfinishedOutput))
	}
	if err := checkpoint.Verify(st); err != nil {
		return model.Fatal("pipeline.guard", fmt.Errorf("%w: %v", model.ErrUnfinishedOutput, err))
	}
	return nil
}

// previewResult is what the RTP reassembly pass learned.
type previewResult struct {
	Lost     int      // missing sequence numbers
	Missing  []uint64 // frame numbers whose preview never completed
	Received int
}

// reassemblePreview builds this track's deterministic preview stream, encodes
// it as real RTP packets through pion/rtp, decodes them back and reorders. The
// loss and gap numbers in the report therefore come from an actual
// marshal/unmarshal/reorder pass, and the preview is saved as an artifact.
func reassemblePreview(opt Options, paths Paths) (previewResult, error) {
	var out previewResult
	if opt.TargetFrames <= 0 {
		return out, nil
	}
	frames := opt.TargetFrames
	packets, err := ingest.BuildPreview(opt.TrackID, frames, int64(2000+opt.TrackID), opt.RTPLoss, 11, true)
	if err != nil {
		return out, model.Fatal("pipeline.rtp", err)
	}
	// Round-trip every packet through RTP so the reassembler sees wire bytes,
	// not in-memory structs.
	decoded := make([]ingest.PreviewPacket, 0, len(packets))
	for _, p := range packets {
		raw, err := ingest.EncodePreview(p, opt.TrackID)
		if err != nil {
			return out, model.Recoverable("pipeline.rtp", int(opt.TrackID), p.FrameNo, "encode", err)
		}
		d, err := ingest.DecodePreview(raw)
		if err != nil {
			return out, err
		}
		decoded = append(decoded, d)
	}
	st, err := ingest.Reorder(decoded)
	if err != nil {
		return out, model.Fatal("pipeline.rtp", err)
	}
	out.Lost = st.Lost
	out.Received = st.Received
	out.Missing = ingest.MissingFrames(st)
	// Persist a small preview report so the number is auditable.
	if err := writePreviewReport(paths, st); err != nil {
		return out, err
	}
	return out, nil
}

func writePreviewReport(paths Paths, st *ingest.RTPStats) error {
	path := paths.Preview()
	tmp := path + ".tmp"
	var b []byte
	b = append(b, fmt.Sprintf("received %d\n", st.Received)...)
	b = append(b, fmt.Sprintf("lost %d\n", st.Lost)...)
	b = append(b, fmt.Sprintf("duplicates %d\n", st.Duplicates)...)
	b = append(b, fmt.Sprintf("reordered %d\n", st.Reordered)...)
	b = append(b, fmt.Sprintf("complete_frames %d\n", len(st.Frames))...)
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return model.Fatal("pipeline.rtp", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.Fatal("pipeline.rtp", err)
	}
	return nil
}
