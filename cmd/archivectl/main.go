// archivectl is the terminal front-end of the media archive library.
// It only demonstrates and verifies; all processing lives in internal/.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"mediarchive/internal/bitrate"
	"mediarchive/internal/clock"
	"mediarchive/internal/event"
	"mediarchive/internal/fixture"
	"mediarchive/internal/index"
	"mediarchive/internal/manifest"
	"mediarchive/internal/repair"
	"mediarchive/internal/rtp"
	"mediarchive/internal/segment"
	"mediarchive/internal/timeline"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: archivectl <command> [flags]\ncommands: demo")
		return 2
	}
	if args[0] == "demo" {
		return demo(args[1:])
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
	return 2
}

func demo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	codec := fs.String("codec", "opus", "codec: opus | aac")
	dur := fs.Duration("duration", 60*time.Second, "source material duration")
	seed := fs.Int64("seed", 1, "deterministic generator seed")
	lossBurst := fs.Int("loss-burst", 0, "packets dropped per loss burst")
	reorder := fs.Int("reorder", 0, "packets delayed by one position")
	dup := fs.Int("duplicate", 0, "packets emitted twice")
	lateWindow := fs.Duration("late-window", time.Second, "lateness window for replay recompute")
	target := fs.Float64("target-kbps", 96, "target media bitrate")
	corruptTail := fs.Int("corrupt-tail", 0, "tail packets truncated")
	outDir := fs.String("out", "out", "output directory for segments and manifest")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dur <= 0 || *target <= 0 || *lateWindow < 0 ||
		*lossBurst < 0 || *reorder < 0 || *dup < 0 || *corruptTail < 0 {
		fmt.Fprintln(os.Stderr, "invalid parameters: durations and rates must be positive, counts non-negative")
		return 2
	}
	if strings.Contains(*outDir, "..") {
		fmt.Fprintln(os.Stderr, "invalid --out: path traversal rejected")
		return 2
	}

	stream, err := fixture.Generate(fixture.Params{
		Codec: *codec, Duration: *dur, Seed: *seed,
		LossBurst: *lossBurst, Reorder: *reorder, Duplicate: *dup,
		CorruptTail: *corruptTail, TargetKbps: *target,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid parameters:", err)
		return 2
	}

	// Ingest: RTP receive -> unified clock -> watermark timeline.
	recv := rtp.NewReceiver(fixture.SSRC)
	tl := timeline.New(clock.FromDuration(*lateWindow))
	var quarantine []event.Quarantine
	malformed := 0
	for i, raw := range stream.Raw {
		p, q, err := recv.Accept(raw, stream.Arrival[i], stream.Config)
		if err != nil {
			var ee *event.Error
			if errors.As(err, &ee) && ee.Reason == event.ReasonUndecodableConfig {
				quarantine = append(quarantine, event.Quarantine{Reason: ee.Reason, Detail: ee.Error()})
				continue
			}
			malformed++
			continue
		}
		if q != nil {
			quarantine = append(quarantine, *q)
			continue
		}
		if p == nil { // duplicate or foreign SSRC
			continue
		}
		tl.Add(p)
	}

	// Repair -> index -> segment -> manifest.
	res := repair.Repair(tl.Sorted(), quarantine)
	idx := index.Build(res.Packets, segment.DefaultDuration)
	var infos []segment.Info
	if len(res.Packets) > 0 {
		infos, err = segment.Write(*outDir, res.Packets, stream.Config, segment.DefaultDuration)
		if err != nil {
			fmt.Fprintln(os.Stderr, "segment:", err)
			return 1
		}
	}
	sha := fmt.Sprintf("%x", tl.Digest())
	mani := manifest.Build(stream.Config, infos, res.Gaps, tl.Unrepairable,
		len(quarantine), res.Degraded, sha)
	var nominal clock.Time
	if len(res.Packets) > 0 {
		nominal = res.Packets[0].Duration
	}
	align, problems := mani.Validate(idx, nominal)
	if err := mani.Write(*outDir); err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		return 1
	}

	// Bitrate and quality summary.
	stats := bitrate.Counter{
		PacketsIn:   recv.Stats.PacketsIn,
		Duplicates:  recv.Stats.Duplicates,
		LateReplays: tl.Replays,
		TooLate:     tl.TooLate,
		Quarantined: len(quarantine),
		RepairedTS:  res.RepairedTS,
		TargetKbps:  *target,
	}
	for _, g := range res.Gaps {
		stats.Lost += int(g.Count)
		if g.Source == "loss_burst" {
			stats.LossBursts++
		}
	}
	for _, p := range res.Packets {
		stats.PayloadBytes += int64(len(p.Payload))
		stats.MediaTime += p.Duration
	}

	status := "ok"
	switch {
	case res.Fatal():
		status = "failed"
	case len(problems) > 0 || len(res.Degraded) > 0 || len(quarantine) > 0 || tl.TooLate > 0:
		status = "degraded"
	}

	fmt.Printf("status=%s\n", status)
	fmt.Printf("packets_in=%d\n", stats.PacketsIn)
	fmt.Printf("accepted=%d\n", len(res.Packets))
	fmt.Printf("duplicates=%d\n", stats.Duplicates)
	fmt.Printf("malformed=%d\n", malformed)
	fmt.Printf("lost_packets=%d\n", stats.Lost)
	fmt.Printf("loss_bursts=%d\n", stats.LossBursts)
	fmt.Printf("late_replays=%d\n", stats.LateReplays)
	fmt.Printf("too_late=%d\n", stats.TooLate)
	fmt.Printf("quarantined=%d\n", stats.Quarantined)
	fmt.Printf("repaired_ts=%d\n", stats.RepairedTS)
	fmt.Printf("index_entries=%d\n", len(idx.Entries))
	fmt.Printf("segments=%d\n", len(infos))
	fmt.Printf("target_kbps=%.3f\n", stats.TargetKbps)
	fmt.Printf("actual_kbps=%.3f\n", stats.ActualKbps())
	fmt.Printf("bitrate_error_pct=%.3f\n", stats.ErrorPct())
	fmt.Printf("quality_score=%.2f\n", stats.QualityScore())
	fmt.Printf("manifest_alignment_ms=%.3f\n", align.Millis())
	fmt.Printf("timeline_sha256=%s\n", sha)
	for _, q := range quarantine {
		fmt.Printf("quarantine_reason=%s seq=%d detail=%q\n", q.Reason, q.SeqExt, q.Detail)
	}
	for _, d := range res.Degraded {
		fmt.Printf("degrade_reason=%s: %s\n", d.Reason, d.Detail)
	}
	for _, pr := range problems {
		fmt.Printf("manifest_problem=%s\n", pr)
	}
	fmt.Printf("out_dir=%s\n", *outDir)

	if res.Fatal() {
		return 1
	}
	return 0
}
