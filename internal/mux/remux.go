// Package mux rewrites container structure only. Coded sample bytes are
// copied verbatim from input to output; nothing is decoded, re-encoded or
// rescaled. Each track's output sample stream is hashed so the manifest can
// prove the copy is lossless.
package mux

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"mediacore/internal/media"
	"mediacore/internal/pipeline"
)

// Result is the outcome of one remux.
type Result struct {
	OutputPath  string
	OutputSize  int64
	Samples     int
	Tracks      []TrackResult
	Interrupted bool
	Resumed     bool
}

// TrackResult records per-track integrity data.
type TrackResult struct {
	Key          string
	OutputID     uint32
	Samples      int
	InputSHA256  string
	OutputSHA256 string
	Bytes        int64
}

// Match reports whether the copied sample stream is byte-identical to the
// input stream.
func (t TrackResult) Match() bool {
	return t.InputSHA256 != "" && t.InputSHA256 == t.OutputSHA256
}

// InterruptError reports a deliberate stop at a checkpoint boundary.
type InterruptError struct{ After int }

func (e *InterruptError) Error() string {
	return fmt.Sprintf("interrupted after %d samples", e.After)
}

// Options control a remux run.
type Options struct {
	OutputPath     string
	CheckpointPath string
	Resume         *pipeline.ResumePlan
	InterruptAfter int
	Fingerprint    string
	MappingSign    string
	TotalSamples   int
	Chunk          int
	// WaveSize is the number of samples between checkpoints within a track.
	WaveSize int
	// ShouldStop is called after every written sample. It returns true when
	// the run must stop at this checkpoint boundary.
	ShouldStop func(produced int) bool
	// Queue, when set, receives every sample through its bounded buffer
	// before the bytes are copied. The remuxer then reads the bytes from the
	// queued copy, so a slow queue applies backpressure to the whole run.
	Queue *pipeline.Queue
	// QueueLimits is used when Queue is nil and a bounded path is wanted.
	QueueLimits *pipeline.Limits
	// ConsumerDelay simulates a slow consumer, which exercises backpressure.
	ConsumerDelay time.Duration
	// QueuePeak receives the observed peak in-flight bytes.
	QueuePeak *int64
}

// Remuxer copies the samples of ordered tracks into a fresh MP4.
type Remuxer struct {
	Tracks   []*media.Track
	Plan     map[string]uint32
	Resolved map[string]ResolvedMeta
	Source   media.Source
}

// Run performs the remux.
func (r *Remuxer) Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.OutputPath == "" {
		return nil, media.NewError(media.ClassUsage, "no output path supplied")
	}
	if samePath(r.Source.Name(), opts.OutputPath) {
		return nil, media.NewError(media.ClassUsage,
			"output path %s is the input path; inputs are never overwritten", opts.OutputPath)
	}
	if opts.Chunk <= 0 {
		opts.Chunk = 64 << 10
	}

	tracks, start, err := planOutput(r.Tracks, r.Plan, r.Resolved)
	if err != nil {
		return nil, err
	}

	flags := os.O_CREATE | os.O_RDWR | os.O_TRUNC
	if opts.Resume != nil {
		// Resume keeps the verified prefix already on disk; the header is
		// rewritten in place and only unwritten samples are appended.
		flags = os.O_CREATE | os.O_RDWR
	}
	out, err := os.OpenFile(opts.OutputPath, flags, 0o644)
	if err != nil {
		return nil, media.NewError(media.ClassIO, "cannot open output %s", opts.OutputPath).WithCause(err)
	}
	defer out.Close()

	resume := opts.Resume
	next := make(map[string]int, len(r.Tracks))
	hashes := make(map[string]hash.Hash, len(r.Tracks))
	for _, t := range r.Tracks {
		key := t.ID.String()
		hashes[key] = sha256.New()
		if resume != nil {
			next[key] = resume.Written[key]
			if st := resume.HashState[key]; st != "" {
				if err := restoreHash(hashes[key], st); err != nil {
					return nil, media.NewError(media.ClassCheckpointCorrupt,
						"checkpoint hash state for %s is invalid", key).WithCause(err)
				}
			}
		}
	}

	// Build the header against the *planned* placement, which is the final
	// placement for a complete run. A resumed run keeps the same layout, so
	// the identical header is rewritten and the already-verified prefix is
	// left in place.
	if err := writeMP4Header(out, tracks, start); err != nil {
		return nil, err
	}

	res := &Result{OutputPath: opts.OutputPath, Resumed: resume != nil}
	mgr := pipeline.NewManager(opts.CheckpointPath)
	if resume == nil {
		mgr.Init(opts.Fingerprint, opts.MappingSign, opts.OutputPath, opts.TotalSamples)
	} else {
		mgr.Adopt(opts.Fingerprint, opts.MappingSign, opts.OutputPath, opts.TotalSamples, resume)
	}

	if opts.InterruptAfter > 0 && opts.ShouldStop == nil {
		limit := opts.InterruptAfter
		opts.ShouldStop = func(produced int) bool { return produced >= limit }
	}
	waveSize := opts.WaveSize
	if waveSize <= 0 {
		waveSize = 1
	}

	// Output is laid out track-major, so the payload is a concatenation of
	// per-track sample streams and every checkpoint boundary is a contiguous
	// output prefix. Resume therefore continues at the first unwritten
	// sample of the first incomplete track, with no replay.
	producerNext := make(map[string]int, len(tracks))
	for _, t := range tracks {
		from := next[t.key]
		if from > len(t.sizes) {
			return nil, media.NewError(media.ClassCheckpointCorrupt,
				"checkpoint claims %d samples for %s but only %d exist",
				from, t.key, len(t.sizes))
		}
		producerNext[t.key] = from
	}

	pos := start
	if resume != nil {
		pos = resume.Offset
	}
	produced := 0
	wave := 0
	if resume != nil {
		wave = resume.Wave
	}
	waveStart := pos
	buf := make([]byte, opts.Chunk)

	// The producer reads ahead into the bounded queue; the consumer writes,
	// hashes and checkpoints. A slow consumer therefore blocks the producer,
	// and peak queue occupancy is bounded by the configured limits.
	prodCtx, prodCancel := context.WithCancel(ctx)
	defer prodCancel()
	queue := opts.Queue
	if queue == nil {
		limits := pipeline.Limits{MaxSamples: 64, MaxBytes: 64 << 20}
		if opts.QueueLimits != nil {
			limits = *opts.QueueLimits
		}
		queue = pipeline.NewQueue(r.Source, limits)
	}
	prodErr := make(chan error, 1)
	go func() {
		err := r.produce(prodCtx, queue, tracks, producerNext)
		queue.Close()
		prodErr <- err
	}()

	consumerErr := error(nil)
	for {
		err := queue.Next(ctx, func(qs *pipeline.Sample) error {
			if opts.ConsumerDelay > 0 {
				time.Sleep(opts.ConsumerDelay)
			}
			if err := r.writeQueued(ctx, out, qs, pos, hashes[qs.Track.ID.String()]); err != nil {
				return err
			}
			pos += qs.Meta.Size
			next[qs.Track.ID.String()] = qs.Index + 1
			produced++
			if opts.stopRequested(produced) {
				return errStopAfter
			}
			if produced%waveSize == 0 {
				wave++
				if err := mgr.AppendSegment(out, waveStart, pos, wave, r.snapshot(tracks, hashes, next)); err != nil {
					return err
				}
				waveStart = pos
			}
			return nil
		})
		if err == nil {
			continue
		}
		if err == io.EOF {
			break
		}
		if err == errStopAfter || errors.Is(err, errStopAfter) {
			prodCancel()
			<-prodErr
			if pos > waveStart {
				wave++
				if err := mgr.AppendSegment(out, waveStart, pos, wave, r.snapshot(tracks, hashes, next)); err != nil {
					return nil, err
				}
			}
			if err := out.Sync(); err != nil {
				return nil, media.NewError(media.ClassIO, "cannot flush output").WithCause(err)
			}
			res.Interrupted = true
			res.Samples = produced
			return res, &InterruptError{After: produced}
		}
		consumerErr = err
		break
	}
	prodCancel()
	if perr := <-prodErr; perr != nil && consumerErr == nil {
		consumerErr = perr
	}
	if opts.QueuePeak != nil {
		if peak := queue.Stats().MaxQueueBytes; peak > *opts.QueuePeak {
			*opts.QueuePeak = peak
		}
	}
	if consumerErr != nil {
		return nil, consumerErr
	}
	if pos > waveStart {
		wave++
		if err := mgr.AppendSegment(out, waveStart, pos, wave, r.snapshot(tracks, hashes, next)); err != nil {
			return nil, err
		}
	}
	if err := out.Truncate(pos); err != nil {
		return nil, media.NewError(media.ClassIO, "cannot trim output to its final size").WithCause(err)
	}

	if err := out.Sync(); err != nil {
		return nil, media.NewError(media.ClassIO, "cannot flush output").WithCause(err)
	}
	res.Samples = produced
	if err := out.Sync(); err != nil {
		return nil, media.NewError(media.ClassIO, "cannot flush output before verification").WithCause(err)
	}
	// The input stream was hashed as it was copied. The output hash is
	// computed by re-reading the written sample ranges, so the comparison
	// proves the bytes on disk match the bytes taken from the input.
	final := make(map[string]string, len(r.Tracks))
	for _, t := range tracks {
		outSum, err := hashOutput(out, t, buf)
		if err != nil {
			return nil, err
		}
		inSum, err := hashInput(r.Source, t.key, r.Tracks, buf)
		if err != nil {
			return nil, err
		}
		if err := mgr.RecordFinal(t.key, inSum, outSum); err != nil {
			return nil, err
		}
		final[t.key] = outSum
		res.Tracks = append(res.Tracks, TrackResult{
			Key:          t.key,
			OutputID:     t.id,
			Samples:      len(t.sizes),
			InputSHA256:  inSum,
			OutputSHA256: outSum,
			Bytes:        payloadBytes(t),
		})
	}
	if err := mgr.Complete(final); err != nil {
		return nil, err
	}
	info, err := out.Stat()
	if err != nil {
		return nil, media.NewError(media.ClassIO, "cannot stat output").WithCause(err)
	}
	res.OutputSize = info.Size()
	return res, nil
}

var errStopAfter = errors.New("stop after the requested sample count")

// produce pushes samples in output order into the bounded queue. It stops as
// soon as the queue reports cancellation or a fault, which is how a slow or
// failed consumer applies backpressure.
func (r *Remuxer) produce(ctx context.Context, q *pipeline.Queue, tracks []*outTrack, next map[string]int) error {
	for _, t := range tracks {
		src := r.Tracks[trackOrdinal(r.Tracks, t.key)]
		if src == nil {
			return media.NewError(media.ClassMalformed, "no source track for %s", t.key)
		}
		for i := next[t.key]; i < len(t.sizes); i++ {
			if err := q.Push(ctx, src, i); err != nil {
				return err
			}
		}
	}
	return nil
}

// finishInterrupt commits the checkpoint and returns the interrupt signal.
func (r *Remuxer) finishInterrupt(out *os.File, mgr *pipeline.Manager, res *Result,
	tracks []*outTrack, hashes map[string]hash.Hash, next map[string]int,
	upto int, wave int, pos, waveStart int64, produced int) (*Result, error) {
	if err := out.Sync(); err != nil {
		return nil, media.NewError(media.ClassIO, "cannot flush output").WithCause(err)
	}
	if pos > waveStart {
		if err := mgr.AppendSegment(out, waveStart, pos, wave, r.snapshot(tracks, hashes, next)); err != nil {
			return nil, err
		}
	}
	res.Interrupted = true
	res.Samples = produced
	return res, &InterruptError{After: produced}
}

func (o *Options) stopRequested(produced int) bool {
	if o.ShouldStop == nil {
		return false
	}
	return o.ShouldStop(produced)
}

func (r *Remuxer) snapshot(tracks []*outTrack, hashes map[string]hash.Hash, next map[string]int) []pipeline.TrackState {
	out := make([]pipeline.TrackState, 0, len(tracks))
	for _, t := range tracks {
		state := ""
		if m, ok := hashes[t.key].(encoding.BinaryMarshaler); ok {
			if b, err := m.MarshalBinary(); err == nil {
				state = hex.EncodeToString(b)
			}
		}
		out = append(out, pipeline.TrackState{
			Key: t.key, OutputID: t.id, Written: next[t.key],
			SHA256:    hex.EncodeToString(hashes[t.key].Sum(nil)),
			HashState: state,
		})
	}
	return out
}

// trackOrdinal finds the source track index for an output key.
func trackOrdinal(tracks []*media.Track, key string) int {
	for i, t := range tracks {
		if t.ID.String() == key {
			return i
		}
	}
	return -1
}

func restoreHash(h hash.Hash, encoded string) error {
	b, err := hex.DecodeString(encoded)
	if err != nil {
		return err
	}
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return errors.New("hash does not support state restoration")
	}
	return u.UnmarshalBinary(b)
}

// writeQueued copies an already-buffered sample to its planned offset and
// feeds the running hash. The queued copy is the authority for the bytes
// written, so the remuxer never re-reads the input for a queued sample.
func (r *Remuxer) writeQueued(ctx context.Context, dst *os.File, qs *pipeline.Sample, dstOff int64, h hash.Hash) error {
	if int64(len(qs.Data)) != qs.Meta.Size {
		return media.NewError(media.ClassCheckpointCorrupt,
			"queued sample %d has %d bytes, expected %d", qs.Index, len(qs.Data), qs.Meta.Size)
	}
	remaining := qs.Data
	off := dstOff
	for len(remaining) > 0 {
		if err := ctx.Err(); err != nil {
			return media.NewError(media.ClassCancelled, "cancelled while writing a queued sample").WithCause(err)
		}
		n, err := dst.WriteAt(remaining, off)
		if n > 0 {
			h.Write(remaining[:n])
			off += int64(n)
			remaining = remaining[n:]
		}
		if err != nil {
			return media.NewError(media.ClassIO, "cannot write queued sample bytes").
				WithOffset(off).WithCause(err)
		}
		if n == 0 {
			return media.NewError(media.ClassIO, "output accepted no bytes").
				WithOffset(off)
		}
	}
	return nil
}

// copySample copies exactly meta.Size bytes from the input to dstOff. It
// returns the copied bytes so a resumed run can rebuild the running hash
// from the verified output prefix instead of trusting a serialized hash
// state to match the writer's exact byte order.
func (r *Remuxer) copySample(ctx context.Context, dst *os.File, buf []byte, meta media.Sample, dstOff int64, h hash.Hash) error {
	remaining := meta.Size
	srcOff := meta.Offset
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return media.NewError(media.ClassCancelled, "cancelled while copying a sample").WithCause(err)
		}
		chunk := int64(len(buf))
		if chunk > remaining {
			chunk = remaining
		}
		n, err := r.Source.ReadAt(buf[:chunk], srcOff)
		if n > 0 {
			if _, werr := dst.WriteAt(buf[:n], dstOff); werr != nil {
				return media.NewError(media.ClassIO, "cannot write sample bytes").
					WithOffset(dstOff).WithCause(werr)
			}
			h.Write(buf[:n])
			dstOff += int64(n)
			srcOff += int64(n)
			remaining -= int64(n)
		}
		if remaining == 0 {
			return nil
		}
		if err != nil {
			return media.NewError(media.ClassTruncated, "input ends inside a coded sample").
				WithOffset(srcOff).WithTolerance(0).WithCause(err)
		}
		if n == 0 {
			return media.NewError(media.ClassTruncated,
				"input supplied no bytes inside a coded sample").
				WithOffset(srcOff).WithTolerance(0)
		}
	}
	return nil
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return aa == bb
	}
	return a == b
}

// hashInput hashes the source bytes of one track in sample order.
func hashInput(src media.Source, key string, tracks []*media.Track, buf []byte) (string, error) {
	ord := trackOrdinal(tracks, key)
	if ord < 0 {
		return "", media.NewError(media.ClassMalformed, "no source track for %s", key)
	}
	t := tracks[ord]
	h := sha256.New()
	for i := 0; i < t.SampleCount(); i++ {
		s, err := t.Index.At(i)
		if err != nil {
			return "", media.WithTrackContext(err, t.ID.Container, t.ID.Value)
		}
		remaining := s.Size
		off := s.Offset
		for remaining > 0 {
			chunk := int64(len(buf))
			if chunk > remaining {
				chunk = remaining
			}
			n, rerr := src.ReadAt(buf[:chunk], off)
			if n > 0 {
				h.Write(buf[:n])
				off += int64(n)
				remaining -= int64(n)
			}
			if remaining == 0 {
				break
			}
			if rerr != nil {
				return "", media.NewError(media.ClassTruncated,
					"input ends inside sample %d of track %s", i, key).
					WithOffset(off).WithCause(rerr)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashOutput hashes the sample stream as it now exists in the output file.
func hashOutput(f *os.File, t *outTrack, buf []byte) (string, error) {
	h := sha256.New()
	for i, size := range t.sizes {
		remaining := int64(size)
		off := t.offsets[i]
		for remaining > 0 {
			chunk := int64(len(buf))
			if chunk > remaining {
				chunk = remaining
			}
			n, err := f.ReadAt(buf[:chunk], off)
			if n > 0 {
				h.Write(buf[:n])
				off += int64(n)
				remaining -= int64(n)
			}
			if remaining == 0 {
				break
			}
			if err != nil {
				return "", media.NewError(media.ClassCheckpointCorrupt,
					"output sample %d of track %s is short", i, t.key).
					WithOffset(off).WithCause(err)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func payloadBytes(t *outTrack) int64 {
	var n int64
	for _, s := range t.sizes {
		n += int64(s)
	}
	return n
}
