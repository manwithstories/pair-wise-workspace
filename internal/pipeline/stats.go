package pipeline

import (
	"time"

	"mediarun/internal/ingest"
	"mediarun/internal/mapping"
	"mediarun/internal/mediaerr"
	"mediarun/internal/metadata"
	"mediarun/internal/timebase"
)

// Options configures one pipeline run.
type Options struct {
	WorkDir        string
	InputPath      string
	OutputName     string
	ManifestName   string
	QueueDepth     int
	TargetBitrate  int64 // bits per second target for deviation reporting
	Resume         bool
	InterruptAfter int64 // >0: stop cleanly after this many samples (demo fault)
	Mapping        mapping.Config
	// Metadata inputs per track for resolution/arbitration.
	Metadata map[string]metadata.Input
}

// Stats is the structured result reported to the operator. All timing values
// that feed alignment are derived from exact rationals.
type Stats struct {
	Format              string  `json:"format"`
	SamplesIn           int64   `json:"samples_in"`
	SamplesOut          int64   `json:"samples_out"`
	DuplicateSamples    int64   `json:"duplicate_samples"`
	DroppedSamples      int64   `json:"dropped_samples"`
	QualityChecked      int64   `json:"quality_checked"`
	QualityErrors       int64   `json:"quality_errors"`
	TargetBitrateBPS    int64   `json:"target_bitrate_bps"`
	ActualBitrateBPS    int64   `json:"actual_bitrate_bps"`
	BitrateDeviationPct float64 `json:"bitrate_deviation_pct"`
	ManifestAlignMillis int64   `json:"manifest_alignment_ms"`
	OutputSHA256        string  `json:"output_sha256"`
	OutputCommitted     bool    `json:"output_committed"`
	ResumeFromSample    int64   `json:"resume_from_sample"`
	InputSHA256         string  `json:"input_sha256"`
	MappingSHA256       string  `json:"mapping_sha256"`
	DurationSeconds     float64 `json:"duration_seconds"`

	// trace is internal bookkeeping, never serialized in Stats.
	trace *Trace

	// per-track exact timing/byte accumulators (not serialized).
	ticks map[string]int64
	bytes map[string]int64
	scale map[string]int64
}

// qualityCheck validates one sample without decoding: payload size is positive
// and within the block ceiling. It is the QC counter source in Stats.
func qualityCheck(ev *ingest.SampleEvent) error {
	if ev.Size <= 0 {
		return errQuality("zero-size sample")
	}
	if ev.Size > ingest.MaxBlockBytes {
		return errQuality("sample exceeds block ceiling")
	}
	return nil
}

// timelineDuration computes the exact media duration in seconds for a track by
// summing sample durations in its own timebase, then converting to seconds.
func timelineDuration(tb timebase.Timebase, totalTicks int64) timebase.Rational {
	return tb.ToSeconds(totalTicks)
}

// alignmentError measures, in milliseconds, the absolute end-time difference
// between the video and audio tracks of a manifest. A value within +/-1 ms is
// the acceptance threshold enforced by the runner.
func alignmentError(videoSec, audioSec timebase.Rational) int64 {
	diff := videoSec.Sub(audioSec)
	ms := timebase.Millis(diff)
	if ms < 0 {
		ms = -ms
	}
	return ms
}

// deadline is a small helper retained for future stage-level timeouts.
func deadline(d time.Duration) time.Time { return time.Now().Add(d) }

// errInterrupt is the internal sentinel for a clean, checkpointed interrupt
// (demo --interrupt-after). It is a mediaerr so Is() checks work uniformly.
var errInterrupt = mediaerr.New(mediaerr.CodeIO, "INTERRUPT_BOUNDARY")

// errInterruptCode is the stable code used to recognize a clean interrupt.
const errInterruptCode = mediaerr.CodeIO

// errQuality builds a lightweight quality-failure message.
func errQuality(msg string) error { return mediaerr.New(mediaerr.CodeIO, msg) }
