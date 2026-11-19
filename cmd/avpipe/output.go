package main

import (
	"fmt"
	"io"

	"avpipe/internal/report"
)

// printSummary renders the terminal view. Every acceptance field is printed
// on its own line so an operator can read the outcome without opening JSON.
func printSummary(w io.Writer, rep *report.Report, self bool) {
	name := "run"
	if self {
		name = "selftest"
	}
	fmt.Fprintf(w, "avpipe %s seed=%d layout=%s rate=%d profile=%s\n",
		name, rep.Seed, rep.Layout, rep.SampleRate, rep.Profile)
	for _, m := range rep.Metrics {
		status := "ok"
		if m.Checked && !m.Passed {
			status = "FAIL"
		}
		if !m.Checked {
			status = "n/a"
		}
		if m.Name == "queue_peak_bytes" {
			fmt.Fprintf(w, "  %-22s %12d  limit=%g  %s\n", m.Name, int64(m.Value), m.Limit, status)
			continue
		}
		fmt.Fprintf(w, "  %-22s %12.4f  limit=%g  %s\n", m.Name, m.Value, m.Limit, status)
	}
	fmt.Fprintf(w, "  %-22s %12.4f\n", "rms_before", rep.RMSBefore)
	fmt.Fprintf(w, "  %-22s %12.4f\n", "rms_after", rep.RMSAfter)
	fmt.Fprintf(w, "  %-22s %12.2f\n", "gain_db", rep.GainDB)
	fmt.Fprintf(w, "  %-22s %12.4f\n", "sync_p95_before_ms", rep.SyncP95BeforeMs)
	fmt.Fprintf(w, "  %-22s %12.4f\n", "drift_estimate_ppm", rep.DriftEstimatePPM)
	fmt.Fprintf(w, "  %-22s %12d\n", "rtp_lost", rep.RTPLost)
	fmt.Fprintf(w, "  %-22s %12d\n", "rtp_unrecoverable", rep.RTPUnrecoverable)
	fmt.Fprintf(w, "  %-22s %12d\n", "silence_fills", rep.SilenceFills)
	fmt.Fprintf(w, "  %-22s %12.6f\n", "resample_rounding_err", rep.RoundingError)
	for _, ts := range rep.Tracks {
		fmt.Fprintf(w, "  track %-12s %s\n", ts.Name, ts.Status)
	}
	for _, s := range rep.Scenarios {
		status := "ok"
		if !s.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  scenario %-16s %s  %s\n", s.Name, status, s.Detail)
	}
	if rep.RootError != nil {
		fmt.Fprintf(w, "  root_error %s %s", rep.RootError.Code, rep.RootError.Op)
		if rep.RootError.Track != "" {
			fmt.Fprintf(w, " track=%s", rep.RootError.Track)
		}
		if rep.RootError.Offset != nil {
			fmt.Fprintf(w, " offset=%d", *rep.RootError.Offset)
		}
		if rep.RootError.Detail != "" {
			fmt.Fprintf(w, ": %s", rep.RootError.Detail)
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, "  root_error null")
	}
	verdict := "PASS"
	if !rep.Passed {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "result=%s exit=%d\n", verdict, rep.ExitCode)
}
