// Command medpipe demonstrates the reusable media processing library:
// deterministic fixture generation, scan/index validation, streaming
// processing and RTP loss recovery, all from a terminal with no UI or
// external services.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"

	"medpipe/internal/fixture"
	"medpipe/internal/index"
	"medpipe/internal/metadata"
	"medpipe/internal/pipeline"
	"medpipe/internal/rtp"
	"medpipe/internal/scan"
)

const (
	exitOK       = 0
	exitCorrupt  = 1
	exitUsage    = 2
	syncLimitMS  = 20.0
	recoverLimit = 95.0
	p95LimitMS   = 120.0
)

type demoFlags struct {
	seed            int64
	lossBurst       int
	latencyMS       int
	frames          int
	corruptFrame    int
	truncateBytes   int
	indexIncomplete bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "help", "-h", "--help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `medpipe - local media processing demo

usage:
  medpipe demo [flags]

flags:
  --seed int             deterministic RNG seed (default 42)
  --loss-burst int       consecutive packets dropped per burst (default 3)
  --latency-ms int       target jitter budget, informational (default 80)
  --frames int           video frames to generate (default 240)
  --corrupt-frame int    corrupt the selected video frame (-1 disables)
  --truncate-bytes int   drop N bytes from the container tail
  --index-incomplete     leave a stranded temp index to force rebuild

exit codes: 0 ok and metrics pass; 1 corrupt input or failed metrics; 2 usage
`)
}

func parseFlags(args []string) (*demoFlags, *flag.FlagSet) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	d := &demoFlags{seed: 42, lossBurst: 3, latencyMS: 80, frames: 240, corruptFrame: -1}
	fs.Int64Var(&d.seed, "seed", d.seed, "deterministic RNG seed")
	fs.IntVar(&d.lossBurst, "loss-burst", d.lossBurst, "burst loss length")
	fs.IntVar(&d.latencyMS, "latency-ms", d.latencyMS, "jitter budget ms")
	fs.IntVar(&d.frames, "frames", d.frames, "video frame count")
	fs.IntVar(&d.corruptFrame, "corrupt-frame", d.corruptFrame, "frame to corrupt")
	fs.IntVar(&d.truncateBytes, "truncate-bytes", d.truncateBytes, "tail bytes to drop")
	fs.BoolVar(&d.indexIncomplete, "index-incomplete", false, "simulate interrupted index write")
	return d, fs
}

func runDemo(args []string) int {
	d, fs := parseFlags(args)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if d.frames <= 0 || d.lossBurst < 0 || d.latencyMS < 0 {
		fmt.Fprintln(os.Stderr, "invalid flag value")
		return exitUsage
	}

	// 1. Generate the container entirely in process.
	raw, err := fixture.Generate(fixture.Options{Seed: d.seed, Frames: d.frames})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture: %v\n", err)
		return exitCorrupt
	}
	if d.corruptFrame >= 0 {
		if _, cerr := fixture.CorruptFrame(raw, uint32(d.corruptFrame)); cerr != nil {
			fmt.Fprintf(os.Stderr, "corrupt: %v\n", cerr)
			return exitUsage
		}
	}
	if d.truncateBytes > 0 {
		raw = fixture.Truncate(raw, d.truncateBytes)
	}

	// 2. Scan and distinguish tail truncation from mid-stream corruption.
	rd, err := scan.NewReader(bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan: %v\n", err)
		return exitCorrupt
	}
	res, err := rd.Scan()
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan: %v\n", err)
		return exitCorrupt
	}
	if res.FirstCorruption != nil {
		fmt.Printf("first_corrupt_frame_offset=%d frame_seq=%d code=%s\n",
			res.FirstCorruption.Offset, res.FirstCorruption.Seq, res.FirstCorruption.Code)
	}
	tailRecovered := 0
	if res.TailTruncated {
		tailRecovered = res.CompleteRecords
		fmt.Printf("truncated_tail=recoverable boundary_offset=%d\n", res.RecoverableOffset)
	}
	fmt.Printf("tail_recovered_frames=%d\n", tailRecovered)

	// 3. Digest-bound index with atomic store and rebuild reasons.
	digest, size, derr := scan.DigestSource(bytes.NewReader(raw))
	if derr != nil {
		fmt.Fprintf(os.Stderr, "digest: %v\n", derr)
		return exitCorrupt
	}
	store := index.NewStore(os.TempDir())
	sourceName := fmt.Sprintf("medpipe-seed-%d-frames-%d", d.seed, d.frames)
	if d.indexIncomplete {
		// A canonical index from an earlier run must not mask the stranded
		// temp file that proves the interrupted write.
		_ = os.Remove(store.Path(sourceName))
		if err := store.MarkIncomplete(sourceName, []byte(`{"schema_version":`)); err != nil {
			fmt.Fprintf(os.Stderr, "index: %v\n", err)
			return exitCorrupt
		}
	}
	idx, reason, lerr := store.LoadOrRebuild(sourceName, digest, size, func() (*scan.ScanIndex, error) {
		return scan.BuildIndex(res, digest, size), nil
	})
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "index: %v\n", lerr)
		return exitCorrupt
	}
	if reason != "" {
		fmt.Printf("index_rebuild_reason=%s\n", reason)
	} else {
		fmt.Printf("index_rebuild_reason=reused\n")
	}

	// 4. Parse MP4 metadata via go-mp4 from the in-memory generated fixture.
	mp4Bytes, merr := metadata.Marshal(metadata.VideoTrackSpec{
		TrackID: 1, Timescale: 24000, Width: 64, Height: 64,
		Duration: uint64(d.frames) * 1000,
	}, uint32(d.frames))
	if merr != nil {
		fmt.Fprintf(os.Stderr, "metadata: %v\n", merr)
		return exitCorrupt
	}
	if _, perr := metadata.Probe(bytes.NewReader(mp4Bytes)); perr != nil {
		fmt.Fprintf(os.Stderr, "metadata probe: %v\n", perr)
		return exitCorrupt
	}

	// 5. Stream through the full processing pipeline.
	runner, err := pipeline.New(pipeline.Config{
		WindowNS:      1_000_000_000,
		SilenceThresh: 4,
		SilenceMin:    160,
		RTP: rtp.Config{
			Seed: d.seed, LossBurst: d.lossBurst, BurstEvery: 40,
			ReorderWindow: 3, BufferSlots: pipeline.MaxBufferedFrames,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipeline: %v\n", err)
		return exitUsage
	}
	metrics, perr := runner.Run(context.Background(), res.Frames, res.Audio, tailRecovered, nil)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "pipeline: %v\n", perr)
		return exitCorrupt
	}

	printMetrics(metrics, idx)
	passed := metrics.AVSyncErrorMS <= syncLimitMS &&
		metrics.PacketRecoveryPct >= recoverLimit &&
		metrics.LatencyP95MS <= p95LimitMS &&
		metrics.FramesIndexed == d.frames &&
		res.FirstCorruption == nil
	if !passed {
		fmt.Println("status=metrics_or_integrity_failed")
		return exitCorrupt
	}
	fmt.Println("status=ok")
	return exitOK
}

func printMetrics(m *pipeline.Metrics, idx *scan.ScanIndex) {
	fmt.Printf("frames_indexed=%d\n", m.FramesIndexed)
	fmt.Printf("truncated_tail_recovered=%d\n", m.TruncatedTailRecovered)
	fmt.Printf("segments=%d silence_intervals=%d unique_payloads=%d reused_refs=%d\n",
		m.Segments, m.SilenceIntervals, m.UniquePayloads, m.ReusedRefs)
	fmt.Printf("packets_sent=%d packets_lost=%d frames_lost=%d unrecoverable=%v\n",
		m.PacketsSent, m.PacketsLost, m.FramesLost, m.UnrecoverableSeqs)
	fmt.Printf("packet_recovery_pct=%.2f\n", m.PacketRecoveryPct)
	fmt.Printf("av_sync_error_ms=%.3f\n", m.AVSyncErrorMS)
	fmt.Printf("latency_p95_ms=%.3f\n", m.LatencyP95MS)
	fmt.Printf("buffer_peak=%d run_digest=%s\n", m.FramesBufferedPeak, m.Digest)
	if idx != nil {
		fmt.Printf("index_frames=%d index_audio=%d time_span_ns=%d..%d\n",
			idx.FrameCount, idx.AudioCount, idx.TimeMinNS, idx.TimeMaxNS)
	}
}
