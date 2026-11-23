// archivectl is the archive pipeline acceptance CLI. It is a pure
// terminal tool: all behavior is driven by flags and reported on stdout.
//
// Exit codes: 0 success; 2 corrupt input or unrecoverable dependency;
// 1 internal error.
package main

import (
	"flag"
	"fmt"
	"os"
)

// demoOpts carries the demo flags.
type demoOpts struct {
	seed            int64
	memoryMiB       int
	consumerDelayMs int
	lossBurst       int
	fixture         string
	workDir         string // tests inject t.TempDir(); empty = temp dir
}

func main() {
	os.Exit(realMain(os.Args))
}

func realMain(args []string) int {
	if len(args) < 2 || args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: archivectl demo [--seed N] [--memory-mib N] "+
			"[--consumer-delay-ms N] [--loss-burst N] [--fixture ok|truncated]")
		return 1
	}
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic fixture seed")
	memMiB := fs.Int("memory-mib", 16, "pipeline memory budget in MiB")
	delayMs := fs.Int("consumer-delay-ms", 8, "slow-consumer delay per packet (ms)")
	lossBurst := fs.Int("loss-burst", 5, "injected RTP loss burst size")
	fixture := fs.String("fixture", "ok", "fixture variant: ok|truncated")
	if err := fs.Parse(args[2:]); err != nil {
		return 1
	}
	return runDemo(demoOpts{
		seed: *seed, memoryMiB: *memMiB, consumerDelayMs: *delayMs,
		lossBurst: *lossBurst, fixture: *fixture,
	}, os.Stdout)
}
