// Package stream consumes a scanned index and drives the processing pipeline:
// samples are read in bounded chunks (memory never scales with media
// duration), audio is normalised and RTP packets are generated and recovered.
// Unreadable spans are isolated to their own time interval and never cause
// later timestamps to shift.
package stream

import (
	"context"
	"fmt"
	"os"
	"time"

	"mediaroute/internal/audio"
	"mediaroute/internal/clock"
	merr "mediaroute/internal/errors"
	"mediaroute/internal/index"
	"mediaroute/internal/rtp"
)

// Limits.
const (
	// ChunkSamples is the maximum number of samples fetched per batch read.
	ChunkSamples = 32
	// MaxChunkBytes bounds the bytes held per chunk.
	MaxChunkBytes = 1 << 20
	// AudioChunkFrames is the number of stereo frames processed per streaming
	// block, keeping the working set constant regardless of duration.
	AudioChunkFrames = 4800 // 100 ms at 48 kHz
	// LoudnessAnalysisFrames is the longest analysis window fed to the sliding
	// loudness normaliser. Audio is accumulated to this size so gain decisions
	// are made on a meaningful window; a final short remainder is still
	// normalised so no audio is dropped.
	LoudnessAnalysisFrames = audio.TargetRate * 2 // 2 s
)

// Options configures a run.
type Options struct {
	// BaseDir resolves relative manifest sources.
	BaseDir string
	// ReorderWindow is the RTP reorder buffer size.
	ReorderWindow int
	// LoudnessWindowMS is the sliding loudness window in milliseconds.
	LoudnessWindowMS int
	// GenerateRTP turns on synthetic RTP packetisation and recovery.
	GenerateRTP bool
	// LossBurst drops this many consecutive RTP packets once per track to
	// model a link outage. Zero disables synthetic loss.
	LossBurst int
}

// Event kinds emitted to the metrics sink.
type EventKind string

const (
	// EventSample is one read-and-presented sample.
	EventSample EventKind = "sample"
	// EventCorrupt marks an isolated unreadable span.
	EventCorrupt EventKind = "corrupt"
	// EventAudio is one normalised audio block.
	EventAudio EventKind = "audio"
	// EventRTP is one recovered RTP packet.
	EventRTP EventKind = "rtp"
	// EventSync is one cross-track synchronisation observation.
	EventSync EventKind = "sync"
)

// Event is a single observation consumed by the metrics reporter.
type Event struct {
	Kind    EventKind
	TrackID uint32
	Sample  int64
	// Source is the absolute timeline position of the sample.
	Source clock.Rational
	// EditMapped is the source time after edit-list mapping.
	EditMapped clock.Rational
	// Output is the processed output time.
	Output clock.Rational
	// Duration is the validity span of the event.
	Duration clock.Rational
	// Latency is wall-clock processing latency for this event.
	Latency time.Duration
	// Bytes is payload size in bytes.
	Bytes int
	// Audio reports the loudness outcome when Kind is EventAudio.
	Audio *audio.LoudnessResult
	// Reason carries a human-readable cause for corrupt events.
	Reason string
}

// Result summarises a streaming run.
type Result struct {
	// Processed counts samples read successfully.
	Processed int
	// Isolated counts unreadable samples.
	Isolated int
	// AudioBlocks counts normalised audio blocks.
	AudioBlocks int
	// AudioFrames counts normalised stereo frames.
	AudioFrames int64
	// RTPStats is the recovery summary, when RTP was generated.
	RTPStats rtp.Stats
	// PeakMemory is the largest single allocation for sample buffers, in bytes.
	PeakMemory int
	// Elapsed is wall-clock run time (excluding fixture generation).
	Elapsed time.Duration
	// Layout records the audio layout decision.
	Layout audio.LayoutDecision
	// Warnings collects non-fatal observations.
	Warnings []string
}

// Sink receives events. Implementations must not block for long.
type Sink func(Event)

// Run processes the index, sending events to sink. It returns a recoverable
// model error when isolated corruption was present.
func Run(ctx context.Context, ix *index.Index, opts Options) (*Result, error) {
	if ix == nil {
		return nil, merr.New(merr.KindConfig, 0, -1, -1, false, "nil index")
	}
	start := time.Now()
	res := &Result{}
	var firstErr error
	for _, track := range ix.Tracks {
		if err := ctx.Err(); err != nil {
			return res, canceled(err)
		}
		if err := processTrack(ctx, track, ix.CorruptRanges, opts, res, sinkFromContext(ctx)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	res.Elapsed = time.Since(start)
	if firstErr == nil && res.Isolated > 0 {
		firstErr = merr.New(merr.KindTruncated, 0, -1, -1, true, "%d samples isolated as corrupt", res.Isolated)
	}
	return res, firstErr
}

// WithSink attaches an event sink to the context.
func WithSink(ctx context.Context, s Sink) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, sinkKey{}, s)
}

type sinkKey struct{}

func sinkFromContext(ctx context.Context) Sink {
	if v, ok := ctx.Value(sinkKey{}).(Sink); ok {
		return v
	}
	return func(Event) {}
}

func canceled(err error) error {
	return merr.New(merr.KindCanceled, 0, -1, -1, false, "stream canceled: %v", err)
}

// processTrack streams one track's samples in bounded chunks.
func processTrack(ctx context.Context, tr index.Track, ranges []index.CorruptRange, opts Options, res *Result, sink Sink) error {
	tb, err := clock.NewTimebase(tr.Timescale, tr.Origin.Num*int64(tr.Timescale)/tr.Origin.Den)
	if err != nil {
		return merr.New(merr.KindTimeline, tr.ID, -1, -1, false, "timebase: %v", err)
	}
	path := tr.Source
	if opts.BaseDir != "" {
		path = joinBase(opts.BaseDir, tr.Source)
	}
	f, err := os.Open(path)
	if err != nil {
		return merr.New(merr.KindScan, tr.ID, -1, -1, true, "open %s: %v", tr.Source, err)
	}
	defer f.Close()

	corruptBySample := map[int64]index.CorruptRange{}
	firstCorrupt := int64(-1)
	for _, r := range ranges {
		if r.TrackID != tr.ID {
			continue
		}
		if firstCorrupt < 0 || r.FirstSample < firstCorrupt {
			firstCorrupt = r.FirstSample
		}
		for n := r.FirstSample; n <= r.LastSample; n++ {
			corruptBySample[n] = r
		}
	}
	if firstCorrupt >= 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("track %d: corrupt media isolated from sample %d; later timestamps unchanged", tr.ID, firstCorrupt))
	}

	var recoverer *rtp.Recoverer
	var seq uint16
	burstDropped := false
	if opts.GenerateRTP && tr.Kind == "audio" {
		recoverer = rtp.New(opts.ReorderWindow)
	}

	var pendingAudioFrames []float32
	analysisFrames := LoudnessAnalysisFrames
	for i := 0; i < len(tr.Samples); i += ChunkSamples {
		if err := ctx.Err(); err != nil {
			return canceled(err)
		}
		end := i + ChunkSamples
		if end > len(tr.Samples) {
			end = len(tr.Samples)
		}
		chunk, err := readChunk(ctx, f, tr.Samples[i:end])
		if err != nil {
			return err
		}
		if chunk.peak > res.PeakMemory {
			res.PeakMemory = chunk.peak
		}
		batchEnd := end
		if chunk.budgetStop {
			// Stop before the sample that did not fit so it is retried as the
			// head of the next batch. The last successfully read local index
			// marks the consumed prefix.
			last := -1
			for idx := range chunk.payloads {
				if idx > last {
					last = idx
				}
			}
			for idx := range chunk.readErr {
				if idx > last {
					last = idx
				}
			}
			if last < 0 {
				return merr.New(merr.KindInternal, tr.ID, -1, -1, false,
					"sample of %d bytes exceeds chunk budget %d", tr.Samples[i].Size, MaxChunkBytes)
			}
			batchEnd = i + last + 1
		}
		for j, s := range tr.Samples[i:batchEnd] {
			t0 := time.Now()
			src, terr := tb.At(s.DTS - tr.EditMediaTicks)
			if terr != nil {
				return merr.New(merr.KindTimeline, tr.ID, s.Number, s.Offset, false, "timeline: %v", terr)
			}
			editMapped := src
			output, oerr := tb.At(s.CTS - tr.EditMediaTicks)
			if oerr != nil {
				return merr.New(merr.KindTimeline, tr.ID, s.Number, s.Offset, false, "output time: %v", oerr)
			}
			dur, derr := clock.FromTicks(s.Duration, tr.Timescale)
			if derr != nil {
				return merr.New(merr.KindTimeline, tr.ID, s.Number, s.Offset, false, "duration: %v", derr)
			}
			if !s.Readable {
				res.Isolated++
				r := corruptBySample[s.Number]
				sink(Event{
					Kind: EventCorrupt, TrackID: tr.ID, Sample: s.Number,
					Source: src, EditMapped: editMapped, Output: output, Duration: dur,
					Reason: r.Reason,
				})
				continue
			}
			if rerr, failed := chunk.readErr[j]; failed {
				// A sample that was readable at scan time but failed now is
				// isolated in place; later timestamps are untouched.
				res.Isolated++
				sink(Event{
					Kind: EventCorrupt, TrackID: tr.ID, Sample: s.Number,
					Source: src, EditMapped: editMapped, Output: output, Duration: dur,
					Reason: "read failed at stream time: " + rerr.Error(),
				})
				continue
			}
			res.Processed++
			sink(Event{
				Kind: EventSample, TrackID: tr.ID, Sample: s.Number,
				Source: src, EditMapped: editMapped, Output: output, Duration: dur,
				Latency: time.Since(t0), Bytes: int(s.Size),
			})
			if tr.Kind == "audio" {
				payload := chunk.payloads[j]
				if len(payload) > 0 {
					frames := pcmFromBytes(payload)
					pendingAudioFrames = append(pendingAudioFrames, frames...)
					for len(pendingAudioFrames) >= analysisFrames*audio.TargetChannels {
						block := pendingAudioFrames[:analysisFrames*audio.TargetChannels]
						if err := processAudio(ctx, tr, block, output, opts, res, sink, &seq, &recoverer, &burstDropped); err != nil {
							return err
						}
						pendingAudioFrames = append([]float32(nil), pendingAudioFrames[analysisFrames*audio.TargetChannels:]...)
					}
				}
			}
		}
		// The loop's post-statement advances by ChunkSamples; rewind so a
		// budget stop resumes exactly at the first unconsumed sample.
		if batchEnd < end {
			i = batchEnd - ChunkSamples
			continue
		}
	}
	if len(pendingAudioFrames) > 0 {
		if err := processAudio(ctx, tr, pendingAudioFrames, clock.Rational{Num: 0, Den: 1}, opts, res, sink, &seq, &recoverer, &burstDropped); err != nil {
			return err
		}
	}
	if recoverer != nil {
		recoverer.Flush()
		res.RTPStats = recoverer.Stats()
	}
	sink(Event{Kind: EventSync, TrackID: tr.ID, Source: tr.Duration})
	_ = tb
	return nil
}

func processAudio(ctx context.Context, tr index.Track, block []float32, at clock.Rational, opts Options, res *Result, sink Sink, seq *uint16, rec **rtp.Recoverer, dropped *bool) error {
	if err := ctx.Err(); err != nil {
		return canceled(err)
	}
	t0 := time.Now()
	cfg := audio.Config{SampleRate: tr.Entry.SampleRate, Channels: tr.Entry.Channels, Layout: tr.Entry.Layout}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = audio.TargetRate
	}
	if cfg.Channels == 0 {
		cfg.Channels = audio.TargetChannels
	}
	converted, decision, err := audio.Convert(cfg, block)
	if err != nil {
		return err
	}
	res.Layout = decision
	windowMS := opts.LoudnessWindowMS
	if windowMS <= 0 {
		windowMS = 400
	}
	norm := audio.Normalize(converted, windowMS)
	res.AudioBlocks++
	res.AudioFrames += int64(len(norm.Samples) / audio.TargetChannels)
	sink(Event{
		Kind: EventAudio, TrackID: tr.ID, Source: at, Duration: clock.Rational{Num: int64(len(norm.Samples) / audio.TargetChannels), Den: audio.TargetRate},
		Audio: &norm, Latency: time.Since(t0), Bytes: len(norm.Samples) * 4,
	})
	if *rec == nil {
		return nil
	}
	// Packetise the normalised block into 20 ms RTP packets and feed the
	// recoverer with a deterministic shuffle to model network reordering.
	framesPerPacket := audio.TargetRate / 50 // 20 ms
	packets := packetize(norm.Samples, framesPerPacket, seq)
	if err := simulateNetwork(ctx, packets, rec, res, sink, at, opts.LossBurst, dropped); err != nil {
		return err
	}
	return nil
}

// joinBase joins the base directory with a possibly-absolute source.
func joinBase(base, src string) string {
	if len(src) > 0 && src[0] == '/' {
		return src
	}
	return fmt.Sprintf("%s/%s", base, src)
}
