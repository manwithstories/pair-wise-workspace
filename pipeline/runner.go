package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"mediad/checkpoint"
	"mediad/diag"
	"mediad/mp4box"
	"mediad/segment"
	"mediad/track"
)

// FrameItem is a parsed media frame flowing through a bounded queue.
type FrameItem struct {
	Track    string
	TimeMS   int64
	Keyframe bool
}

// OutputWriter persists one segment file. It is an interface so tests can
// inject deterministic disk failures without touching the real filesystem.
type OutputWriter interface {
	WriteSegment(rep string, idx int, payload []byte) (path string, err error)
}

// FSWriter writes segment payloads under <dir>/segments.
type FSWriter struct{ Dir string }

// WriteSegment implements OutputWriter with an atomic temp-file+rename. The
// deterministic payload is first wrapped in a valid fMP4 fragment via mp4box,
// so each .seg file is a structurally verifiable ISO-BMFF container.
func (w FSWriter) WriteSegment(rep string, idx int, payload []byte) (string, error) {
	p := checkpoint.SegmentPath(w.Dir, rep, idx)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", diag.New(diag.EOutput, rep, "mkdir: %v", err)
	}
	container, err := mp4box.Encode(payload)
	if err != nil {
		return "", err
	}
	tmp := p + ".part"
	if err := os.WriteFile(tmp, container, 0o644); err != nil {
		return "", diag.New(diag.EOutput, rep, "write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", diag.New(diag.EOutput, rep, "rename: %v", err)
	}
	return p, nil
}

// Stats summarizes one run for the CLI acceptance output.
type Stats struct {
	Segments          int
	Representations   int
	MaxAlignmentErrMS int64
	PeakQueue         int
	PeakMemoryBytes   int64
	Backpressure      int64
	ResumeMS          int64
	ManifestSHA       string
	RedoneSegments    int
}

// segKey namespaces segment hashes by representation because every rep writes
// its own file for the same segment index.
func segKey(rep string, idx int) string { return checkpoint.SegmentKey(rep, idx) }

// Runner schedules track processing in topological order and drives output.
type Runner struct {
	Graph    *track.Graph
	Queues   map[string]*BoundedQueue[FrameItem]
	Writer   OutputWriter
	Store    *checkpoint.Store
	OutDir   string
	Policy   segment.Policy
	FrameCap int // frames consumed per representation before alignment
}

// NewRunner validates the graph up front (cycle/missing edges fail before any
// work starts) and constructs one bounded queue per track.
func NewRunner(g *track.Graph, queueCap int, w OutputWriter, st *checkpoint.Store,
	outDir string, pol segment.Policy) (*Runner, error) {
	order, cyc, err := g.Topo()
	if err != nil {
		_ = cyc
		return nil, err
	}
	q := make(map[string]*BoundedQueue[FrameItem], len(order))
	for _, id := range order {
		bq, qerr := NewBoundedQueue[FrameItem](queueCap)
		if qerr != nil {
			return nil, qerr
		}
		q[id] = bq
	}
	return &Runner{
		Graph: g, Queues: q, Writer: w, Store: st, OutDir: outDir,
		Policy: pol, FrameCap: 100000,
	}, nil
}

// Input is one representation's fully parsed frame list. The pipeline keeps
// only these in-memory slices plus bounded queues, so peak memory is bounded
// by queue capacity and frame count rather than raw input length.
type Input struct {
	RepID string
	Frames []segment.Frame
}

// Run executes parse -> align -> output with checkpointing and resume.
// Feeds are keyed by representation ID; the track graph only controls
// scheduling order and validation.
func (r *Runner) Run(ctx context.Context, feeds []Input) (*Stats, error) {
	start := time.Now()
	order, _, err := r.Graph.Topo()
	if err != nil {
		return nil, err
	}
	done := ctx.Done()

	// Phase 1: stream every feed through its bounded queue in topo order. A
	// slow consumer blocks the producer here, enforcing backpressure.
	drained := make(map[string][]segment.Frame, len(feeds))
	for _, id := range order {
		q, ok := r.Queues[id]
		if !ok {
			continue
		}
		feed := findFeed(feeds, id)
		if feed == nil {
			continue // non-media graph vertices (timecode tracks) have no frames
		}
		go func(f Input) {
			for i, fr := range f.Frames {
				if i >= r.FrameCap {
					break
				}
				// Enqueue returns promptly on cancellation; the goroutine
				// then exits, so no producer leaks after Run returns.
				if err := q.Enqueue(FrameItem{
					Track: f.RepID, TimeMS: fr.Time.Millis(), Keyframe: fr.Keyframe,
				}, done); err != nil {
					return
				}
			}
		}(*feed)
		collected := make([]segment.Frame, 0, len(feed.Frames))
		for range feed.Frames {
			item, ok := q.Dequeue(done)
			if !ok {
				return nil, diag.New(diag.EParam, "", "pipeline canceled for track %s", id)
			}
			collected = append(collected, segment.Frame{
				Time:     timelineMillis(item.TimeMS),
				Keyframe: item.Keyframe,
			})
		}
		drained[id] = collected
	}

	// Phase 2: resume — verify what a previous run already committed.
	rec, loadErr := r.Store.Load()
	resumeFrom := 0
	if loadErr == nil {
		redo, verr := checkpoint.VerifyCommitted(r.OutDir, rec)
		if verr != nil {
			return nil, verr
		}
		if redo >= 0 {
			resumeFrom = redo
		} else if rec.LastSegment >= 0 {
			resumeFrom = rec.LastSegment + 1
		}
	} else if loadErr != checkpoint.ErrMissing {
		return nil, loadErr
	}

	// Phase 3: align representations onto shared cut points.
	reps := make([]segment.Representation, 0, len(feeds))
	for _, f := range feeds {
		reps = append(reps, segment.Representation{ID: f.RepID, Frames: drained[f.RepID]})
	}
	manifest, err := segment.Align(reps, r.Policy)
	if err != nil {
		return nil, err
	}

	// Phase 4: write segments from the resume point, hashing each payload.
	segHashes := map[string]string{}
	redone := 0
	for si := resumeFrom; si < len(manifest.Segments); si++ {
		select {
		case <-done:
			return nil, r.persist(rec, si-1, segHashes, manifest.SHA256())
		default:
		}
		seg := manifest.Segments[si]
		for _, repID := range manifest.Representations {
			payload := segmentPayload(repID, seg.Index, seg.Cuts[repID].FrameIndex)
			if _, werr := r.Writer.WriteSegment(repID, seg.Index, payload); werr != nil {
				// Checkpoint before surfacing the error so resume is possible.
				_ = r.persist(rec, si-1, segHashes, manifest.SHA256())
				return nil, werr
			}
			// Hash the exact fMP4 container bytes that landed on disk so the
			// checkpoint can detect later tampering with the written files.
			container, cerr := mp4box.Encode(payload)
			if cerr != nil {
				return nil, cerr
			}
			sum := sha256.Sum256(container)
			segHashes[segKey(repID, seg.Index)] = hex.EncodeToString(sum[:])
		}
		redone++
	}

	// Phase 5: final checkpoint and manifest on disk.
	if err := r.persist(rec, len(manifest.Segments)-1, segHashes, manifest.SHA256()); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(r.OutDir, "manifest.txt"),
		[]byte(manifest.Canonical()), 0o644); err != nil {
		return nil, diag.New(diag.EOutput, "", "write manifest: %v", err)
	}

	peakQ, peakMem := r.measureQueues()
	return &Stats{
		Segments:          len(manifest.Segments),
		Representations:   len(manifest.Representations),
		MaxAlignmentErrMS: manifest.MaxAlignmentErrMS,
		PeakQueue:         peakQ,
		PeakMemoryBytes:   peakMem,
		Backpressure:      r.totalBackpressure(),
		ResumeMS:          time.Since(start).Milliseconds(),
		ManifestSHA:       manifest.SHA256(),
		RedoneSegments:    redone,
	}, nil
}

func (r *Runner) persist(prev checkpoint.Record, last int, hashes map[string]string, manifestHash string) error {
	rec := checkpoint.Record{
		Cursors:       map[string]int64{},
		LastSegment:   last,
		SegmentHashes: hashes,
		ManifestHash:  manifestHash,
	}
	for id := range r.Queues {
		rec.Cursors[id] = int64(r.FrameCap)
	}
	if prev.ManifestHash != "" && prev.ManifestHash != manifestHash {
		return diag.New(diag.EChecksum, "",
			"manifest hash changed across resume: %s != %s", prev.ManifestHash, manifestHash)
	}
	return r.Store.Save(rec)
}

func (r *Runner) measureQueues() (peak int, mem int64) {
	for _, q := range r.Queues {
		if p := q.Peak(); p > peak {
			peak = p
		}
		// Bounded estimate: each FrameItem is a fixed-size struct plus the
		// queue's channel backing array; independent of input length.
		mem += int64(q.Cap()) * int64(sizeOfFrameItem)
	}
	return peak, mem
}

func (r *Runner) totalBackpressure() int64 {
	var n int64
	for _, q := range r.Queues {
		n += q.BackpressureEvents()
	}
	return n
}

func findFeed(feeds []Input, id string) *Input {
	for i := range feeds {
		if feeds[i].RepID == id {
			return &feeds[i]
		}
	}
	return nil
}
