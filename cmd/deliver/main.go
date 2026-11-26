// Command deliver is the post-tour media delivery verifier: it
// calibrates timecodes onto a unified clock, streams cues and
// synthetic AV events through a bounded pipeline into a segment
// aligner, renders HLS manifests and prints a verification report.
//
// Exit codes: 0 done; 2 recoverable corruption with valid manifest;
// 1 unrecoverable error; 130 interrupted, checkpoint saved for resume.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"deliver/internal/cue"
	"deliver/internal/hls"
	"deliver/internal/metrics"
	"deliver/internal/model"
	"deliver/internal/pipe"
	"deliver/internal/timecode"
)

var errInterrupt = errors.New("simulated interruption")

type faultSet map[string]bool

func (f faultSet) has(k string) bool { return f["all"] || f[k] }

type cfg struct {
	seconds, rendN, queueCap int
	seg, slow                time.Duration
	faults                   faultSet
	ckpt                     string
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "verify" {
		fmt.Fprintln(os.Stderr, "usage: deliver verify [--seconds N] [--renditions N] "+
			"[--segment D] [--queue N] [--slow-consumer D] [--faults LIST]")
		os.Exit(1)
	}
	os.Exit(runVerify(os.Args[2:]))
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	c := cfg{}
	fs.IntVar(&c.seconds, "seconds", 180, "timeline length in seconds")
	fs.IntVar(&c.rendN, "renditions", 3, "bitrate ladder rungs")
	fs.DurationVar(&c.seg, "segment", 2*time.Second, "segment length")
	fs.IntVar(&c.queueCap, "queue", pipe.DefaultCapacity, "per-stage queue capacity")
	fs.DurationVar(&c.slow, "slow-consumer", 0, "artificial consumer delay per event")
	faultsFlag := fs.String("faults", "", "comma list: badcue,wrap,loss,interrupt,all")
	workdir := fs.String("workdir", "out", "scratch dir for checkpoints")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	c.faults = faultSet{}
	for _, f := range strings.Split(*faultsFlag, ",") {
		if f != "" {
			c.faults[f] = true
		}
	}
	c.ckpt = filepath.Join(*workdir, "checkpoint")
	os.Remove(c.ckpt) // fresh verify run

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	clock := model.NewClock(time.Now())

	rep := &metrics.Report{}
	cues, skipped, err := loadCues(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		return 1
	}
	rep.SkippedRecords = skipped
	cams := defaultCams(c.faults.has("wrap"))
	events := genEvents(c.seconds, cams)
	rep.AVSyncMaxMs = maxAvSkew(events, len(cams))
	if err := checkWrap(cams, c.seconds); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		return 1
	}
	stream, retry := events, []hls.Event(nil)
	if c.faults.has("loss") {
		stream, retry = injectFaults(events)
	}
	totalMs := int64(c.seconds) * 1000
	segs := hls.Segments(totalMs, c.seg.Milliseconds(), timecode.FPS25, cues)
	rep.AlignedSegments = len(segs)

	// Pass 1, optionally truncated by the interrupt fixture.
	committed, resumed := int64(0), false
	cut := int64(-1)
	if c.faults.has("interrupt") {
		cut = int64(len(events)) * 3 / 5
	}
	pr, err := runPipeline(ctx, clock, c, stream, retry, committed, cut, rep)
	if errors.Is(err, errInterrupt) {
		if werr := hls.WriteCheckpoint(c.ckpt, pr); werr != nil {
			fmt.Fprintln(os.Stderr, "fatal:", werr)
			return 1
		}
		fmt.Fprintf(os.Stderr, "interrupted at seq %d; checkpoint saved, resuming\n", pr)
		seq, ok, rerr := hls.ReadCheckpoint(c.ckpt)
		if rerr != nil || !ok {
			fmt.Fprintln(os.Stderr, "fatal: checkpoint unreadable")
			return 1
		}
		committed, resumed = seq, true
	} else if err != nil {
		return fatalOrInterrupted(ctx, c.ckpt, committed, err)
	}
	committed, err = runPipeline(ctx, clock, c, stream, retry, committed, -1, rep)
	if err != nil {
		return fatalOrInterrupted(ctx, c.ckpt, committed, err)
	}
	rep.Resumed = resumed
	if committed != int64(len(events)) {
		fmt.Fprintf(os.Stderr, "fatal: committed %d of %d events\n", committed, len(events))
		return 1
	}

	clean, err := buildManifests(c.rendN, segs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		return 1
	}
	rep.CleanHash = metrics.Hash(clean)
	replay, err := replayHash(c) // deterministic re-run
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		return 1
	}
	rep.ReplayHash = replay
	fmt.Print(rep.String())
	if rep.CleanHash != rep.ReplayHash {
		fmt.Fprintln(os.Stderr, "fatal: replay hash mismatch")
		return 1
	}
	if rep.SkippedRecords > 0 {
		fmt.Fprintln(os.Stderr, "exit 2: recoverable corruption skipped, manifest valid")
		return 2
	}
	return 0
}

// loadCues parses the generated subtitle stream, reporting skips.
func loadCues(c cfg) ([]cue.Cue, int, error) {
	cues, errs := cue.Parse(strings.NewReader(genSRT(c.seconds, c.faults.has("badcue"))), false, 0)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "skip:", e)
	}
	cue.SortStable(cues)
	return cues, len(errs), nil
}

// checkWrap resolves camera timecode checkpoints and requires them to
// land on the unified timeline within one frame of their true slot.
func checkWrap(cams []camSpec, seconds int) error {
	cps, err := genCheckpoints(cams, seconds)
	if err != nil {
		return err
	}
	n := int64(0)
	for _, cp := range cps {
		slot := (cp.Seq - 1_000_000) % int64(seconds) * 1000
		if d := cp.MediaMs - slot; d > 40 || d < -40 {
			return model.Fatalf(model.StageTimecode,
				"cam %s checkpoint off by %dms", cp.Cam, d)
		}
		n++
	}
	return nil
}

// runPipeline pushes the stream through the bounded queue into the
// reassembler. cutSeq >= 0 simulates an interruption at that sequence.
func runPipeline(ctx context.Context, clock *model.Clock, c cfg, stream, retry []hls.Event,
	startSeq, cutSeq int64, rep *metrics.Report) (int64, error) {
	meter := &pipe.Meter{}
	q := pipe.NewQueue[hls.Event](c.queueCap, meter)
	reasm := hls.NewReassembler(startSeq, 16)
	for _, e := range retry {
		if e.Seq >= startSeq {
			reasm.Cache(e)
		}
	}
	producer := func(ctx context.Context) error {
		defer q.Close()
		for _, e := range stream {
			if e.Seq < startSeq {
				continue
			}
			if cutSeq >= 0 && e.Seq >= cutSeq {
				return errInterrupt
			}
			e.EnqMs = clock.Now()
			if err := q.Push(ctx, e); err != nil {
				return err
			}
		}
		return nil
	}
	consumer := func(ctx context.Context) error {
		for {
			e, ok := q.Pop(ctx)
			if !ok {
				for _, ev := range reasm.Flush() {
					rep.Observe(clock.Since(ev.EnqMs))
				}
				return nil
			}
			for _, ev := range reasm.Push(e) {
				rep.Observe(clock.Since(ev.EnqMs))
			}
			if c.slow > 0 {
				time.Sleep(c.slow) // slow consumer: backpressure propagates
			}
		}
	}
	err := pipe.Run(ctx, producer, consumer)
	if meter.Peak > rep.PeakBufferBytes {
		rep.PeakBufferBytes = meter.Peak
	}
	rep.LossRecovered += reasm.Healed
	rep.LossTotal += reasm.Healed + reasm.Lost
	return reasm.Next(), err
}

// buildManifests renders master and media playlists and independently
// re-parses each media playlist to verify segment boundaries.
func buildManifests(rendN int, segs []hls.Segment) (string, error) {
	rends := make([]hls.Rendition, rendN)
	for i := range rends {
		rends[i] = hls.Rendition{Name: fmt.Sprintf("v%d", i), Bandwidth: uint32(800_000 * (i + 1))}
	}
	var b strings.Builder
	b.WriteString(hls.BuildMaster(rends))
	for _, r := range rends {
		pl, err := hls.BuildMediaPlaylist(r, segs)
		if err != nil {
			return "", err
		}
		if err := hls.VerifyMediaPlaylist(pl, segs, 40); err != nil {
			return "", err
		}
		b.WriteString(pl)
	}
	return b.String(), nil
}

// replayHash deterministically re-runs parsing, alignment and manifest
// rendering; the hash must match the clean run.
func replayHash(c cfg) (string, error) {
	cues, errs := cue.Parse(strings.NewReader(genSRT(c.seconds, c.faults.has("badcue"))), false, 0)
	if len(errs) > 0 {
		// same recoverable skips as the clean run
	}
	cue.SortStable(cues)
	segs := hls.Segments(int64(c.seconds)*1000, c.seg.Milliseconds(), timecode.FPS25, cues)
	m, err := buildManifests(c.rendN, segs)
	if err != nil {
		return "", err
	}
	return metrics.Hash(m), nil
}

// maxAvSkew returns the worst cross-camera av skew per time slot.
func maxAvSkew(evs []hls.Event, nCams int) int64 {
	var worst int64
	for i := 0; i+nCams <= len(evs); i += nCams {
		lo, hi := evs[i].MediaMs, evs[i].MediaMs
		for _, e := range evs[i : i+nCams] {
			if e.MediaMs < lo {
				lo = e.MediaMs
			}
			if e.MediaMs > hi {
				hi = e.MediaMs
			}
		}
		if hi-lo > worst {
			worst = hi - lo
		}
	}
	return worst
}

// fatalOrInterrupted maps a pipeline error to an exit code, saving a
// checkpoint on signal interruption for a later resume.
func fatalOrInterrupted(ctx context.Context, ckpt string, committed int64, err error) int {
	if ctx.Err() != nil {
		hls.WriteCheckpoint(ckpt, committed)
		fmt.Fprintf(os.Stderr, "interrupted: checkpoint saved at seq %d; rerun to resume\n", committed)
		return 130
	}
	fmt.Fprintln(os.Stderr, "fatal:", err)
	return 1
}
