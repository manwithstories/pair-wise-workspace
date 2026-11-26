package demo

import (
	"fmt"

	"medialab/internal/format"
	"medialab/internal/index"
	"medialab/internal/metadata"
	"medialab/internal/model"
	"medialab/internal/pipeline"
	"medialab/internal/report"
	"medialab/internal/rtp"
	"medialab/internal/sample"
	"medialab/internal/sync"
)

// Options are validated CLI demo parameters.
type Options struct {
	Seed          int64
	BurstLoss     int
	BandwidthKbps []int
}

// Result contains everything the terminal adapter can render.
type Result struct {
	Options         Options
	Metrics         report.Metrics
	Segments        []report.SegmentResult
	Packet          PacketDiagnostics
	SyncReport      *sync.Report
	Targets         []int
	IndexSHA256     string
	TruncatedOffset int64
	CropOffset      int64
	Retransmitted   int
	XORRecovered    int
	Provenance      []model.SegmentRef
}

// Run executes all deterministic generation, parsing, recovery and checks.
func Run(opts Options) (*Result, error) {
	pd := runPacketDiagnostics()
	fx := sample.BuildMP4Fixtures()
	mxfFx := sample.BuildMXFFixtures()

	mp4Container, err := format.ParseMP4(fx.Normal)
	if err != nil {
		return nil, fmt.Errorf("generated MP4 fixture failed parse: %w", err)
	}
	mxfContainer, err := format.ParseMXF(mxfFx.Normal)
	if err != nil {
		return nil, fmt.Errorf("generated MXF fixture failed parse: %w", err)
	}
	_, truncErr := format.ParseMXF(mxfFx.TruncatedKLV)
	truncOffset := model.FirstOffset(truncErr)

	badCropContainer, err := format.ParseMP4(fx.BadCrop)
	if err != nil {
		return nil, fmt.Errorf("bad-crop MP4 should parse so metadata can isolate it: %w", err)
	}
	badMeta := metadata.Resolve(badCropContainer)
	badPipeline := pipeline.Build(badMeta)
	cropOffset := badPipeline.FirstErrorOffset()

	explicit := &format.ColorDescription{Primaries: "display-p3", Transfer: "srgb", Matrix: "bt709", Source: metadata.SourceExplicit, Offset: -1}
	metaReport := metadata.Resolve(mp4Container, metadata.Override{TrackID: 1, Color: explicit})
	normalPipeline := pipeline.Build(metaReport)

	videoTrack := trackByKind(mp4Container, format.KindVideo)
	if videoTrack == nil {
		return nil, fmt.Errorf("generated MP4 has no video track")
	}
	kfIndex := index.Build(videoTrack, 33)
	hits, total := index.HitRate(kfIndex, videoTrack.Keyframes)

	burst, err := runBurstScenario(opts.Seed, opts.BurstLoss)
	if err != nil {
		return nil, err
	}
	for i, seg := range burst.segments {
		if i == 0 || i == 3 || i == 6 || i == 9 {
			kfIndex.MarkVerified(uint32(i+1), seg.Hash)
		}
	}
	indexHash := report.HashKeyframeIndex(kfIndex)

	targets := make([]int, len(opts.BandwidthKbps))
	adaptive := rtp.NewAdaptiveBuffer(opts.BandwidthKbps[0])
	for i, kbps := range opts.BandwidthKbps {
		targets[i] = adaptive.SetBandwidth(kbps)
	}

	passed, roundtripTotal := roundtripChecks(roundtripInputs{
		mp4: mp4Container, mxf: mxfContainer, meta: metaReport, pipe: normalPipeline,
		truncErr: truncErr, expectedTruncOffset: mxfFx.TruncatedOffset,
		badPipe: badPipeline,
	})
	firstOffset := truncOffset
	if cropOffset >= 0 && (firstOffset < 0 || cropOffset < firstOffset) {
		firstOffset = cropOffset
	}
	m := report.Metrics{
		RoundtripPassed:   passed,
		RoundtripTotal:    roundtripTotal,
		KeyframeHits:      hits,
		KeyframeTotal:     total,
		SegmentMatched:    report.MatchedCount(burst.segments),
		SegmentTotal:      sample.SegmentCount,
		DuplicateDropped:  pd.DuplicateDropped,
		ConflictsIsolated: pd.ConflictsIsolated,
		ReorderedPackets:  pd.ReorderedPackets,
		BurstRecovered:    burst.engine.Retransmitted + burst.engine.XORRecovered,
		BurstLost:         opts.BurstLoss,
		UnrecoverableLoss: burst.engine.Unrecoverable,
		AVSkewMS:          burst.syncReport.AVSkewMS,
		FirstErrorOffset:  firstOffset,
		BandwidthTargets:  targets,
		IndexSHA256:       indexHash,
	}
	provenance := append([]model.SegmentRef(nil), burst.refs...)
	if len(badPipeline.Rejected) == 1 && badPipeline.Rejected[0].Origin != nil {
		origin := badPipeline.Rejected[0].Origin
		provenance = append(provenance, model.SegmentRef{
			Index: -1, TrackID: origin.TrackID, SourceOff: origin.Offset,
			State: model.StateRejected, OriginError: origin,
		})
	}
	return &Result{
		Options: opts, Metrics: m, Segments: burst.segments, Packet: pd,
		SyncReport: burst.syncReport, Targets: targets, IndexSHA256: indexHash, TruncatedOffset: truncOffset,
		CropOffset: cropOffset, Retransmitted: burst.engine.Retransmitted,
		XORRecovered: burst.engine.XORRecovered, Provenance: provenance,
	}, nil
}

func trackByKind(c *format.Container, kind format.Kind) *format.Track {
	for _, t := range c.Tracks {
		if t.Kind == kind {
			return t
		}
	}
	return nil
}

type roundtripInputs struct {
	mp4                 *format.Container
	mxf                 *format.Container
	meta                *metadata.Report
	pipe                *pipeline.Result
	truncErr            error
	expectedTruncOffset int64
	badPipe             *pipeline.Result
}

func roundtripChecks(in roundtripInputs) (int, int) {
	video := trackByKind(in.mp4, format.KindVideo)
	audio := trackByKind(in.mp4, format.KindAudio)
	mxfVideo := trackByKind(in.mxf, format.KindVideo)
	mxfAudio := trackByKind(in.mxf, format.KindAudio)
	colorOK := false
	transformOK := false
	for _, rt := range in.meta.Tracks {
		if rt.Track.ID == 1 {
			colorOK = rt.Color.Selected != nil && rt.Color.Priority == metadata.SourceExplicit && len(rt.Color.Discarded) == 2
		}
	}
	for _, tr := range in.pipe.Transforms {
		if tr.TrackID == 1 {
			transformOK = tr.Valid && tr.OutputW == 172 && tr.OutputH == 304
		}
	}
	checks := []bool{
		len(in.mp4.Tracks) == 2,
		video != nil && video.Width == 320 && video.Height == 180,
		video != nil && video.Rotation == 90,
		video != nil && len(video.Keyframes) == 4,
		audio != nil && audio.Kind == format.KindAudio,
		colorOK,
		transformOK,
		len(in.mxf.Tracks) == 2,
		mxfVideo != nil && mxfVideo.Width == 640 && mxfVideo.Rotation == 180,
		mxfVideo != nil && len(mxfVideo.Keyframes) == 3 && mxfAudio != nil,
		in.truncErr != nil && model.FirstOffset(in.truncErr) == in.expectedTruncOffset,
		len(in.badPipe.Rejected) == 1 && in.badPipe.FirstErrorOffset() >= 0,
	}
	passed := 0
	for _, ok := range checks {
		if ok {
			passed++
		}
	}
	return passed, len(checks)
}

// ExpectedPathsComplete reports whether every advertised acceptance path met
// its deterministic invariant.
func (r *Result) ExpectedPathsComplete() bool {
	m := r.Metrics
	return m.RoundtripPassed == m.RoundtripTotal &&
		m.KeyframeHits == m.KeyframeTotal && m.KeyframeTotal > 0 &&
		m.SegmentMatched == m.SegmentTotal && m.SegmentTotal == sample.SegmentCount &&
		m.BurstRecovered == m.BurstLost && m.UnrecoverableLoss == 0 &&
		r.Packet.SequenceWrapOK && r.Packet.TimestampWrapOK &&
		r.Packet.WindowOverflowOK && r.Packet.ContinuousLossOK &&
		r.Packet.ConflictsIsolated == 1
}
