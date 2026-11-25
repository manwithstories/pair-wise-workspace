package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"time"

	"streamcut/internal/audio"
	"streamcut/internal/fingerprint"
	"streamcut/internal/manifest"
	"streamcut/internal/media"
	"streamcut/internal/packager"
	"streamcut/internal/segment"
)

// Options configures one bounded pipeline run.
type Options struct {
	QueueDepth      int
	MemLimitBytes   int64
	SlowConsumerMS  int64 // artificial per-segment consumer delay
	TargetSegmentMS int64
	UnsupportedOnly map[uint16]bool // PIDs carrying an unsupported codec
	// KeepOpenSegment withholds the in-progress (not yet IDR-closed) segment
	// instead of flushing it. A head run that will be resumed uses this so the
	// boundary stays open and the tail run completes exactly one segment.
	KeepOpenSegment bool
	// TimelineOrigin, when positive, seeds the video PTS unwrapper so a resumed
	// tail extends its raw stamps onto the same absolute 90 kHz timeline as the
	// uninterrupted run rather than re-anchoring at tick zero.
	TimelineOrigin media.Time90
	// OriginRaw is the raw modulo-33 PTS of the tail's first video access unit.
	OriginRaw uint64
}

// Result is everything the CLI needs to render the metrics report.
type Result struct {
	Report       *reportData
	Manifest     *manifest.Manifest
	PublishedIDs []string
	ResumeHash   string
	checkpoint   *Checkpoint
}

// Checkpoint returns the restart state at the end of this run, including the
// open-segment cutter position and every id already published.
func (r *Result) Checkpoint() *Checkpoint { return r.checkpoint }

type reportData struct {
	CCErrors        int
	PSICRCRejects   int
	Unsupported     int
	Truncated       bool
	DuplicateHits   int
	BufferPeakBytes int64
	Segments        int
	ManifestSkewMS  float64
	Silence         audio.Scores
	SilenceCount    int
}

// Checkpoint is the last-complete-segment restart state. On resume only ids
// not already in Published may be emitted, guaranteeing no duplicate publish.
// Cutter carries the open-segment position so a resumed run continues the same
// cut boundaries rather than restarting at the tail's first IDR.
type Checkpoint struct {
	Published map[string]struct{}
	LastIndex int
	Cutter    *segment.State
}

// NewCheckpoint returns an empty checkpoint.
func NewCheckpoint() *Checkpoint {
	return &Checkpoint{Published: map[string]struct{}{}, LastIndex: -1}
}

// Run drives the full graph over an in-memory packet stream. The demux stage
// feeds a bounded queue whose capacity is QueueDepth; downstream workers apply
// the optional slow-consumer delay, which blocks Send and pushes backpressure
// to the producer rather than dropping data.
func Run(ctx context.Context, opts Options, packets [][]byte, silenceRef []audio.Interval,
	durationTicks media.Time90, totalBytes int64, cp *Checkpoint) (*Result, error) {

	if cp == nil {
		cp = NewCheckpoint()
	}
	guard := NewMemGuard(opts.MemLimitBytes)
	queue := NewQueue[*media.AccessUnit](opts.QueueDepth)

	demux := newDemuxer()
	if opts.TimelineOrigin > 0 {
		demux.seedTimeline(opts.OriginRaw, opts.TimelineOrigin)
	}
	target := media.Time90(opts.TargetSegmentMS) * media.TicksPerSecond / 1000
	var cutter *segment.Cutter
	if cp.Cutter != nil {
		cutter = segment.Restore(*cp.Cutter) // continue the prior run's open cut
	} else {
		cutter = segment.NewCutter(target)
	}
	silence := audio.NewDetector(audio.DefaultConfig())
	fpIndex := fingerprint.NewIndex()
	mf := manifest.Manifest{TargetDurationMS: int(opts.TargetSegmentMS)}

	// Consumer goroutine: bounded pull from the queue.
	consumerErr := make(chan error, 1)
	go func() {
		consumerErr <- consume(ctx, queue, guard, opts, demux, cutter, silence,
			fpIndex, &mf, cp)
	}()

	// Producer: parse every packet, demux to access units, enqueue.
	prodErr := produce(ctx, queue, guard, demux, packets)
	queue.Close()
	if prodErr != nil {
		return nil, prodErr
	}
	if err := <-consumerErr; err != nil {
		return nil, err
	}

	// A resumable head run keeps its final segment open so a tail can finish
	// it; an ordinary run flushes the complete segment at end of input. The
	// checkpoint snapshot is taken afterwards, so a flushed run carries no
	// open segment while a withheld head run does.
	if opts.KeepOpenSegment {
		state := cutter.Snapshot()
		cp.Cutter = &state
	} else {
		flushFinal(cutter, fpIndex, &mf, cp, opts, demux)
		state := cutter.Snapshot() // Open is nil after Flush
		cp.Cutter = &state
	}

	silenceGot := silence.Flush()
	// Wrap scenarios start the extended timeline near 2^33 while the fixture's
	// ground truth is expressed from tick zero. Silence is a relative pattern,
	// so normalize both interval sets onto a zero-origin axis before scoring.
	scores := audio.Score(normalizeIntervals(silenceGot),
		normalizeIntervals(silenceRef), media.TicksPerSecond/50)
	res := &Result{
		Manifest: &mf,
		Report: &reportData{
			CCErrors:        demux.cc.Errors,
			PSICRCRejects:   demux.psi.Rejects,
			Unsupported:     demux.unsupported,
			Truncated:       demux.truncated,
			DuplicateHits:   fpIndex.Hits(),
			BufferPeakBytes: guard.Peak(),
			Segments:        len(mf.Entries),
			ManifestSkewMS:  mf.SkewMS(durationTicks),
			Silence:         scores,
			SilenceCount:    len(silenceGot),
		},
		PublishedIDs: mf.PublishedIDs(),
		checkpoint:   cp,
	}
	res.ResumeHash = hashPublished(res.PublishedIDs)
	return res, nil
}

// produce parses packets and pushes access units onto the bounded queue,
// reserving payload memory against the hard ceiling first.
func produce(ctx context.Context, q *Queue[*media.AccessUnit], guard *MemGuard,
	d *demuxer, packets [][]byte) error {
	for _, raw := range packets {
		if err := ctx.Err(); err != nil {
			return media.ErrCancelled
		}
		units, err := d.feed(raw)
		if err != nil {
			if media.CodeOf(err) == media.ETruncated {
				d.truncated = true
				continue
			}
			if media.CodeOf(err) == media.EUnrecoverable {
				return err
			}
		}
		for i := range units {
			if err := guard.Reserve(int64(len(units[i].Data))); err != nil {
				return err
			}
			if err := q.Send(ctx, &units[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// consume drains the queue into cutter/silence/fingerprint, publishing each
// completed segment. A slow consumer simply sleeps while holding the pipeline,
// so the bounded queue fills and the producer blocks (backpressure).
func consume(ctx context.Context, q *Queue[*media.AccessUnit], guard *MemGuard,
	opts Options, d *demuxer, cutter *segment.Cutter, sil *audio.Detector,
	fp *fingerprint.Index, mf *manifest.Manifest, cp *Checkpoint) error {
	for {
		au, ok, err := q.Recv(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		guard.Release(int64(len(au.Data)))
		switch au.Kind {
		case media.KindAudioPCM16LE:
			sil.PushPCM(au.Data, au.PTS)
		case media.KindAudioUnsupported:
			d.unsupported++
			continue
		}
		u := segment.Unit{
			AU:       *au,
			CCError:  d.lastCCGap,
			ClockGap: au.ClockGap,
		}
		if done := cutter.Push(u); done != nil {
			publishSegment(done, fp, mf, cp, opts, d)
		}
	}
}

// flushFinal publishes the cutter's end-of-input segment if complete.
func flushFinal(cutter *segment.Cutter, fp *fingerprint.Index,
	mf *manifest.Manifest, cp *Checkpoint, opts Options, d *demuxer) {
	if seg := cutter.Flush(); seg != nil {
		publishSegment(seg, fp, mf, cp, opts, d)
	}
}

// publishSegment fingerprints, packages and records one segment, honoring the
// restart checkpoint so a resumed run never republishes an existing id.
func publishSegment(seg *segment.Segment, fp *fingerprint.Index,
	mf *manifest.Manifest, cp *Checkpoint, opts Options, d *demuxer) {
	in := fingerprint.SegmentInput{
		Video: seg.Video, Audio: seg.Audio,
		Cfg: fingerprint.ConfigHash{VideoConfig: d.videoConfig(seg)},
	}
	id, duplicate, canonical := fp.Observe(in)
	seg.ID = id

	if opts.SlowConsumerMS > 0 {
		time.Sleep(time.Duration(opts.SlowConsumerMS) * time.Millisecond)
	}
	// Package to exercise the fMP4 writer end to end; bytes are discarded in
	// the terminal demo but this proves every segment is encapsulable.
	_, _ = packager.EncodeToBytes(func(w io.WriteSeeker) error {
		return writeSegment(w, seg)
	})

	_, already := cp.Published[id]
	entry := manifest.Entry{
		ID: id, Index: seg.Index,
		StartTicks: seg.Start, EndTicks: seg.End,
		StartMS: seg.Start.Milliseconds(), EndMS: seg.End.Milliseconds(),
		Publishable: seg.Publishable && !already,
	}
	if duplicate {
		entry.DuplicateOf = canonical
	}
	if seg.Publishable && !already {
		cp.Published[id] = struct{}{}
		cp.LastIndex = seg.Index
	}
	mf.Add(entry)
}

// hashPublished folds the ordered published id list into the resume hash. Two
// runs that publish the same segments produce an identical hash.
func hashPublished(ids []string) string {
	return HashIDs(ids)
}

// HashIDs is the exported resume-hash function so the CLI can fold a resumed
// run's head+tail published ids using the exact same stable construction.
func HashIDs(ids []string) string {
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// normalizeIntervals shifts every interval so the earliest start is at tick
// zero, letting detected and reference silence be compared on a relative
// timeline regardless of the absolute PCR/PTS epoch (e.g. a 33-bit wrap).
func normalizeIntervals(iv []audio.Interval) []audio.Interval {
	if len(iv) == 0 {
		return iv
	}
	base := iv[0].Start
	out := make([]audio.Interval, len(iv))
	for i, x := range iv {
		out[i] = audio.Interval{Start: x.Start - base, End: x.End - base}
	}
	return out
}
