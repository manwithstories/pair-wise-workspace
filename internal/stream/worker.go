package stream

import (
	"context"
	"errors"
	"fmt"

	"mediacore/internal/audio"
	"mediacore/internal/hash"
	"mediacore/internal/index"
)

// SegmentWorker turns one indexed segment into a hashed, normalized result.
//
// It is deliberately streaming: Prepare decodes and normalizes only the
// audio frames inside the requested segment, so peak memory tracks the queue
// budget rather than the media duration.
type SegmentWorker struct {
	Index       *index.Index
	FrameSource FrameSource
	// SegmentSizeUS is the requested segment length; the final segment may be
	// shorter when the media ends.
	SegmentSizeUS int64
	// Normalizer is created lazily from the audio track's own format.
	Normalizer *audio.Normalizer
	// Meter measures loudness over the run.
	Meter *audio.Meter
	// Records accumulates the metadata hashed per segment.
	Records map[int][]index.Record
	// Faults counts quarantined records observed by the scanner.
	Faults int
	// Reports holds each segment's digest in completion order.
	Reports []hash.Report
}

// FrameSource yields decoded audio for the requested window.
type FrameSource interface {
	// AudioFormat reports the decoded sample format details.
	AudioFormat() AudioFormat
	// ReadIntoBuffer returns the audio frames whose unified timestamps fall
	// inside [startUS, endUS), in order and at the source sample rate.
	ReadIntoBuffer(ctx context.Context, startUS, endUS int64) (audio.Buffer, error)
}

// AudioFormat describes the source decoder output.
type AudioFormat struct {
	Rate     int
	Channels int
	Layout   string
	// SampleFormat is the astiav sample format name ("fltp", "s16", ...).
	SampleFormat string
}

// NewSegmentWorker prepares a worker for an index.
func NewSegmentWorker(ix *index.Index, src FrameSource, segmentSizeUS int64) (*SegmentWorker, error) {
	if ix == nil || src == nil {
		return nil, errors.New("stream: worker needs an index and a frame source")
	}
	if segmentSizeUS <= 0 {
		return nil, fmt.Errorf("stream: invalid segment size %d", segmentSizeUS)
	}
	return &SegmentWorker{Index: ix, FrameSource: src, SegmentSizeUS: segmentSizeUS, Records: map[int][]index.Record{}}, nil
}

// Prepare decodes the job's audio window and normalizes it. The resulting
// buffer is attached to Job.Data and accounted by Job.Cost.
//
// Each segment gets a fresh resampler and loudness meter. A streaming
// resampler carries filter history across calls, which would make a segment's
// leading samples depend on whether it was reached by a full run or by a
// resume. A per-segment normalizer makes the output a pure function of the
// segment's own source window, so a resumed run reproduces the uninterrupted
// run byte for byte.
func (w *SegmentWorker) Prepare(ctx context.Context, job *Job) error {
	// Reset per-job payload so a retried job never hashes stale audio.
	job.Data = nil
	job.Cost = 0
	f := w.FrameSource.AudioFormat()
	normalizer, err := audio.NewNormalizer(f.Rate, f.Channels, f.Layout)
	if err != nil {
		return err
	}
	buf, err := w.FrameSource.ReadIntoBuffer(ctx, job.StartUS, job.EndUS)
	if err != nil {
		return err
	}
	norm, err := normalizer.Process(buf)
	if err != nil {
		return err
	}
	tail := normalizer.Flush()
	if tail.Frames() > 0 {
		norm.Planes[0] = append(norm.Planes[0], tail.Planes[0]...)
		norm.Planes[1] = append(norm.Planes[1], tail.Planes[1]...)
	}
	if w.Normalizer == nil {
		w.Normalizer = normalizer
	}
	// Loudness is measured on the normalized 48 kHz stereo signal so the
	// gain decision matches what a listener would hear.
	if w.Meter != nil {
		if err := w.Meter.Add(norm); err != nil {
			return err
		}
	}
	job.Data = norm
	job.Cost = int64(norm.Frames()*norm.Channels()*4) + 256
	return nil
}

// Consume hashes the normalized audio and the metadata records that fall in
// the segment.
func (w *SegmentWorker) Consume(_ context.Context, job *Job) (string, error) {
	seg := hash.Segment{Index: job.ID, StartUS: job.StartUS, EndUS: job.EndUS}
	h := hash.NewSegmentHasher(seg)
	if norm, ok := job.Data.(audio.Buffer); ok {
		if err := h.AddAudio(norm.Planes); err != nil {
			return "", err
		}
	}
	recs := w.Index.RecordsBetween(job.StartUS, job.EndUS)
	for _, r := range recs {
		if err := h.AddRecord(string(r.Kind), r.TimeUS, r.Payload); err != nil {
			return "", err
		}
	}
	w.Records[job.ID] = recs
	report := h.Digest(seg)
	w.Reports = append(w.Reports, report)
	job.Data = nil
	return report.SHA256, nil
}

// Loudness returns the run's measured loudness.
func (w *SegmentWorker) Loudness() audio.Loudness {
	if w.Meter == nil {
		return audio.Loudness{}
	}
	return w.Meter.Result()
}
