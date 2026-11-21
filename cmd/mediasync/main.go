// Command mediasync is the terminal-only acceptance adapter. It generates its
// own MXF/MP4 metadata fixtures and synthetic RTP streams, runs the full
// metadata -> transport -> timeline -> render pipeline, and prints the
// acceptance metrics. Exit codes: 0 ok, 1 processing failure, 2 bad arguments.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const usageText = `mediasync — local media synchronization pipeline

Usage:
  mediasync demo --seed <int> --duration <dur> --loss-burst <dur> \
                 --bandwidth <bps,bps,bps> --output <dir>

Durations accept Go-style units (12s, 80ms). Bandwidth accepts k/M suffixes.
Exit codes: 0 success, 1 processing failure, 2 parameter error.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usageText)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		if err := runDemo(os.Args[2:]); err != nil {
			if isArgError(err) {
				fmt.Fprintf(os.Stderr, "parameter error: %v\n%s\n", err, usageText)
				os.Exit(2)
			}
			fmt.Fprintf(os.Stderr, "processing failure: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		fmt.Println(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s\n", os.Args[1], usageText)
		os.Exit(2)
	}
}

// argError marks argument failures so main can map them to exit code 2.
type argError struct{ msg string }

func (e *argError) Error() string { return e.msg }

func isArgError(err error) bool {
	_, ok := err.(*argError)
	return ok
}

func argf(format string, args ...interface{}) error {
	return &argError{msg: fmt.Sprintf(format, args...)}
}

// options is the parsed demo command line.
type options struct {
	seed       int64
	durationMs int64
	burstMs    int64
	bandwidths []int64
	output     string
}

func parseFlags(args []string) (options, error) {
	var o options
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if !strings.HasPrefix(flag, "--") {
			return o, argf("unexpected argument %q", flag)
		}
		if i+1 >= len(args) {
			return o, argf("flag %s needs a value", flag)
		}
		values[strings.TrimPrefix(flag, "--")] = args[i+1]
		i++
	}
	need := []string{"seed", "duration", "loss-burst", "bandwidth", "output"}
	for _, k := range need {
		if _, ok := values[k]; !ok {
			return o, argf("missing required flag --%s", k)
		}
	}
	seed, err := strconv.ParseInt(values["seed"], 10, 64)
	if err != nil {
		return o, argf("--seed must be an integer: %v", err)
	}
	o.seed = seed
	o.durationMs, err = parseDurationMs(values["duration"])
	if err != nil {
		return o, err
	}
	if o.durationMs <= 0 {
		return o, argf("--duration must be positive")
	}
	o.burstMs, err = parseDurationMs(values["loss-burst"])
	if err != nil {
		return o, err
	}
	if o.burstMs < 0 || o.burstMs >= o.durationMs {
		return o, argf("--loss-burst must be within [0, duration)")
	}
	for _, raw := range strings.Split(values["bandwidth"], ",") {
		bps, err := parseBandwidth(strings.TrimSpace(raw))
		if err != nil {
			return o, err
		}
		o.bandwidths = append(o.bandwidths, bps)
	}
	if len(o.bandwidths) == 0 {
		return o, argf("--bandwidth needs at least one value")
	}
	o.output = values["output"]
	if strings.TrimSpace(o.output) == "" {
		return o, argf("--output must not be empty")
	}
	return o, nil
}

// parseDurationMs accepts ms/s units and returns milliseconds.
func parseDurationMs(s string) (int64, error) {
	switch {
	case strings.HasSuffix(s, "ms"):
		v, err := strconv.ParseInt(strings.TrimSuffix(s, "ms"), 10, 64)
		return v, err
	case strings.HasSuffix(s, "s"):
		v, err := strconv.ParseInt(strings.TrimSuffix(s, "s"), 10, 64)
		if err != nil {
			return 0, err
		}
		return v * 1000, nil
	default:
		return 0, argf("duration %q must end in ms or s", s)
	}
}

// parseBandwidth accepts raw bps or k/M suffixes.
func parseBandwidth(s string) (int64, error) {
	mult := int64(1)
	num := s
	switch {
	case strings.HasSuffix(s, "M"):
		mult, num = 1_000_000, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "k"):
		mult, num = 1_000, strings.TrimSuffix(s, "k")
	}
	v, err := strconv.ParseInt(num, 10, 64)
	if err != nil || v <= 0 {
		return 0, argf("invalid bandwidth %q", s)
	}
	return v * mult, nil
}
