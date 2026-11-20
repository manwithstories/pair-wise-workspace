// Command-line entry point for the archive timeline tool. The heavy logic
// lives in internal/cmdarch so it can be tested without a subprocess.
package main

import (
	"context"
	"fmt"
	"os"

	"archivetl/internal/cmdarch"
)

const usageText = `archive-tl — multi-track subtitle timeline archiver

usage:
  archive demo --fps <rate> --wrap-day --slow-consumer-ms <n> --output <dir>

flags:
  --fps                24 | 25 | 30 | 30000/1001
  --wrap-day           fold timecodes across the midnight boundary
  --slow-consumer-ms   inject a per-segment sink delay (slow archive disk)
  --output             output directory for segments and manifest

exit codes: 0 ok, 2 usage, 3 verification failed, 4 unrecoverable I/O error
`

func main() {
	code := run(os.Args[1:])
	os.Exit(int(code))
}

func run(args []string) cmdarch.ExitCode {
	if len(args) == 0 || args[0] != "demo" {
		fmt.Fprint(os.Stderr, usageText)
		return cmdarch.ExitUsage
	}
	opts, err := parseDemoArgs(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n\n%s", err, usageText)
		return cmdarch.ExitUsage
	}
	result, err := cmdarch.RunDemo(context.Background(), opts)
	if result != nil && result.Summary != nil {
		raw, mErr := result.Summary.Marshal()
		if mErr == nil {
			fmt.Print(string(raw))
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	if err != nil {
		return cmdarch.ExitCodeFor(err)
	}
	if result != nil {
		return result.Exit
	}
	return cmdarch.ExitOK
}

func parseDemoArgs(args []string) (cmdarch.DemoOptions, error) {
	opts := cmdarch.DemoOptions{FPS: "30000/1001"}
	flags := map[string]func(string) error{
		"--fps":    func(v string) error { opts.FPS = v; return nil },
		"--output": func(v string) error { opts.Output = v; return nil },
		"--slow-consumer-ms": func(v string) error {
			n, err := parseNonNegative(v)
			if err != nil {
				return err
			}
			opts.SlowConsumerMs = n
			return nil
		},
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--wrap-day" {
			opts.WrapDay = true
			continue
		}
		set, ok := flags[a]
		if !ok {
			return opts, fmt.Errorf("unknown flag %q", a)
		}
		if i+1 >= len(args) {
			return opts, fmt.Errorf("flag %s needs a value", a)
		}
		if err := set(args[i+1]); err != nil {
			return opts, err
		}
		i++
	}
	if opts.Output == "" {
		return opts, fmt.Errorf("--output is required")
	}
	return opts, nil
}

func parseNonNegative(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a non-negative integer: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
