// Package cli implements the terminal commands and exit-code contract:
//
//	0 - verification passed, report complete
//	1 - input damage found, but the report itself is complete
//	2 - bad arguments, unreadable input or unusable checkpoint
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediascan/internal/checkpoint"
	"mediascan/internal/clock"
	"mediascan/internal/fixture"
	"mediascan/internal/pipeline"
	"mediascan/internal/report"
)

// Exit codes per the acceptance contract.
const (
	ExitOK       = 0
	ExitDamaged  = 1
	ExitBadUsage = 2
)

// manifestName records the exact input list and params so resume can rebuild
// the pipeline.Input slice without re-scanning a directory.
const manifestName = "manifest.json"

type manifest struct {
	Params pipeline.Params  `json:"params"`
	Inputs []pipeline.Input `json:"inputs"`
}

// Run dispatches the subcommand and returns the process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		return usage("missing subcommand")
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "resume":
		return runResume(args[1:])
	case "-h", "--help", "help":
		printHelp()
		return ExitOK
	default:
		return usage("unknown subcommand %q", args[0])
	}
}

func usage(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n\n", args...)
	printHelp()
	return ExitBadUsage
}

func printHelp() {
	fmt.Fprint(os.Stderr,
		"mediascan - local media damage scanner\n\n"+
			"usage:\n"+
			"  mediascan demo --out DIR                 generate fixtures and run a full scan\n"+
			"  mediascan resume --dir DIR [--interrupt-after N]\n"+
			"                                            resume or interrupt a scan in DIR\n\n"+
			"exit codes: 0 ok, 1 damage found (report complete), 2 bad arguments/input/checkpoint\n")
}

// parseFlags turns --key value pairs into a map, rejecting unknown flags.
func parseFlags(args []string, allowed map[string]bool) (map[string]string, int) {
	flags := make(map[string]string)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 3 || a[:2] != "--" || !allowed[a[2:]] {
			return nil, ExitBadUsage
		}
		if i+1 >= len(args) {
			return nil, ExitBadUsage
		}
		flags[a[2:]] = args[i+1]
		i++
	}
	return flags, ExitOK
}

func runDemo(args []string) int {
	flags, code := parseFlags(args, map[string]bool{"out": true})
	if code != 0 {
		return usage("demo requires --out DIR")
	}
	out := flags["out"]
	if out == "" {
		return usage("--out is required")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return failSetup("cannot create output dir: %v", err)
	}
	set, err := fixture.Generate(out)
	if err != nil {
		return failSetup("fixture generation failed: %v", err)
	}
	params := pipeline.Params{Rate: clock.Rate25}
	inputs := buildInputs(set)
	if err := writeManifest(out, params, inputs); err != nil {
		return failSetup("manifest write failed: %v", err)
	}

	fmt.Println("== mediascan demo: full (uninterrupted) scan ==")
	full, fullCode := executeScan(out, inputs, params, 0, 0)
	if fullCode == ExitBadUsage {
		return fullCode
	}
	printReport("full-scan", full, 0)

	// A second identical scan must produce the identical checksum.
	repeat, _ := executeScan(filepath.Join(out, "repeat"), inputs, params, 0, 0)
	if report.Build(repeat).SHA256 != report.Build(full).SHA256 {
		fmt.Fprintln(os.Stderr, "error: deterministic checksum mismatch between repeated runs")
		return ExitBadUsage
	}
	fmt.Println("determinism: repeated run produced identical sha256")
	return fullCode
}

func runResume(args []string) int {
	flags, code := parseFlags(args, map[string]bool{"dir": true, "interrupt-after": true})
	if code != 0 {
		return usage("resume requires --dir DIR")
	}
	dir := flags["dir"]
	if dir == "" {
		return usage("--dir is required")
	}
	interruptAfter := 0
	if v := flags["interrupt-after"]; v != "" {
		if _, err := fmt.Sscanf(v, "%d", &interruptAfter); err != nil || interruptAfter < 0 {
			return usage("--interrupt-after must be a non-negative integer")
		}
	}
	mf, err := loadManifest(dir)
	if err != nil {
		return failSetup("manifest unavailable: %v", err)
	}
	fmt.Printf("== mediascan resume: dir=%s interrupt-after=%d ==\n", dir, interruptAfter)

	// A demo directory may already hold a complete checkpoint. To still
	// demonstrate the interrupt -> resume lifecycle on demand, run the
	// interrupted pass in an isolated workspace that shares the manifest and
	// inputs, then resume that same workspace to completion.
	scanDir := dir
	didInterrupt := false
	existing, err := checkpoint.Load(dir)
	if err != nil {
		return failSetup("checkpoint unusable: %v", err)
	}
	if interruptAfter > 0 && existing != nil && existing.CursorIndex >= len(mf.Inputs) {
		scanDir = filepath.Join(dir, "interrupted-run")
		if err := os.MkdirAll(scanDir, 0o755); err != nil {
			return failSetup("cannot create interrupt workspace: %v", err)
		}
		if err := writeManifest(scanDir, mf.Params, mf.Inputs); err != nil {
			return failSetup("interrupt manifest write failed: %v", err)
		}
		fmt.Printf("interrupt pass: fresh workspace %s, stopping after %d segments\n", scanDir, interruptAfter)
		intRes, intCode := executeScan(scanDir, mf.Inputs, mf.Params, interruptAfter, 0)
		if intCode == ExitBadUsage {
			return intCode
		}
		printReport("interrupted", intRes, 0)
		if intRes.Interrupted {
			didInterrupt = true
		}
	}

	start := time.Now()
	// In the isolated-demo path the interruption already happened, so resume
	// runs to completion. Otherwise honor the requested interrupt limit.
	resumeLimit := interruptAfter
	if didInterrupt {
		resumeLimit = 0
	}
	res, code := executeScan(scanDir, mf.Inputs, mf.Params, resumeLimit, start.UnixMilli())
	if code == ExitBadUsage {
		return code
	}
	resumeMS := time.Since(start).Milliseconds()
	printReport("resume", res, resumeMS)

	// On a completed resume, compare against an uninterrupted reference run.
	if !res.Interrupted {
		refDir := filepath.Join(dir, "reference")
		ref, refCode := executeScan(refDir, mf.Inputs, mf.Params, 0, 0)
		if refCode == ExitBadUsage {
			return refCode
		}
		if report.Build(res).SHA256 != report.Build(ref).SHA256 {
			fmt.Fprintln(os.Stderr, "error: resumed report checksum differs from uninterrupted run")
			return ExitBadUsage
		}
		fmt.Println("resume integrity: checksum matches uninterrupted reference run")
	}
	if res.Interrupted {
		// A clean interruption is an expected stop, not input damage.
		return ExitOK
	}
	if didInterrupt {
		return ExitOK
	}
	return code
}

// executeScan runs the pipeline and maps its outcome onto an exit code.
func executeScan(dir string, inputs []pipeline.Input, params pipeline.Params,
	interruptAfter int, _ int64) (*pipeline.Result, int) {
	res, err := pipeline.Run(dir, inputs, params, interruptAfter)
	if err != nil {
		if errors.Is(err, pipeline.ErrInterrupted) {
			return res, ExitOK // an interruption is a clean, expected stop
		}
		// Every other failure (bad input, fingerprint/params mismatch,
		// corrupt checkpoint) maps to the exit-code-2 class.
		fmt.Fprintf(os.Stderr, "error: scan cannot proceed: %v\n", err)
		return nil, ExitBadUsage
	}
	// Damage present but report complete -> exit 1; otherwise 0.
	if report.FirstDamageOffset(res.Faults) >= 0 || res.RecoveredSegments > 0 {
		return res, ExitDamaged
	}
	return res, ExitOK
}

func printReport(label string, r *pipeline.Result, resumeMS int64) {
	if r == nil {
		return
	}
	damage := report.FirstDamageOffset(r.Faults)
	fmt.Printf("[%s]\n", label)
	for _, line := range report.KeyLines(r, resumeMS, damage) {
		fmt.Println("  " + line)
	}
	sum := report.Build(r)
	fmt.Println("  --- deterministic summary ---")
	fmt.Print(indent(sum.Text))
}

func indent(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		if line != "" {
			out += "  " + line + "\n"
		}
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func buildInputs(set *fixture.Set) []pipeline.Input {
	return []pipeline.Input{
		{Path: set.GoodSRT, Kind: pipeline.KindSRT},
		{Path: set.GoodVTT, Kind: pipeline.KindVTT},
		{Path: set.HalfLineSRT, Kind: pipeline.KindSRT},
		{Path: set.WrapSRT, Kind: pipeline.KindSRT},
		{Path: set.GoodMP4, Kind: pipeline.KindMP4},
		{Path: set.TruncatedMP4, Kind: pipeline.KindMP4},
		{Path: set.GoodRTP, Kind: pipeline.KindRTP},
	}
}

func writeManifest(dir string, params pipeline.Params, inputs []pipeline.Input) error {
	data, err := json.MarshalIndent(manifest{Params: params, Inputs: inputs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".manifest.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, manifestName))
}

func loadManifest(dir string) (*manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if len(m.Inputs) == 0 {
		return nil, errors.New("manifest contains no inputs")
	}
	return &m, nil
}

func failSetup(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	return ExitBadUsage
}
