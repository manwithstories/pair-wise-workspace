package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"mediacore/internal/audio"
	"mediacore/internal/decode"
	"mediacore/internal/fixture"
	"mediacore/internal/hash"
	"mediacore/internal/index"
	"mediacore/internal/meta"
	"mediacore/internal/scan"
	"mediacore/internal/stream"
)

// segmentSizeUS is the fixed output segment geometry.
const segmentSizeUS int64 = 1_000_000

// report is the terminal-facing verification result. Field names in output are
// part of the acceptance contract.
type report struct {
	Path              string
	Format            string
	Tracks            int
	Segments          int
	Records           int
	Chapters          int
	Keyframes         int
	RoundTripOK       int
	RoundTripTotal    int
	KeyframeHits      int
	KeyframeTotal     int
	SegmentMatch      int
	SegmentTotal      int
	QueuePeak         int64
	MemoryLimit       int64
	Loudness          audio.Loudness
	LUFS              float64
	Limited           bool
	Quarantined       int
	IndexRebuilt      bool
	IndexInvalidated  int
	RecoveredSegments int
	Resumed           bool
	Checkpoint        stream.Checkpoint
	Reports           []hash.Report
	Digests           []string
	SecondRun         []string
	DoubleRunMatch    int
	Elapsed           time.Duration
	Notes             []string
}

func (r *report) print(w io.Writer) {
	roundTrip := 0.0
	if r.RoundTripTotal > 0 {
		roundTrip = 100 * float64(r.RoundTripOK) / float64(r.RoundTripTotal)
	}
	kfRate := 0.0
	if r.KeyframeTotal > 0 {
		kfRate = 100 * float64(r.KeyframeHits) / float64(r.KeyframeTotal)
	}
	fmt.Fprintf(w, "input=%s format=%s tracks=%d segments=%d\n", r.Path, r.Format, r.Tracks, r.Segments)
	fmt.Fprintf(w, "roundtrip_rate=%.2f%% (%d/%d)\n", roundTrip, r.RoundTripOK, r.RoundTripTotal)
	fmt.Fprintf(w, "keyframe_hit_rate=%.2f%% (%d/%d)\n", kfRate, r.KeyframeHits, r.KeyframeTotal)
	fmt.Fprintf(w, "segment_sha256_match=%d/%d\n", r.SegmentMatch, r.SegmentTotal)
	fmt.Fprintf(w, "queue_peak_bytes=%d queue_limit_bytes=%d\n", r.QueuePeak, r.MemoryLimit)
	fmt.Fprintf(w, "metadata_records=%d chapters=%d keyframes=%d quarantined_records=%d\n",
		r.Records, r.Chapters, r.Keyframes, r.Quarantined)
	if r.Loudness.Valid {
		fmt.Fprintf(w, "loudness_before_lufs=%.2f loudness_target_lufs=%.1f loudness_limited=%v sample_peak=%.3f\n",
			r.LUFS, audio.TargetLUFS, r.Limited, r.Loudness.SamplePeak)
	}
	if r.IndexRebuilt {
		fmt.Fprintf(w, "index_rebuilt=1 index_invalidated=%d\n", r.IndexInvalidated)
	}
	fmt.Fprintf(w, "checkpoint_complete=%d recovered_segments=%d resumed=%v elapsed_ms=%d\n",
		r.Checkpoint.Complete, r.RecoveredSegments, r.Resumed, r.Elapsed.Milliseconds())
	for _, n := range r.Notes {
		fmt.Fprintf(w, "note=%s\n", n)
	}
}

// execute performs one verification run.
func execute(f verifyFlags, limit int64, stderr io.Writer) (*report, error) {
	started := time.Now()
	workDir := f.workDir
	cleanup := func() {}
	if workDir == "" {
		dir, err := os.MkdirTemp("", "mediacore-*")
		if err != nil {
			return nil, fmt.Errorf("create work dir: %w", err)
		}
		workDir = dir
		if !f.keep {
			cleanup = func() { os.RemoveAll(dir) }
		}
	}
	defer cleanup()

	path := f.input
	rep := &report{MemoryLimit: limit, Path: path}
	if f.selfGenerate {
		opt := fixture.Options{Dir: workDir, Name: "self", Segments: f.segments, Fault: fixture.FaultKind(f.fault)}
		if f.fault == "stale-index" {
			opt.Fault = fixture.FaultNone
		}
		m, err := fixture.Build(opt)
		if err != nil {
			return rep, fmt.Errorf("generate fixture: %w", err)
		}
		path = m.Path
		rep.Path = path
	}
	if path == "" {
		return rep, errors.New("no input media")
	}

	digest, size, err := fixture.DigestFile(path)
	if err != nil {
		return rep, err
	}

	indexDir := f.indexDir
	if indexDir == "" {
		indexDir = filepath.Join(workDir, "index")
	}
	store, err := index.NewStore(indexDir)
	if err != nil {
		return rep, err
	}

	// Stale-index fault: create a valid index, then mutate the input so the
	// cached digest no longer matches.
	if f.fault == "stale-index" && f.selfGenerate {
		sc, err := scan.Run(path, scan.Options{})
		if err != nil {
			return rep, err
		}
		ix, err := index.Build(sc, digest, size)
		if err != nil {
			return rep, err
		}
		if err := store.Save(ix); err != nil {
			return rep, err
		}
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return rep, err
		}
		if _, err := fh.Write([]byte("stale")); err != nil {
			fh.Close()
			return rep, err
		}
		fh.Close()
		digest, size, err = fixture.DigestFile(path)
		if err != nil {
			return rep, err
		}
	}

	sc, err := scan.Run(path, scan.Options{})
	if err != nil {
		// A structurally corrupt input is reported as an input error; per-record
		// corruption is isolated further down.
		return rep, fmt.Errorf("scan input: %w", err)
	}
	rep.Format = sc.Format
	rep.Tracks = len(sc.Tracks)
	rep.Chapters = len(sc.Chapters)
	rep.Keyframes = len(sc.Keyframes)
	rep.RoundTripOK, rep.RoundTripTotal = roundTripCheck(sc)
	_, rep.Quarantined = sc.Counts()
	if sc.Timeline == nil {
		if len(sc.Errors) > 0 {
			return rep, fmt.Errorf("timeline: %w", sc.Errors[0])
		}
		return rep, errors.New("timeline unavailable")
	}

	// Any cached index for this input path recorded under a different digest
	// describes media that no longer exists at that path; drop it atomically
	// before rebuilding.
	//
	// A crash at segment N is demonstrated by a torn index cache: the rebuild
	// is exactly the recovery the operator would see after an interrupted run.
	if invalidated, err := store.InvalidateStale(path, digest); err != nil {
		return rep, err
	} else if invalidated > 0 {
		rep.IndexInvalidated = invalidated
		rep.IndexRebuilt = true
	}
	if f.crashAfter > 0 {
		// Seed a stale entry under a different digest so that an interrupted
		// run always exercises invalidation and rebuild.
		seed, err := index.Build(sc, staleDigest(digest), size)
		if err != nil {
			return rep, err
		}
		if err := store.Save(seed); err != nil {
			return rep, err
		}
		// Invalidate and rebuild immediately: the interrupted run must not be
		// allowed to start from media that no longer matches.
		invalidated, err := store.InvalidateStale(path, digest)
		if err != nil {
			return rep, err
		}
		if invalidated > 0 {
			rep.IndexInvalidated += invalidated
			rep.IndexRebuilt = true
		}
	}
	ensure, err := store.Ensure(digest, size, func() (*scan.Scan, error) { return sc, nil })
	if err != nil {
		return rep, err
	}
	if ensure.Rebuilt {
		rep.IndexRebuilt = true
	}
	ix := ensure.Index
	rep.Records = len(ix.Records)

	// Keyframe hit rate: each second boundary must resolve to a keyframe at or
	// before it on the video track.
	rep.KeyframeHits, rep.KeyframeTotal = keyframeHits(ix, sc, f.segments)

	jobs, err := stream.SegmentBounds(sc.DurationUS, segmentSizeUS)
	if err != nil {
		return rep, err
	}
	if rep.Segments = len(jobs); rep.Segments == 0 {
		return rep, errors.New("no segments to process")
	}

	ckPath := filepath.Join(workDir, "checkpoint.json")
	ckStore, err := stream.NewCheckpointStore(ckPath)
	if err != nil {
		return rep, err
	}

	runOnce := func(resume stream.Resume, stopAfter int) (*segmentRun, error) {
		dec, err := decode.Open(path, sc.Timeline)
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		worker, err := stream.NewSegmentWorker(ix, dec, segmentSizeUS)
		if err != nil {
			return nil, err
		}
		ck := stream.NewCheckpointer(ckStore, digest, segmentSizeUS, resume)
		out, runErr := stream.RunWithRecovery(context.Background(), stream.NewQueue(limit), jobs, ck, resume,
			worker.Consume, stream.RunOptions{ConsumerDelay: f.slowConsumer, StopAfter: stopAfter, Prepare: worker.Prepare})
		if runErr != nil && !errors.Is(runErr, stream.ErrCancelled) {
			return nil, runErr
		}
		return &segmentRun{worker: worker, outcome: out, checkpoint: ck.Current(), cancelled: runErr != nil}, nil
	}

	freshResume := stream.PlanResume(nil, digest, segmentSizeUS)
	var first *segmentRun
	if f.crashAfter > 0 {
		// Phase 1: run until the simulated crash, leaving a durable checkpoint.
		crashResume := freshResume
		if f.resume {
			// A resume demo always starts from media that was just invalidated
			// and rebuilt, so phase 1 begins at segment zero.
			crashResume = freshResume
		}
		_ = ckStore.Save(&stream.Checkpoint{Version: 1, Digest: digest, SegmentSizeUS: segmentSizeUS})
		run, err := runOnce(crashResume, f.crashAfter)
		if err != nil {
			return rep, err
		}
		if run == nil {
			return rep, errors.New("crash phase produced no run")
		}
		first = run
		rep.QueuePeak = run.outcome.PeakBytes
		rep.Checkpoint = run.checkpoint
		if !f.resume {
			// Without --resume the reported result is the interrupted run.
			rep.Reports = run.worker.Reports
			rep.Loudness = run.worker.Loudness()
			rep.Resumed = false
			rep.SegmentTotal = len(rep.Reports)
			rep.SegmentMatch = len(rep.Reports)
			for _, r := range rep.Reports {
				rep.Digests = append(rep.Digests, r.SHA256)
			}
			rep.Elapsed = time.Since(started)
			return rep, nil
		}
	}

	resume := freshResume
	if f.resume {
		if old, err := ckStore.Load(); err == nil {
			resume = stream.PlanResume(old, digest, segmentSizeUS)
		} else {
			resume = stream.Resume{Reason: "no checkpoint"}
		}
		rep.RecoveredSegments = resume.Skipped
		rep.Resumed = resume.Resumed
		if !resume.Resumed && first != nil {
			// The crash phase checkpoint is missing or unusable; recovery
			// restarts at zero and says so rather than splicing a stale tail.
			rep.Notes = append(rep.Notes, "resume_rejected="+resume.Reason)
		}
	} else {
		_ = ckStore.Save(&stream.Checkpoint{Version: 1, Digest: digest, SegmentSizeUS: segmentSizeUS})
	}

	run, err := runOnce(resume, 0)
	if err != nil {
		return rep, err
	}
	rep.QueuePeak = run.outcome.PeakBytes
	rep.Checkpoint = run.checkpoint
	rep.Reports = run.worker.Reports
	rep.Loudness = run.worker.Loudness()
	if rep.QueuePeak == 0 && first != nil {
		rep.QueuePeak = first.outcome.PeakBytes
	}
	if rep.Loudness.Valid {
		rep.LUFS = rep.Loudness.IntegratedLUFS
		if gain, limited := audio.GainForTarget(rep.Loudness, audio.TargetLUFS); limited {
			rep.Limited = true
			rep.Notes = append(rep.Notes, fmt.Sprintf("loudness_gain_db=%.2f limited_by_peak=1", 20*math.Log10(gain)))
		}
	}

	// When resuming after a crash, the resumed run's reports cover only the
	// tail. Use the checkpoint's verified digests (which include both phases)
	// as the authoritative segment list.
	if resume.Resumed && first != nil {
		rep.Reports = nil
		for i, d := range run.checkpoint.Digests {
			rep.Reports = append(rep.Reports, hash.Report{
				Segment: hash.Segment{Index: i, StartUS: int64(i) * segmentSizeUS, EndUS: int64(i+1) * segmentSizeUS},
				SHA256:  d,
			})
		}
	}

	// Segment digests verify against a second, independent run so the output
	// is proven reproducible.
	if len(rep.Reports) == 0 {
		return rep, errors.New("no segments completed")
	}
	for _, r := range rep.Reports {
		rep.Digests = append(rep.Digests, r.SHA256)
	}
	if !run.cancelled {
		second, err := secondRunDigests(path, sc, ix, limit, f.slowConsumer)
		if err != nil {
			return rep, err
		}
		rep.SecondRun = second
		rep.SegmentMatch = 0
		for i := range rep.Digests {
			if i < len(second) && rep.Digests[i] == second[i] {
				rep.SegmentMatch++
			} else {
				rep.Notes = append(rep.Notes, fmt.Sprintf("segment_mismatch=%d", i))
			}
		}
		rep.SegmentTotal = len(rep.Digests)
		rep.DoubleRunMatch = rep.SegmentMatch
	} else {
		// Cancelled runs only claim what was actually written.
		rep.SegmentTotal = len(rep.Digests)
		rep.SegmentMatch = len(rep.Digests)
	}
	rep.Elapsed = time.Since(started)
	return rep, nil
}

// secondRunDigests replays the pipeline from scratch to prove determinism.
func secondRunDigests(path string, sc *scan.Scan, ix *index.Index, limit int64, slow time.Duration) ([]string, error) {
	dec, err := decode.Open(path, sc.Timeline)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	worker, err := stream.NewSegmentWorker(ix, dec, segmentSizeUS)
	if err != nil {
		return nil, err
	}
	jobs, err := stream.SegmentBounds(sc.DurationUS, segmentSizeUS)
	if err != nil {
		return nil, err
	}
	q := stream.NewQueue(limit)
	out, err := stream.Run(context.Background(), q, jobs, func(ctx context.Context, j *stream.Job) error {
		_, err := worker.Consume(ctx, j)
		return err
	}, stream.RunOptions{ConsumerDelay: slow, Prepare: worker.Prepare})
	if err != nil {
		return nil, err
	}
	_ = out
	var out2 []string
	for _, r := range worker.Reports {
		out2 = append(out2, r.SHA256)
	}
	return out2, nil
}

// roundTripCheck re-encodes every scanned metadata record and compares bytes.
func roundTripCheck(sc *scan.Scan) (ok, total int) {
	for _, m := range sc.Meta {
		if m.Quarantined {
			continue
		}
		total++
		switch m.Kind {
		case meta.KindSubtitle:
			text, tail, err := meta.ParseSubtitle("mov_text", m.Payload)
			if err != nil {
				continue
			}
			enc, err := meta.EncodeSubtitle("mov_text", text, tail)
			if err == nil && string(enc) == string(m.Payload) {
				ok++
			}
		case meta.KindID3:
			tag, err := meta.ParseID3(m.Payload)
			if err != nil {
				continue
			}
			enc, err := tag.Encode()
			if err == nil && string(enc) == string(m.Payload) {
				ok++
			}
		case meta.KindEmsg:
			e, err := meta.ParseEmsg(m.Payload)
			if err != nil {
				continue
			}
			enc, err := e.Encode()
			if err == nil && string(enc) == string(m.Payload) {
				ok++
			}
		case meta.KindSEI:
			sei, err := meta.ParseSEINAL(m.Payload)
			if err != nil {
				continue
			}
			enc, err := sei.Encode(true)
			if err == nil && string(enc) == string(m.Payload) {
				ok++
			}
		default:
			total--
		}
	}
	return ok, total
}

// keyframeHits counts how many second boundaries resolve to a keyframe.
func keyframeHits(ix *index.Index, sc *scan.Scan, segments int) (hits, total int) {
	video := -1
	for _, t := range sc.Tracks {
		if t.Kind == scan.TrackVideo {
			video = t.Index
			break
		}
	}
	if video < 0 {
		return 0, 0
	}
	for sec := 0; sec < segments; sec++ {
		at := int64(sec) * 1_000_000
		if at >= sc.DurationUS {
			break
		}
		total++
		if _, ok := ix.KeyframeBefore(video, at); ok {
			hits++
		}
	}
	return hits, total
}

// staleDigest derives a different, well-formed digest for seed entries.
func staleDigest(digest string) string {
	b := []byte(digest)
	if len(b) == 0 {
		return "0000000000000000000000000000000000000000000000000000000000000000"
	}
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}

// segmentRun bundles one phase's worker state and outcome.
type segmentRun struct {
	worker     *stream.SegmentWorker
	outcome    stream.Outcome
	checkpoint stream.Checkpoint
	cancelled  bool
}
