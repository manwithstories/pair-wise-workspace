// Command mediapack is the terminal acceptance adapter for the media kernel.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pairwise/mediapack/internal/fixture"
	"github.com/pairwise/mediapack/internal/verify"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		fmt.Fprintln(stderr, "usage: mediapack verify [--seed N] [--faults a,b,...] ...")
		return verify.ExitBadArgs
	}

	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", 73, "deterministic fixture seed")
	faultsFlag := fs.String("faults", "", "comma-separated faults: pcr-wrap,cc-gap,psi-update,bad-crc,truncated")
	bufferPackets := fs.Int("buffer-packets", 4096, "bounded TS packet queue capacity")
	consumerDelay := fs.Duration("consumer-delay", 20*time.Millisecond, "simulated slow-consumer work per packet")
	crashAfter := fs.Int("crash-after", 2, "inject crash after this many WAL records")
	resume := fs.Bool("resume", false, "recover WAL and rerun after injected crash")
	workDir := fs.String("work-dir", ".mediapack", "base directory for generated samples and state")
	silenceMS := fs.Int("silence-ms", fixture.SilenceMS, "minimum silence duration in milliseconds")

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return verify.ExitOK
		}
		return verify.ExitBadArgs
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return verify.ExitBadArgs
	}

	var faults []string
	if strings.TrimSpace(*faultsFlag) != "" {
		for _, f := range strings.Split(*faultsFlag, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				faults = append(faults, f)
			}
		}
	}

	cfg := verify.Config{
		Seed:          *seed,
		Faults:        faults,
		BufferPackets: *bufferPackets,
		ConsumerDelay: *consumerDelay,
		CrashAfter:    *crashAfter,
		Resume:        *resume,
		WorkDir:       *workDir,
		SilenceMS:     *silenceMS,
	}
	result, err := verify.Run(context.Background(), cfg)
	if result != nil {
		verify.WriteReport(stdout, result)
	}
	if err != nil {
		var exitErr *verify.ExitError
		if errors.As(err, &exitErr) {
			fmt.Fprintf(stderr, "verify failed: %v\n", err)
			return exitErr.Code
		}
		fmt.Fprintf(stderr, "verify failed: %v\n", err)
		return verify.ExitUnrecoverable
	}
	fmt.Fprintln(stdout, "verification=PASS")
	return verify.ExitOK
}
