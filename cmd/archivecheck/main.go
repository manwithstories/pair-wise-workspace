// archivecheck is the terminal acceptance entry point for the archive
// restore-and-segment pipeline. All output is key=value lines.
//
// Exit codes: 0 = completed and verified, 2 = input corruption located,
// 3 = blocked dependency, 1 = runtime or verification failure.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"archivecheck/internal/fault"
	"archivecheck/internal/pipeline"
	"archivecheck/internal/scheduler"
)

func main() { os.Exit(run(os.Args)) }

func run(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: archivecheck <demo|resume> [flags]")
		return 1
	}
	var code int
	switch args[1] {
	case "demo":
		code = demo(args[2:])
	case "resume":
		code = resume(args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[1])
		code = 1
	}
	return code
}

// flags is a minimal --key value / --key=value parser (no external deps).
type flags struct{ m map[string]string }

func parseFlags(args []string) flags {
	f := flags{m: map[string]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		a = a[2:]
		if k, v, ok := strings.Cut(a, "="); ok {
			f.m[k] = v
		} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			f.m[a] = args[i+1]
			i++
		} else {
			f.m[a] = "true"
		}
	}
	return f
}

func (f flags) str(k, def string) string {
	if v, ok := f.m[k]; ok {
		return v
	}
	return def
}

func parseDuration(s string) (int64, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return int64(d / time.Second), nil
}

func printFaults(fl fault.List, inject string) {
	first := fl.First()
	if first == nil {
		return
	}
	fmt.Printf("fault_kind=%s\n", first.Kind)
	fmt.Printf("fault_track=%d\n", first.Track)
	fmt.Printf("fault_sample=%d\n", first.Sample)
	if first.Offset >= 0 {
		fmt.Printf("fault_offset=%d\n", first.Offset)
	}
	if injTrack, injSample, err := pipeline.ParseInject(inject); err == nil && injTrack > 0 {
		for _, f := range fl {
			if f.Track == injTrack {
				diff := f.Sample - injSample
				if diff < 0 {
					diff = -diff
				}
				fmt.Printf("locate_error_samples=%d\n", diff)
				break
			}
		}
	}
	if name := fl.FirstBlockedName(); name != "" {
		fmt.Printf("blocked_by=%s\n", name)
	}
}

func printFinal(fo *pipeline.FinalOut) {
	fmt.Printf("segments=%d\n", fo.Segments)
	fmt.Printf("representations=%d\n", fo.Representations)
	fmt.Printf("max_boundary_skew_ms=%d\n", fo.MaxBoundarySkewMs)
	fmt.Printf("checksum=%s\n", fo.Checksum)
}

// exitCode maps the run outcome to the documented exit codes.
func exitCode(rep *scheduler.Report, fl fault.List) int {
	if rep.BlockedBy != "" {
		return 3
	}
	if len(fl) > 0 {
		return 2
	}
	return 0
}

func demo(args []string) int {
	f := parseFlags(args)
	durSec, err := parseDuration(f.str("duration", "7200s"))
	if err != nil || durSec <= 0 || durSec > 24*3600 {
		fmt.Fprintln(os.Stderr, "invalid --duration (max 24h)")
		return 1
	}
	bitrates, err := pipeline.ParseBitrates(f.str("bitrates", "800k,1800k,3500k"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	segSec, err := parseDuration(f.str("segment", "2s"))
	if err != nil || segSec <= 0 {
		fmt.Fprintln(os.Stderr, "invalid --segment")
		return 1
	}
	p := pipeline.Params{
		DurationSec:  durSec,
		BitratesKbps: bitrates,
		Inject:       f.str("inject", ""),
		ColorPolicy:  f.str("color-policy", "keep-source"),
		SegmentSec:   segSec,
	}
	if _, _, err := pipeline.ParseInject(p.Inject); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ws := f.str("workspace", "./tmp/archive-demo")
	inputDir := filepath.Join(ws, "inputs")

	var r *scheduler.Runner
	if scheduler.StateExists(ws) {
		if r, err = scheduler.LoadState(ws, p.Hash()); err != nil {
			fmt.Fprintf(os.Stderr, "cannot reuse workspace state: %v\n", err)
			return 1
		}
	} else {
		r = scheduler.New(p.Hash())
	}
	if err := pipeline.Build(r, p, inputDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rep, err := r.Run(f.str("interrupt-after", ""))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := r.SaveState(ws); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := pipeline.SaveParams(ws, p); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	var fl fault.List
	var final *pipeline.FinalOut
	if rep.Completed {
		var fo pipeline.FinalOut
		if err := r.Output("checksum", &fo); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		final, fl = &fo, fo.Faults
	} else {
		var so pipeline.ScanOut
		if err := r.Output("scan", &so); err == nil {
			fl = so.Faults
		}
	}
	if rep.BlockedBy != "" {
		fmt.Printf("blocked_by=%s\n", rep.BlockedBy)
		if rep.Fault != nil {
			fmt.Printf("fault_kind=%s\n", rep.Fault.Kind)
			fmt.Printf("fault_track=%d\n", rep.Fault.Track)
			fmt.Printf("fault_sample=%d\n", rep.Fault.Sample)
		}
		return 3
	}
	printFaults(fl, p.Inject)
	if rep.Interrupted {
		fmt.Printf("interrupted_after=%s\n", rep.InterruptedAfter)
	}
	if final != nil {
		printFinal(final)
	}
	return exitCode(rep, fl)
}

func resume(args []string) int {
	f := parseFlags(args)
	ws := f.str("workspace", "./tmp/archive-demo")
	p, err := pipeline.LoadParams(ws)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no params in workspace: %v\n", err)
		return 1
	}
	r, err := scheduler.LoadState(ws, p.Hash())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := pipeline.Build(r, p, filepath.Join(ws, "inputs")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	start := time.Now()
	rep, err := r.Run("")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resumeMs := time.Since(start).Milliseconds()
	if err := r.SaveState(ws); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("resume_ms=%d\n", resumeMs)
	fmt.Printf("rerun_nodes=%d\n", rep.RerunNodes)
	fmt.Printf("skipped_nodes=%d\n", rep.SkippedNodes)
	if rep.BlockedBy != "" {
		fmt.Printf("blocked_by=%s\n", rep.BlockedBy)
		return 3
	}
	if !rep.Completed {
		fmt.Fprintln(os.Stderr, "run did not complete")
		return 1
	}
	var fo pipeline.FinalOut
	if err := r.Output("checksum", &fo); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	printFaults(fo.Faults, p.Inject)
	printFinal(&fo)
	return exitCode(rep, fo.Faults)
}
