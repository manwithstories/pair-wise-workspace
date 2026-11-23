package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"

	"mediatimeline/internal/bitstream"
	"mediatimeline/internal/cache"
	"mediatimeline/internal/clock"
	"mediatimeline/internal/mediaerr"
	"mediatimeline/internal/render"
	"mediatimeline/internal/timeline"
)

// --- stage registration ---

func init() {
	registerStage(StageScan, (*runCtx).computeScan)
	registerStage(StageClock, (*runCtx).computeClock)
	registerStage(StageCompose, (*runCtx).computeCompose)
	registerStage(StageRender, (*runCtx).computeRender)
}


// --- scan ---

func (c *runCtx) computeScan() ([]byte, []string, error) {
	res := &scanResult{}
	sources := c.inputFiles()
	for _, spec := range c.meta.Tracks {
		st, corrupt := c.scanTrack(spec)
		if corrupt != nil {
			res.Corrupt = append(res.Corrupt, *corrupt)
			continue
		}
		res.Tracks = append(res.Tracks, *st)
	}
	payload, err := json.Marshal(res)
	return payload, sources, err
}

func (c *runCtx) scanTrack(spec TrackSpec) (*scannedTrack, *CorruptInfo) {
	fail := func(err error) (*scannedTrack, *CorruptInfo) {
		if me, ok := mediaerr.As(err); ok && me.Recoverable {
			return nil, &CorruptInfo{Track: spec.ID, Offset: me.Offset, Reason: me.Msg}
		}
		return nil, &CorruptInfo{Track: spec.ID, Offset: 0, Reason: err.Error()}
	}
	codec, err := bitstream.ParseCodec(spec.Codec)
	if err != nil {
		return fail(err)
	}
	streamPath := filepath.Join(c.dir, "inputs", spec.Stream)
	data, err := os.ReadFile(streamPath)
	if err != nil {
		return fail(err)
	}
	var idx *bitstream.Index
	if spec.Kind == "rtp" {
		es, rtpSamples, err := bitstream.DepacketizeRTP(data, spec.ID)
		if err != nil {
			return fail(err)
		}
		samples, err := bitstream.Scan(es, codec, spec.ID)
		if err != nil {
			return fail(err)
		}
		rawTS := make([]uint64, len(rtpSamples))
		for i, s := range rtpSamples {
			rawTS[i] = s.RawTS
		}
		idx = bitstream.BuildIndex(spec.ID, codec, spec.TBNum, spec.TBDen, es, samples, rawTS)
		if err := bitstream.ValidateIndex(es, samples, idx, spec.ID); err != nil {
			return fail(err)
		}
		data = es
	} else {
		samples, err := bitstream.Scan(data, codec, spec.ID)
		if err != nil {
			return fail(err)
		}
		idx, err = bitstream.ReadIndexFile(filepath.Join(c.dir, "inputs", spec.Index))
		if err != nil {
			return fail(err)
		}
		if err := bitstream.ValidateIndex(data, samples, idx, spec.ID); err != nil {
			return fail(err)
		}
	}
	st := &scannedTrack{
		ID: spec.ID, Codec: spec.Codec, TBNum: spec.TBNum, TBDen: spec.TBDen,
		TsBits: spec.TsBits, StreamDigest: bitstream.DigestOf(data),
	}
	for i, e := range idx.Entries {
		st.Samples = append(st.Samples, scannedSample{
			Seq: i, Offset: e.Offset, Length: e.Length,
			Keyframe: e.Keyframe, RawTS: e.Timestamp, Digest: e.Digest,
		})
	}
	return st, nil
}

// --- clock ---

func (c *runCtx) computeClock() ([]byte, []string, error) {
	res := &clockResult{}
	specByID := make(map[string]TrackSpec, len(c.meta.Tracks))
	prioByID := make(map[string]int, len(c.meta.Tracks))
	for i, s := range c.meta.Tracks {
		specByID[s.ID] = s
		prioByID[s.ID] = i
	}
	for _, st := range c.scan.Tracks {
		spec := specByID[st.ID]
		uw := clock.NewUnwrapper(st.TsBits)
		locals := make([]int64, len(st.Samples))
		for k, s := range st.Samples {
			locals[k] = uw.Extend(s.RawTS)
		}
		anchors := c.loadAnchors(spec)
		fit := clock.Fitter{
			Nominal:     clock.NewRational(st.TBNum, st.TBDen),
			MinAnchors:  3,
			MaxDriftPPM: c.params.MaxDriftPPM,
		}.Fit(anchors)
		at := alignedTrack{ID: st.ID, Priority: prioByID[st.ID], Aligned: fit.Aligned}
		for k, s := range st.Samples {
			at.Samples = append(at.Samples, timeline.SampleRef{
				Seq: s.Seq, RefStart: clock.MapTime(fit, locals[k]),
				SrcOffset: s.Offset, Length: s.Length,
				Keyframe: s.Keyframe, Digest: s.Digest,
			})
		}
		res.Tracks = append(res.Tracks, at)
	}
	payload, err := json.Marshal(res)
	return payload, c.anchorFiles(), err
}

func (c *runCtx) loadAnchors(spec TrackSpec) []clock.Anchor {
	if spec.Anchors == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(c.dir, "inputs", spec.Anchors))
	if err != nil {
		return nil
	}
	var anchors []clock.Anchor
	if err := json.Unmarshal(data, &anchors); err != nil {
		return nil
	}
	return anchors
}

// --- compose ---

func (c *runCtx) computeCompose() ([]byte, []string, error) {
	var tracks []timeline.Track
	var aligned []bool
	for _, at := range c.clock.Tracks {
		tracks = append(tracks, timeline.Track{
			ID: at.ID, Priority: at.Priority, Samples: at.Samples,
		})
		aligned = append(aligned, at.Aligned)
	}
	composed := timeline.Compose(tracks, aligned)
	payload, err := json.Marshal(&composeResult{
		Frames: composed.Frames, Reports: composed.Reports,
	})
	return payload, nil, err
}

// --- render ---

// manifest is the deterministic render manifest (no wall-clock data).
type manifest struct {
	Format        string                 `json:"format"`
	Width         int                    `json:"width"`
	Height        int                    `json:"height"`
	Params        RunParams              `json:"params"`
	TrackReports  []timeline.TrackReport `json:"track_reports"`
	Corrupt       []CorruptInfo          `json:"corrupt"`
	Frames        []render.FrameOut      `json:"frames"`
	SummarySHA256 string                 `json:"summary_sha256"`
}

func (c *runCtx) computeRender() ([]byte, []string, error) {
	framesPath := filepath.Join(c.dir, "frames.bin")
	f, err := os.Create(framesPath)
	if err != nil {
		return nil, nil, err
	}
	frameOuts, err := render.Render(c.comp.Frames, f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if err := f.Close(); err != nil {
		return nil, nil, err
	}
	m := &manifest{
		Format: "mediatimeline/1", Width: render.Width, Height: render.Height,
		Params: c.params, TrackReports: c.comp.Reports, Corrupt: c.scan.Corrupt,
		Frames: frameOuts, SummarySHA256: render.Summary(frameOuts),
	}
	if m.Corrupt == nil {
		m.Corrupt = []CorruptInfo{}
	}
	mdata, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(c.dir, "manifest.json"), mdata, 0o644); err != nil {
		return nil, nil, err
	}
	res := &renderResult{
		Summary:        m.SummarySHA256,
		ManifestDigest: cache.ResultDigest(mdata),
		FrameCount:     len(frameOuts),
	}
	payload, err := json.Marshal(res)
	return payload, nil, err
}
