package pipeline

import (
	"context"
	"os"
	"path/filepath"

	"avpipe/internal/fixture"
	"avpipe/internal/report"
)

// Run executes the full pipeline and returns the finished report. It never
// returns a nil report on a handled error: the error is captured in
// root_error and the exit code, which is what the CLI prints.
func Run(ctx context.Context, opts Options) *report.Report {
	opts.Defaults()
	rep := &report.Report{
		Command: "run", Seed: opts.Seed, SampleRate: opts.TargetRate,
		Layout: opts.TargetLayout, TargetLUFS: opts.TargetLUFS,
	}
	fail := func(err error) *report.Report {
		rep.RootError = report.FromError(err)
		rep.Errors = append(rep.Errors, *rep.RootError)
		rep.ExitCode = classify(err)
		rep.Passed = rep.ExitCode == 0
		return rep
	}

	audioRes, qstats, err := runAudio(ctx, opts)
	if err != nil {
		return fail(err)
	}
	// Every run generates a diagnostic MP4 in a scratch directory and probes
	// it back, so a broken writer or bad box layout fails the video track
	// instead of passing silently. Scratch data never lands in the repo: an
	// explicit --work-dir keeps the clip, and otherwise a private temp dir is
	// created and removed after the probe.
	workDir := opts.WorkDir
	ephemeral := false
	if workDir == "" {
		dir, terr := os.MkdirTemp("", "avpipe-work-")
		if terr != nil {
			workDir = ""
		} else {
			workDir, ephemeral = dir, true
		}
	}
	haveMP4 := false
	if workDir != "" {
		if err := os.MkdirAll(workDir, 0o755); err == nil {
			clip := filepath.Join(workDir, "diagnostic.mp4")
			if err := fixture.WriteClip(fixture.ClipConfig{Path: clip, Frames: 30, FPS: 30}); err == nil {
				if _, _, err := fixture.VerifyClip(clip); err == nil {
					haveMP4 = true
					rep.MP4Clip = clip
				}
			}
		}
		if ephemeral {
			defer os.RemoveAll(workDir)
			// The probe has already completed; do not leak the path of a
			// file that will not exist when the report is read.
			rep.MP4Clip = "generated"
		}
	}
	outcome, trackErr := runTrack(ctx, opts, haveMP4)
	rep.Tracks = toTrackStatus(outcome)
	if trackErr != nil {
		return fail(trackErr)
	}

	rec, err := runRTP(ctx, opts, audioRes.before)
	if err != nil {
		return fail(err)
	}
	align, err := runSync(opts, audioRes.before)
	if err != nil {
		return fail(err)
	}

	rep.QueuePeakBytes = qstats.PeakBytes
	rep.QueuePeakTicks = qstats.PeakTicks
	rep.Blocks = len(audioRes.before)
	rep.Frames = len(audioRes.before)
	rep.Consumed = qstats.Dequeued
	rep.Dropped = qstats.Dropped + audioRes.dropped
	rep.Evicted = qstats.Evicted + audioRes.evicted
	rep.SilenceFills = len(audioRes.plan.SilenceFills)
	rep.RMSBefore = audioRes.stats.RMSBefore
	rep.RMSAfter = audioRes.stats.RMSAfter
	rep.PeakBefore = audioRes.stats.PeakBefore
	rep.PeakAfter = audioRes.stats.PeakAfter
	rep.GainDB = audioRes.stats.GainDB
	rep.RoundingError = audioRes.roundingErr
	rep.Resamples = int64(len(audioRes.before))
	rep.LatencySamples = len(audioRes.latencies)
	rep.LatencyP95Ms = percentile(audioRes.latencies, 0.95)
	rep.PacketRecovery = rec.Rate
	rep.RTPLost = rec.Lost
	rep.RTPRecovered = rec.Recovered
	rep.RTPUnrecoverable = rec.Unrecoverable
	rep.RTPConflicts = rec.Conflicts
	rep.SyncErrorMs = align.P95Ms
	rep.SyncP95BeforeMs = align.P95BeforeMs
	rep.DriftEstimatePPM = align.DriftEstPPM
	rep.DriftResolvable = align.DriftResolvable
	rep.DriftStdErrPPM = align.StdErrPPM
	profile, th := ProfileFor(opts)
	rep.Profile = string(profile)
	rep.Metrics = metricsFor(th, rep)
	rep.Finalize()
	return rep
}

// Profile names one acceptance class.
type Profile string

const (
	ProfileNormal       Profile = "normal"
	ProfileBoundary     Profile = "drift-jitter-boundary"
	ProfileSlowConsumer Profile = "slow-consumer"
)

// ProfileFor selects the acceptance class from the injected conditions:
//
//   - a heavy consumer (>=100ms per block) is the slow-consumer sample,
//     budgeted at 250ms p95 latency;
//   - jitter above 20ms or drift above 2000ppm is the drift-and-jitter
//     boundary sample, budgeted at 80ms p95 sync;
//   - everything else is the normal sample: 20ms p95 sync, 120ms latency.
//
// Recovery and queue limits apply to every profile.
func ProfileFor(opts Options) (Profile, report.Thresholds) {
	th := report.Thresholds{RecoveryRateMin: 0.90, QueuePeakMax: 262144}
	switch {
	case opts.ConsumerDelayMs >= 100:
		th.SyncP95Ms, th.LatencyP95Ms = 20, 250
		return ProfileSlowConsumer, th
	case opts.JitterMs > 20 || opts.DriftPPM > 2000:
		th.SyncP95Ms, th.LatencyP95Ms = 80, 120
		return ProfileBoundary, th
	default:
		th.SyncP95Ms, th.LatencyP95Ms = 20, 120
		return ProfileNormal, th
	}
}

// metricsFor evaluates a report against thresholds.
func metricsFor(t report.Thresholds, r *report.Report) []report.Metric {
	return []report.Metric{
		t.Check("sync_p95_ms", r.SyncErrorMs, t.SyncP95Ms, true),
		t.Check("latency_p95_ms", r.LatencyP95Ms, t.LatencyP95Ms, true),
		t.Check("packet_recovery_rate", r.PacketRecovery, t.RecoveryRateMin, false),
		{Name: "queue_peak_bytes", Value: float64(r.QueuePeakBytes),
			Limit: float64(t.QueuePeakMax), Checked: t.QueuePeakMax != 0,
			Passed: t.QueuePeakMax == 0 || r.QueuePeakBytes <= t.QueuePeakMax},
	}
}
