package pipeline

import (
	"mediascan/internal/index"
	"mediascan/internal/media"
	"mediascan/internal/segment"
	"mediascan/internal/stream"
	"mediascan/internal/subtitle"
	"mediascan/internal/timeline"
)

// Run executes the full pipeline over the input. It always returns a non-nil
// Output describing what happened; the error is reserved for caller bugs. All
// localized faults are reported through Output.Issues and Output.Corrupt.
func Run(in Input, cfg Config) (*Output, error) {
	cfg = cfg.normalize()
	out := &Output{ReplayEqual: true}

	// 1. Subtitles -> normalized events + round-trip score.
	subEvents, _, err := runSubtitles(in, cfg, out)
	if err != nil {
		return out, err
	}

	// 2. MP4 sample analysis + keyframe index -> media events.
	mediaEvents, trackIDs, err := runMedia(in, cfg, out)
	if err != nil {
		return out, err
	}

	// 3. RTP packet sequence analysis (informational ranges).
	runRTP(in, cfg, out)

	// 4. Timeline merge, watermark commit, late-event replay.
	eng := timeline.NewEngine(timeline.Config{
		SegmentNs: cfg.SegmentNs, TrackIDs: trackIDs, CheckpointEvery: cfg.CheckpointEvery,
	})
	evs := mergeEvents(subEvents, mediaEvents)
	for _, ev := range evs {
		eng.AdvanceWatermark(ev.StartNs)
		eng.Add(ev)
	}
	if len(evs) > 0 {
		eng.AdvanceWatermark(lastBoundary(evs, cfg.SegmentNs))
	}
	out.Issues.Add(eng.Issues()...)
	out.ReplayEqual = eng.ReplayEqual()
	out.ReplayCount = eng.ReplayCount()
	out.LateEvents = len(eng.LateEvents())

	// 5. Emit finalized segments with short hashes.
	for _, s := range eng.Segments() {
		out.Segments = append(out.Segments, toSegment(s))
	}

	// 6. Subtitle round-trip below 1.0 is itself a localized defect.
	if in.SubtitleBytes != nil && out.SubtitleRoundtrip < 1.0 {
		out.Issues.Add(media.NewIssue(media.ClassSubtitleTruncatedCue, media.Locator{},
			"subtitle round trip matched only %.3f of cues", out.SubtitleRoundtrip))
	}

	out.Issues.Sort()
	out.Fatal = out.Issues.Fatal()
	return out, nil
}

func runSubtitles(in Input, cfg Config, out *Output) ([]subtitle.TimelineEvent, uint32, error) {
	if in.SubtitleBytes == nil {
		return nil, cfg.SubtitleTrackID, nil
	}
	doc, err := subtitle.Parse(in.SubtitleFormat, in.SubtitleBytes, cfg.Limits.Subtitle)
	if err != nil {
		if is, ok := err.(media.Issue); ok {
			out.Issues.Add(is)
			out.Fatal = true
			return nil, 0, nil
		}
		return nil, 0, err
	}
	out.Cues = len(doc.Cues)
	out.Issues.Add(doc.Issues...)
	out.SubtitleRoundtrip = roundtripFraction(doc)
	tid := in.SubtitleTrackID
	if tid == 0 {
		tid = cfg.SubtitleTrackID
	}
	evs := make([]subtitle.TimelineEvent, 0, len(doc.Cues))
	for _, c := range doc.Cues {
		evs = append(evs, subtitle.TimelineEvent{TrackID: tid, Cue: c, Source: "subtitle"})
	}
	return evs, tid, nil
}

func roundtripFraction(doc *subtitle.Document) float64 {
	if doc == nil {
		return 0
	}
	if len(doc.Cues) == 0 {
		if doc.RoundtripEqual {
			return 1.0
		}
		return 0
	}
	if doc.RoundtripEqual {
		return 1.0
	}
	return 0.0
}

func runMedia(in Input, cfg Config, out *Output) ([]timeline.Event, []uint32, error) {
	if in.MP4Bytes == nil {
		return nil, nil, nil
	}
	fa, err := stream.AnalyzeMP4(in.MP4Bytes, cfg.Limits.Stream)
	if err != nil {
		if is, ok := err.(media.Issue); ok {
			out.Issues.Add(is)
			out.Fatal = true
			return nil, nil, nil
		}
		return nil, nil, err
	}
	out.Issues.Add(fa.Issues...)
	out.Corrupt = append(out.Corrupt, fa.CorruptRanges...)
	out.Tracks = len(fa.Tracks)

	var events []timeline.Event
	var trackIDs []uint32
	var hits, expected int
	for _, tr := range fa.Tracks {
		trackIDs = append(trackIDs, tr.TrackID)
		br := index.Build(tr)
		out.Issues.Add(br.Issues...)
		if br.Rebuilt {
			out.RebuiltIndexes++
		}
		hits += indexHits(br.Index, tr)
		expected += indexExpected(tr)
		if tr.Kind == "video" {
			out.KeyframeIntervals = append(out.KeyframeIntervals, br.Index.Intervals(tr)...)
		}
		events = append(events, trackEvents(in.MP4Bytes, tr, br.Index)...)
	}
	if expected > 0 {
		out.KeyframeHitRate = float64(hits) / float64(expected)
	} else {
		out.KeyframeHitRate = 1.0
	}
	return events, trackIDs, nil
}

func indexHits(ix index.Index, tr *stream.Track) int {
	have := map[int]bool{}
	for _, n := range ix.Synced {
		have[n] = true
	}
	hits := 0
	for _, s := range tr.Samples {
		if s.Sync && have[s.Number] {
			hits++
		}
	}
	return hits
}

func indexExpected(tr *stream.Track) int {
	n := 0
	for _, s := range tr.Samples {
		if s.Sync {
			n++
		}
	}
	return n
}

func runRTP(in Input, cfg Config, out *Output) {
	if len(in.RTPPackets) == 0 {
		return
	}
	ra := stream.AnalyzeRTP(in.RTPPackets, in.RTPClockRate, cfg.Limits.RTP)
	out.Issues.Add(ra.Issues...)
	out.Packets = len(ra.Packets)
	out.Recovered = append(out.Recovered, ra.Recovered...)
}

func mergeEvents(subs []subtitle.TimelineEvent, meds []timeline.Event) []timeline.Event {
	out := make([]timeline.Event, 0, len(subs)+len(meds))
	for i, s := range subs {
		out = append(out, timeline.Event{
			Seq: i, Kind: timeline.KindSubtitle, TrackID: s.TrackID,
			StartNs: s.Cue.StartNs, EndNs: s.Cue.EndNs,
			Recovery: segment.Clean,
			Source: segment.Source{
				Kind: "subtitle", Ref: s.Source, CueIndex: s.Cue.Index,
				ByteOffset: int64(s.Cue.LineOffset),
			},
		})
	}
	out = append(out, meds...)
	return timeline.SortEvents(out)
}

func lastBoundary(evs []timeline.Event, segNs int64) int64 {
	var maxEnd int64
	for _, e := range evs {
		if e.EndNs > maxEnd {
			maxEnd = e.EndNs
		}
	}
	return (maxEnd/segNs + 1) * segNs
}

func toSegment(s timeline.Segment) Segment {
	return Segment{
		Index: s.Index, StartNs: s.StartNs, EndNs: s.EndNs, Recovery: s.Recovery,
		TrackIDs: s.TrackIDs, Sources: s.Sources, Classes: s.Classes, Late: s.Late,
		Hash: s.Hash, HashShort: segment.Short(s.Hash, 12),
	}
}
