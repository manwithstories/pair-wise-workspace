// Command avcore is the terminal adapter for the offline media kernel. The
// only interactive command is "selftest", which synthesizes its own fixtures
// and proves the full pipeline. Exit codes follow the documented contract.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"avcore/internal/adapter"
	"avcore/internal/selftest"
)

// Exit codes from the acceptance contract.
const (
	exitOK            = 0
	exitInternal      = 1
	exitBadArgument   = 2
	exitUnrecoverable = 3
	exitInterrupted   = 75
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitBadArgument
	}
	switch args[0] {
	case "selftest":
		return runSelftest(args[1:])
	case "-h", "--help", "help":
		usage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage(os.Stderr)
		return exitBadArgument
	}
}

func runSelftest(flags []string) int {
	p, err := parseSelftestFlags(flags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
		return exitBadArgument
	}
	rep, pass, err := selftest.Run(context.Background(), p)
	if err != nil {
		if ae, ok := err.(*adapter.Error); ok {
			fmt.Fprintf(os.Stderr, "input error: %v\n", ae)
			return exitUnrecoverable
		}
		fmt.Fprintf(os.Stderr, "internal error: %v\n", err)
		return exitInternal
	}
	selftest.Print(os.Stdout, rep, pass)
	if !pass {
		return exitInternal
	}
	return exitOK
}

// parseSelftestFlags accepts --out, --seed, --duration, --segment and
// --bitrates. Duration/segment accept a trailing "s".
func parseSelftestFlags(flags []string) (selftest.Params, error) {
	p := selftest.Params{Seed: 41, DurationS: 8, SegmentS: 1,
		Bitrates: []int{450000, 900000}}
	for i := 0; i < len(flags); i++ {
		key := flags[i]
		if !strings.HasPrefix(key, "--") {
			return p, fmt.Errorf("unexpected token %q", key)
		}
		val, err := valueOf(flags, &i, key)
		if err != nil {
			return p, err
		}
		switch key {
		case "--out":
			p.OutDir = val
		case "--seed":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return p, fmt.Errorf("--seed must be a non-negative integer")
			}
			p.Seed = n
		case "--duration":
			p.DurationS, err = parseSeconds(val)
		case "--segment":
			p.SegmentS, err = parseSeconds(val)
		case "--bitrates":
			p.Bitrates, err = parseBitrates(val)
		default:
			return p, fmt.Errorf("unknown flag %q", key)
		}
		if err != nil {
			return p, err
		}
	}
	if p.OutDir == "" {
		return p, fmt.Errorf("--out is required")
	}
	if p.DurationS < 2 {
		return p, fmt.Errorf("--duration must be at least 2 seconds")
	}
	if p.SegmentS < 1 {
		return p, fmt.Errorf("--segment must be at least 1 second")
	}
	if len(p.Bitrates) < 2 {
		return p, fmt.Errorf("--bitrates needs at least two values")
	}
	return p, nil
}

func valueOf(flags []string, i *int, key string) (string, error) {
	if *i+1 >= len(flags) {
		return "", fmt.Errorf("%s requires a value", key)
	}
	*i++
	return flags[*i], nil
}

func parseSeconds(s string) (int, error) {
	s = strings.TrimSuffix(s, "s")
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid seconds value %q", s)
	}
	return n, nil
}

func parseBitrates(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid bitrate %q", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func usage(w *os.File) {
	fmt.Fprintln(w, "avcore - offline media processing kernel")
	fmt.Fprintln(w, "usage: avcore selftest --out DIR [--seed N] [--duration 8s]")
	fmt.Fprintln(w, "       [--segment 1s] [--bitrates 450000,900000]")
	fmt.Fprintln(w, "exit codes: 0 pass | 1 internal | 2 bad argument | 3 unrecoverable input | 75 interrupted")
}
