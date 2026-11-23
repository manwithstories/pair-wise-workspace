package pipeline

import (
	"fmt"
	"io"
	"math"
	"strconv"
)

// Finish computes gates and the exit code. It is the single place that
// decides whether a run was complete, partial or failed.
func (r *Report) Finish(g Gates) {
	r.Gates = GateResults{
		AVSyncOK:   r.AVSyncP95Ms <= g.AVSyncP95Ms,
		RecoveryOK: r.PacketRecovery >= g.PacketRecovery,
		LatencyOK:  r.LatencyP95Ms <= g.LatencyP95Ms,
		FramesOK:   g.ExpectedFrames == 0 || r.Frames == g.ExpectedFrames,
		HashesOK:   r.FrameHashes == r.Frames,
	}
	switch {
	case r.CorruptRanges > 0 && r.Frames > 0:
		// Partial recovery still produced a reviewable report.
		if r.Gates.AVSyncOK || r.Gates.RecoveryOK || r.Gates.LatencyOK {
			r.ExitCode = ExitPartial
			return
		}
		r.ExitCode = ExitError
	case !r.Gates.FramesOK || !r.Gates.HashesOK:
		r.ExitCode = ExitError
	default:
		r.ExitCode = ExitOK
	}
}

// String renders the single-line field summary the demo prints.
func (r *Report) String() string {
	return fmt.Sprintf(
		"frames=%d av_sync_p95_ms=%.2f packet_recovery_rate=%.4f latency_p95_ms=%.2f frame_sha256_count=%d recovered_prefix_frames=%d corrupt_ranges=%d exit_code=%d",
		r.Frames, r.AVSyncP95Ms, r.PacketRecovery, r.LatencyP95Ms, r.FrameHashes,
		r.RecoveredPrefix, r.CorruptRanges, r.ExitCode)
}

// Fields prints one contract assertion per line in the exact form the
// acceptance checklist greps for, e.g. "av_sync_p95_ms<=20". When a gate is
// missed the same line is printed with "<" so the failure is visible without
// hiding the value that was measured. Recovery is the one threshold that is
// a lower bound, so its satisfied form is ">=".
func (r *Report) Fields(w io.Writer) {
	fmt.Fprintf(w, "frames=%d\n", r.Frames)
	r.thresholdLine(w, "av_sync_p95_ms", r.AVSyncP95Ms, 20, true)
	r.thresholdLine(w, "packet_recovery_rate", r.PacketRecovery, 0.90, false)
	r.thresholdLine(w, "latency_p95_ms", r.LatencyP95Ms, 150, true)
	fmt.Fprintf(w, "frame_sha256_count=%d\n", r.FrameHashes)
	if r.RecoveredPrefix > 0 {
		fmt.Fprintln(w, "recovered_prefix_frames>0")
	} else {
		fmt.Fprintln(w, "recovered_prefix_frames=0")
	}
	// Zero corrupt ranges is the good case; for the damaged fixtures the
	// checklist expects at least one. Print the satisfied relation either way
	// so the line is true, and let "measured:" carry the actual count.
	if r.CorruptRanges >= 1 {
		fmt.Fprintln(w, "corrupt_ranges>=1")
	} else {
		fmt.Fprintln(w, "corrupt_ranges=0")
	}
	fmt.Fprintf(w, "measured: av_sync_p95_ms=%.2f packet_recovery_rate=%.4f latency_p95_ms=%.2f recovered_prefix_frames=%d recovered_indexed_frames=%d corrupt_ranges=%d\n",
		r.AVSyncP95Ms, r.PacketRecovery, r.LatencyP95Ms, r.RecoveredPrefix, r.RecoveredIndexed, r.CorruptRanges)
	fmt.Fprintf(w, "exit_code=%d\n", r.ExitCode)
}

// formatLimit prints 20 as "20" and 0.9 as "0.90", matching the checklist.
func formatLimit(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// thresholdLine prints a threshold assertion whose operator matches reality:
// "<=" when the measured value is within an upper bound and ">" when it is
// not, or ">=" for a lower-bound threshold. The printed line is therefore
// always true, whether the gate passed or not, and the measured value is
// printed separately.
func (r *Report) thresholdLine(w io.Writer, name string, value, limit float64, upper bool) {
	var sym string
	switch {
	case upper && value <= limit:
		sym = "<="
	case upper:
		sym = ">"
	case value >= limit:
		sym = ">="
	default:
		sym = "<"
	}
	limitText := formatLimit(limit)
	fmt.Fprintf(w, "%s%s%s\n", name, sym, limitText)
}
