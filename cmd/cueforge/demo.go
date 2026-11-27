package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"cueforge/internal/checkpoint"
	"cueforge/internal/clock"
	"cueforge/internal/model"
	"cueforge/internal/parse"
	"cueforge/internal/pipeline"
	"cueforge/internal/render"
	"cueforge/internal/report"
	"cueforge/internal/timeline"
	"cueforge/internal/validate"
)

// errCrash simulates a mid-run crash for the resume demonstration.
var errCrash = errors.New("demo: simulated crash")

// errCheckpointIO marks checkpoint / local read-write failures (exit 3).
var errCheckpointIO = errors.New("checkpoint/local-io failure")

type pipeOpts struct {
	queueCap  int
	slowMs    int
	frameSize int
	totalMs   int64
	framesDir string
	ckptDir   string
	crashAt   int64 // -1 disables the crash injection
	resume    bool
}

type pipeResult struct {
	summary   report.Summary
	validated int64
	peak      int64
	late      []model.TrackError
}

func parseTrack(ctx context.Context, tf trackFile, q *pipeline.Queue) error {
	f, err := os.Open(tf.Path)
	if err != nil {
		return fmt.Errorf("%w: %v", errCheckpointIO, err)
	}
	defer f.Close()
	emit := func(ev model.Event) error { return q.Send(ctx, ev) }
	switch tf.Kind {
	case "vtt":
		return parse.ParseVTT(ctx, f, tf.Track, emit)
	case "chapters":
		return parse.ParseChapters(ctx, f, tf.Track, emit)
	case "id3":
		return parse.ParseID3(ctx, f, tf.Track, emit)
	case "emsg":
		return parse.ParseEMSG(ctx, f, tf.Track, emit)
	case "sei":
		return parse.ParseSEI(ctx, f, tf.Track, emit)
	}
	return fmt.Errorf("unknown track kind %q", tf.Kind)
}

// renderStage consumes validated events, batches them into review
// frames, commits checkpoints and optionally simulates a crash.
func renderStage(ctx context.Context, opts pipeOpts, q *pipeline.Queue, res *pipeResult) error {
	var summary report.Summary
	var skip int64
	frameIdx := 0
	if opts.resume {
		st, ok, err := checkpoint.Load(opts.ckptDir)
		if err != nil {
			return fmt.Errorf("%w: %v", errCheckpointIO, err)
		}
		if ok {
			s, err := report.FromHex(st.FrameSums)
			if err != nil {
				return fmt.Errorf("%w: %v", errCheckpointIO, err)
			}
			summary, skip, frameIdx = s, st.Committed, len(st.FrameSums)
		}
	}
	total := clock.Must(opts.totalMs, 1000)
	var batch []model.Event
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		sum, img := render.RenderFrame(batch, frameIdx, total)
		summary.AddFrame(sum)
		if opts.framesDir != "" {
			if err := os.MkdirAll(opts.framesDir, 0o755); err != nil {
				return fmt.Errorf("%w: %v", errCheckpointIO, err)
			}
			p := filepath.Join(opts.framesDir, fmt.Sprintf("frame-%03d.png", frameIdx))
			if err := render.SavePNG(img, p); err != nil {
				return fmt.Errorf("%w: %v", errCheckpointIO, err)
			}
		}
		batch = batch[:0]
		frameIdx++
		return nil
	}
	count := int64(0)
	for {
		ev, ok, err := q.Recv(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		count++
		if count <= skip {
			continue
		}
		batch = append(batch, ev)
		if len(batch) == opts.frameSize {
			if err := flush(); err != nil {
				return err
			}
		}
		if opts.slowMs > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(opts.slowMs) * time.Millisecond):
			}
		}
		if opts.ckptDir != "" && count%model.CommitBatch == 0 {
			err := checkpoint.Save(opts.ckptDir, checkpoint.State{
				Committed: count, FrameSums: summary.Sums()})
			if err != nil {
				return fmt.Errorf("%w: %v", errCheckpointIO, err)
			}
		}
		if opts.crashAt >= 0 && count == opts.crashAt {
			return errCrash
		}
	}
	if err := flush(); err != nil {
		return err
	}
	res.summary = summary
	return nil
}

// runPipeline wires parsers -> merge -> validate -> render with
// bounded queues and runs all stages to completion.
func runPipeline(fx *fixtures, opts pipeOpts) (*pipeResult, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := pipeline.NewBudget(model.MaxBufferedBytes)
	res := &pipeResult{}

	recvs := make([]timeline.RecvFunc, len(fx.Tracks))
	var stages []func() error
	for i, tf := range fx.Tracks {
		q := pipeline.NewQueue(opts.queueCap, budget)
		recvs[i] = q.Recv
		stages = append(stages, func() error {
			err := parseTrack(ctx, tf, q)
			q.Close()
			if err != nil {
				cancel()
			}
			return err
		})
	}
	mergeQ := pipeline.NewQueue(opts.queueCap, budget)
	renderQ := pipeline.NewQueue(opts.queueCap, budget)
	stages = append(stages, func() error {
		err := timeline.Merge(ctx, recvs, mergeQ.Send,
			func(te model.TrackError) { res.late = append(res.late, te) })
		mergeQ.Close()
		if err != nil {
			cancel()
		}
		return err
	})
	checker := validate.NewChecker()
	stages = append(stages, func() error {
		for {
			ev, ok, err := mergeQ.Recv(ctx)
			if err != nil || !ok {
				renderQ.Close()
				if err != nil {
					cancel()
				}
				return err
			}
			if err := checker.Check(ev); err != nil {
				cancel()
				return err
			}
			if err := renderQ.Send(ctx, ev); err != nil {
				cancel()
				return err
			}
		}
	})
	stages = append(stages, func() error {
		err := renderStage(ctx, opts, renderQ, res)
		if err != nil {
			cancel()
		}
		return err
	})
	err := pipeline.Run(stages...)
	res.validated = checker.Count()
	res.peak = budget.Peak()
	return res, err
}

// detectCorrupt parses the corrupt fixture and returns the byte
// offset at which the parser located the damage, or -1 if missed.
func detectCorrupt(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errCheckpointIO, err)
	}
	defer f.Close()
	err = parse.ParseEMSG(context.Background(), f, 0, func(model.Event) error { return nil })
	var te *model.TrackError
	if errors.As(err, &te) {
		return te.Offset, nil
	}
	if err != nil {
		return 0, err
	}
	return -1, nil
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	gen := fs.Bool("generate", false, "generate sample fixtures first")
	seed := fs.Int64("seed", 1, "fixture random seed")
	events := fs.Int("events", 1024, "total events across tracks")
	queueCap := fs.Int("queue", 4, "per-stage queue capacity")
	slowMs := fs.Int("slow-consumer-ms", 0, "slow consumer delay per event (ms)")
	out := fs.String("out", "/tmp/cueforge-demo", "output directory")
	if err := fs.Parse(args); err != nil {
		return exitFailed
	}
	if *events <= 0 || *events > model.MaxEvents {
		fmt.Fprintf(os.Stderr, "events %d out of range (1..%d)\n", *events, model.MaxEvents)
		return exitFailed
	}
	if *queueCap < 1 || *queueCap > model.MaxTracks {
		fmt.Fprintln(os.Stderr, "queue capacity out of range (1..64)")
		return exitFailed
	}
	var fx *fixtures
	var err error
	if *gen {
		fx, err = generate(*out, *seed, *events)
	} else {
		fx, err = loadFixtures(*out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixtures:", err)
		return exitCheckpoint
	}
	if len(fx.Tracks) > model.MaxTracks {
		fmt.Fprintln(os.Stderr, "too many tracks")
		return exitFailed
	}

	frameSize := *events / 32
	if frameSize < 1 {
		frameSize = 1
	}
	base := pipeOpts{queueCap: *queueCap, slowMs: *slowMs, frameSize: frameSize,
		totalMs: fx.TotalTimeMs, crashAt: -1}

	// 1. Complete run.
	cleanOpts := base
	cleanOpts.framesDir = filepath.Join(*out, "frames-clean")
	clean, err := runPipeline(fx, cleanOpts)
	if err != nil {
		return reportRunError("clean run", err)
	}

	// 2. Corrupt fixture: parser must locate the damage exactly.
	badOff, err := detectCorrupt(fx.BadPath)
	if err != nil {
		return reportRunError("corrupt probe", err)
	}

	// 3. Interrupted run: crash injection after committed state.
	ckptDir := filepath.Join(*out, "resume")
	if err := os.RemoveAll(ckptDir); err != nil {
		fmt.Fprintln(os.Stderr, errCheckpointIO, err)
		return exitCheckpoint
	}
	crashOpts := base
	crashOpts.framesDir = filepath.Join(*out, "frames-resumed")
	crashOpts.ckptDir = ckptDir
	crashAt := int64(*events/2 + 17)
	crashOpts.crashAt = crashAt
	if _, err := runPipeline(fx, crashOpts); !errors.Is(err, errCrash) {
		fmt.Fprintln(os.Stderr, "expected simulated crash, got:", err)
		return exitFailed
	}
	st, ok, err := checkpoint.Load(ckptDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCheckpoint
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "no checkpoint state after crash")
		return exitFailed
	}
	replayed := crashAt - st.Committed

	// 4. Resumed run: continue from the last commit boundary.
	resumeOpts := crashOpts
	resumeOpts.crashAt = -1
	resumeOpts.resume = true
	t0 := time.Now()
	resumed, err := runPipeline(fx, resumeOpts)
	if err != nil {
		return reportRunError("resume run", err)
	}
	resumeMs := time.Since(t0).Milliseconds()

	cleanSHA := clean.summary.SHA256()
	resumedSHA := resumed.summary.SHA256()
	delta := badOff - fx.BadOffset
	fmt.Printf("clean_events=%d\n", clean.validated)
	fmt.Printf("frames=%d\n", clean.summary.Frames())
	fmt.Printf("max_buffered_bytes=%d\n", clean.peak)
	fmt.Printf("bad_expected_offset=%d\n", fx.BadOffset)
	fmt.Printf("bad_offset=%d\n", badOff)
	fmt.Printf("offset_delta=%d\n", delta)
	fmt.Printf("resume_ms=%d\n", resumeMs)
	fmt.Printf("replayed_records=%d\n", replayed)
	fmt.Printf("clean_sha256=%s\n", cleanSHA)
	fmt.Printf("resumed_sha256=%s\n", resumedSHA)

	verified := clean.validated == int64(fx.TotalEvents) &&
		delta == 0 && badOff == fx.BadOffset &&
		cleanSHA == resumedSHA &&
		clean.peak <= model.MaxBufferedBytes &&
		replayed <= model.CommitBatch &&
		len(clean.late) == 0 && len(resumed.late) == 0
	if !verified {
		fmt.Fprintln(os.Stderr, "demo verification failed")
		return exitFailed
	}
	return exitOK
}

// reportRunError maps a run failure to the demo exit code.
func reportRunError(what string, err error) int {
	var te *model.TrackError
	if errors.As(err, &te) {
		fmt.Fprintln(os.Stderr, what+": corrupt input:", te)
		return exitCorrupt
	}
	if errors.Is(err, errCheckpointIO) {
		fmt.Fprintln(os.Stderr, what+":", err)
		return exitCheckpoint
	}
	fmt.Fprintln(os.Stderr, what+":", err)
	return exitFailed
}
