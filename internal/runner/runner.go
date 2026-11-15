package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	loopice "rtprelay/internal/ice"
	"rtprelay/internal/relay"
	"rtprelay/internal/report"
	"rtprelay/internal/signal"
)

const DefaultConnectTimeout = 15 * time.Second

type Options struct {
	Duration        time.Duration
	PPS             int
	Seed            int64
	Jitter          time.Duration
	Faults          relay.Faults
	InjectCandidate string
	MaxJitterMS     float64
	Out             string
	PacketLog       string
	Timeout         time.Duration
	ClockRate       uint32
}

type Result struct {
	Report   report.Report
	ExitCode int
}

func Run(ctx context.Context, opts Options) Result {
	return runSelfTest(ctx, opts)
}

func normalize(opts Options) Options {
	if opts.Out == "" {
		opts.Out = filepath.Join("out", "report.json")
	}
	if opts.PacketLog == "" {
		opts.PacketLog = filepath.Join(filepath.Dir(opts.Out), "packets.jsonl")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultConnectTimeout
	}
	if opts.ClockRate == 0 {
		opts.ClockRate = 48000
	}
	return opts
}

func baseReport(opts Options) report.Report {
	return report.Report{
		Schema: "rtprelay.report.v1", Mode: "selftest", OK: true,
		DurationMS: opts.Duration.Milliseconds(), PPS: opts.PPS, Seed: opts.Seed,
		JitterUS: opts.Jitter.Microseconds(), Faults: opts.Faults,
		SignalVersion: signal.ProtocolVersion,
	}
}

func fail(base report.Report, code int, err error) Result {
	base.OK = false
	if err != nil {
		base.Error = err.Error()
	}
	base.SyncStats()
	return Result{Report: base, ExitCode: code}
}

func injectedRejection(base report.Report, candidate string) Result {
	base.Gathered = 1
	base.Accepted = 0
	base.Rejected = 1
	base.RejectedCandidates = 1
	base.RejectedAddresses = []string{candidate}
	base.PairsEvaluated = 0
	base.PairsSucceeded = 0
	return fail(base, 4, fmt.Errorf("injected candidate %s rejected: not loopback", candidate))
}

func validateInjected(base report.Report, candidate string) (report.Report, *Result) {
	if strings.TrimSpace(candidate) == "" {
		return base, nil
	}
	if _, err := loopice.Parse(candidate); err != nil {
		result := injectedRejection(base, candidate)
		return base, &result
	}
	return base, nil
}

func sdpFingerprint(sdp string) string {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=fingerprint:") {
			return strings.TrimPrefix(line, "a=fingerprint:")
		}
	}
	return ""
}
