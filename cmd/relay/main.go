// Command relay is the terminal acceptance adapter for HLS timeline relay.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/relayteam/relay/internal/app"
	"github.com/relayteam/relay/internal/media"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: relay <demo|run> [flags]")
		return media.ExitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(ctx, args[1:])
	case "run":
		return runRun(ctx, args[1:], os.Stdout)
	case "-h", "--help", "help":
		fmt.Println("usage: relay <demo|run> [flags]")
		return media.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		return media.ExitUsage
	}
}

type commonFlags struct {
	root             string
	chunkRecords     int
	maxInflightBytes int64
	slowEvery        int
	interruptAfter   int
}

func bindCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.root, "root", "", "local output root")
	fs.IntVar(&c.chunkRecords, "chunk-records", 64, "records per checkpoint chunk")
	fs.Int64Var(&c.maxInflightBytes, "max-inflight-bytes", 4<<20, "weighted in-memory ceiling")
	fs.IntVar(&c.slowEvery, "slow-every", 0, "slow consumer hook frequency (0 disables)")
	fs.IntVar(&c.interruptAfter, "interrupt-after", 0, "committed chunks after which to interrupt")
	return c
}

func (c commonFlags) validate() error {
	if c.root == "" {
		return errors.New("--root is required")
	}
	if c.chunkRecords <= 0 {
		return errors.New("--chunk-records must be positive")
	}
	if c.maxInflightBytes <= 4096 {
		return errors.New("--max-inflight-bytes must exceed one record weight")
	}
	if c.slowEvery < 0 || c.interruptAfter < 0 {
		return errors.New("--slow-every and --interrupt-after must be non-negative")
	}
	return nil
}

func runRun(ctx context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := bindCommon(fs)
	input := fs.String("input", "", "NDJSON input path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return media.ExitOK
		}
		fmt.Fprintf(out, "relay_result path=run status=arg_error error=%q\n", err.Error())
		return media.ExitUsage
	}
	if err := c.validate(); err != nil || *input == "" {
		msg := err
		if *input == "" {
			msg = errors.New("--input is required")
		}
		fmt.Fprintf(out, "relay_result path=run status=arg_error error=%q\n", msg.Error())
		return media.ExitUsage
	}
	res, err := app.Run(ctx, app.RunConfig{
		Root: c.root, InputPath: *input, ChunkRecords: c.chunkRecords,
		MaxInflightBytes: c.maxInflightBytes, SlowEvery: c.slowEvery,
		InterruptAfterChunks: c.interruptAfter,
	})
	if res == nil {
		res = &app.RunResult{}
	}
	if err != nil {
		code := media.ExitCode(err)
		firstBad := res.FirstBadRecord
		if me, ok := media.First(err); ok && firstBad == 0 {
			firstBad = me.Meta.RecordNo
		}
		fmt.Fprintf(out, "relay_result path=run status=error exit_code=%d records=%d first_bad_record=%d precision_ticks=%d backpressure_peak_bytes=%d restore_ms=%d recover_match=false sha256=%s error_code=%s\n",
			code, res.Records, firstBad, res.PrecisionTicks, res.BackpressurePeakBytes,
			res.RestoreMS, emptyDigest(res.SHA256), media.CodeOf(err))
		fmt.Fprintln(os.Stderr, err)
		return code
	}
	fmt.Fprintf(out, "relay_result path=run status=ok records=%d first_bad_record=0 precision_ticks=%d backpressure_peak_bytes=%d restore_ms=%d recover_match=true sha256=%s chunks=%d\n",
		res.Records, res.PrecisionTicks, res.BackpressurePeakBytes, res.RestoreMS, res.SHA256, res.Chunks)
	return media.ExitOK
}

func emptyDigest(s string) string {
	if s != "" {
		return s
	}
	return strings.Repeat("0", 64)
}
