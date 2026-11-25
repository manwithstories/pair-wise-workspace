package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"fieldqc/internal/fault"
	"fieldqc/internal/manifest"
	"fieldqc/internal/report"
	"fieldqc/internal/stream"
)

// locatedSpan maps a located manifest validation error onto the shared
// fault span model for machine-readable --json output.
func locatedSpan(e *manifest.Error) fault.Span {
	return fault.Span{Track: e.Asset + "/" + e.Field, Code: e.Code, Detail: e.Msg}
}

func init() {
	registerCommand("manifest", "validate a manifest JSON file (- for stdin)", runManifestCmd)
	registerCommand("cancel", "demonstrate backpressure cancellation status", runCancelCmd)
}

// runManifestCmd validates a manifest file. Invalid parameters or an
// invalid/locating manifest exit 1; an unreadable path exits 3.
func runManifestCmd(argv []string) int {
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "write each located error as a JSON span")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitInvalid
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: fieldqc manifest [--json] <path|->")
		return exitInvalid
	}
	var (
		data []byte
		err  error
	)
	if p := fs.Arg(0); p == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(p)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fieldqc: read manifest: %v\n", err)
		return exitAborted
	}
	mf, perr := manifest.Parse(data)
	if perr != nil {
		if errs, ok := perr.(manifest.Errors); ok {
			for _, e := range errs {
				if *asJSON {
					if b, mErr := json.Marshal(locatedSpan(e)); mErr == nil {
						fmt.Fprintln(os.Stdout, string(b))
					}
				} else {
					fmt.Fprintln(os.Stderr, e.Error())
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "fieldqc: %v\n", perr)
		}
		return exitInvalid
	}
	assets, tracks := 0, 0
	if mf != nil {
		assets = len(mf.Assets)
		for _, a := range mf.Assets {
			tracks += len(a.Tracks)
		}
	}
	fmt.Fprintf(os.Stdout, "{\"valid\":true,\"assets\":%d,\"tracks\":%d}\n", assets, tracks)
	return exitOK
}

// runCancelCmd runs a bounded pipeline with an output stage that blocks
// until cancellation, then emits the recognizable aborted status and
// exits 3. It is deterministic and does not depend on wall-clock speed.
func runCancelCmd(argv []string) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	after := fs.Duration("after", 20*time.Millisecond, "delay before cancellation")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitInvalid
	}

	ctx, cancel := context.WithCancel(context.Background())
	src := make(chan stream.Item, 1)
	src <- stream.Item{Window: 0, Size: 1 << 20}
	close(src)

	blocking := func(ctx context.Context, in <-chan stream.Item, _ chan<- stream.Item) error {
		select {
		case <-ctx.Done():
			return stream.ErrAborted
		case <-in:
		}
		<-ctx.Done() // slow output never completes before cancel
		return stream.ErrAborted
	}
	go func() {
		time.Sleep(*after)
		cancel()
	}()

	if err := stream.New(1, 8<<20).Run(ctx, src, blocking); !errors.Is(err, stream.ErrAborted) {
		fmt.Fprintf(os.Stderr, "fieldqc: cancel demo did not abort: %v\n", err)
		return exitAborted
	}
	_ = report.WriteLine(os.Stdout, report.Line{Case: "cancel", Status: "aborted"})
	return exitAborted
}
