// Command mediqc runs deterministic media quality verification and remux jobs.
package main

import (
	"flag"
	"fmt"
	"os"

	"mediqc/internal/audit"
	"mediqc/internal/media"
	"mediqc/internal/pipeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 1
	}
	switch args[0] {
	case "run":
		return runBatch(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		printUsage(os.Stderr)
		return 1
	}
}

func runBatch(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seed := fs.Int64("seed", 0, "deterministic fixture seed")
	suite := fs.String("suite", "all", "suite to execute (currently: all)")
	out := fs.String("out", "", "output directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "run: --out is required")
		return 1
	}

	manifest, err := pipeline.Run(pipeline.Options{Seed: *seed, Suite: *suite, Out: *out})
	if code := handlePipelineError(err); code != 0 {
		return code
	}
	verified, verr := audit.VerifyDirectory(*out)
	if verr != nil {
		printMediaError("verify", verr)
		return exitCodeFor(verr)
	}
	printRunSummary(manifest, verified.ManifestHash, *out)
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "", "run output directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "verify: --dir is required")
		return 1
	}
	verified, err := audit.VerifyDirectory(*dir)
	if err != nil {
		printMediaError("verify", err)
		return exitCodeFor(err)
	}
	m := verified.Manifest
	fmt.Printf("verify OK dir=%s\n", *dir)
	fmt.Printf("manifest_sha256=%s\n", verified.ManifestHash)
	fmt.Printf("cases=%d clean=%d repaired=%d rejected=%d\n", m.Cases, m.Clean, m.Repaired, m.Rejected)
	fmt.Printf("sample_hash_match=%s manifest_skew_ms=%d replay_sha256_equal=%t\n",
		m.SampleHashMatch, m.ManifestSkewMS, m.ReplayHashEqual)
	return 0
}

func printRunSummary(m *audit.SuiteManifest, hash, out string) {
	fmt.Println("mediqc run OK")
	fmt.Printf("out=%s\n", out)
	fmt.Printf("cases=%d clean=%d repaired=%d rejected=%d\n", m.Cases, m.Clean, m.Repaired, m.Rejected)
	fmt.Printf("cc_gaps=%d psi_switches=%d pcr_wraps=%d late_replays=%d\n",
		m.CCGaps, m.PSISwitches, m.PCRWraps, m.LateReplays)
	if m.RTPDuplicates > 0 || m.RTPOutsideWindow > 0 {
		fmt.Printf("rtp_duplicates=%d rtp_outside_window=%d\n", m.RTPDuplicates, m.RTPOutsideWindow)
	}
	fmt.Printf("bitrate_target_bps=%d bitrate_actual_bps=%d bitrate_delta_ppm=%d\n",
		m.BitrateTargetBPS, m.BitrateActualBPS, m.BitrateDeltaPPM)
	fmt.Printf("sample_hash_match=%s manifest_skew_ms=%d replay_sha256_equal=%t\n",
		m.SampleHashMatch, m.ManifestSkewMS, m.ReplayHashEqual)
	fmt.Printf("manifest_sha256=%s\n", hash)
}

func handlePipelineError(err *media.Error) int {
	if err == nil {
		return 0
	}
	printMediaError("run", err)
	return exitCodeFor(err)
}

func exitCodeFor(err *media.Error) int {
	if err == nil {
		return 0
	}
	switch err.Code {
	case media.CodeUnsupported, media.CodeMappingInvalid:
		return 2
	default:
		return 1
	}
}

func printMediaError(prefix string, err *media.Error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "%s: code=%s recoverable=%t fatal=%t msg=%s\n",
		prefix, err.Code, err.Recoverable, err.Fatal, err.Message)
}

func printUsage(w *os.File) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  mediqc run --seed N --suite all --out DIR")
	fmt.Fprintln(w, "  mediqc verify --dir DIR")
	fmt.Fprintln(w, "  mediqc inspect --input FILE.ts")
}
