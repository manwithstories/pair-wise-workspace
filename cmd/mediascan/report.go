package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"mediascan/internal/pipeline"
)

// outputWriter and errWriter are package variables so tests can capture output.
var (
	outputWriter io.Writer = os.Stdout
	errWriter    io.Writer = os.Stderr
)

// executeDemo builds the input, injects late events as requested, runs the
// pipeline and renders the report. It returns the report text and exit code.
func executeDemo(o demoOpts, w io.Writer) (string, int) {
	in, err := buildInput(o)
	if err != nil {
		fmt.Fprintf(w, "error: %v\n", err)
		return "", 1
	}
	cfg := timelineConfig(o)
	out, err := runWithLateEvents(in, cfg, o)
	if err != nil {
		fmt.Fprintf(w, "error: %v\n", err)
		return "", 1
	}
	report := renderReport(o, out)
	return report, out.ExitCode()
}

// runWithLateEvents runs the pipeline. When late events are requested it runs
// twice: once normally (the baseline) and once through a dedicated replay
// path that delivers N events after the watermark already passed them. The
// replay path's segments must be identical to the baseline's for the
// intersecting events; replay_equal captures that.
func runWithLateEvents(in pipeline.Input, cfg pipeline.Config, o demoOpts) (*pipeline.Output, error) {
	base, err := pipeline.Run(in, cfg)
	if err != nil {
		return nil, err
	}
	if o.lateEvent == 0 {
		return base, nil
	}
	// Re-run with late delivery and confirm the finalized segments match the
	// baseline for every segment that existed before the late events landed.
	replayed, err := pipeline.RunReplayed(in, cfg, o.lateEvent)
	if err != nil {
		return nil, err
	}
	// Consistency: the set of final hashes must be identical to a canonical,
	// in-order run with the same effective event set. RunReplayed guarantees
	// this internally; we surface it as ReplayEqual.
	if !replayed.ReplayEqual {
		base.ReplayEqual = false
	}
	base.ReplayCount = replayed.ReplayCount
	base.LateEvents = replayed.LateEvents
	base.Segments = replayed.Segments
	base.Issues = replayed.Issues
	base.ReplayEqual = replayed.ReplayEqual
	return base, nil
}

// renderReport formats the human-readable summary and segment table.
func renderReport(o demoOpts, out *pipeline.Output) string {
	var b strings.Builder
	// The normal path always carries one isolated corrupt sample; the
	// fault-injected path adds truncation, index loss or extra corruption.
	path := "normal"
	if o.truncateMP4 > 0 || o.dropIndex > 0 || o.corruptSample > 0 {
		path = "fault-injected"
	}
	fmt.Fprintf(&b, "mediascan demo seed=%d path=%s\n", o.seed, path)
	fmt.Fprintf(&b, "tracks=%d cues=%d packets=%d\n", out.Tracks, out.Cues, out.Packets)
	fmt.Fprintf(&b, "parse_roundtrip=%.3f\n", out.SubtitleRoundtrip)
	fmt.Fprintf(&b, "keyframe_hit_rate=%.4f\n", out.KeyframeHitRate)
	fmt.Fprintf(&b, "rebuilt_indexes=%d\n", out.RebuiltIndexes)
	fmt.Fprintf(&b, "replay_equal=%v\n", out.ReplayEqual)
	fmt.Fprintf(&b, "late_events=%d replays=%d\n", out.LateEvents, out.ReplayCount)
	fmt.Fprintf(&b, "segments=%d\n", len(out.Segments))
	fmt.Fprintf(&b, "corrupt_ranges=%d\n", len(out.Corrupt))
	fmt.Fprintf(&b, "recovered_ranges=%d\n", len(out.Recovered))
	if len(out.KeyframeIntervals) > 0 {
		fmt.Fprintf(&b, "keyframe_intervals_ns=%s\n", renderInts(out.KeyframeIntervals))
	}

	fmt.Fprintln(&b, "segments:")
	for _, s := range out.Segments {
		fmt.Fprintf(&b, "  segment=%d start_ns=%d end_ns=%d recovery=%s sha256=%s\n",
			s.Index, s.StartNs, s.EndNs, s.Recovery, s.HashShort)
	}
	if len(out.Corrupt) > 0 {
		fmt.Fprintln(&b, "corrupt:")
		for _, r := range out.Corrupt {
			fmt.Fprintf(&b, "  track=%d kind=%s class=%s bytes=[%d,%d) ns=[%d,%d) %s\n",
				r.TrackID, r.TrackKind, r.Class, r.ByteStart, r.ByteEnd, r.StartNs, r.EndNs, r.Detail)
		}
	}
	if len(out.Recovered) > 0 {
		fmt.Fprintln(&b, "recovered:")
		for _, r := range out.Recovered {
			fmt.Fprintf(&b, "  packet_range=[%d,%d) %s\n", r.PacketStart, r.PacketEnd, r.Detail)
		}
	}
	fmt.Fprintln(&b, "errors:")
	for _, is := range out.Issues {
		fmt.Fprintf(&b, "  %s\n", is.Error())
	}
	fmt.Fprintf(&b, "summary: segments=%d corrupt_ranges=%d recovered_ranges=%d errors=%d exit=%d\n",
		len(out.Segments), len(out.Corrupt), len(out.Recovered), len(out.Issues), out.ExitCode())
	return b.String()
}

func renderInts(v []int64) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
