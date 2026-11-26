package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mediaroute/internal/clock"
	"mediaroute/internal/flow"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
	"mediaroute/internal/model"
	"mediaroute/internal/output"
	"mediaroute/internal/verify"
)

// repContext bundles one representation's parse-subsystem products.
type repContext struct {
	entry manifest.Entry
	track *model.Track
	ix    *index.Index
}

// deliveredChunk is one verified-deliverable chunk plus its class.
type deliveredChunk struct {
	result    flow.ChunkResult
	recovered bool // source representation carried recovered faults
}

// pipelineResult aggregates one end-to-end pipeline run.
type pipelineResult struct {
	delivered    []deliveredChunk
	quarantined  []flow.ChunkResult
	mismatches   int
	peakBytes    int64
	backpressure uint64
}

// runPipeline fans representations out into per-bitrate byte-bounded
// queues, builds chunks in workers, and fans results in on the shared
// clock for boundary cross-checks. Cancellation and write failures
// propagate through the queues and stop the whole pipeline.
func runPipeline(reps []*repContext, outDir, quarantineDir string,
	segDur, queueBytes int64, slowMs int) (*pipelineResult, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, model.New(model.CodeIO, "pipeline outdir", err)
	}
	if err := os.MkdirAll(quarantineDir, 0o755); err != nil {
		return nil, model.New(model.CodeIO, "pipeline quarantine", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	queues := map[uint32]*flow.Queue{}
	var stops []func()
	for _, r := range reps {
		if queues[r.entry.Bitrate] == nil {
			q := flow.NewQueue(fmt.Sprintf("bitrate-%d", r.entry.Bitrate), queueBytes)
			stops = append(stops, q.WatchCancel(ctx))
			queues[r.entry.Bitrate] = q
		}
	}
	defer func() {
		for _, s := range stops {
			s()
		}
	}()

	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	results := make(chan flow.ChunkResult, 64)
	var prodWG, consWG sync.WaitGroup
	for _, r := range reps {
		prodWG.Add(1)
		go produce(r, queues[r.entry.Bitrate], segDur, &prodWG, fail)
	}
	for _, q := range queues {
		consWG.Add(1)
		go consume(q, outDir, slowMs, results, &consWG, fail)
	}
	go func() {
		prodWG.Wait()
		for _, q := range queues {
			q.Close(nil)
		}
	}()
	go func() {
		consWG.Wait()
		close(results)
	}()

	recovered := map[string]bool{}
	for _, r := range reps {
		recovered[r.entry.Camera+"/"+fmt.Sprint(r.entry.Bitrate)] = len(r.track.Faults) > 0
	}
	fi := flow.NewFanIn(2*clock.Rate, len(queues))
	res := &pipelineResult{}
	handle := func(d *flow.SegmentDecision) {
		if d.Mismatch {
			res.mismatches++
		}
		for _, r := range d.Delivered {
			res.delivered = append(res.delivered, deliveredChunk{
				result:    r,
				recovered: recovered[r.Job.Camera+"/"+fmt.Sprint(r.Job.Bitrate)],
			})
		}
		for _, r := range d.Quarantined {
			if r.OutPath != "" {
				dst := filepath.Join(quarantineDir, filepath.Base(r.OutPath))
				if err := os.Rename(r.OutPath, dst); err != nil {
					fail(model.New(model.CodeIO, "quarantine", err))
					continue
				}
				r.OutPath = dst
			}
			res.quarantined = append(res.quarantined, r)
		}
	}
	for r := range results {
		if r.Err != nil {
			fail(r.Err)
		}
		if d := fi.Add(r); d != nil {
			handle(d)
		}
	}
	for _, d := range fi.Flush() {
		d := d
		handle(&d)
	}
	for _, q := range queues {
		peak, bp := q.Stats()
		if peak > res.peakBytes {
			res.peakBytes = peak
		}
		res.backpressure += bp
	}
	if firstErr != nil {
		return res, firstErr
	}
	return res, nil
}

// produce cuts one repaired track into segment jobs and pushes payload
// bytes into the bitrate queue. Sample payloads are read lazily per
// segment; nothing larger than one segment is resident.
func produce(rc *repContext, q *flow.Queue, segDur int64,
	wg *sync.WaitGroup, fail func(error)) {
	defer wg.Done()
	f, err := os.Open(rc.entry.Path)
	if err != nil {
		fail(model.New(model.CodeIO, "produce open "+rc.entry.Path, err))
		return
	}
	defer f.Close()
	var job *flow.Job
	var payload []byte
	flush := func() bool {
		if job == nil {
			return true
		}
		job.Payload = payload
		if err := q.Put(*job); err != nil {
			return false // queue released: cancellation propagates
		}
		job = nil
		payload = nil
		return true
	}
	for _, s := range rc.track.Samples {
		if s.Truncated {
			continue
		}
		seg := int(s.Clock / segDur)
		if job == nil || seg != job.Segment {
			if !flush() {
				return
			}
			job = &flow.Job{
				Camera: rc.entry.Camera, Bitrate: rc.entry.Bitrate,
				Segment: seg, StartClock: s.Clock, Source: rc.entry.Path,
				Timescale: rc.track.Timescale, StartSample: s.Index,
			}
			payload = nil
		}
		buf := make([]byte, s.Size)
		if _, err := f.ReadAt(buf, int64(s.Offset)); err != nil {
			fail(model.New(model.CodeIO, "produce read", err))
			return
		}
		payload = append(payload, buf...)
		job.Samples = append(job.Samples, s)
		job.EndClock = int64(seg+1) * segDur // grid end of the cut point
	}
	flush()
}

// consume builds chunks from queued jobs. A slow archive disk is
// simulated with a per-job delay, creating backpressure upstream.
func consume(q *flow.Queue, outDir string, slowMs int,
	results chan<- flow.ChunkResult, wg *sync.WaitGroup, fail func(error)) {
	defer wg.Done()
	for {
		j, err := q.Get()
		if err != nil {
			return
		}
		if slowMs > 0 {
			time.Sleep(time.Duration(slowMs) * time.Millisecond)
		}
		name := fmt.Sprintf("%s_%d_seg%04d.mp4", j.Camera, j.Bitrate, j.Segment)
		digest := verify.DigestSamples(j.Samples)
		path, sha, size, err := output.WriteChunk(outDir, name, output.Chunk{
			Timescale: j.Timescale, Samples: j.Samples, Payload: j.Payload,
		})
		j.Payload = nil // release payload memory before reporting
		r := flow.ChunkResult{
			Job: j, OutPath: path, SHA256: sha, Bytes: size, Digest: digest, Err: err,
		}
		if err != nil {
			fail(err)
		}
		results <- r
	}
}
