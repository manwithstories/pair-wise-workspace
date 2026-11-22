package pipeline

import (
	"mediascan/internal/media"
	"mediascan/internal/timeline"
)

// RunReplayed runs the pipeline and then exercises the late-event path: the
// last `lateCount` events are withheld, the watermark is advanced past them,
// and they are then delivered, forcing the timeline to replay committed
// segments from a checkpoint.
//
// Consistency contract: because the timeline recomputes segments from the full
// event set, the replayed output must equal a canonical in-order run over the
// same events. RunReplayed verifies this within the timeline engine and
// reports it through Output.ReplayEqual.
func RunReplayed(in Input, cfg Config, lateCount int) (*Output, error) {
	cfg = cfg.normalize()
	out := &Output{ReplayEqual: true}

	subEvents, _, err := runSubtitles(in, cfg, out)
	if err != nil {
		return out, err
	}
	mediaEvents, trackIDs, err := runMedia(in, cfg, out)
	if err != nil {
		return out, err
	}
	runRTP(in, cfg, out)

	evs := mergeEvents(subEvents, mediaEvents)

	// Withhold the tail. If the user asked for more late events than exist,
	// cap at the available count.
	if lateCount > len(evs) {
		lateCount = len(evs)
	}
	onTime := evs[:len(evs)-lateCount]
	late := evs[len(evs)-lateCount:]

	eng := timeline.NewEngine(timeline.Config{
		SegmentNs: cfg.SegmentNs, TrackIDs: trackIDs, CheckpointEvery: cfg.CheckpointEvery,
	})
	for _, ev := range onTime {
		eng.AdvanceWatermark(ev.StartNs)
		eng.Add(ev)
	}
	// The watermark now sits past the late events' start times, so delivering
	// them is genuinely late and triggers replay.
	if len(late) > 0 {
		eng.AdvanceWatermark(lastBoundary(evs, cfg.SegmentNs))
	}
	for _, ev := range late {
		eng.Add(ev)
	}
	if len(evs) > 0 {
		eng.AdvanceWatermark(lastBoundary(evs, cfg.SegmentNs))
	}

	out.Issues.Add(eng.Issues()...)
	out.ReplayCount = eng.ReplayCount()
	out.LateEvents = len(eng.LateEvents())
	for _, s := range eng.Segments() {
		out.Segments = append(out.Segments, toSegment(s))
	}
	// The strongest consistency check: a fresh, canonical in-order run over
	// the same event set must produce byte-identical per-segment hashes.
	// replay_equal is true only if the late-delivery path and the canonical
	// path agree everywhere.
	tlCfg := timeline.Config{
		SegmentNs: cfg.SegmentNs, TrackIDs: trackIDs, CheckpointEvery: cfg.CheckpointEvery,
	}
	canon, err := runCanonical(evs, tlCfg)
	if err != nil {
		return out, err
	}
	out.ReplayEqual = eng.ReplayEqual() && timeline.VerifyReplay(evs, tlCfg) && hashesEqual(canon, out.Segments)
	if !out.ReplayEqual {
		out.Issues.Add(media.NewFatal(media.ClassPipelineInconsistent, media.Locator{},
			"late-event replay diverged from the canonical run"))
	}
	if in.SubtitleBytes != nil && out.SubtitleRoundtrip < 1.0 {
		out.Issues.Add(media.NewIssue(media.ClassSubtitleTruncatedCue, media.Locator{},
			"subtitle round trip matched only %.3f of cues", out.SubtitleRoundtrip))
	}
	out.Issues.Sort()
	out.Fatal = out.Issues.Fatal()
	return out, nil
}

// runCanonical performs a fresh in-order run over evs and returns its
// finalized segments. It is the reference the late-delivery path is checked
// against.
func runCanonical(evs []timeline.Event, cfg timeline.Config) ([]Segment, error) {
	eng := timeline.NewEngine(cfg)
	for _, ev := range evs {
		eng.AdvanceWatermark(ev.StartNs)
		eng.Add(ev)
	}
	if len(evs) > 0 {
		var maxEnd int64
		for _, e := range evs {
			if e.EndNs > maxEnd {
				maxEnd = e.EndNs
			}
		}
		eng.AdvanceWatermark((maxEnd/cfg.SegmentNs + 1) * cfg.SegmentNs)
	}
	out := make([]Segment, 0, len(eng.Segments()))
	for _, s := range eng.Segments() {
		out = append(out, toSegment(s))
	}
	return out, nil
}

// hashesEqual reports whether two segment lists agree on count and every
// per-segment digest, which is the replay_equal contract.
func hashesEqual(a, b []Segment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Index != b[i].Index || a[i].Hash != b[i].Hash {
			return false
		}
	}
	return true
}
