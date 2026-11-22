// Command archiver is the terminal entry point for the archival recovery
// pipeline. It has one demonstrated workflow, `demo`, which generates a
// self-contained corpus, runs the pipeline (optionally interrupting mid-way),
// resumes, and prints the acceptance metrics. Exit codes follow the delivery
// contract: 0 pass, 2 input/mapping, 3 remux/verify, 4 checkpoint.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mediabridge/internal/fault"
	"mediabridge/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mediabridge archiver — lossless media recovery

usage:
  archiver demo   [flags]   generate corpus, run, interrupt, resume, verify
  archiver verify [flags]   run the pipeline over an existing manifest

demo flags:
  --seed N              fixture seed (default 41)
  --interrupt-after N   commit N packets then simulate a crash, resume after
  --resume              enable the interrupt+resume cycle
  --loss F              RTP loss probability (default 0)
  --reorder F           RTP reorder probability (default 0)
  --out DIR             output directory (default ./archive-demo)
`)
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "fixture seed")
	interruptAfter := fs.Int64("interrupt-after", 0, "commit N packets then crash")
	resume := fs.Bool("resume", false, "interrupt then resume")
	loss := fs.Float64("loss", 0, "RTP loss probability")
	reorder := fs.Float64("reorder", 0, "RTP reorder probability")
	outDir := fs.String("out", "archive-demo", "output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *resume && *interruptAfter <= 0 {
		*interruptAfter = 1
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error_code=%s: %v\n", fault.CodePath, err)
		return 4
	}

	// 1. Generate the self-contained corpus.
	demo, err := pipeline.GenerateDemo(pipeline.DemoOptions{
		Seed: *seed, Loss: *loss, Reorder: *reorder, RootDir: *outDir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error_code=%s: %v\n", fault.CodeCorruptContainer, err)
		return 2
	}
	fmt.Printf("demo_corpus seed=%d assets=%d manifest=%s\n", *seed, demo.Assets, demo.ManifestPath)
	fmt.Printf("corpus_packets video=%d audio=%d aux=%d rtp_delivered=%d rtp_lost=%d\n",
		demo.VideoPackets, demo.AudioPackets, demo.AuxPackets, demo.RTPDelivered, demo.RTPLost)

	outMedia := filepath.Join(*outDir, "out")

	// 2. Optional interrupted run.
	var interrupted bool
	if *resume && *interruptAfter > 0 {
		fmt.Printf("phase=interrupted_run interrupt_after=%d\n", *interruptAfter)
		results, runErr := pipeline.Run(demo.ManifestPath, pipeline.Options{
			OutDir: outMedia, InterruptAfter: *interruptAfter,
		})
		interrupted = runErr.Interrupted
		for _, r := range results {
			fmt.Printf("  asset=%s status=%s %s\n", r.AssetID, r.Status, summary(r))
		}
		if runErr.Fatal != nil {
			fmt.Fprintf(os.Stderr, "fatal: %v\n", runErr.Fatal)
			return fault.ExitCode(runErr.Fatal)
		}
		fmt.Printf("phase=interrupted_run end resumed_needed=%v\n", interrupted)
	}

	// 3. Resume / full run.
	fmt.Printf("phase=final_run\n")
	results, runErr := pipeline.Run(demo.ManifestPath, pipeline.Options{OutDir: outMedia})
	if runErr.Fatal != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", runErr.Fatal)
		return fault.ExitCode(runErr.Fatal)
	}

	var accepted, rejected, resumed int
	var metrics pipeline.Metrics
	var firstRejected *fault.Error
	for _, r := range results {
		switch r.Status {
		case "rejected":
			rejected++
			if firstRejected == nil {
				firstRejected = r.Error
			}
			fmt.Printf("asset=%s status=rejected %s\n", r.AssetID, r.Error)
		default:
			accepted++
			if r.ResumedTracks > 0 || r.Quarantined > 0 {
				resumed++
			}
			metrics = aggregate(metrics, r)
			fmt.Printf("asset=%s status=%s %s\n", r.AssetID, r.Status, summary(r))
		}
	}

	fmt.Printf("\n== acceptance ==\n")
	fmt.Printf("assets_accepted=%d assets_rejected=%d assets_resumed=%d\n", accepted, rejected, resumed)
	fmt.Printf("sync_error_ms=%.4f\n", metrics.SyncErrorMS)
	fmt.Printf("packet_recovery_rate=%.4f\n", metrics.PacketRecoveryRate)
	fmt.Printf("p95_latency_ms=%.3f\n", metrics.P95LatencyMS)
	fmt.Printf("copied_packet_hash_match=%.2f%%\n", metrics.CopiedPacketHashMatchPct)
	fmt.Printf("resumed_duplicate_packets=%d\n", metrics.ResumedDuplicatePackets)
	fmt.Printf("provenance_coverage=%.2f%%\n", metrics.ProvenanceCoveragePct)
	fmt.Printf("recovered_assets=%d\n", accepted)

	// Report the corrupted sample's located error.
	if firstRejected != nil {
		fmt.Printf("\n== corrupt_sample ==\n")
		fmt.Printf("error_code=%s\n", firstRejected.Code)
		if firstRejected.Loc.HasOffset {
			fmt.Printf("source_offset=%d\n", firstRejected.Loc.Offset)
		} else {
			fmt.Printf("source_offset=0\n")
		}
		fmt.Printf("source_path=%s\n", firstRejected.Loc.SourcePath)
		fmt.Printf("recovered_assets=%d\n", accepted)
	}

	// Acceptance gate.
	failed := acceptanceFailures(metrics)
	if len(failed) > 0 {
		for _, f := range failed {
			fmt.Printf("FAIL %s\n", f)
		}
		return 3
	}
	if accepted == 0 {
		fmt.Println("FAIL no assets accepted")
		return 3
	}
	fmt.Println("RESULT=PASS")
	return 0
}

// Metrics is a flattened acceptance view across assets.
type Metrics = pipeline.Metrics

func aggregate(acc pipeline.Metrics, r pipeline.AssetResult) pipeline.Metrics {
	m := r.Metrics
	if m.SyncErrorMS > acc.SyncErrorMS {
		acc.SyncErrorMS = m.SyncErrorMS
	}
	if m.P95LatencyMS > acc.P95LatencyMS {
		acc.P95LatencyMS = m.P95LatencyMS
	}
	if m.PacketRecoveryRate > 0 && (acc.PacketRecoveryRate == 0 || m.PacketRecoveryRate < acc.PacketRecoveryRate) {
		acc.PacketRecoveryRate = m.PacketRecoveryRate
	}
	if m.CopiedPacketHashMatchPct > 0 && (acc.CopiedPacketHashMatchPct == 0 || m.CopiedPacketHashMatchPct < acc.CopiedPacketHashMatchPct) {
		acc.CopiedPacketHashMatchPct = m.CopiedPacketHashMatchPct
	}
	if m.ProvenanceCoveragePct > 0 && (acc.ProvenanceCoveragePct == 0 || m.ProvenanceCoveragePct < acc.ProvenanceCoveragePct) {
		acc.ProvenanceCoveragePct = m.ProvenanceCoveragePct
	}
	acc.ResumedDuplicatePackets += m.ResumedDuplicatePackets
	return acc
}

func summary(r pipeline.AssetResult) string {
	return fmt.Sprintf("tracks_resumed=%d quarantined=%d hash_match=%.0f%% coverage=%.0f%% recovery=%.4f sync_ms=%.3f latency_ms=%.2f",
		r.ResumedTracks, r.Quarantined, r.Metrics.CopiedPacketHashMatchPct,
		r.Metrics.ProvenanceCoveragePct, r.Metrics.PacketRecoveryRate,
		r.Metrics.SyncErrorMS, r.Metrics.P95LatencyMS)
}

func acceptanceFailures(m Metrics) []string {
	var out []string
	if m.SyncErrorMS > 1.0 {
		out = append(out, fmt.Sprintf("sync_error_ms %.3f > 1.0", m.SyncErrorMS))
	}
	if m.PacketRecoveryRate < 0.97 {
		out = append(out, fmt.Sprintf("packet_recovery_rate %.4f < 0.97", m.PacketRecoveryRate))
	}
	if m.P95LatencyMS > 250 {
		out = append(out, fmt.Sprintf("p95_latency_ms %.1f > 250", m.P95LatencyMS))
	}
	if m.CopiedPacketHashMatchPct < 100 {
		out = append(out, fmt.Sprintf("copied_packet_hash_match %.2f%% < 100%%", m.CopiedPacketHashMatchPct))
	}
	if m.ResumedDuplicatePackets != 0 {
		out = append(out, fmt.Sprintf("resumed_duplicate_packets %d != 0", m.ResumedDuplicatePackets))
	}
	if m.ProvenanceCoveragePct < 100 {
		out = append(out, fmt.Sprintf("provenance_coverage %.2f%% < 100%%", m.ProvenanceCoveragePct))
	}
	return out
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	outDir := fs.String("out", "archive-out", "output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "verify requires a manifest path")
		return 2
	}
	results, runErr := pipeline.Run(fs.Arg(0), pipeline.Options{OutDir: *outDir})
	if runErr.Fatal != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", runErr.Fatal)
		return fault.ExitCode(runErr.Fatal)
	}
	code := 0
	for _, r := range results {
		if r.Status == "rejected" {
			fmt.Printf("asset=%s rejected %v\n", r.AssetID, r.Error)
			if code == 0 {
				code = fault.ExitCode(r.Error)
			}
			continue
		}
		fmt.Printf("asset=%s ok hash_match=%.0f%% coverage=%.0f%%\n",
			r.AssetID, r.Metrics.CopiedPacketHashMatchPct, r.Metrics.ProvenanceCoveragePct)
	}
	return code
}
