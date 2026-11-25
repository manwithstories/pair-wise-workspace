package pipeline

import (
	"math"

	"relaycheck/internal/audio"
	"relaycheck/internal/media"
	"relaycheck/internal/meta"
	"relaycheck/internal/segment"
	"relaycheck/internal/transport"
)

// PlanSegment is one deterministically built fragment.
type PlanSegment struct {
	Index      int
	Bytes      []byte
	SHA        string
	KeyframeID int
}

// Plan is the full pure deterministic build before any WAL/disk action.
type Plan struct {
	Clock         *media.Clock
	RTP           *RTPPlan
	Pacer         *transport.Pacer
	Segments      []PlanSegment
	RefHashes     []string
	MetaConflicts int
}

// resolveMetadata parses and adjudicates the deterministic MP4/MXF fixtures.
func resolveMetadata() (conflicts int, f *media.Fault) {
	mp4Bytes := meta.BuildMP4Case("ok")
	mp4Meta, rf := meta.ReadMP4(mp4Bytes)
	if rf != nil {
		return 0, rf
	}
	d, rf := mp4Meta.Reconcile(meta.Layer{}, meta.Layer{})
	if rf != nil {
		return 0, rf
	}
	conflicts += len(d.Conflicts)

	mxfMeta, rf := meta.ReadMXF(meta.BuildMXFCase("color-conflict"))
	if rf != nil {
		return 0, rf
	}
	md, rf := mxfMeta.Reconcile(meta.Layer{})
	if rf != nil {
		return 0, rf
	}
	conflicts += len(md.Conflicts)
	return conflicts, nil
}

// BuildPlan constructs every canonical fragment deterministically. It performs
// no disk I/O, so a resumed run reproduces the uninterrupted hashes exactly.
func BuildPlan(cfg Config) (*Plan, *media.Fault) {
	if cfg.Layout != "stereo" {
		return nil, media.NewFault(media.CodeInvalidInput, "only stereo layout is supported, got %q", cfg.Layout)
	}
	conflicts, f := resolveMetadata()
	if f != nil {
		return nil, f
	}
	clock, f := media.NewClock(int64(cfg.TargetRate))
	if f != nil {
		return nil, f
	}
	rtpPlan, f := buildRTPPlan(cfg, clock)
	if f != nil {
		return nil, f
	}
	pacer, f := transport.NewPacer(cfg.BandwidthKbps, SegmentCount)
	if f != nil {
		return nil, f
	}

	p := &Plan{Clock: clock, RTP: rtpPlan, Pacer: pacer, MetaConflicts: conflicts}
	p.Segments = make([]PlanSegment, SegmentCount)
	p.RefHashes = make([]string, SegmentCount)
	lufs100 := int32(math.Round(cfg.IntegratedLUFS * 100))

	for i := 0; i < SegmentCount; i++ {
		rate := audio.SourceRates[i%len(audio.SourceRates)]
		ch := 2
		if i%4 == 0 {
			ch = 1
		}
		srcFrames := rate / 50 // 20 ms at the source rate
		src := audio.TonePCM(rate, ch, srcFrames, cfg.Seed+int64(i)*104729)

		rng, rf := clock.Range(int64(i)*audio.FramesPerSegment, int64(i+1)*audio.FramesPerSegment)
		if rf != nil {
			return nil, rf
		}
		converted, rf := audio.Convert(src, audio.Options{
			TrackID: 1, TargetRate: cfg.TargetRate, OutputChannels: 2,
			ExpectedFrames: audio.FramesPerSegment,
			GapPolicy:      audio.GapSilence, Gaps: rtpPlan.SegGaps[i],
			SegmentRange: rng,
		})
		if rf != nil {
			return nil, rf
		}
		out, _, rf := audio.NormalizeLoudness(converted, audio.NormalizeOptions{
			TrackID: 1, TargetLUFS: cfg.IntegratedLUFS, CeilingDBFS: -1, Range: rng,
		})
		if rf != nil {
			return nil, rf
		}
		if cf := audio.DetectClipping(out, 1, 1, rng); cf != nil {
			return nil, cf
		}

		segGaps := make([]segment.Gap, 0, len(rtpPlan.SegGaps[i]))
		for _, g := range rtpPlan.SegGaps[i] {
			segGaps = append(segGaps, segment.Gap{StartFrame: int64(g.StartFrame), EndFrame: int64(g.EndFrame)})
		}
		seg := &segment.Segment{
			H: segment.Header{
				Index: int32(i), TrackID: 1, Seed: cfg.Seed,
				StartNum: rng.Start.Num, StartDen: rng.Start.Den,
				EndNum: rng.End.Num, EndDen: rng.End.Den,
				SampleRate: uint32(cfg.TargetRate), SourceRate: uint32(rate),
				Frames: audio.FramesPerSegment, Channels: 2, LUFS100: lufs100,
			},
			Gaps: segGaps, Samples: out.Samples,
		}
		b := seg.Encode()
		sha := segment.HashBytes(b)
		p.Segments[i] = PlanSegment{Index: i, Bytes: b, SHA: sha, KeyframeID: i}
		p.RefHashes[i] = sha
	}
	return p, nil
}
