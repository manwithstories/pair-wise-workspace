// Command archivectl is the terminal-only archiver: it generates local
// fixtures (demo), independently verifies a workspace (verify) and can
// scan a single fragment for structural damage (scan-file).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"archivectl/internal/audit"
	"archivectl/internal/demo"
	"archivectl/internal/fault"
	"archivectl/internal/layout"
	"archivectl/internal/pipeline"
	"archivectl/internal/scan"
)

const usage = `archivectl - cold-storage timeline repair and random-access index

usage:
  archivectl demo --root DIR [--seed N] [--segments N] \
      [--fault-profile boundary-v1] [--resume-check]
  archivectl verify --root DIR [--repeat N]
  archivectl scan-file --path FILE

exit codes: 0 pass, 2 bad arguments, 3 unrecoverable structural damage,
            4 verification failure
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "scan-file":
		return runScanFile(args[1:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	root := fs.String("root", "", "workspace root")
	seed := fs.Int64("seed", 41, "deterministic fixture seed")
	segments := fs.Int("segments", 48, "segments per track")
	profile := fs.String("fault-profile", demo.ProfileBoundaryV1, "fault profile")
	resumeCheck := fs.Bool("resume-check", false, "inject a crash and verify resume")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		fmt.Fprintln(os.Stderr, "demo: --root is required")
		return 2
	}
	if *segments < 4 {
		fmt.Fprintln(os.Stderr, "demo: --segments must be >= 4")
		return 2
	}
	if *profile != demo.ProfileBoundaryV1 {
		fmt.Fprintf(os.Stderr, "demo: unsupported --fault-profile %q\n", *profile)
		return 2
	}

	if err := demo.Generate(*root, *segments, *seed, *profile); err != nil {
		fmt.Fprintf(os.Stderr, "demo: generate failed: %v\n", err)
		return 2
	}

	for _, group := range []string{"clean", "damaged"} {
		var (
			rep *pipeline.Report
			err error
		)
		if group == "damaged" && *resumeCheck {
			rep, err = pipeline.ResumeCheck(*root, group, *segments)
		} else {
			rep, err = pipeline.Run(*root, group, pipeline.Options{})
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo: run %s failed: %v\n", group, err)
			return fault.ExitCode(err)
		}
		if b, err := json.MarshalIndent(rep, "", "  "); err == nil {
			_ = os.WriteFile(layout.ReportPath(*root, group), b, 0o644)
		}
		fmt.Printf("[demo/%s] chunks=%d written=%d skipped=%d index_sha256=%s resume_check=%v\n",
			group, rep.ChunkCount, rep.Written, rep.Skipped, rep.IndexSHA256, *resumeCheck && group == "damaged")
	}
	fmt.Println("demo: generated clean and damaged workspaces")
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	root := fs.String("root", "", "workspace root produced by demo")
	repeat := fs.Int("repeat", 2, "number of deterministic index rebuilds to compare")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		fmt.Fprintln(os.Stderr, "verify: --root is required")
		return 2
	}
	if *repeat < 1 {
		fmt.Fprintln(os.Stderr, "verify: --repeat must be >= 1")
		return 2
	}

	worst := 0
	for _, group := range []string{"clean", "damaged"} {
		m, err := audit.Verify(*root, group, *repeat)
		fmt.Printf("===== group: %s =====\n", group)
		b, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(b))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] verify failed: %v\n", group, err)
			if code := fault.ExitCode(err); code == 3 {
				worst = 3
			} else if worst == 0 {
				worst = 4
			}
		}
	}
	if worst == 0 {
		fmt.Println("verify: PASS - metrics and invariants hold for clean and damaged groups")
	}
	return worst
}

func runScanFile(args []string) int {
	fs := flag.NewFlagSet("scan-file", flag.ContinueOnError)
	path := fs.String("path", "", "fragment file to scan")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(os.Stderr, "scan-file: --path is required")
		return 2
	}
	rep, err := scan.ScanFile(*path, "cli")
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan-file: %v\n", err)
		return 4
	}
	for _, f := range rep.Record.Faults {
		fmt.Println(f.Error())
	}
	if rep.Record.HasFatal() {
		fmt.Fprintf(os.Stderr, "scan-file: %d structural fault(s)\n", len(rep.Record.Faults))
		return 3
	}
	fmt.Printf("scan-file: %d fragment(s), no structural faults\n", len(rep.Fragments))
	return 0
}
