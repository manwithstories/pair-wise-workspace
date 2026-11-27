// Package demo wires the generated fixtures through the full pipeline and
// produces the terminal acceptance report. It owns no audio maths of its own:
// every stage is the same package used by the production path.
package demo

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"avpipe/internal/audit"
	"avpipe/internal/layout"
	"avpipe/internal/loudness"
	"avpipe/internal/media"
	"avpipe/internal/resample"
	"avpipe/internal/stream"
	"avpipe/internal/timeline"
	"avpipe/internal/wav"
)

// pipeline adapts a WAV reader to the runner's Source interface.
type wavSource struct {
	reader     *wav.Reader
	blockFrame int
}

func (s *wavSource) Read(strict bool) (stream.SourceItem, error) {
	chunk, err := s.reader.ReadBlock("program", s.blockFrame, strict)
	if err != nil {
		return stream.SourceItem{}, err
	}
	return stream.SourceItem{
		Block:        chunk.Block,
		Partial:      chunk.Partial,
		DroppedBytes: chunk.DroppedBytes,
		RawLen:       chunk.RawLen,
	}, nil
}

// chain runs the resampler, layout matrix and loudness controller in order.
// Flushing propagates through every stage so the output tail is complete.
type chain struct {
	resampler *resample.Resampler
	matrix    *layout.Matrix
	loudness  *loudness.Controller
	track     string
}

func (c *chain) Process(in media.Block) ([]media.Block, error) {
	resampled, err := c.resampler.Process(in)
	if err != nil {
		return nil, err
	}
	return c.transform(resampled)
}

func (c *chain) Flush() ([]media.Block, error) {
	tail, err := c.resampler.Flush()
	if err != nil {
		return nil, err
	}
	return c.transform(tail)
}

func (c *chain) transform(blocks []media.Block) ([]media.Block, error) {
	out := make([]media.Block, 0, len(blocks))
	for _, b := range blocks {
		mapped, err := c.matrix.Apply(b)
		if err != nil {
			return nil, err
		}
		leveled, err := c.loudness.Update(mapped)
		if err != nil {
			return nil, err
		}
		out = append(out, leveled)
	}
	return out, nil
}

// renderSink writes the final PCM16 WAV and feeds the audit index. It is the
// consumer side of the bounded queue.
type renderSink struct {
	file   *os.File
	index  *audit.Index
	rec    *wav.Recorder
	sumSq  float64
	frames int64
	peak   float64
}

// newRenderSink creates the temporary output file next to the final path.
func newRenderSink(path string, rate media.Rate, channels int) (*renderSink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "create", err)
	}
	idx, err := audit.New(rate, channels)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &renderSink{file: f, index: idx, rec: wav.NewRecorder(f, rate, channels)}, nil
}

func (s *renderSink) Consume(b media.Block) error {
	if err := s.rec.Write(b); err != nil {
		return err
	}
	if err := s.index.Consume(b); err != nil {
		return err
	}
	for _, v := range b.Samples {
		s.sumSq += v * v
		if a := math.Abs(v); a > s.peak {
			s.peak = a
		}
	}
	s.frames += int64(b.Frames())
	return nil
}

// loudnessDBFS reports the rendered program's mean-square level.
func (s *renderSink) loudnessDBFS(channels int) float64 {
	if s.frames == 0 || s.sumSq <= 0 {
		return math.Inf(-1)
	}
	ms := s.sumSq / float64(s.frames*int64(channels))
	return 10 * math.Log10(ms)
}

func (s *renderSink) peakDBFS() float64 {
	if s.peak <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(s.peak)
}

func (s *renderSink) Close() error {
	if s.rec != nil {
		if err := s.rec.Close(); err != nil {
			s.file.Close()
			return err
		}
		s.rec = nil
	}
	s.index.Finalize()
	return s.file.Close()
}

// digestOf hashes a file's bytes, used for the atomic-repeat comparison.
func digestOf(path string) ([32]byte, error) {
	var zero [32]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return zero, err
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// atomicInstall moves the temporary render into place only after it is whole.
func atomicInstall(tmp, final string) error {
	if err := os.Rename(tmp, final); err != nil {
		return media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "rename", err)
	}
	return nil
}

// runOnce executes one full pipeline pass against a fresh timeline.
func runOnce(ctx context.Context, fx Fixtures, opts Options, tl *timeline.Timeline, outPath string) (runStats, error) {
	limits := wav.DefaultLimits()
	reader, err := wav.Open(fx.Normal, limits)
	if err != nil {
		return runStats{}, err
	}
	defer reader.Close()

	outRate := media.Rate{Num: 48000, Den: 1}
	rs, err := resample.New(resample.Config{Src: reader.Info().Rate, Dst: outRate, MaxBlockFrames: 1024})
	if err != nil {
		return runStats{}, err
	}
	mx, err := layout.BuildMatrix(fx.Labels, layout.Stereo)
	if err != nil {
		return runStats{}, err
	}
	lc, err := loudness.New(loudness.Config{Rate: outRate, Channels: len(layout.Stereo.Labels)})
	if err != nil {
		return runStats{}, err
	}
	proc := &chain{resampler: rs, matrix: mx, loudness: lc}

	sink, err := newRenderSink(outPath, outRate, len(layout.Stereo.Labels))
	if err != nil {
		return runStats{}, err
	}
	for _, a := range fx.Anchors {
		clock, _ := media.NewClock(outRate)
		sink.index.AddKeyframe(clock.Frames(a.At), a.Label)
	}
	slow := stream.StallSink{
		Inner: sink,
		Delay: time.Duration(opts.ConsumerStallMS) * time.Millisecond,
	}
	runner, err := stream.NewRunner(stream.RunnerOptions{
		Capacity:     opts.Queue,
		MaxGapFrames: 48000,
		MaxBlock:     limits.MaxBlockFrames,
		Track:        "program",
		Strict:       true,
		Timeline:     tl,
	})
	if err != nil {
		sink.Close()
		return runStats{}, err
	}
	result, runErr := runner.Run(ctx, &wavSource{reader: reader, blockFrame: 1024}, proc, slow)
	closeErr := sink.Close()
	if runErr != nil {
		return runStats{}, runErr
	}
	if closeErr != nil {
		return runStats{}, closeErr
	}
	stats := runStats{
		result:       result,
		summary:      sink.index.Summarize(),
		digest:       sink.index.Digest(),
		segs:         sink.index.Segments(),
		loudnessDBFS: sink.loudnessDBFS(len(layout.Stereo.Labels)),
		peakDBFS:     sink.peakDBFS(),
		gainDB:       lc.Stats().GainDB,
	}
	return stats, nil
}

// runStats captures one pass for the report.
type runStats struct {
	result       stream.Result
	summary      audit.Summary
	digest       string
	segs         []audit.Segment
	loudnessDBFS float64
	peakDBFS     float64
	gainDB       float64
}

// segmentHashes renders the per-segment 64-bit hashes.
func (s runStats) segmentHashes() []uint64 {
	out := make([]uint64, len(s.segs))
	for i, seg := range s.segs {
		out[i] = audit.SegmentSHA(seg)
	}
	return out
}

// strictReject confirms that a truncated input fails in strict mode.
func strictReject(fx Fixtures) (bool, error) {
	reader, err := wav.Open(fx.Truncated, wav.DefaultLimits())
	if err != nil {
		return false, err
	}
	defer reader.Close()
	for i := 0; i < 100000; i++ {
		_, err := reader.ReadBlock("program", 1024, true)
		if err == nil {
			continue
		}
		if media.ExitFor(err) == media.ExitInput {
			return true, nil
		}
		return false, err
	}
	return false, fmt.Errorf("truncated input was never rejected")
}
