package main

import (
	"errors"
	"fmt"
	"os"

	"mediacheck/internal/fixture"
	"mediacheck/internal/journal"
	"mediacheck/internal/metrics"
	"mediacheck/internal/txn"
)

// runDemo executes the full acceptance flow and returns the exit code.
func runDemo(cfg config) int {
	rep := metrics.New()
	rep.Setf("seed", "%d", cfg.seed)
	rep.Set("fixture_case", cfg.fixtureCase)
	rep.Setf("loss_burst", "%d", cfg.lossBurst)
	rep.Setf("bandwidth_mbps", "%v", cfg.bandwidth)

	if !cfg.resume {
		os.RemoveAll(cfg.workDir)
	}
	wal, err := journal.Open(cfg.workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "journal: %v\n", err)
		return exitRecovery
	}
	tx := txn.New(wal, cfg.workDir)
	stream := fixture.Generate(fixture.Options{
		Seed: cfg.seed, Fragments: cfg.fragments,
		LossBurst: cfg.lossBurst, Bandwidth: cfg.bandwidth, Case: cfg.fixtureCase,
	})
	if cfg.resume {
		action, err := tx.Recover()
		if err != nil {
			fmt.Fprintf(os.Stderr, "recover: %v\n", err)
			return exitRecovery
		}
		rep.Set("recovery_action", action)
	}

	// The crashed first pass records into a scratch report so failures
	// are not duplicated when the resumed pass re-records them.
	scratch := rep
	if cfg.crashAfterStage != "" {
		scratch = metrics.New()
	}
	res, crashed, err := runStages(cfg, wal, tx, stream, cfg.crashAfterStage, scratch)
	if err != nil {
		return fail(rep, err)
	}
	recovered := false
	if crashed {
		// Simulate the crash: flush, reopen the journal from disk,
		// roll back or complete the interrupted transaction, then
		// replay the remaining stages exactly once.
		rep.Set("crashed_after_stage", cfg.crashAfterStage)
		wal.Close()
		wal, err = journal.Open(cfg.workDir)
		if err != nil {
			return fail(rep, err)
		}
		tx = txn.New(wal, cfg.workDir)
		action, rerr := tx.Recover()
		if rerr != nil {
			return fail(rep, rerr)
		}
		rep.Set("recovery_action", action)
		res, _, err = runStages(cfg, wal, tx, stream, "", rep)
		if err != nil {
			return fail(rep, err)
		}
		recovered = true
	}
	defer wal.Close()

	// Recovery verification: exactly one committed record for txn 1.
	committed := 0
	for _, r := range wal.Records() {
		if r.Type == journal.TypeCommitted && r.Txn == 1 {
			committed++
		}
	}
	recoveryOK := committed == 1
	if crashed || cfg.resume {
		rep.Setf("recovery_ok", "%t", recoveryOK && (recovered || cfg.resume))
	} else {
		rep.Setf("recovery_ok", "%t", recoveryOK)
	}
	rep.Check("single_commit", committed == 1, fmt.Sprintf("committed records=%d", committed))

	// Required output fields.
	rep.Setf("av_sync_ms", "%.3f", res.avSyncP95)
	rep.Setf("loss_recovery_rate", "%.3f", res.stats.LossRecoveryRate())
	rep.Setf("latency_p95_ms", "%.1f", res.stats.LatencyP95())
	rep.Setf("duplicate_packets", "%d", res.stats.Duplicates)
	rep.Setf("timeline_rollbacks", "%d", res.timeline.Rollbacks)
	rep.Setf("committed_segments", "%d", res.committedSegments)
	rep.Set("timeline_summary", res.timeline.Summary())
	rep.Setf("silent_intervals", "%d", res.silentIntervals)
	rep.Setf("duplicate_segments", "%d", res.duplicates)
	rep.Setf("parse_failures", "%d", res.parseFailures)
	rep.Setf("lost_packets", "%d", res.stats.Lost)
	rep.Setf("recovered_packets", "%d", res.stats.Recovered)
	rep.Setf("late_packets", "%d", res.stats.Late)
	rep.Set("commit_digest", res.commitDigest)
	if res.commitSkipped {
		rep.Set("commit_skipped", "true")
	}

	// Metric target checks.
	rep.Check("av_sync", res.avSyncP95 <= 20, fmt.Sprintf("P95=%.3f ms target<=20", res.avSyncP95))
	rep.Check("loss_recovery", res.stats.LossRecoveryRate() >= 0.90,
		fmt.Sprintf("rate=%.3f target>=0.90", res.stats.LossRecoveryRate()))
	rep.Check("latency", res.stats.LatencyP95() <= 250,
		fmt.Sprintf("P95=%.1f ms target<=250", res.stats.LatencyP95()))

	fmt.Print(rep.Render())
	switch {
	case !recoveryOK:
		return exitRecovery
	case !rep.ChecksPass():
		return exitRecovery
	case res.parseFailures > 0:
		return exitCorrupt
	}
	return exitOK
}

// fail renders the report so far and maps the error to an exit code.
func fail(rep *metrics.Report, err error) int {
	rep.AddFailure("pipeline", err.Error())
	fmt.Print(rep.Render())
	if errors.Is(err, errDeterminism) {
		return exitRecovery
	}
	return exitRecovery
}
