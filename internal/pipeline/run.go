package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sync"

	"mediascan/internal/core"
	"mediascan/internal/index"
	"mediascan/internal/segment"
)

// Options control one pipeline run.
type Options struct {
	Root        string // input root (segment paths resolve against it)
	StateDir    string
	Hashes      bool // compute per-segment SHA-256
	Concurrency int  // worker count, clamped to [1, 16]
	StopAfter   int  // >0: interrupt after this many newly processed segments
}

// Outcome summarizes a finished (or interrupted) run.
type Outcome struct {
	Checkpoint   *Checkpoint
	NewProcessed int          // segments that reached a terminal state in this run
	Damage       []core.Range // merged segment-source damage ranges
	Interrupted  bool         // stopped early by StopAfter; resume to continue
}

const maxConcurrency = 16

// Run executes the pipeline over all non-terminal segments of the index,
// in index order, with bounded concurrency. The checkpoint is saved
// atomically after every segment. Results are deterministic regardless
// of worker count.
func Run(idx *index.Index, opts Options) (*Outcome, error) {
	cp, err := LoadCheckpoint(opts.StateDir, idx)
	if err != nil {
		return nil, err
	}
	var todo []index.SegmentRef
	for _, s := range idx.Segments {
		if !cp.States[s.ID].Terminal() {
			todo = append(todo, s)
		}
	}
	out := &Outcome{Checkpoint: cp}
	conc := opts.Concurrency
	if conc <= 0 {
		conc = 4
	}
	if conc > maxConcurrency {
		conc = maxConcurrency
	}

	jobs := make(chan index.SegmentRef)
	results := make(chan *SegResult)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				results <- processSegment(opts.Root, s, opts.Hashes)
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	next, inflight := 0, 0
	stopped := false
	for inflight > 0 || (next < len(todo) && !stopped) {
		var jobCh chan<- index.SegmentRef
		var job index.SegmentRef
		if next < len(todo) && !stopped {
			jobCh, job = jobs, todo[next]
		}
		select {
		case jobCh <- job:
			next++
			inflight++
		case res := <-results:
			inflight--
			cp.States[res.ID] = res.State
			cp.Results[res.ID] = res
			out.NewProcessed++
			if err := cp.Save(opts.StateDir); err != nil {
				return nil, err
			}
			if opts.StopAfter > 0 && out.NewProcessed >= opts.StopAfter {
				stopped = true
			}
		}
	}
	close(jobs)

	// Aggregate damage from terminal results, in index order.
	for _, s := range idx.Segments {
		if res := cp.Results[s.ID]; res != nil {
			out.Damage = append(out.Damage, res.Damage...)
		}
	}
	out.Damage = core.MergeRanges(out.Damage)
	remaining := 0
	for _, s := range idx.Segments {
		if !cp.States[s.ID].Terminal() {
			remaining++
		}
	}
	out.Interrupted = remaining > 0
	return out, nil
}

// processSegment analyzes one segment. Failures mark only this
// segment's time range; they never abort other tracks.
func processSegment(root string, s index.SegmentRef, hash bool) *SegResult {
	res := &SegResult{ID: s.ID, Path: s.Path, Start: s.Start, End: s.End, State: StateDone}
	fail := func(format string, args ...any) *SegResult {
		res.State = StateFailed
		msg := fmt.Sprintf(format, args...)
		res.Errors = append(res.Errors, msg)
		res.Damage = append(res.Damage, core.Range{
			Start: s.Start, End: s.End, Source: "segment", Reason: msg,
		})
		return res
	}
	p, err := index.SafeJoin(root, s.Path)
	if err != nil {
		return fail("unsafe path: %v", err)
	}
	data, err := index.ReadLimited(p, index.MaxSegmentSize)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fail("segment missing: %s", s.Path)
		}
		return fail("read: %v", err)
	}
	if hash {
		sum := sha256.Sum256(data)
		res.SHA256 = hex.EncodeToString(sum[:])
	}
	frag, err := segment.ParseFragment(data)
	if err != nil {
		if segment.IsTruncated(err) {
			return fail("truncated: %v", err)
		}
		return fail("parse: %v", err)
	}
	res.Boxes = frag.Boxes
	if frag.MdatSize == 0 {
		return fail("no mdat box")
	}
	mdat := data[frag.MdatOffset : frag.MdatOffset+frag.MdatSize]
	kf, err := segment.FindKeyframes(mdat, frag.Samples, segment.CodecAVC)
	if err != nil {
		return fail("keyframes: %v", err)
	}
	res.Keyframes = kf
	res.Gaps = segment.KeyframeGaps(kf)
	return res
}
