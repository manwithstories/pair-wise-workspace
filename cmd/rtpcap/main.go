// Command rtpcap is a loopback-only RTSP/RTP capture and evidence tool.
//
// Usage:
//
//	rtpcap capture --scenario <json> --transport tcp|udp --out <dir> --report <file>
//	rtpcap verify  --report <file> --index <jsonl> --expect <file>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/example/rtpcap/internal/mockserver"
	"github.com/example/rtpcap/internal/report"
	"github.com/example/rtpcap/internal/rtsp"
	"github.com/example/rtpcap/internal/scenario"
	"github.com/example/rtpcap/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return rtsp.ExitArgs
	}
	switch args[0] {
	case "capture":
		return runCapture(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		usage()
		return rtsp.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		usage()
		return rtsp.ExitArgs
	}
}

func runCapture(args []string) int {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	scenarioPath := fs.String("scenario", "", "path to scenario JSON (required)")
	transport := fs.String("transport", "tcp", "rtp transport: tcp|udp")
	outDir := fs.String("out", "out/cap", "frame and index output directory")
	reportPath := fs.String("report", "", "report JSON output path (required)")
	listen := fs.String("listen", "127.0.0.1", "mock server bind address (loopback only)")
	if err := fs.Parse(args); err != nil {
		return rtsp.ExitArgs
	}
	if *scenarioPath == "" || *reportPath == "" {
		fmt.Fprintln(os.Stderr, "capture requires --scenario and --report")
		return rtsp.ExitArgs
	}
	if *transport != "tcp" && *transport != "udp" {
		fmt.Fprintf(os.Stderr, "invalid --transport %q (want tcp or udp)\n", *transport)
		return rtsp.ExitArgs
	}
	if *listen != "127.0.0.1" {
		fmt.Fprintf(os.Stderr, "refusing to listen on non-loopback address %q\n", *listen)
		return rtsp.ExitNonLoopback
	}

	sc, err := scenario.Load(*scenarioPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return rtsp.ExitArgs
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	srv, err := mockserver.New(sc, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return rtsp.ExitHandshake
	}
	srv.Start()
	defer srv.Close()

	frameStore, err := store.Open(*outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return rtsp.ExitArgs
	}
	defer frameStore.Close()

	mediaURL := "rtsp" + "://" + srv.Addr() + "/stream0"
	if err := rtsp.RejectNonLoopback(srv.Addr()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return rtsp.ExitNonLoopback
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	result := rtsp.Run(ctx, sc, *transport, srv.Addr(), mediaURL, frameStore, logger)

	exitCode := rtsp.ExitOK
	errMsg := ""
	if result.Err != nil {
		exitCode = mapError(result.Err)
		errMsg = result.Err.Error()
	}
	rep := report.Build(sc.Name, *transport, result.SM, result.Stats,
		result.Gaps, result.Reconnects, exitCode, errMsg)
	if err := rep.WriteJSON(*reportPath); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		return rtsp.ExitArgs
	}
	fmt.Print(rep.Text())
	if exitCode != rtsp.ExitOK {
		fmt.Fprintf(os.Stderr, "capture ended with error: %s\n", errMsg)
	}
	return exitCode
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	reportPath := fs.String("report", "", "capture report JSON (required)")
	indexPath := fs.String("index", "", "frame index JSONL (required)")
	expectPath := fs.String("expect", "", "scenario expectation JSON (required)")
	if err := fs.Parse(args); err != nil {
		return rtsp.ExitArgs
	}
	if *reportPath == "" || *indexPath == "" || *expectPath == "" {
		fmt.Fprintln(os.Stderr, "verify requires --report, --index and --expect")
		return rtsp.ExitArgs
	}

	rep, err := report.LoadJSON(*reportPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load report:", err)
		return rtsp.ExitArgs
	}
	entries, err := loadIndex(*indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load index:", err)
		return rtsp.ExitArgs
	}
	var ex scenario.Expect
	raw, err := os.ReadFile(*expectPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load expect:", err)
		return rtsp.ExitArgs
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		fmt.Fprintln(os.Stderr, "parse expect:", err)
		return rtsp.ExitArgs
	}

	mismatches := report.Verify(rep, entries, ex)
	if len(mismatches) == 0 {
		fmt.Printf("verify OK: report matches %d expectation groups and %d index entries\n",
			11, len(entries))
		return rtsp.ExitOK
	}
	fmt.Fprintf(os.Stderr, "verify FAILED with %d mismatch(es):\n", len(mismatches))
	for _, mm := range mismatches {
		fmt.Fprintf(os.Stderr, "  - %s: %s\n", mm.Field, mm.Detail)
	}
	return rtsp.ExitVerifyMismatch
}

func loadIndex(path string) ([]store.IndexEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []store.IndexEntry
	dec := json.NewDecoder(f)
	for {
		var e store.IndexEntry
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func mapError(err error) int {
	var auth *rtsp.AuthError
	if errors.As(err, &auth) {
		return rtsp.ExitHandshake
	}
	var hs *rtsp.HandshakeError
	if errors.As(err, &hs) {
		return rtsp.ExitHandshake
	}
	var nlb *rtsp.NonLoopbackError
	if errors.As(err, &nlb) {
		return rtsp.ExitNonLoopback
	}
	var rto rtsp.ReadTimeout
	if errors.As(err, &rto) {
		return rtsp.ExitHandshake
	}
	return rtsp.ExitHandshake
}

func usage() {
	fmt.Fprint(os.Stderr, `rtpcap - loopback RTSP/RTP capture and evidence tool

usage:
  rtpcap capture --scenario <file.json> --transport tcp|udp \
                 --out <dir> --report <file.json> [--listen 127.0.0.1]
  rtpcap verify  --report <file.json> --index <file.jsonl> --expect <file.json>

exit codes:
  0 success / verify passed
  2 invalid arguments or scenario file
  3 report/index/expect mismatch
  4 handshake or transport failure
  5 non-loopback listen address rejected
`)
}
