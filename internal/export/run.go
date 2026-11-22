package export

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"medpipe/internal/fault"
	"medpipe/internal/merge"
	rtpstream "medpipe/internal/rtp"
)

// RunParams carries every knob the demo/verify entry points accept.
type RunParams struct {
	Seed           int64
	Tracks         int
	Packets        int // total RTP packets across all tracks
	Loss           float64
	ReorderWindow  int
	Workers        int
	Queue          int
	CheckpointPath string
	OutDir         string
	Resume         bool
	MaxOutBytes    int64 // 0 selects the default hard limit
	// ClockJumpTrack injects an RTCP clock jump on one track (-1
	// disables); exercises the drift-isolation path end to end.
	ClockJumpTrack int
}

// digest hashes the parameters that affect output content. Worker and
// queue sizes only affect scheduling, so they are excluded.
func (p RunParams) digest() string {
	canonical := struct {
		Seed    int64   `json:"seed"`
		Tracks  int     `json:"tracks"`
		Packets int     `json:"packets"`
		Loss    float64 `json:"loss"`
		Window  int     `json:"reorder_window"`
		Jump    int     `json:"clock_jump_track"`
	}{p.Seed, p.Tracks, p.Packets, p.Loss, p.ReorderWindow, p.ClockJumpTrack}
	b, _ := json.Marshal(canonical)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p RunParams) genOptions() (rtpstream.GenOptions, error) {
	if p.Tracks < 1 {
		return rtpstream.GenOptions{}, fault.New(fault.KindResource,
			fault.Unknown, fault.Unknown, 0, false, "track count must be positive")
	}
	if p.Packets < 1 {
		return rtpstream.GenOptions{}, fault.New(fault.KindResource,
			fault.Unknown, fault.Unknown, 0, false,
			"empty input: packet count must be positive")
	}
	if p.Packets%p.Tracks != 0 {
		return rtpstream.GenOptions{}, fault.New(fault.KindResource,
			fault.Unknown, fault.Unknown, 0, false,
			"packets must divide evenly across tracks")
	}
	return rtpstream.GenOptions{
		Seed:             p.Seed,
		Tracks:           p.Tracks,
		PacketsPerTrack:  p.Packets / p.Tracks,
		PayloadSize:      160,
		LossRate:         p.Loss,
		DupRate:          0.01,
		CorruptRate:      0.005,
		ReorderWindow:    p.ReorderWindow,
		ClockJumpTrack:   p.ClockJumpTrack,
		ClockJumpSeconds: 2,
		TailLoss:         4,
		TruncateTail:     3,
	}, nil
}

// decode is the pool's unit of work: hash the payload and build an
// event, or pass a sender report through untouched.
func decode(_ context.Context, j merge.Job) merge.Result {
	rec := j.Rec
	if rec.Type == rtpstream.RecRTCP {
		return merge.Result{Index: j.Index,
			SR: &merge.SRUpdate{Track: rec.Track, SR: rec.SR, Offset: rec.Offset}}
	}
	h := sha256.Sum256(rec.Packet.Payload)
	return merge.Result{Index: j.Index, Ev: &merge.Event{
		Track:        rec.Track,
		Seq:          rec.Packet.SequenceNumber,
		RTPTimestamp: rec.Packet.Timestamp,
		Payload:      rec.Packet.Payload,
		Hash:         h,
		Offset:       rec.Offset,
	}}
}

// Run executes the full pipeline (or resumes a checkpointed one) and
// returns the run metrics.
func Run(ctx context.Context, params RunParams) (*Metrics, error) {
	genOpts, err := params.genOptions()
	if err != nil {
		return nil, err
	}
	return RunGen(ctx, params, genOpts)
}

// RunGen is Run with an explicit sample-stream configuration; tests use
// it to inject faults the demo parameters do not expose.
func RunGen(ctx context.Context, params RunParams, genOpts rtpstream.GenOptions) (*Metrics, error) {
	start := time.Now()
	if err := CheckSafePath(params.OutDir); err != nil {
		return nil, err
	}
	if err := CheckSafePath(params.CheckpointPath); err != nil {
		return nil, err
	}
	if params.Queue == 0 {
		params.Queue = 256
	}
	if params.MaxOutBytes == 0 {
		params.MaxOutBytes = MaxOutputBytes
	}
	gen, err := rtpstream.Generate(genOpts)
	if err != nil {
		return nil, err
	}
	inputSum := sha256.Sum256(gen.Stream)
	inputSHA := hex.EncodeToString(inputSum[:])

	if params.Resume {
		if m, reuse, done := tryResume(params, inputSHA, start); done {
			return m, nil
		} else if len(reuse) > 0 {
			return execute(ctx, params, gen.Stream, inputSHA, start, reuse)
		}
	}
	return execute(ctx, params, gen.Stream, inputSHA, start, nil)
}

// tryResume inspects the checkpoint. It returns done=true when a
// complete, fully verified checkpoint makes any further work
// unnecessary, or a set of verified segments for a partial resume.
func tryResume(params RunParams, inputSHA string, start time.Time) (m *Metrics, reuse map[int]Segment, done bool) {
	cp, err := LoadCheckpoint(params.CheckpointPath)
	if err != nil || cp == nil {
		return nil, nil, false
	}
	if cp.InputSHA256 != inputSHA || cp.ParamsSHA256 != params.digest() {
		return nil, nil, false // stale checkpoint: full run
	}
	if cp.Complete {
		for rel, want := range cp.Outputs {
			sum, err := hashFile(joinPath(params.OutDir, rel))
			if err != nil || sum != want {
				return nil, nil, false // tampered output: full run
			}
		}
		mm := cp.Metrics
		mm.ResumeSeconds = time.Since(start).Seconds()
		return &mm, nil, true
	}
	// Partial resume: keep only segments whose files verify.
	reuse = map[int]Segment{}
	for _, seg := range cp.Segments {
		bin := joinPath(params.OutDir, segBinRel(seg.Index))
		sum, err := hashFile(bin)
		if err != nil || sum != seg.SHA256 {
			break // fall back to the last valid boundary
		}
		jsum, err := hashFile(joinPath(params.OutDir, segJSONLRel(seg.Index)))
		if err != nil || jsum != seg.JSHA256 {
			break
		}
		reuse[seg.Index] = seg
	}
	return nil, reuse, false
}

// execute runs the streaming pipeline over the generated stream.
func execute(ctx context.Context, params RunParams, stream []byte, inputSHA string, start time.Time, reuse map[int]Segment) (*Metrics, error) {
	if len(reuse) == 0 {
		cleanArtifacts(params.OutDir)
	}
	fl := &fault.List{Cap: 4096}
	merger := merge.NewMerger(params.ReorderWindow, 0.5, fl)
	// The child context lets a failing consumer stop the producer and
	// the workers: every blocked select in the pipeline unblocks on it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool, err := merge.NewPool(ctx, params.Workers, params.Queue, decode)
	if err != nil {
		return nil, err
	}
	sw := newSegmentWriter(params, inputSHA)
	sw.reuse = reuse

	// Consumer: restore stream order, feed the merger, emit frames.
	consumerDone := make(chan error, 1)
	go func() {
		consumerDone <- consume(ctx, pool, merger, sw, fl)
		cancel() // unblock the producer and workers on consumer failure
	}()

	// Producer: parse the stream and submit records in order.
	parser := rtpstream.NewParser(bytes.NewReader(stream), fl)
	var idx uint64
	var prodErr error
	for {
		rec, perr := parser.Next()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			prodErr = perr
			break
		}
		if err := pool.Submit(ctx, merge.Job{Index: idx, Rec: rec}); err != nil {
			prodErr = err
			break
		}
		idx++
		if merr := merger.Fatal(); merr != nil {
			prodErr = merr
			break
		}
	}
	pool.Close()
	consErr := <-consumerDone
	// A consumer failure is the root cause when both exist: the
	// producer's error is then just the cancelled context.
	if consErr != nil && !errors.Is(consErr, context.Canceled) {
		return nil, consErr
	}
	if prodErr != nil {
		return nil, prodErr
	}
	if consErr != nil {
		return nil, consErr
	}
	if err := merger.Fatal(); err != nil {
		return nil, err
	}
	if fl.HasFatal() {
		return nil, fl.Errs[0]
	}
	m, err := sw.finalize(params, merger, parser, inputSHA)
	if err != nil {
		return nil, err
	}
	m.ResumeSeconds = time.Since(start).Seconds()
	return m, nil
}

// consume applies pool results to the merger in stream order.
func consume(ctx context.Context, pool *merge.Pool, merger *merge.Merger, sw *segmentWriter, fl *fault.List) error {
	next := uint64(0)
	pending := map[uint64]merge.Result{}
	apply := func(res merge.Result) error {
		switch {
		case res.Err != nil:
			fl.Add(res.Err)
		case res.SR != nil:
			merger.AddSenderReport(res.SR.Track, res.SR.SR, res.SR.Offset)
		default:
			merger.AddEvent(res.Ev)
		}
		for {
			ev := merger.Pop()
			if ev == nil {
				break
			}
			if err := sw.add(ev); err != nil {
				return err
			}
		}
		return nil
	}
	for res := range pool.Results() {
		pending[res.Index] = res
		for {
			r, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if err := apply(r); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	merger.Flush()
	for {
		ev := merger.Pop()
		if ev == nil {
			break
		}
		if err := sw.add(ev); err != nil {
			return err
		}
	}
	return sw.close()
}
