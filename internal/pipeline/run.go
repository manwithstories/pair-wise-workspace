package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pion/rtp"

	"mediarun/internal/ebml"
	"mediarun/internal/render"
	"mediarun/internal/report"
	"mediarun/internal/timeline"
	"mediarun/internal/transport"
)

// Options configure one pipeline run.
type Options struct {
	Seed           int64
	DurationNs     int64
	TargetBps      int64
	Fps            int
	SegmentNs      int64
	OutDir         string
	CacheDir       string
	CheckpointPath string
	InterruptAfter int // stop after committing this many segments; 0 = no limit
}

// Input carries the source media and its (possibly impaired) delivery.
type Input struct {
	Source  []byte
	Tracks  []report.TrackMap
	Packets []*rtp.Packet
}

// Error is a rejection with a stable code and optional byte offset.
type Error struct {
	Code   string
	Offset int64
	Err    error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Code, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// Result summarizes a run.
type Result struct {
	Resumed          bool
	Interrupted      bool
	ReusedSegments   int
	DuplicateCommits int
	CheckpointEndMs  int64
	Manifest         *report.Manifest
}

// TracksString renders the track set for cache keys.
func TracksString(tracks []report.TrackMap) string {
	parts := make([]string, len(tracks))
	for i, t := range tracks {
		parts[i] = fmt.Sprintf("%d=%s", t.ID, t.Kind)
	}
	return strings.Join(parts, ",")
}

func paramsSHA(seed, durationNs, targetBps int64, fps, segIdx int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("seed=%d|dur=%d|bps=%d|fps=%d|seg=%d",
		seed, durationNs, targetBps, fps, segIdx)))
	return hex.EncodeToString(sum[:])
}

// SegmentCacheKey builds the content-addressed cache key for a segment.
func SegmentCacheKey(sourceSHA, tracksStr, timebase string, seed, durationNs, targetBps int64, fps, segIdx int) Key {
	return Key{
		SourceSHA: sourceSHA,
		Tracks:    tracksStr,
		Timebase:  timebase,
		ParamsSHA: paramsSHA(seed, durationNs, targetBps, fps, segIdx),
	}
}

// Run executes the pipeline: transport reassembly, EBML parse,
// timeline merge, then per-segment cached rendering with checkpointed
// commits.
func Run(opts Options, in Input) (*Result, error) {
	if opts.Fps <= 0 {
		opts.Fps = 2
	}
	if opts.SegmentNs <= 0 {
		opts.SegmentNs = 2_000_000_000
	}
	if opts.DurationNs <= 0 || opts.TargetBps <= 0 {
		return nil, fmt.Errorf("invalid duration or target bitrate")
	}
	if opts.OutDir == "" || opts.CacheDir == "" || opts.CheckpointPath == "" {
		return nil, fmt.Errorf("out, cache and checkpoint paths are required")
	}
	for _, d := range []string{opts.OutDir, opts.CacheDir, filepath.Dir(opts.CheckpointPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	sourceSHA := shaHex(in.Source)

	// Stage 1: RTP reassembly with dedup/reorder; duplicates never
	// count towards the bitrate.
	tres := transport.Process(in.Packets)
	joined := transport.PayloadBytes(tres)
	if shaHex(joined) != sourceSHA {
		return nil, &Error{Code: "TRANSPORT_LOSS",
			Err: fmt.Errorf("reassembly digest mismatch, lost ranges %v", tres.Stats.LostRanges)}
	}
	actualBps := tres.Stats.UniqueBytes * 8 * 1_000_000_000 / opts.DurationNs

	// Stage 2: EBML parse. Any structured error rejects the input.
	doc, err := ebml.Parse(joined)
	if err != nil {
		var ee *ebml.Error
		if errors.As(err, &ee) {
			return nil, &Error{Code: string(ee.Code), Offset: ee.Offset, Err: err}
		}
		return nil, err
	}

	// Stage 3: unified timeline.
	merger := timeline.NewMerger(timeline.NanosPerTick(doc.TimeScale))
	for i := range doc.Blocks {
		b := &doc.Blocks[i]
		ts := b.Timestamp
		merger.Add(b.Track, &ts, b.Duration, len(b.Data))
	}
	extent := int64(merger.Extent())

	// Stage 4: checkpoint (quarantines corrupted or foreign files).
	cp, _, err := LoadCheckpoint(opts.CheckpointPath, sourceSHA)
	if err != nil {
		return nil, err
	}

	// Stage 5: manifest, reused when it belongs to this source.
	manifest, merr := report.Load(opts.OutDir)
	if merr != nil || manifest.SourceSHA256 != sourceSHA {
		manifest = &report.Manifest{}
	}
	manifest.SourceSHA256 = sourceSHA
	manifest.Tracks = in.Tracks
	manifest.ParamsSHA256 = paramsSHA(opts.Seed, opts.DurationNs, opts.TargetBps, opts.Fps, -1)
	manifest.TargetBps = opts.TargetBps
	manifest.ActualBps = actualBps
	manifest.BitrateDeltaPct = math.Abs(float64(actualBps-opts.TargetBps)) / float64(opts.TargetBps) * 100
	manifest.Errors = nil
	for _, ei := range merger.Errors() {
		manifest.Errors = append(manifest.Errors, report.ErrorInterval{
			Track: ei.Track, StartNs: int64(ei.Start), EndNs: int64(ei.End), Reason: ei.Reason,
		})
	}

	// Stage 6: incremental segment processing.
	cache := &Cache{Dir: opts.CacheDir}
	tracksStr := TracksString(in.Tracks)
	timebase := fmt.Sprintf("%d/1", doc.TimeScale)
	nSeg := int(opts.DurationNs / opts.SegmentNs)
	framesPerSeg := int(opts.SegmentNs * int64(opts.Fps) / 1_000_000_000)
	rp := render.Params{Width: 320, Height: 180, Seed: opts.Seed}
	ref := render.Reference(rp)
	res := &Result{}
	committed := 0

	for s := 0; s < nSeg; s++ {
		startMs := int64(s) * opts.SegmentNs / 1_000_000
		endMs := int64(s+1) * opts.SegmentNs / 1_000_000
		if cp.Covers(startMs, endMs) && segmentReusable(opts.OutDir, manifest, s, framesPerSeg) {
			res.ReusedSegments++
			continue
		}
		key := SegmentCacheKey(sourceSHA, tracksStr, timebase,
			opts.Seed, opts.DurationNs, opts.TargetBps, opts.Fps, s)
		_, frames, hit := cache.Get(key)
		if !hit {
			frames = frames[:0]
			for i := 0; i < framesPerSeg; i++ {
				frames = append(frames, render.Frame(rp, s*framesPerSeg+i))
			}
			if err := cache.Put(key, frames); err != nil {
				return nil, err
			}
		}
		for i, f := range frames {
			idx := s*framesPerSeg + i
			path, err := report.CheckPathInside(opts.OutDir,
				filepath.Join("frames", fmt.Sprintf("frame_%04d.raw", idx)))
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
			if err := writeFileAtomic(path, f); err != nil {
				return nil, err
			}
			upsertFrame(manifest, report.FrameRef{
				Index:  idx,
				TimeNs: int64(idx) * 1_000_000_000 / int64(opts.Fps),
				SHA256: render.SHA256(f),
			})
			if psnr := render.PSNR(f, ref); manifest.QualityPSNRdB == 0 || psnr < manifest.QualityPSNRdB {
				manifest.QualityPSNRdB = psnr
			}
		}
		// Commit: outputs and manifest land atomically before the
		// checkpoint advances. Never append an already-covered interval.
		if cp.Covers(startMs, endMs) {
			res.DuplicateCommits++
		} else {
			cp.Done = append(cp.Done, Interval{StartMs: startMs, EndMs: endMs})
		}
		manifest.Checkpoints = toReportIntervals(cp.Done)
		manifest.AlignmentMs = float64(abs64(cp.End()*1_000_000-extent)) / 1e6
		if err := report.Write(opts.OutDir, manifest); err != nil {
			return nil, err
		}
		if err := SaveCheckpoint(opts.CheckpointPath, cp); err != nil {
			return nil, err
		}
		committed++
		if opts.InterruptAfter > 0 && committed >= opts.InterruptAfter {
			res.Interrupted = true
			res.CheckpointEndMs = cp.End()
			res.Manifest = manifest
			return res, nil
		}
	}
	res.Resumed = res.ReusedSegments > 0
	res.CheckpointEndMs = cp.End()
	res.Manifest = manifest
	return res, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func toReportIntervals(in []Interval) []report.Interval {
	out := make([]report.Interval, len(in))
	for i, iv := range in {
		out[i] = report.Interval{StartMs: iv.StartMs, EndMs: iv.EndMs}
	}
	return out
}

func upsertFrame(m *report.Manifest, fr report.FrameRef) {
	for i := range m.Frames {
		if m.Frames[i].Index == fr.Index {
			m.Frames[i] = fr
			return
		}
	}
	m.Frames = append(m.Frames, fr)
	sort.Slice(m.Frames, func(i, j int) bool { return m.Frames[i].Index < m.Frames[j].Index })
}

// segmentReusable verifies that every frame of a committed segment is
// still on disk with the digest recorded in the manifest.
func segmentReusable(outDir string, m *report.Manifest, segIdx, framesPerSeg int) bool {
	for i := 0; i < framesPerSeg; i++ {
		idx := segIdx*framesPerSeg + i
		var fr *report.FrameRef
		for j := range m.Frames {
			if m.Frames[j].Index == idx {
				fr = &m.Frames[j]
				break
			}
		}
		if fr == nil {
			return false
		}
		path, err := report.CheckPathInside(outDir,
			filepath.Join("frames", fmt.Sprintf("frame_%04d.raw", idx)))
		if err != nil {
			return false
		}
		data, err := os.ReadFile(path)
		if err != nil || shaHex(data) != fr.SHA256 {
			return false
		}
	}
	return true
}
