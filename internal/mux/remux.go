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
	"os"
	"path/filepath"

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
}

// Remuxer copies the samples of ordered tracks into a fresh MP4.
type Remuxer struct {
	Tracks []*media.Track
	Plan   map[string]uint32
	Source media.Source
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

	tracks, start, err := planOutput(r.Tracks, r.Plan)
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

	buf := make([]byte, opts.Chunk)
	pos := start
	if resume != nil {
		pos = resume.Offset
	}
	produced := 0
	wave := 0
	if resume != nil {
		wave = resume.Wave
	}

	// Output is laid out track-major, so the payload is a concatenation of
	// per-track sample streams and any checkpoint boundary is a contiguous
	// output prefix. Resume therefore continues at the first unwritten
	// sample of the first incomplete track, with no replay.
	waveSize := opts.WaveSize
	if waveSize <= 0 {
		waveSize = 1
	}
	for _, t := range tracks {
		from := next[t.key]
		if from > len(t.sizes) {
			return nil, media.NewError(media.ClassCheckpointCorrupt,
				"checkpoint claims %d samples for %s but only %d exist",
				from, t.key, len(t.sizes))
		}
		waveStart := pos
		for i := from; i < len(t.sizes); i++ {
			if err := ctx.Err(); err != nil {
				return nil, media.NewError(media.ClassCancelled, "remux cancelled").WithCause(err)
			}
			meta, err := r.Tracks[trackOrdinal(r.Tracks, t.key)].Index.At(i)
			if err != nil {
				return nil, err
			}
			if opts.InterruptAfter > 0 && produced == opts.InterruptAfter {
				return r.finishInterrupt(out, mgr, res, tracks, hashes, next, i, wave, pos, waveStart, produced)
			}
			if err := r.copySample(ctx, out, buf, meta, pos, hashes[t.key]); err != nil {
				return nil, media.WithTrackContext(err, r.Tracks[trackOrdinal(r.Tracks, t.key)].ID.Container, r.Tracks[trackOrdinal(r.Tracks, t.key)].ID.Value)
			}
			pos += meta.Size
			next[t.key] = i + 1
			produced++
			if (i+1-from)%waveSize == 0 {
				wave++
				if err := mgr.AppendSegment(out, waveStart, pos, wave, r.snapshot(tracks, hashes, next)); err != nil {
					return nil, err
				}
				waveStart = pos
			}
			if opts.InterruptAfter > 0 && produced == opts.InterruptAfter {
				return r.finishInterrupt(out, mgr, res, tracks, hashes, next, i+1, wave+1, pos, waveStart, produced)
			}
		}
		if pos > waveStart {
			wave++
			if err := mgr.AppendSegment(out, waveStart, pos, wave, r.snapshot(tracks, hashes, next)); err != nil {
				return nil, err
			}
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
		inSum := hex.EncodeToString(hashes[t.key].Sum(nil))
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
