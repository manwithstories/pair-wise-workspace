package pipeline

import (
	"fmt"
	"io"
	"os"

	"mediad/internal/index"
	"mediad/internal/ingest"
	"mediad/internal/manifest"
	"mediad/internal/metrics"
	"mediad/internal/model"
)

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

// recomputeMetrics derives the report from measured artifacts, not from the
// pass's in-memory counters.
func recomputeMetrics(paths Paths, opt Options, res *TrackResult, indexEntries int, skewMS float64) (metrics.TrackMetrics, error) {
	var m metrics.TrackMetrics
	m.Track = opt.TrackID
	m.IndexEntries = indexEntries
	m.RTPLost = res.Metrics.RTPLost
	m.ManifestSkewMS = skewMS

	// Frame tallies are re-derived by scanning the source's log of statuses is
	// not persisted; instead recompute from the source cursor once more. This
	// keeps the numbers honest even if the pass was resumed.
	frames, err := countFrames(paths)
	if err != nil {
		return m, err
	}
	m.FramesOK, m.FramesBad = frames.ok, frames.bad
	res.Duplicates, res.Conflicts = frames.dup, frames.conflict

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
		switch fr.Status {
		case model.FrameOK:
			t.ok++
			// Recompute quality from the payload as written, not from any value
			// carried in memory during ingestion.
			if fb, rerr := ingest.ReadFrameAt(f, fr.Offset, uint64(fr.Size)); rerr == nil {
				if score, qerr := ingest.QualityScore(fb.Payload, fr.Meta.Width, fr.Meta.Height); qerr == nil {
					t.qsum += score
					t.qn++
				}
			}
		case model.FrameDuplicate:
			t.dup++
		case model.FrameConflict:
			t.conflict++
			t.bad++
		default:
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

// indexStats is the bounded result of validating an index.
type indexStats struct {
	Entries int
}

type entryCursor struct {
	f   *os.File
	buf [index.EntrySize]byte
	ini bool
	n   int
}

type frameTally struct {
	ok, bad     int
	dup         int
	conflict    int
	total       int
	first, last int64
	interval    int64
	qsum        float64
	qn          int
}

const specFPS = 24
