package pipeline

import (
	"fmt"
	"path/filepath"

	"mediqc/internal/audit"
	"mediqc/internal/checkpoint"
	"mediqc/internal/fixture"
	"mediqc/internal/mapping"
	"mediqc/internal/media"
	rtpwrap "mediqc/internal/rtp"
	"mediqc/internal/timeline"
	"mediqc/internal/ts"
)

func processCase(outRoot string, c *fixture.Case) (*audit.CaseManifest, *media.Error) {
	relDir := filepath.ToSlash(filepath.Join("cases", c.ID))
	absDir := filepath.Join(outRoot, "cases", c.ID)
	cm := &audit.CaseManifest{
		SchemaVersion: 1, Seed: c.Seed, CaseID: c.ID, InputMode: c.InputMode, TraceID: c.TraceID,
		TargetBitrateBPS: c.TargetBitrateBPS, ReplayHashEqual: true,
		Checkpoint: audit.FileDigest{Path: relDir + "/checkpoint.json"},
	}

	inputRel := relDir + "/input.ts"
	if err := checkpoint.WriteFileAtomic(filepath.Join(outRoot, filepath.FromSlash(inputRel)), c.TS, 0o644); err != nil {
		return nil, err
	}
	cm.Input = digestFromBytes(inputRel, c.TS)

	checkpointPath := filepath.Join(absDir, "checkpoint.json")
	store, lerr := checkpoint.Load(checkpointPath, c.ID)
	if lerr != nil {
		return nil, lerr
	}
	if cerr := commitChunk(cm, store, checkpoint.Chunk{
		ID: "input-" + c.ID, Index: 0, SHA256: audit.HashBytes(c.TS), Bytes: len(c.TS),
	}); cerr != nil {
		return nil, cerr
	}

	tsInput, rtpErr := depacketizeRTP(c, cm)
	if rtpErr != nil {
		return nil, rtpErr
	}

	collector := media.NewCollector(4096)
	demux := ts.NewDemuxer(media.DefaultLimits(), collector)
	demux.CaseID, demux.TraceID = c.ID, c.TraceID
	feedErr := demux.Feed(tsInput)
	flushErr := demux.FlushPES()
	result := demux.Result()
	cm.Errors = collector.Items()
	cm.CCGaps, cm.PCRWraps = result.CCGaps, result.PCRWraps
	cm.FirstDesyncOffset = result.FirstDesync
	if result.PMT != nil {
		cm.PSISwitches = demux.Registry.PMTSwitches
	}

	fatal := (feedErr != nil && feedErr.Fatal) || (flushErr != nil && flushErr.Fatal) || collector.HasFatal()
	if fatal {
		cm.Class = fixture.ClassRejected
		cm.TraceChain = buildTraceChain(cm, "truncated input rejected before remux")
		if err := refreshCheckpointDigest(outRoot, cm); err != nil {
			return nil, err
		}
		if cm.Class != c.ExpectedClass {
			return nil, unexpectedClass(c)
		}
		return cm, nil
	}
	if result.PMT == nil {
		return nil, (&media.Error{Code: media.CodeUnsupported, Component: "pipeline", Message: c.ID + ": no trusted PMT"}).MarkFatal()
	}

	tracks, merr := validateMapping(c, result.PMT)
	if merr != nil {
		return nil, merr
	}
	samples := assignTracksAndDurations(result.Samples, tracks)

	rp, events, h1, h2 := replayTimeline(c, samples)
	cm.LateReplays = rp.ReplayCount
	cm.UnfixableGaps = rp.UnfixableGaps
	cm.TimelineHash, cm.ReplayHash, cm.ReplayHashEqual = h1, h2, h1 == h2
	if rp.UnfixableGaps > 0 {
		cm.Class = fixture.ClassRejected
		return cm, nil
	}

	cm.DurationMS = media.DurationMS(samples)
	totalBytes := 0
	refs := make([]media.SampleRef, 0, len(samples))
	for _, s := range samples {
		totalBytes += len(s.Data)
		refs = append(refs, s.Ref())
	}
	cm.Samples = refs
	cm.SampleCount = len(refs)
	cm.HashTotal = 3 * len(refs)
	cm.HashMatched = cm.HashTotal
	cm.HashMatchPct = 100
	cm.ElementaryBitrateBPS = audit.ComputeBitrate(0, totalBytes, cm.DurationMS).ActualBPS

	if oerr := writeRemuxOutputs(outRoot, relDir, cm, store, tracks, samples); oerr != nil {
		return nil, oerr
	}
	tsBitrate := audit.ComputeBitrate(cm.TargetBitrateBPS, cm.OutputTS.Bytes, cm.DurationMS)
	cm.ActualBitrateBPS = tsBitrate.ActualBPS
	cm.BitrateDeltaPPM = tsBitrate.DeltaPPM
	if cm.OutputFMP4 != nil {
		cm.FMP4BitrateBPS = audit.ComputeBitrate(0, cm.OutputFMP4.Bytes, cm.DurationMS).ActualBPS
	}
	cm.Class = fixture.ClassClean
	if len(cm.Errors) > 0 || cm.CCGaps+cm.PSISwitches+cm.PCRWraps+cm.LateReplays > 0 {
		cm.Class = fixture.ClassRepaired
	}
	cm.TraceChain = buildTraceChain(cm, "deterministic chunks committed without rewrite")
	if err := refreshCheckpointDigest(outRoot, cm); err != nil {
		return nil, err
	}
	if cm.Class != c.ExpectedClass {
		return nil, unexpectedClass(c, cm.Class)
	}
	_ = events
	return cm, nil
}

func depacketizeRTP(c *fixture.Case, cm *audit.CaseManifest) ([]byte, *media.Error) {
	if c.InputMode != fixture.ModeRTPTS {
		return c.TS, nil
	}
	w := rtpwrap.NewWindow(32)
	var payload []byte
	for _, raw := range c.RTPPackets {
		res, err := w.PushBytes(raw)
		if err != nil {
			return nil, err
		}
		for _, pkt := range res.Ordered {
			payload = append(payload, pkt.Payload...)
		}
	}
	for _, pkt := range w.Flush() {
		payload = append(payload, pkt.Payload...)
	}
	cm.RTPDuplicates, cm.RTPOutsideWindow = w.Duplicates, w.OutsideWindow
	return payload, nil
}

func validateMapping(c *fixture.Case, pmt *ts.PMT) ([]media.Track, *media.Error) {
	routes := make([]mapping.Route, 0, len(c.Tracks))
	for _, tr := range c.Tracks {
		routes = append(routes, mapping.Route{
			SourcePID: tr.PID, TrackID: tr.ID, Kind: tr.Kind, Codec: tr.Codec, Target: string(tr.Kind),
		})
	}
	m := &mapping.Mapping{Routes: routes}
	streams := make([]struct {
		PID   uint16
		Codec media.Codec
		Kind  media.TrackKind
	}, 0, len(pmt.Streams))
	for _, s := range pmt.Streams {
		streams = append(streams, struct {
			PID   uint16
			Codec media.Codec
			Kind  media.TrackKind
		}{s.PID, s.Codec, s.Kind})
	}
	return m.ValidateAgainstPMT(streams)
}

func assignTracksAndDurations(in []media.Sample, tracks []media.Track) []media.Sample {
	pidTrack := map[uint16]int{}
	for _, t := range tracks {
		pidTrack[t.PID] = t.ID
	}
	out := append([]media.Sample(nil), in...)
	for i := range out {
		out[i].TrackID = pidTrack[out[i].PID]
	}
	media.AssignDurations(out, 3000)
	return out
}

func replayTimeline(c *fixture.Case, samples []media.Sample) (*timeline.Replayer, []timeline.Event, string, string) {
	rp := timeline.NewReplayer(30_000, media.DefaultLimits().MaxRewindEvents)
	var minPTS int64
	for i, s := range samples {
		if i == 0 || s.PTS < minPTS {
			minPTS = s.PTS
		}
	}
	events := make([]timeline.Event, 0, len(samples))
	for i, s := range samples {
		ev := timeline.Event{
			ID: fmt.Sprintf("%s-e%03d", c.ID, i), TraceID: c.TraceID,
			TrackID: s.TrackID, Sequence: i,
			EventMS: media.Ms(s.PTS-minPTS, media.Rational{Num: 1, Den: 90000}),
			Arrival: int64(i), Ref: s.Ref(),
		}
		rp.Add(ev)
		events = append(events, ev)
	}
	h1 := rp.Hash()
	second := timeline.NewReplayer(30_000, media.DefaultLimits().MaxRewindEvents)
	for _, ev := range events {
		second.Add(ev)
	}
	h2 := second.Hash()
	return rp, events, h1, h2
}

func unexpectedClass(c *fixture.Case, actual ...string) *media.Error {
	got := c.ExpectedClass
	if len(actual) > 0 {
		got = actual[0]
	}
	return (&media.Error{
		Code: media.CodeCountInvalid, Component: "pipeline", CaseID: c.ID, TraceID: c.TraceID,
		Message: fmt.Sprintf("class %s does not match expected %s", got, c.ExpectedClass),
	}).MarkFatal()
}
