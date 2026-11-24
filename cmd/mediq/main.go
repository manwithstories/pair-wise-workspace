// Command mediq runs local RTP/RTCP ingestion, timeline, DSP, MP4 packaging
// and recovery demonstrations.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/clearliu/mediq/internal/core"
	"github.com/clearliu/mediq/internal/manifest"
	"github.com/clearliu/mediq/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprintln(stderr, "usage: mediq demo --case <clean|damaged|recovery> --out DIR [--resume] [--strict] [--crash-after checkpoint]")
		return 1
	}

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	caseName := fs.String("case", "", "demo case: clean, damaged, or recovery")
	outDir := fs.String("out", "", "output directory")
	resume := fs.Bool("resume", false, "resume from the atomic checkpoint")
	strict := fs.Bool("strict", false, "reject damaged input with exit code 2")
	crashAfter := fs.String("crash-after", "", "fault injection point: checkpoint")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}

	m, err := pipeline.Run(pipeline.Options{
		Case:       *caseName,
		OutDir:     *outDir,
		Resume:     *resume,
		Strict:     *strict,
		CrashAfter: *crashAfter,
	})
	if m != nil {
		printReport(stdout, m)
	}
	if err != nil {
		code := core.ExitCode(err)
		fmt.Fprintf(stderr, "error kind=%v exit_code=%d: %v\n", core.KindOf(err), code, err)
		if code == 75 {
			fmt.Fprintln(stdout, "injected_interrupt=checkpoint resumable=true")
		}
		return code
	}

	fmt.Fprintln(stdout, "status=completed exit_code=0")
	return 0
}

func printReport(w io.Writer, m *manifest.Manifest) {
	fmt.Fprintf(w, "case=%s\n", m.Source.Case)
	fmt.Fprintf(w, "target_bps=%d\n", m.Quality.TargetBPS)
	fmt.Fprintf(w, "actual_bps=%.3f\n", m.Quality.ActualBPS)
	fmt.Fprintf(w, "deviation_pct=%.6f\n", m.Quality.DeviationPct)
	fmt.Fprintf(w, "missing_samples=%d\n", m.Counts.MissingSamples)
	if len(m.Counts.MissingSeqRanges) > 0 {
		fmt.Fprintf(w, "missing_seq_ranges=%s\n", strings.Join(m.Counts.MissingSeqRanges, ","))
	}
	fmt.Fprintf(w, "duplicate_packets=%d\n", m.Counts.DuplicatePackets)
	fmt.Fprintf(w, "late_replays=%d\n", m.Counts.LateReplays)
	fmt.Fprintf(w, "late_expired=%d\n", m.Counts.LateExpired)
	fmt.Fprintf(w, "integrated_lufs=%.3f\n", m.Quality.IntegratedLUFS)
	fmt.Fprintf(w, "true_peak_dbtp=%.3f\n", m.Quality.TruePeakDBTP)
	fmt.Fprintf(w, "manifest_error_ms=%.3f\n", m.Quality.ManifestErrorMS)
	fmt.Fprintf(w, "clock_model=%s clock_drift_ppm=%.3f sender_reports=%d\n",
		m.Quality.ClockModel, m.Quality.ClockDriftPPM, m.Quality.SenderReports)
	fmt.Fprintf(w, "rtp_frames=%d rtcp_frames=%d parse_errors=%d\n",
		m.Counts.RTPFrames, m.Counts.RTCPFrames, m.Counts.ParseErrors)
	fmt.Fprintf(w, "gap_packets=%d late_packets=%d out_of_order_packets=%d\n",
		m.Counts.GapPackets, m.Counts.LatePackets, m.Counts.OutOfOrder)
	if m.FirstParseErrorOffset >= 0 {
		fmt.Fprintf(w, "first_error_offset=%d\n", m.FirstParseErrorOffset)
		fmt.Fprintf(w, "parse_error_offset=%d\n", m.FirstParseErrorOffset)
		fmt.Fprintf(w, "first_parse_error_offset=%d\n", m.FirstParseErrorOffset)
		fmt.Fprintf(w, "parse_error=%q\n", m.FirstParseError)
	}
	fmt.Fprintf(w, "rtp_seq_range=%d-%d extended_seq_range=%d-%d\n",
		m.Source.RTPSeqFirst, m.Source.RTPSeqLast, m.Source.ExtendedSeqFirst, m.Source.ExtendedSeqLast)
	fmt.Fprintf(w, "time_range_ms=%.3f-%.3f\n", m.Source.StartTimeMS, m.Source.EndTimeMS)
	fmt.Fprintf(w, "committed_output_samples=%d\n", m.CommittedOutputSamples)
	if m.Product.SHA256 != "" {
		fmt.Fprintf(w, "product_file=%s\n", m.Product.File)
		fmt.Fprintf(w, "product_sha256=%s\n", m.Product.SHA256)
		fmt.Fprintf(w, "product_bytes=%d\n", m.Product.Bytes)
	}
	if m.CheckpointSHA256 != "" {
		fmt.Fprintf(w, "checkpoint_sha256=%s\n", m.CheckpointSHA256)
	}
}
