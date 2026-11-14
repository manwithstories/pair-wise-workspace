// Command rtspreplay is the terminal entry point. It drives the shared
// session FSM over either transport against a loopback RTSP server and
// prints the per-mode metrics as one JSON line.
//
//	rtspreplay selftest --transport both --faults drop=4,dup=2,reorder=3 \
//	    --packets 500 --out out/report.json
//	rtspreplay stream --url rtsp://127.0.0.1:8554/live --transport tcp
//
// Exit codes: 0 = metrics within budget, 1 = internal error,
// 2 = gap or frame budget exceeded.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"rtspreplay/internal/client"
	"rtspreplay/internal/mocksrv"
	"rtspreplay/internal/stats"
)

const (
	exitOK = iota
	exitInternal
	exitBudget
)

const usage = `usage:
  rtspreplay selftest [--transport udp|tcp|both] [--faults drop=4,dup=2,reorder=3]
                      [--packets 500] [--out out/report.json] [--addr 127.0.0.1:8554]
                      [--min-frames 100] [--max-gaps 10]
  rtspreplay stream   --url rtsp://127.0.0.1:8554/live [--transport udp|tcp]
                      [--out report.json] [--user u] [--pass p]
exit codes: 0=ok 1=internal error 2=gap/frame budget exceeded
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return exitInternal
	}
	switch args[0] {
	case "selftest":
		return cmdSelftest(args[1:])
	case "stream":
		return cmdStream(args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return exitInternal
	}
}

func cmdSelftest(args []string) int {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	transport := fs.String("transport", "both", "udp, tcp or both")
	faults := fs.String("faults", "", "fault injection: drop=N,dup=N,reorder=N")
	packets := fs.Int("packets", 500, "RTP packets per session")
	out := fs.String("out", "", "report output path")
	addr := fs.String("addr", "127.0.0.1:8554", "mock server listen address")
	rate := fs.Int("rate", 500, "mock send pace in packets/second")
	seed := fs.Int64("seed", 1, "mock stream seed")
	user := fs.String("user", "user", "digest username")
	pass := fs.String("pass", "pass", "digest password")
	minFrames := fs.Int("min-frames", 100, "frame budget floor per mode")
	maxGaps := fs.Int("max-gaps", 10, "gap budget ceiling per mode")
	if err := fs.Parse(args); err != nil {
		return exitInternal
	}
	drop, dup, reorder, err := mocksrv.ParseFaults(*faults)
	if err != nil {
		fmt.Fprintln(os.Stderr, "selftest:", err)
		return exitInternal
	}
	srv := mocksrv.New(mocksrv.Config{
		Addr: *addr, Path: "/live", Packets: *packets, Rate: *rate,
		Drop: drop, Dup: dup, Reorder: reorder,
		Seed: *seed, User: *user, Pass: *pass,
	})
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "selftest:", err)
		return exitInternal
	}
	defer srv.Close()

	modes, err := modeList(*transport)
	if err != nil {
		fmt.Fprintln(os.Stderr, "selftest:", err)
		return exitInternal
	}
	rep := stats.New()
	failed := false
	for _, mode := range modes {
		res, rerr := client.Run(client.Options{
			URL:           "rtsp://" + srv.Addr() + "/live",
			Transport:     mode,
			User:          *user,
			Pass:          *pass,
			StateTimeout:  10 * time.Second,
			IdleTimeout:   2 * time.Second,
			ReorderWindow: 32,
		})
		st := &stats.ModeStats{}
		if res != nil {
			st.Transitions = res.Transitions
			st.Gaps = res.Gaps
			st.Frames = res.Frames
			st.Duplicates = res.Duplicates
		}
		rep.Add(mode, st)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "selftest: mode %s: %v\n", mode, rerr)
			failed = true
		}
	}
	if !emit(rep, *out) {
		return exitInternal
	}
	if failed {
		return exitInternal
	}
	if overBudget(rep, *minFrames, *maxGaps) {
		return exitBudget
	}
	return exitOK
}

func cmdStream(args []string) int {
	fs := flag.NewFlagSet("stream", flag.ContinueOnError)
	url := fs.String("url", "", "rtsp://127.0.0.1:8554/live")
	transport := fs.String("transport", "udp", "udp or tcp")
	out := fs.String("out", "", "report output path")
	user := fs.String("user", "user", "digest username")
	pass := fs.String("pass", "pass", "digest password")
	idle := fs.Duration("idle-timeout", 2*time.Second, "stop after this much RTP silence")
	minFrames := fs.Int("min-frames", 0, "frame budget floor")
	maxGaps := fs.Int("max-gaps", -1, "gap budget ceiling, -1 disables")
	if err := fs.Parse(args); err != nil {
		return exitInternal
	}
	if *url == "" {
		fmt.Fprintln(os.Stderr, "stream: --url is required")
		return exitInternal
	}
	res, err := client.Run(client.Options{
		URL:           *url,
		Transport:     *transport,
		User:          *user,
		Pass:          *pass,
		StateTimeout:  10 * time.Second,
		IdleTimeout:   *idle,
		ReorderWindow: 32,
	})
	rep := stats.New()
	st := &stats.ModeStats{}
	if res != nil {
		st.Transitions = res.Transitions
		st.Gaps = res.Gaps
		st.Frames = res.Frames
		st.Duplicates = res.Duplicates
	}
	rep.Add(*transport, st)
	if !emit(rep, *out) {
		return exitInternal
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stream:", err)
		return exitInternal
	}
	if st.Frames < *minFrames || (*maxGaps >= 0 && st.Gaps > *maxGaps) {
		return exitBudget
	}
	return exitOK
}

// emit prints the report as one JSON line and optionally stores it.
func emit(rep *stats.Report, out string) bool {
	raw, err := rep.Marshal()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	fmt.Println(string(raw))
	if out != "" {
		if err := rep.WriteFile(out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return false
		}
	}
	return true
}

func modeList(transport string) ([]string, error) {
	switch transport {
	case "both":
		return []string{"udp", "tcp"}, nil
	case "udp", "tcp":
		return []string{transport}, nil
	}
	return nil, fmt.Errorf("unknown transport %q", transport)
}

func overBudget(rep *stats.Report, minFrames, maxGaps int) bool {
	for _, st := range rep.Modes {
		if st.Frames < minFrames || st.Gaps > maxGaps {
			return true
		}
	}
	return false
}
