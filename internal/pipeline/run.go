package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"mediad/internal/checkpoint"
	"mediad/internal/codec"
	"mediad/internal/cut"
	"mediad/internal/dedup"
	"mediad/internal/silence"
	"mediad/internal/timeline"
)

// Run executes one codec stream. A simulated interruption returns
// ErrInterrupted after a durable segment record; callers may Run again with
// Resume=true.
func Run(opts Options) (*Result, error) {
	if opts.Media == nil {
		return nil, timeline.New(timeline.KindArgument, "pipeline.options", -1, -1, "Media is required")
	}
	if !opts.Codec.Valid() {
		return nil, timeline.New(timeline.KindArgument, "pipeline.options", -1, -1, "invalid codec %q", opts.Codec)
	}
	stream := opts.Media.Streams[opts.Codec]
	if stream == nil {
		return nil, timeline.New(timeline.KindArgument, "pipeline.options", -1, -1, "missing stream %s", opts.Codec)
	}
	if opts.OutDir == "" {
		return nil, timeline.New(timeline.KindArgument, "pipeline.options", -1, -1, "OutDir is required")
	}
	if opts.QueueCapacity <= 0 {
		opts.QueueCapacity = 8
	}

	c := opts.Codec
	journalName := fmt.Sprintf("journal-%s.json", c)
	manifestName := fmt.Sprintf("manifest-%s.json", c)
	initName := fmt.Sprintf("init-%s.mp4", c)
	journalPath, err := checkpoint.SafePath(opts.OutDir, journalName)
	if err != nil {
		return nil, err
	}
	manifestPath, err := checkpoint.SafePath(opts.OutDir, manifestName)
	if err != nil {
		return nil, err
	}

	var journal *checkpoint.Journal
	if opts.Resume {
		loaded, exists, err := checkpoint.Load(journalPath)
		if err != nil {
			return nil, err
		}
		if exists {
			if err := loaded.VerifyFiles(opts.OutDir); err != nil {
				return nil, err
			}
			journal = loaded
		}
	}
	if journal == nil {
		journal = checkpoint.NewJournal(string(c), opts.Media.Seed, opts.Media.TargetBPS)
	}

	initPath, err := checkpoint.SafePath(opts.OutDir, initName)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(initPath); errors.Is(err, os.ErrNotExist) {
		initBytes, err := buildInit()
		if err != nil {
			return nil, err
		}
		if err := checkpoint.AtomicWriteFile(initPath, initBytes, 0o644); err != nil {
			return nil, timeline.Wrap(timeline.KindStorage, "pipeline.write_init", -1, -1, err)
		}
	} else if err != nil {
		return nil, timeline.Wrap(timeline.KindStorage, "pipeline.stat_init", -1, -1, err)
	}

	spans, err := timeline.Build(stream.Indexes, opts.Media.DriftPPM)
	if err != nil {
		return nil, err
	}
	rn := &runner{
		opts: opts, stream: stream, spans: spans,
		reports: codec.ValidateStream(c, stream.Blob, stream.Indexes),
		journal: journal, journalPath: journalPath,
		manifestPath: manifestPath, initName: initName, manifestName: manifestName,
		cutter: cut.New(cut.Config{}), store: dedup.NewStore(),
		contents: make(map[string]dedup.Content), flow: newRTPFlow(),
		firstErrorByte: -1,
	}

	if err := rn.processQueue(); err != nil {
		res := rn.result(nil, "")
		res.Interrupted = errors.Is(err, ErrInterrupted)
		return res, err
	}
	last, err := rn.cutter.Flush()
	if err != nil {
		return rn.result(nil, ""), err
	}
	for _, seg := range last {
		if err := rn.commitSegment(seg, len(stream.Indexes)); err != nil {
			res := rn.result(nil, "")
			res.Interrupted = errors.Is(err, ErrInterrupted)
			return res, err
		}
	}

	sil := silence.DetectS16LE(opts.Media.PCM, opts.Media.Gaps)
	manifest := rn.buildManifest(sil)
	hash, err := manifest.Save(manifestPath)
	if err != nil {
		return nil, timeline.Wrap(timeline.KindStorage, "pipeline.write_manifest", -1, -1, err)
	}
	rn.journal.ManifestFile = manifestName
	rn.journal.ManifestSHA256 = hash
	rn.journal.Completed = true
	if err := rn.journal.SaveAtomic(journalPath); err != nil {
		return nil, err
	}
	res := rn.result(manifest, hash)
	res.Silence = sil
	res.ActualBPS = manifest.ActualBPS
	res.DeltaPct = manifest.DeltaPct
	return res, nil
}

func (rn *runner) processQueue() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := make(chan int, rn.opts.QueueCapacity)
	var wg sync.WaitGroup
	var producerPauses int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(jobs)
		for i := range rn.stream.Indexes {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			default:
				atomic.AddInt64(&producerPauses, 1)
				select {
				case jobs <- i: // bounded queue full: upstream read pauses here
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	var processErr error
	for i := range jobs {
		if err := rn.processAU(i); err != nil {
			processErr = err
			cancel()
			break
		}
	}
	wg.Wait()
	rn.pausedReads = atomic.LoadInt64(&producerPauses)
	return processErr
}
