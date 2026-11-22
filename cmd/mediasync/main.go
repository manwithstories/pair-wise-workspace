// mediasync is a terminal-only demo: it generates a deterministic sample,
// repairs the RTP stream, aligns subtitles and segments on one timebase,
// builds the bitrate ladder, commits through a WAL and verifies hashes.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"mediasync/internal/clock"
	"mediasync/internal/demo"
	"mediasync/internal/fixture"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) < 1 || args[0] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: mediasync demo --seed N --fps 30000/1001 "+
			"--bitrates 800,1600,3200 --faults duplicate,reorder,burst-loss [--crash-at commit]")
		return demo.ExitUsage
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	fpsStr := fs.String("fps", "30000/1001", "frame rate: 24, 25, 30 or 30000/1001")
	bitrateStr := fs.String("bitrates", "800,1600,3200", "comma-separated kbps ladder")
	faultStr := fs.String("faults", "", "comma-separated: duplicate,reorder,burst-loss")
	crashAt := fs.String("crash-at", "", "simulate crash before stage commit: commit")
	outDir := fs.String("out", "out", "artifact directory inside the repo")
	if err := fs.Parse(args[1:]); err != nil {
		return demo.ExitUsage
	}
	if !clock.Supported(*fpsStr) {
		fmt.Fprintf(os.Stderr, "unsupported fps %q\n", *fpsStr)
		return demo.ExitUsage
	}
	rational, err := clock.ParseRational(*fpsStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return demo.ExitUsage
	}
	bitrates, err := parseBitrates(*bitrateStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return demo.ExitUsage
	}
	faults, err := parseFaults(*faultStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return demo.ExitUsage
	}
	if *crashAt != "" && *crashAt != "commit" {
		fmt.Fprintf(os.Stderr, "unsupported --crash-at %q\n", *crashAt)
		return demo.ExitUsage
	}
	tb := clock.Timebase{FPS: rational}
	metrics, code := demo.Pipeline(*seed, tb, *fpsStr, bitrates, faults, *crashAt, *outDir)
	for _, line := range metrics {
		fmt.Println(line)
	}
	return code
}

func parseBitrates(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid bitrate %q", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty bitrate ladder")
	}
	return out, nil
}

func parseFaults(s string) (map[string]bool, error) {
	out := map[string]bool{}
	if s == "" {
		return out, nil
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		switch p {
		case fixture.FaultDuplicate, fixture.FaultReorder, fixture.FaultBurstLoss:
			out[p] = true
		default:
			return nil, fmt.Errorf("unknown fault %q", p)
		}
	}
	return out, nil
}
