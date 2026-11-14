package main

import (
	"errors"
	"fmt"
	"io"
	"math"

	"relayctl/internal/relay"
)

func runVerify(args []string, stdout io.Writer) error {
	fs := newFlagSet("verify", stdout)
	reportPath := fs.String("report", "out/report.json", "relayctl JSON report")
	minForwardRatio := fs.Float64("min-forward-ratio", 0.98, "minimum forwarded/emitted ratio")
	maxJitterP95 := fs.Float64("max-jitter-p95-ms", 30, "maximum RFC 3550 jitter p95 in milliseconds")
	if err := fs.Parse(args); err != nil {
		return withCode("bad_args", "see relayctl verify -h", err)
	}
	if fs.NArg() != 0 {
		return withCode("bad_args", "unexpected verify arguments", errors.New(fs.Args()[0]))
	}
	if *minForwardRatio < 0 || *minForwardRatio > 1 || math.IsNaN(*minForwardRatio) {
		return withCode("bad_args", "min-forward-ratio must be in [0,1]", fmt.Errorf("got %v", *minForwardRatio))
	}
	if *maxJitterP95 < 0 || math.IsNaN(*maxJitterP95) {
		return withCode("bad_args", "max-jitter-p95-ms must be non-negative", fmt.Errorf("got %v", *maxJitterP95))
	}
	report, err := relay.ReadReport(*reportPath)
	if err != nil {
		return err
	}
	forwardRatio := 0.0
	if report.Emitted > 0 {
		forwardRatio = float64(report.Forwarded) / float64(report.Emitted)
	}
	var failures []string
	if report.Unique != report.Forwarded {
		failures = append(failures, fmt.Sprintf("unique=%d differs from forwarded=%d", report.Unique, report.Forwarded))
	}
	if forwardRatio+1e-9 < *minForwardRatio {
		failures = append(failures, fmt.Sprintf("forward_ratio=%.6f below %.6f", forwardRatio, *minForwardRatio))
	}
	if report.JitterMS.P95 > *maxJitterP95+1e-9 {
		failures = append(failures, fmt.Sprintf("jitter_p95_ms=%.3f above %.3f", report.JitterMS.P95, *maxJitterP95))
	}
	fmt.Fprintf(stdout, "forward_ratio=%.6f forward_ratio_ok=%t jitter_p95_ms=%.3f jitter_p95_ok=%t verify=%s\n",
		forwardRatio, forwardRatio+1e-9 >= *minForwardRatio,
		report.JitterMS.P95, report.JitterMS.P95 <= *maxJitterP95+1e-9,
		map[bool]string{true: "ok", false: "failed"}[len(failures) == 0])
	if len(failures) > 0 {
		return &thresholdError{failures: failures}
	}
	return nil
}
