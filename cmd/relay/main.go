// Command relay is the terminal entry point of the media relay. It runs the
// full local demo pipeline (demo) or finishes an interrupted run (resume),
// emitting machine-parseable key=value lines and documented exit codes.
package main

import (
	"fmt"
	"os"
	"strings"

	"relay/internal/media"
	"relay/internal/metrics"
	"relay/internal/pipeline"
)

const usage = `relay — local media relay demo

usage:
  relay demo   --work <dir> [--damage checksum] [--crash-after staged]
  relay resume --work <dir>

exit codes: 0 ok, 2 usage, 3 media validation failure (rolled back),
             75 recoverable injected interruption
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		return pipeline.ExitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "resume":
		return runResume(args[1:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return pipeline.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", args[0], usage)
		return pipeline.ExitUsage
	}
}

func parseFlags(args []string) (map[string]string, error) {
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			return nil, fmt.Errorf("unexpected argument %q", a)
		}
		key := strings.TrimPrefix(a, "--")
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			return nil, fmt.Errorf("flag --%s needs a value", key)
		}
		flags[key] = args[i+1]
		i++
	}
	return flags, nil
}

func runDemo(args []string) int {
	flags, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return pipeline.ExitUsage
	}
	work, ok := flags["work"]
	if !ok || work == "" {
		fmt.Fprintln(os.Stderr, "--work is required")
		return pipeline.ExitUsage
	}
	damage := flags["damage"]
	if damage != "" && damage != "checksum" {
		fmt.Fprintln(os.Stderr, "unsupported --damage value")
		return pipeline.ExitUsage
	}
	crash := flags["crash-after"]
	if crash != "" && crash != "staged" {
		fmt.Fprintln(os.Stderr, "unsupported --crash-after value")
		return pipeline.ExitUsage
	}
	out, err := pipeline.Run(pipeline.Config{
		WorkDir: work, Damage: damage, CrashAfter: crash,
	})
	if err != nil {
		// Finalize/stage failures that bypassed the rollback path are still
		// media validation failures when structured as such.
		if me := media.AsMediaError(err); me != nil {
			fmt.Printf("checksum_mismatch=%d\n", b2i(me.Kind == media.ErrChecksumMismatch))
			fmt.Printf("error=%s\n", me.Error())
			return pipeline.ExitMediaError
		}
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return pipeline.ExitInterrupt
	}
	fmt.Print(out.Report.Render())
	if out.ExitCode != 0 {
		return out.ExitCode
	}
	// Healthy runs must also satisfy every acceptance threshold.
	if err := out.Report.Validate(); err != nil {
		fmt.Printf("validation_failed=%s\n", err.Error())
		return pipeline.ExitMediaError
	}
	return pipeline.ExitOK
}

func runResume(args []string) int {
	flags, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return pipeline.ExitUsage
	}
	work, ok := flags["work"]
	if !ok || work == "" {
		fmt.Fprintln(os.Stderr, "--work is required")
		return pipeline.ExitUsage
	}
	out, err := pipeline.Resume(work)
	if err != nil {
		if me := media.AsMediaError(err); me != nil {
			fmt.Println("replayed=0")
			fmt.Printf("error=%s\n", me.Error())
			return pipeline.ExitMediaError
		}
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		return pipeline.ExitInterrupt
	}
	fmt.Print(out.Report.Render())
	return out.ExitCode
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

var _ = metrics.MaxSyncMS
