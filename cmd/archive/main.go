// Command archive reconstructs an explicit media timeline from bare bitstreams
// and sidecar keyframe indexes, and emits an auditable multi-rendition segment
// manifest. See README or run with -h for usage.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"archive.local/recover/internal/pipeline"
	"archive.local/recover/internal/report"
	"archive.local/recover/internal/resume"
	"archive.local/recover/internal/testkit"
)

// Exit codes are a stable contract with automation.
const (
	exitOK            = 0
	exitUsage         = 2
	exitPartial       = 3
	exitUnrecoverable = 4
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain is testable: it never calls os.Exit and writes only to the given
// streams.
func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: archive demo [flags]")
		fmt.Fprintln(stderr, "       archive verify [flags]")
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `archive - recover an auditable media timeline and segment manifest

USAGE
  archive demo [flags]      generate a self-contained sample and process it
  archive verify [flags]    process an existing fixture directory

DEMO FLAGS
  --seed N                 deterministic seed (default 17)
  --duration SECONDS       capture duration (default 120)
  --bitrates a,b,c         rendition bitrates in kbps (default 800,1600,3200)
  --loss F                 RTP packet loss in [0,1) (default 0.03)
  --case NAME              success | corrupt-index | wrap-vfr-drift
  --json                   emit machine-readable JSON (default when --case given)
  --out DIR                also write manifest.json and resume.state here
  --resume                 reuse a matching run journal in --out

EXIT CODES
  0  processed completely
  2  usage error
  3  corrupt input, partial report produced
  4  unrecoverable dependency or internal error
`)
}

type demoFlags struct {
	seed        int64
	durationSec int
	bitrates    string
	loss        float64
	caseName    string
	json        bool
	outDir      string
	resume      bool
}

func parseDemoFlags(args []string, stderr io.Writer) (*demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := &demoFlags{}
	fs.Int64Var(&f.seed, "seed", 17, "deterministic seed")
	fs.IntVar(&f.durationSec, "duration", 120, "capture duration in seconds")
	fs.StringVar(&f.bitrates, "bitrates", "800,1600,3200", "comma-separated bitrates in kbps")
	fs.Float64Var(&f.loss, "loss", 0.03, "RTP loss rate in [0,1)")
	fs.StringVar(&f.caseName, "case", "", "success | corrupt-index | wrap-vfr-drift")
	fs.BoolVar(&f.json, "json", false, "force JSON output")
	fs.StringVar(&f.outDir, "out", "", "directory for manifest and resume journal")
	fs.BoolVar(&f.resume, "resume", false, "reuse a matching run journal")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return f, nil
}

func (f *demoFlags) config() (pipeline.Config, error) {
	cfg := pipeline.DefaultConfig()
	if f.durationSec <= 0 || f.durationSec > 6*3600 {
		return cfg, fmt.Errorf("--duration must be in 1..21600 seconds")
	}
	if f.loss < 0 || f.loss >= 1 {
		return cfg, fmt.Errorf("--loss must be in [0,1)")
	}
	if f.seed < 0 {
		return cfg, fmt.Errorf("--seed must be non-negative")
	}
	bits, err := parseBitrates(f.bitrates)
	if err != nil {
		return cfg, err
	}
	c := f.caseName
	if c == "" {
		c = testkit.CaseSuccess
	}
	switch c {
	case testkit.CaseSuccess, testkit.CaseCorruptIndex, testkit.CaseWrapVFRDrift:
	default:
		return cfg, fmt.Errorf("--case must be success, corrupt-index or wrap-vfr-drift")
	}
	cfg.Seed = f.seed
	cfg.DurationNs = int64(f.durationSec) * 1_000_000_000
	cfg.Bitrates = bits
	cfg.Loss = f.loss
	cfg.Case = c
	return cfg, nil
}

func parseBitrates(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	if len(parts) == 0 || len(parts) > 8 {
		return nil, fmt.Errorf("--bitrates must list 1..8 values")
	}
	out := make([]int, 0, len(parts))
	seen := map[int]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil || v <= 0 || v > 100_000 {
			return nil, fmt.Errorf("invalid bitrate %q (want 1..100000 kbps)", p)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--bitrates must list at least one value")
	}
	sort.Ints(out)
	return out, nil
}

func runDemo(args []string, stdout, stderr io.Writer) int {
	f, err := parseDemoFlags(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "archive demo: %v\n", err)
		return exitUsage
	}
	cfg, err := f.config()
	if err != nil {
		fmt.Fprintf(stderr, "archive demo: %v\n", err)
		return exitUsage
	}

	// Resume: check the journal before doing any work.
	var prev *resume.State
	var inputDigest, cfgDigest string
	if f.outDir != "" {
		inputDigest = pipeline.InputDigest(cfg)
		if d, err := resume.DigestConfig(cfg); err == nil {
			cfgDigest = d
		}
		prev, err = resume.Load(resumePath(f.outDir))
		if err != nil {
			fmt.Fprintf(stderr, "archive demo: resume state: %v\n", err)
			return exitUnrecoverable
		}
		if f.resume && prev != nil {
			d := resume.Check(prev, inputDigest, cfgDigest)
			if !d.Reuse {
				return emitRefused(stdout, stderr, cfg, d)
			}
			// Digests match: reuse the stored manifest instead of recomputing.
			rep, err := loadManifest(f.outDir)
			if err != nil {
				fmt.Fprintf(stderr, "archive demo: reuse manifest: %v\n", err)
				return exitUnrecoverable
			}
			rep.Resume = &report.ResumeReport{
				Used: true, InputDigest: inputDigest, ConfigDigest: cfgDigest,
				Reason: d.Reason,
			}
			rep.Finalize()
			if f.json || f.caseName != "" {
				if err := report.Write(stdout, rep); err != nil {
					return exitUnrecoverable
				}
			} else {
				printHuman(stdout, rep)
			}
			return rep.ExitCode
		}
	}

	res, err := pipeline.Run(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "archive demo: %v\n", err)
		return exitUnrecoverable
	}
	rep := res.Report
	rep.SchemaVersion = report.SchemaVersion
	if f.outDir != "" {
		rep.Resume = &report.ResumeReport{
			Used: false, InputDigest: res.InputDigest, ConfigDigest: cfgDigest,
			Reason: "fresh run",
		}
	}
	rep.Finalize()

	if f.outDir != "" {
		if err := writeOutputs(f.outDir, rep, inputDigest, cfgDigest); err != nil {
			fmt.Fprintf(stderr, "archive demo: write outputs: %v\n", err)
			return exitUnrecoverable
		}
	}

	useJSON := f.json || f.caseName != ""
	if useJSON {
		if err := report.Write(stdout, rep); err != nil {
			fmt.Fprintf(stderr, "archive demo: encode: %v\n", err)
			return exitUnrecoverable
		}
	} else {
		printHuman(stdout, rep)
	}
	return rep.ExitCode
}

// runVerify processes fixtures already on disk. The demo generator writes them
// under --out, so verify is the resume/acceptance path for stored artifacts.
func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "fixture directory written by demo --out")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "archive verify: %v\n", err)
		return exitUsage
	}
	if *dir == "" {
		fmt.Fprintln(stderr, "archive verify: --dir is required")
		return exitUsage
	}
	manifest := *dir + "/manifest.json"
	b, err := resume.ReadAll(manifest, 64<<20)
	if err != nil {
		fmt.Fprintf(stderr, "archive verify: %v\n", err)
		return exitUsage
	}
	var rep report.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		fmt.Fprintf(stderr, "archive verify: manifest is not a report: %v\n", err)
		return exitUnrecoverable
	}
	if *asJSON {
		if err := report.Write(stdout, &rep); err != nil {
			return exitUnrecoverable
		}
	} else {
		printHuman(stdout, &rep)
	}
	if rep.ExitCode == 0 && rep.OK {
		return exitOK
	}
	return rep.ExitCode
}

func emitRefused(stdout, stderr io.Writer, cfg pipeline.Config, d *resume.Decision) int {
	rec := map[string]any{
		"ok":            false,
		"exit_code":     exitUnrecoverable,
		"case":          cfg.Case,
		"seed":          cfg.Seed,
		"resume_reuse":  false,
		"input_digest":  d.InputDigest,
		"config_digest": d.ConfigDigest,
		"reason":        d.Reason,
		"issues":        d.Issues,
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	fmt.Fprintln(stdout, string(b))
	fmt.Fprintf(stderr, "archive demo: refusing to reuse stale products: %s\n", d.Reason)
	return exitUnrecoverable
}

func resumePath(dir string) string { return dir + "/resume.state" }

// loadManifest reads a manifest written by a previous run for reuse.
func loadManifest(dir string) (*report.Report, error) {
	b, err := resume.ReadAll(dir+"/manifest.json", 64<<20)
	if err != nil {
		return nil, err
	}
	var rep report.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, fmt.Errorf("manifest is not a report: %w", err)
	}
	return &rep, nil
}

func writeOutputs(dir string, rep *report.Report, inputDigest, cfgDigest string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	manifestBytes, err := report.Marshal(rep)
	if err != nil {
		return err
	}
	manifestPath := dir + "/manifest.json"
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return err
	}
	st := &resume.State{InputDigest: inputDigest, ConfigDigest: cfgDigest}
	st.MarkCompleted("pipeline", manifestPath, manifestBytes)
	return resume.Save(resumePath(dir), st)
}

func printHuman(w io.Writer, rep *report.Report) {
	fmt.Fprintf(w, "case: %s (seed %d)\n", rep.Case, rep.Seed)
	fmt.Fprintf(w, "result: %s (exit %d)\n", okLabel(rep.OK), rep.ExitCode)
	m := rep.Metrics
	fmt.Fprintf(w, "sync_error_ms: %.3f\n", m.SyncErrorMs)
	fmt.Fprintf(w, "loss_recovery_rate: %.4f\n", m.LossRecoveryRate)
	fmt.Fprintf(w, "p95_latency_ms: %.3f\n", m.P95LatencyMs)
	fmt.Fprintf(w, "segments: %d\n", m.Segments)
	fmt.Fprintf(w, "alignment_delta_ms: %.3f\n", m.AlignmentDeltaMs)
	if rep.RTP != nil {
		fmt.Fprintf(w, "rtp: received=%d duplicates=%d reordered=%d gaps=%d lost=%d\n",
			rep.RTP.Received, rep.RTP.Duplicates, rep.RTP.Reordered, rep.RTP.Gaps, rep.RTP.Lost)
	}
	if rep.Index != nil {
		fmt.Fprintf(w, "index: original=%d repaired=%d pending=%d dropped=%d missing_pages=%d\n",
			rep.Index.Original, rep.Index.Repaired, rep.Index.Pending, rep.Index.Dropped, rep.Index.Missing)
	}
	if rep.Tracks != nil && len(rep.Tracks.Stopped) > 0 {
		fmt.Fprintf(w, "stopped branches: %d\n", len(rep.Tracks.Stopped))
		for _, s := range rep.Tracks.Stopped {
			fmt.Fprintf(w, "  track %d (%s): %s\n", s.TrackID, s.Kind, s.Reason)
		}
	}
	if len(rep.Issues) > 0 {
		counts := rep.IssueCounts
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "issues: %d\n", len(rep.Issues))
		for _, k := range keys {
			fmt.Fprintf(w, "  %s: %d\n", k, counts[k])
		}
	}
}

func okLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "issues"
}

// Compile-time guard that the CLI's exit codes match the report contract.
var _ = func() bool {
	if exitOK != 0 || exitUsage != 2 || exitPartial != 3 || exitUnrecoverable != 4 {
		panic("exit code contract changed")
	}
	return true
}()

var errNoBitrates = errors.New("no bitrates")
