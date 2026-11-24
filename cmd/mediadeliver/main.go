// Command mediadeliver runs the local multi-camera delivery kernel as a
// terminal demo. It generates deterministic fixtures in memory, remuxes them
// through the bounded streaming pipeline and prints machine-readable metrics.
//
// Exit codes: 0 success (corrupt samples handled and recovered), 2 invalid
// input/parameters, 3 processing/verification failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"mediadeliver/internal/fixture"
	"mediadeliver/internal/manifest"
	"mediadeliver/internal/pipeline"
)

func strReader(s string) *strings.Reader { return strings.NewReader(s) }

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 2
	}
	if args[0] != "demo" {
		fmt.Fprintf(os.Stderr, "error: unknown subcommand %q (only \"demo\" is supported)\n", args[0])
		return 2
	}

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic random seed for fixtures")
	queue := fs.Int("queue", 8, "bounded pipeline queue capacity (packets)")
	slowMs := fs.Int("slow-consumer-ms", 20, "per-write delay in milliseconds to force back-pressure")
	corruptEvery := fs.Int("corrupt-every", 7, "1-based write index to fault once per segment (0 disables)")
	maxPkt := fs.Int("max-packet-bytes", 4096, "maximum single packet payload in bytes")
	cpPath := fs.String("checkpoint", "", "checkpoint file path (default: temp dir)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *queue <= 0 || *slowMs < 0 || *corruptEvery < 0 || *maxPkt <= 0 || *seed < 0 {
		fmt.Fprintln(os.Stderr, "error: queue/max-packet-bytes must be > 0 and delays/seeds non-negative")
		return 2
	}

	manifestText, sources, err := buildDemoFixtures(*seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to build fixtures: %v\n", err)
		return 2
	}
	man, err := manifest.Parse(strReader(manifestText))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid generated manifest: %v\n", err)
		return 2
	}

	checkpoint := *cpPath
	if checkpoint == "" {
		// A unique checkpoint per invocation keeps the self-contained demo
		// idempotent: every run exercises the full pipeline instead of
		// resuming a previous process's state. Pass --checkpoint to test
		// explicit resume behavior.
		f, err := os.CreateTemp("", "mediadeliver-cp-*.json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot create checkpoint: %v\n", err)
			return 3
		}
		checkpoint = f.Name()
		_ = f.Close()
		_ = os.Remove(checkpoint) // start from a non-existent file
	}

	cfg := pipeline.Config{
		QueueCapacity:     *queue,
		MaxPacketBytes:    *maxPkt,
		SlowConsumerDelay: time.Duration(*slowMs) * time.Millisecond,
		CorruptEvery:      *corruptEvery,
	}
	runner := pipeline.NewRunner(checkpoint, cfg)
	rep, err := runner.Run(context.Background(), man, []byte(manifestText), sources)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: processing/verification failed: %v\n", err)
		return 3
	}

	if printReport(rep, *queue, *maxPkt) {
		return 0
	}
	return 3
}

// buildDemoFixtures returns the manifest text and matching in-memory sources.
func buildDemoFixtures(seed int64) (string, map[string]*pipeline.MaterialSource, error) {
	rng := fixture.SeededRand(seed)
	const samples = 10

	cam1 := fixture.BuildMP4([]fixture.TrackSpec{
		{ID: 1, Video: true, Codec: "avc1", Timescale: 90000, Samples: samples,
			KeyInterval: 1, PayloadBytes: 48, WithElst: true, MediaTime: 0},
		{ID: 2, Video: false, Codec: "mp4a", Timescale: 48000, Samples: samples, PayloadBytes: 32},
	}, rng)
	cam2 := fixture.BuildMP4([]fixture.TrackSpec{
		{ID: 1, Video: true, Codec: "avc1", Timescale: 90000, Samples: samples,
			KeyInterval: 2, PayloadBytes: 48},
		{ID: 2, Video: false, Codec: "mp4a", Timescale: 48000, Samples: samples, PayloadBytes: 32},
	}, rng)
	rtp := fixture.BuildRTPCapture(fixture.RTPSequence(samples, 0, 1, uint32(seed&0xffff), 0))

	manifestText := fmt.Sprintf(`
MATERIAL cam1 cam1.mp4 mp4
TRACK cam1 1 video avc1
TRACK cam1 2 audio mp4a
MATERIAL cam2 cam2.mp4 mp4
TRACK cam2 1 video avc1
TRACK cam2 2 audio mp4a
MATERIAL rec1 rec1.rtp rtp
TRACK rec1 11 audio mp4a
EDIT segA cam1 1 0 %d 90000
EDIT segA cam1 2 0 %d 48000
MAP segA video cam1 1
MAP segA audio cam1 2
EDIT segB cam2 1 0 %d 90000
EDIT segB cam2 2 0 %d 48000
MAP segB video cam2 1
MAP segB audio cam2 2
EDIT segC rec1 11 0 %d 1
MAP segC audio rec1 11
`, samples, samples, samples, samples, samples)

	sources := map[string]*pipeline.MaterialSource{
		"cam1": {Material: findMat(manifestText, "cam1"), Data: cam1, Format: "mp4"},
		"cam2": {Material: findMat(manifestText, "cam2"), Data: cam2, Format: "mp4"},
		"rec1": {Material: findMat(manifestText, "rec1"), Data: rtp, Format: "rtp"},
	}
	return manifestText, sources, nil
}

// findMat re-parses the manifest to resolve a material descriptor for sources.
func findMat(text, id string) *manifest.Material {
	man, err := manifest.Parse(strReader(text))
	if err != nil {
		panic(err) // generated manifest is always valid
	}
	for _, m := range man.Materials {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func printReport(rep *pipeline.Report, queueCap, maxPkt int) bool {
	fmt.Println("mediadeliver demo run")
	fmt.Printf("parse_consistency=%.2f%%\n", rep.ParseConsistency)
	fmt.Printf("keyframe_hit_rate=%.2f%%\n", rep.KeyframeHitRate)
	fmt.Printf("segments=%d\n", len(rep.Segments))
	for _, rec := range rep.Segments {
		fmt.Printf("segment_sha256[%d]=%s packets=%d bytes=%d ranges=%d\n",
			rec.Index, rec.SHA256Hex, rec.PacketCount, rec.Bytes, len(rec.Ranges))
	}
	// Configured memory ceiling: queue capacity * per-packet payload bound.
	ceiling := int64(queueCap) * int64(maxPkt)
	fmt.Printf("peak_buffer_bytes=%d (configured ceiling %d)\n", rep.PeakBufferBytes, ceiling)
	fmt.Printf("peak_buffer_packets=%d\n", rep.PeakBufferPackets)
	fmt.Printf("backpressure_events=%d\n", rep.BackpressureEvents)
	fmt.Printf("recovered_segments=%d\n", rep.RecoveredSegments)
	fmt.Printf("isolated_tracks=%d\n", len(rep.IsolatedTracks))
	for _, e := range rep.TrackErrors {
		fmt.Printf("track_error: %v\n", e)
	}
	// Explicit pass/fail against the acceptance thresholds.
	ok := rep.ParseConsistency == 100.0 &&
		rep.KeyframeHitRate == 100.0 &&
		len(rep.Segments) >= 3 &&
		rep.PeakBufferBytes <= ceiling &&
		rep.BackpressureEvents > 0 &&
		rep.RecoveredSegments >= 1
	if ok {
		fmt.Println("result=PASS")
	} else {
		fmt.Println("result=FAIL")
	}
	return ok
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: mediadeliver demo [flags]")
	fmt.Fprintln(os.Stderr, "flags: --seed --queue --slow-consumer-ms --corrupt-every")
	fmt.Fprintln(os.Stderr, "       --max-packet-bytes --checkpoint")
}
