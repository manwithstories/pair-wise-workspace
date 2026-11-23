package main

import (
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"archive/internal/media"
	"archive/internal/scheduler"
)

// report prints the acceptance metrics and computes the exit code. It
// works from whatever nodes completed, so a blocked pipeline still
// reports the truncation offset and the blocking node.
func report(out io.Writer, sched *scheduler.Scheduler, runErr error,
	opts demoOpts, heapPeak *atomic.Int64) int {
	res := sched.Results()
	blocked := "none"
	var truncOff int64 = -1
	if runErr != nil {
		var be *scheduler.BlockedError
		if errors.As(runErr, &be) {
			blocked = be.Node
			fmt.Fprintf(out, "dependency_chain=%v\n", be.Chain)
		}
		var me *media.Error
		if errors.As(runErr, &me) && me.Offset >= 0 {
			truncOff = me.Offset
		}
		fmt.Fprintln(out, "error="+runErr.Error())
	}
	if t, ok := res["timeline"].(*timelineOut); ok {
		fmt.Fprintf(out, "av_sync_error_ms=%.1f\n", float64(t.syncUs)/1000)
	}
	entries := 0
	sha := "none"
	if idx, ok := res["index"].(*indexOut); ok {
		entries = idx.entries
		sha = idx.sha256
	}
	if truncOff >= 0 {
		fmt.Fprintf(out, "truncated_offset=%d\n", truncOff)
	}
	var pipePeak int64
	if tr, ok := res["transport"].(*transportOut); ok {
		fmt.Fprintf(out, "loss_recovery_rate=%.2f\n", tr.stats.RecoveryRate())
		fmt.Fprintf(out, "p95_latency_ms=%.0f\n", tr.p95ms)
		pipePeak = tr.pipePeak
	}
	peak := heapPeak.Load()
	if pipePeak > peak {
		peak = pipePeak
	}
	peakMiB := (peak + (1 << 20) - 1) >> 20
	fmt.Fprintf(out, "peak_memory_mib=%d\n", peakMiB)
	fmt.Fprintf(out, "index_entries=%d\n", entries)
	fmt.Fprintf(out, "blocked_node=%s\n", blocked)
	fmt.Fprintf(out, "index_sha256=%s\n", sha)
	if peakMiB > int64(opts.memoryMiB) {
		fmt.Fprintf(out, "error=memory budget exceeded: %d MiB > %d MiB\n",
			peakMiB, opts.memoryMiB)
		return 1
	}
	if runErr != nil {
		return media.ExitCode(runErr)
	}
	return 0
}
