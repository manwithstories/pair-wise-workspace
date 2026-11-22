package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"archivecheck/internal/clock"
	"archivecheck/internal/ebml"
	"archivecheck/internal/fault"
	"archivecheck/internal/fixture"
	"archivecheck/internal/index"
	"archivecheck/internal/manifest"
	"archivecheck/internal/timeline"
	"archivecheck/internal/transport"
	"archivecheck/internal/txn"
)

// generate produces the sealed source and the damaged RTP stream.
func generate(cfg Config) (*fixture.Plan, *fixture.Wire, error) {
	defect := fixture.Defect{TimestampRegressAt: -1, DuplicateAt: -1}
	if cfg.Corrupt {
		defect = fixture.Defect{
			HasRegress: true, TimestampRegressAt: 7,
			HasDuplicate: true, DuplicateAt: 13,
			UnknownSizeSegment: true, UnknownSizeCluster: true,
		}
	}
	plan, err := fixture.Build(cfg.Seed, Tracks(), cfg.FrameCount, defect)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Corrupt {
		defect.TruncateAt = len(plan.Data) - 37
		plan, err = fixture.Build(cfg.Seed, Tracks(), cfg.FrameCount, defect)
		if err != nil {
			return nil, nil, err
		}
	}
	wire, err := fixture.BuildWire(cfg.Seed, mediaPayload(plan), plan.DurationSeconds, fixture.WireConfig{
		LossBurst:     cfg.LossBurst,
		LossAt:        -1,
		Reorder:       cfg.Reorder,
		Duplicate:     cfg.Duplicate,
		BandwidthMbps: cfg.BandwidthMbps,
	})
	if err != nil {
		return nil, nil, err
	}
	return plan, wire, nil
}

// mediaPayload is what the simulated network carries: the video track's frame
// bytes in order.
func mediaPayload(plan *fixture.Plan) []byte {
	var out []byte
	for _, f := range plan.Frames {
		if f.Track != videoTrack {
			continue
		}
		if len(f.PCM) > 0 {
			out = append(out, fixture.EncodePCM(f.PCM)...)
		} else {
			out = append(out, f.Raw...)
		}
	}
	if len(out) == 0 {
		out = append(out, plan.Data...)
	}
	return out
}

// parseSource parses the sealed source, reporting structural damage into fs.
func parseSource(path string, plan *fixture.Plan, fs *fault.Set) (ebml.Header, []*ebml.Block, error) {
	f, err := os.Open(path)
	if err != nil {
		return ebml.Header{}, nil, err
	}
	defer f.Close()
	r := ebml.NewReader(f, int64(len(plan.Data)), ebml.Options{Faults: fs})
	br := ebml.NewBlockReaderWithFaults(r, fs)
	if err := br.Scan(); err != nil {
		return ebml.Header{}, nil, err
	}
	return br.Header(), br.Blocks(), nil
}

// commitParse journals and commits the parse stage.
func commitParse(mgr *txn.Manager, adopted map[txn.Stage]txn.Artifact, rep *Report, h ebml.Header, blocks []*ebml.Block) error {
	if _, ok := adopted[txn.StageParse]; ok {
		return nil
	}
	meta, _ := json.Marshal(map[string]any{
		"blocks":         len(blocks),
		"timestampScale": h.TimestampScale,
		"tracks":         len(h.Tracks),
	})
	_, err := mgr.Stage(txn.StageParse, meta, func(a txn.Artifact) error {
		if h.TimestampScale <= 0 {
			return fmt.Errorf("parse found no timestamp scale")
		}
		return nil
	}, func(tmp string) error {
		return writeJSON(tmp, meta)
	})
	if err != nil {
		return err
	}
	rep.Stages = append(rep.Stages, string(txn.StageParse))
	return nil
}

// commitStage commits an in-memory-validated stage: repair has no separate
// artifact, so its journaled result is the repaired timeline summary.
func commitStage(mgr *txn.Manager, adopted map[txn.Stage]txn.Artifact, stage txn.Stage, validate func() error) error {
	if _, ok := adopted[stage]; ok {
		return nil
	}
	_, err := mgr.Stage(stage, nil, func(a txn.Artifact) error { return validate() }, func(tmp string) error {
		return writeJSON(tmp, map[string]any{"stage": string(stage), "status": "ok"})
	})
	return err
}

// verifyRepair rejects a repair result whose output is not monotonic.
func verifyRepair(res *timeline.Result) error {
	for num, tr := range res.Tracks {
		var prev clock.Rational
		have := false
		for _, f := range tr.Frames {
			if f.Missing {
				continue
			}
			if have && f.Repaired.Cmp(prev) < 0 {
				return fmt.Errorf("track %d output not monotonic at offset 0x%x", num, f.Offset)
			}
			prev = f.Repaired
			have = true
		}
	}
	return nil
}

// buildTimeline converts parsed blocks plus transport loss into tracks.
func buildTimeline(h ebml.Header, blocks []*ebml.Block, res transport.Result, wire *fixture.Wire) *timeline.Timeline {
	tick := clock.MustNew(1, 1)
	if h.TimestampScale > 0 {
		if t, err := clock.FromTicks(1, h.TimestampScale); err == nil {
			tick = t
		}
	}
	tl := timeline.New(tick, timeline.Policy{MaxDrift: clock.MustNew(2, 1000)})
	byTrack := make(map[uint64]*timeline.TrackInfo, len(h.TrackOrder))
	for _, num := range h.TrackOrder {
		kind := "unknown"
		var ns uint64
		scale := clock.MustNew(1, 1)
		if tr := h.Tracks[num]; tr != nil {
			kind = tr.Kind.String()
			ns = tr.DefaultDur
			scale = tr.TimestampScale
		}
		byTrack[num] = &timeline.TrackInfo{Number: num, Kind: kind, DefaultNS: ns, Scale: scale}
	}
	for _, b := range blocks {
		info := byTrack[b.Track]
		if info == nil {
			continue
		}
		info.Frames = append(info.Frames, timeline.Frame{
			Track: b.Track, Original: b.Time, Offset: b.Offset,
			Keyframe: b.Keyframe, Bytes: b.FrameBytes, Digest: b.FrameDigest,
		})
	}
	// Explicit transport gaps: mark recovered payload boundaries as missing
	// slots on the video track so the loss propagates into the timeline.
	if len(res.Gaps) > 0 {
		markGapFrames(byTrack[videoTrack], res, wire)
	}
	for _, num := range h.TrackOrder {
		tl.AddTrack(byTrack[num])
	}
	return tl
}

// markGapFrames inserts explicit missing frames for transport gaps.  The count
// of missing video frames is derived from lost bytes against the measured
// frame payload size; nothing is fabricated, only flagged.
func markGapFrames(info *timeline.TrackInfo, res transport.Result, wire *fixture.Wire) {
	if info == nil || len(info.Frames) == 0 {
		return
	}
	avgLaid := int64(0)
	for _, f := range info.Frames {
		avgLaid += f.Bytes
	}
	avgLaid /= int64(len(info.Frames))
	if avgLaid <= 0 {
		avgLaid = int64(wire.PacketBytes)
	}
	last := info.Frames[len(info.Frames)-1]
	for i, g := range res.Gaps {
		missingBytes := int64(g.Len()) * int64(wire.PacketBytes)
		n := int(missingBytes / avgLaid)
		if n < 1 {
			n = 1
		}
		for k := 0; k < n && k < 64; k++ {
			info.Frames = append(info.Frames, timeline.Frame{
				Track: info.Number, Offset: last.Offset + int64(i+1)<<20 + int64(k),
				Original: last.Repaired, Missing: true,
			})
		}
	}
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func storeIndex(mgr *txn.Manager, stage txn.Stage, outDir string, idx *index.Index) error {
	if _, ok := mgr.Committed()[stage]; ok {
		return nil
	}
	_, err := mgr.Stage(stage, nil, func(a txn.Artifact) error {
		if idx.Digest == "" || len(idx.Entries) == 0 {
			return fmt.Errorf("index has no entries")
		}
		return nil
	}, func(tmp string) error {
		if err := writeJSON(tmp, map[string]any{
			"version": 1, "entries": idx.Entries, "per_track": idx.PerTrack, "digest": idx.Digest,
		}); err != nil {
			return err
		}
		sidecar, err := os.Create(filepath.Join(outDir, "index.mp4"))
		if err != nil {
			return err
		}
		if err := index.NewSidecar(idx).Write(sidecar); err != nil {
			sidecar.Close()
			return err
		}
		if err := sidecar.Close(); err != nil {
			return err
		}
		return copyFile(tmp, filepath.Join(outDir, "index.json"))
	})
	return err
}

func storeManifest(mgr *txn.Manager, stage txn.Stage, outDir string, doc manifest.Manifest, aligned bool) error {
	if _, ok := mgr.Committed()[stage]; ok {
		return nil
	}
	_, err := mgr.Stage(stage, nil, func(a txn.Artifact) error {
		if !aligned {
			return fmt.Errorf("manifest alignment exceeds 1ms")
		}
		return nil
	}, func(tmp string) error {
		if err := doc.WriteJSON(mustCreate(tmp)); err != nil {
			return err
		}
		return copyFile(tmp, filepath.Join(outDir, "manifest.json"))
	})
	return err
}

func mustCreate(path string) *os.File {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	return f
}

// declaredExtents derives each track's intended extent from the source blocks
// themselves: earliest stored time to latest, plus one default frame.  This is
// the archive checklist's expectation, computed before repair, so comparing it
// with the repaired timeline exercises the repair rules and 1 ms tolerance.
func declaredExtents(h ebml.Header, blocks []*ebml.Block) []manifest.Declared {
	type acc struct {
		start, end clock.Rational
		have       bool
	}
	byTrack := map[uint64]*acc{}
	var order []uint64
	for _, b := range blocks {
		a := byTrack[b.Track]
		if a == nil {
			a = &acc{}
			byTrack[b.Track] = a
			order = append(order, b.Track)
		}
		if !a.have {
			a.start, a.end, a.have = b.Time, b.Time, true
			continue
		}
		if b.Time.Cmp(a.start) < 0 {
			a.start = b.Time
		}
		if b.Time.Cmp(a.end) > 0 {
			a.end = b.Time
		}
	}
	out := make([]manifest.Declared, 0, len(order))
	for _, n := range order {
		a := byTrack[n]
		kind := "unknown"
		var defNS uint64
		if tr := h.Tracks[n]; tr != nil {
			kind = tr.Kind.String()
			defNS = tr.DefaultDur
		}
		end := a.end
		if defNS > 0 {
			end = end.Add(clock.MustNew(int64(defNS), 1_000_000_000))
		}
		out = append(out, manifest.Declared{Track: n, Kind: kind, Start: a.start, End: end})
	}
	return out
}
