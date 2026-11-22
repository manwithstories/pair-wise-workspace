package main

import (
	"flag"
	"fmt"
	"io"

	"mediascan/internal/fixture"
	"mediascan/internal/media"
	"mediascan/internal/pipeline"
	"mediascan/internal/subtitle"
)

// demoOpts are the parsed command-line options for the demo command.
type demoOpts struct {
	seed          int64
	lateEvent     int
	truncateMP4   int
	dropIndex     int
	corruptSample int
	subFormat     string
	segmentMs     int
}

// runDemo is the acceptance entry point. It builds fixtures deterministically
// from the seed, runs the pipeline, prints the summary and per-segment rows,
// and returns the contract exit code.
func runDemo(args []string) int {
	opts, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(errWriter, "mediascan demo: %v\n", err)
		return 1
	}
	report, code := executeDemo(opts, outputWriter)
	fmt.Fprint(outputWriter, report)
	return code
}

func parseDemoFlags(args []string) (demoOpts, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o demoOpts
	fs.Int64Var(&o.seed, "seed", 41, "fixture seed")
	fs.IntVar(&o.lateEvent, "late-event", 0, "events delivered after the watermark")
	fs.IntVar(&o.truncateMP4, "truncate-mp4", 0, "bytes cut from the MP4 tail")
	fs.IntVar(&o.dropIndex, "drop-index", 0, "keyframe index entries to omit")
	fs.IntVar(&o.corruptSample, "corrupt-samples", 0, "samples with corrupt declared length")
	fs.StringVar(&o.subFormat, "subtitle-format", "srt", "srt or vtt")
	fs.IntVar(&o.segmentMs, "segment-ms", 1000, "timeline segment length in ms")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.lateEvent < 0 || o.truncateMP4 < 0 || o.dropIndex < 0 || o.corruptSample < 0 {
		return o, fmt.Errorf("negative counts are not allowed")
	}
	if o.lateEvent > 64 {
		return o, fmt.Errorf("--late-event %d exceeds limit 64", o.lateEvent)
	}
	if o.segmentMs <= 0 || o.segmentMs > 60_000 {
		return o, fmt.Errorf("--segment-ms must be in (0, 60000]")
	}
	if o.subFormat != "srt" && o.subFormat != "vtt" {
		return o, fmt.Errorf("--subtitle-format must be srt or vtt")
	}
	return o, nil
}

// buildInput synthesizes the input assets for the demo.
func buildInput(o demoOpts) (pipeline.Input, error) {
	// Video: 30 frames at 29.97 (~1.0s), keyframe every 10 frames.
	const frames = 300
	videoSizes := make([]uint32, frames)
	for i := range videoSizes {
		videoSizes[i] = uint32(600 + (i%7)*40)
	}
	sync := make([]int, 0, frames/10)
	for n := 1; n <= frames; n += 10 {
		sync = append(sync, n)
	}
	// The sample payloads always mark every real keyframe. --drop-index then
	// trims the declared stss only, so the analyzer's sample scan sees the
	// missing entries and rebuilds the index.
	markers := append([]int(nil), sync...)
	declared := append([]int(nil), sync...)
	if o.dropIndex > 0 {
		if o.dropIndex >= len(declared) {
			declared = nil
		} else {
			declared = declared[:len(declared)-o.dropIndex]
		}
	}
	audioSizes := make([]uint32, 94)
	for i := range audioSizes {
		audioSizes[i] = 128
	}
	// The normal path intentionally includes one damaged sample so the demo
	// demonstrates isolating a locatable corrupt range (corrupt_ranges ~ 1)
	// while the rest of both tracks continue to aggregate. --corrupt-samples
	// adds extra damaged samples on top of this baseline.
	corruptIdx := 15
	video := fixture.TrackSpec{
		Kind: "video", Timescale: 30000, SampleDelta: 1001,
		SampleSizes: videoSizes, SyncSamples: declared, KeyframeMarkers: markers,
		CorruptSizeIdx: corruptIdx,
	}
	audio := fixture.TrackSpec{
		Kind: "audio", Timescale: 48000, SampleDelta: 1024,
		SampleSizes: audioSizes, NoSTSS: true, CorruptSizeIdx: -1,
	}
	mp4, err := fixture.MP4([]fixture.TrackSpec{video, audio})
	if err != nil {
		return pipeline.Input{}, fmt.Errorf("build mp4: %w", err)
	}
	if o.truncateMP4 > 0 {
		cut := len(mp4) - o.truncateMP4
		if cut < 0 {
			cut = 0
		}
		mp4 = mp4[:cut]
	}

	// Subtitles: one cue per second, 12 cues.
	step := int64(media.NanosPerSecond)
	dur := int64(700 * 1_000_000)
	var subBytes []byte
	format := subtitle.FormatSRT
	if o.subFormat == "vtt" {
		subBytes = []byte(fixture.VTT(12, 500*1_000_000, step, dur))
		format = subtitle.FormatVTT
	} else {
		subBytes = []byte(fixture.SRT(12, 500*1_000_000, step, dur))
	}
	return pipeline.Input{
		SubtitleBytes: subBytes, SubtitleFormat: format, SubtitleTrackID: 100,
		MP4Bytes: mp4, RTPPackets: rtpFixture(), RTPClockRate: 90000,
	}, nil
}

// rtpFixture builds a small RTP sequence containing a wrap, a duplicate, an
// out-of-order packet and one truncated packet, so the demo exercises every
// RTP fault class.
func rtpFixture() [][]byte {
	pkts := make([][]byte, 0, 24)
	seq := uint16(65530)
	for i := 0; i < 20; i++ {
		pkts = append(pkts, fixture.RTPPacket(seq, uint32(i)*3000, 160))
		seq++
	}
	// Duplicate of the packet just emitted.
	pkts = append(pkts, pkts[len(pkts)-1])
	// Out-of-order: replay an earlier sequence number.
	pkts = append(pkts, fixture.RTPPacket(65535, 0, 160))
	// Truncated packet (shorter than the 12-byte minimum header).
	trunc := fixture.RTPPacket(seq, 60000, 160)
	pkts = append(pkts, trunc[:6])
	return pkts
}

// timelineConfig builds the pipeline config from options.
func timelineConfig(o demoOpts) pipeline.Config {
	cfg := pipeline.Config{
		SegmentNs:       int64(o.segmentMs) * 1_000_000,
		CheckpointEvery: 5,
		SubtitleTrackID: 100,
		VideoFrameRate:  media.Rate2997,
	}
	return cfg
}
