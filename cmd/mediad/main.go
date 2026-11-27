// Command mediad is the terminal-only demo and acceptance entry point
// for the media recovery library. It generates a local RTP/RTCP sample
// stream with injected faults, runs the recovery pipeline (optionally
// crashing and resuming from checkpoints) and prints a run summary.
//
// Exit codes: 0 = completed and summary checks passed; 2 = unrecoverable
// input exists and has been located; 3 = invalid arguments or checkpoint.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"mediad/internal/checkpoint"
	"mediad/internal/ingest"
	"mediad/internal/normalize"
	"mediad/internal/pipeline"
	"mediad/internal/sample"
)

const usage = `mediad - deterministic RTP/RTCP media recovery demo

usage:
  mediad demo --seed N --packets N --fault-profile P --crash-after N \
              --checkpoint PATH --output PATH

fault profiles: all, none
exit codes: 0 ok, 2 unrecoverable input located, 3 invalid args/checkpoint
`

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(3)
	}
	os.Exit(runDemo(os.Args[2:]))
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 1, "deterministic stream seed")
	packets := fs.Int("packets", 60000, "number of stream packets")
	profile := fs.String("fault-profile", "all", "fault profile: all|none")
	crashAfter := fs.Int("crash-after", 0, "simulate a crash after N packets")
	ckptPath := fs.String("checkpoint", "", "checkpoint file base path")
	outPath := fs.String("output", "", "artifact output path")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *packets <= 0 || *ckptPath == "" || *outPath == "" {
		fmt.Fprint(os.Stderr, usage)
		return 3
	}
	if *profile != "all" && *profile != "none" {
		fmt.Fprintf(os.Stderr, "unknown fault profile %q\n", *profile)
		return 3
	}
	return run(*seed, *packets, *profile, *crashAfter, *ckptPath, *outPath)
}

func engineConfig() pipeline.Config {
	return pipeline.Config{
		RewindWindowNanos: 2_000_000_000, // 2 s rewind window
		DrainSlackNanos:   100_000_000,   // 100 ms emission lag
		CheckpointEvery:   5000,
		MaxGapFill:        16,
	}
}

func registerTracks(e *pipeline.Engine) {
	for _, s := range sample.Specs() {
		e.RegisterTrack(ingest.TrackParams{
			SSRC: s.SSRC, ClockRate: s.ClockRate,
			Channels: s.Channels, Layout: s.Layout, IsVideo: s.IsVideo,
		})
	}
	// Initial confirmed color descriptions for the video tracks.
	e.ObserveColor(1003, normalize.ColorInfo{
		Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: "limited"})
	e.ObserveColor(1004, normalize.ColorInfo{
		Primaries: "bt2020", Transfer: "pq", Matrix: "bt2020ncl", Range: "limited"})
}

// applyMilestones replays the scripted signaling changes at fixed
// arrival counts: a missing audio layout span and color metadata
// conflicts. Only the "all" fault profile includes them.
func applyMilestones(e *pipeline.Engine, total int, profile string) {
	if profile != "all" {
		return
	}
	switch e.Arrival() {
	case int64(total) * 40 / 100: // layout goes unconfirmed
		e.UpdateTrack(ingest.TrackParams{SSRC: 1002, ClockRate: 44100, Channels: 6, Layout: ""})
	case int64(total) * 45 / 100: // layout confirmed again
		e.UpdateTrack(ingest.TrackParams{SSRC: 1002, ClockRate: 44100, Channels: 6, Layout: "5.1"})
	case int64(total) * 55 / 100: // color update with a missing field
		e.ObserveColor(1003, normalize.ColorInfo{
			Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", Range: ""})
	case int64(total) * 60 / 100: // conflicting color update
		e.ObserveColor(1004, normalize.ColorInfo{
			Primaries: "bt709", Transfer: "pq", Matrix: "bt2020ncl", Range: "limited"})
	}
}

func feed(e *pipeline.Engine, seed int64, items []sample.Item, total int, profile string) {
	for _, it := range items {
		applyMilestones(e, total, profile)
		raw := sample.Render(seed, it)
		if it.IsRTCP {
			e.ProcessRTCP(raw)
		} else {
			e.ProcessRTP(raw)
		}
	}
}

func run(seed int64, packets int, profile string, crashAfter int, ckptPath, outPath string) int {
	store := checkpoint.New(ckptPath)
	store.Remove()
	out, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "output:", err)
		return 3
	}
	sink := pipeline.NewFileSink(out)
	cfg := engineConfig()

	stream, truth := sample.Generate(seed, packets, profile)

	eng := pipeline.New(cfg, sink, store)
	registerTracks(eng)

	var restartMs int64
	if crashAfter > 0 && crashAfter < len(stream) {
		feed(eng, seed, stream[:crashAfter], packets, profile)
		// Simulate a crash: corrupt the newest checkpoint on disk and
		// restart from a fresh engine over the same artifact file.
		if p := store.CurrentPath(); p != "" {
			if fi, err := os.Stat(p); err == nil && fi.Size() > 64 {
				os.Truncate(p, fi.Size()-32)
			}
		}
		eng = pipeline.New(cfg, sink, store)
		start := time.Now()
		fb, err := eng.Restore()
		if err != nil {
			fmt.Fprintln(os.Stderr, "checkpoint restore:", err)
			return 3
		}
		if fb != nil {
			fmt.Fprintf(os.Stderr, "checkpoint fallback: %s\n", fb.Msg)
		}
		feed(eng, seed, stream[eng.Arrival():], packets, profile)
		eng.Flush()
		restartMs = time.Since(start).Milliseconds()
	} else {
		feed(eng, seed, stream, packets, profile)
		eng.Flush()
	}
	if err := out.Sync(); err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		return 3
	}
	out.Close()
	if err := eng.CheckpointError(); err != nil {
		fmt.Fprintln(os.Stderr, "checkpoint:", err)
		return 3
	}

	sum := eng.Summary()
	sum.RestartMs = restartMs
	sum.LocErrorPackets = abs64(sum.LocatedPacket - truth)
	sum.ChecksumSHA256, err = fileSHA256(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "checksum:", err)
		return 3
	}
	printSummary(sum)
	return selfCheck(sum, crashAfter)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func printSummary(s pipeline.Summary) {
	fmt.Printf("input_packets: %d\n", s.InputPackets)
	fmt.Printf("loss: %d\n", s.Loss)
	fmt.Printf("reordered: %d\n", s.Reordered)
	fmt.Printf("duplicates: %d\n", s.Duplicates)
	fmt.Printf("unrecoverable: %d\n", s.Unrecoverable)
	fmt.Printf("layout_errors: %d\n", s.LayoutErrors)
	fmt.Printf("located_packet: %d\n", s.LocatedPacket)
	fmt.Printf("loc_error_packets: %d\n", s.LocErrorPackets)
	fmt.Printf("rewinds: %d\n", s.Rewinds)
	fmt.Printf("restart_ms: %d\n", s.RestartMs)
	fmt.Printf("checksum_sha256: %s\n", s.ChecksumSHA256)
	fmt.Printf("faults: %d\n", s.Faults)
	for _, d := range s.Diagnostics {
		fmt.Printf("diagnostic: %s\n", d)
	}
}

// selfCheck enforces the acceptance bounds: fault location error of at
// most one packet and recovery of the packet stream in under 2000 ms.
func selfCheck(s pipeline.Summary, crashAfter int) int {
	if s.Unrecoverable > 0 && s.LocatedPacket < 0 {
		fmt.Fprintln(os.Stderr, "unrecoverable input could not be located")
		return 2
	}
	if s.LocErrorPackets > 1 {
		fmt.Fprintf(os.Stderr, "location error %d exceeds 1 packet\n", s.LocErrorPackets)
		return 1
	}
	if crashAfter > 0 && s.RestartMs >= 2000 {
		fmt.Fprintf(os.Stderr, "restart took %d ms, limit is 2000 ms\n", s.RestartMs)
		return 1
	}
	if len(s.ChecksumSHA256) != 64 {
		fmt.Fprintln(os.Stderr, "checksum is not 64 hex characters")
		return 1
	}
	return 0
}
