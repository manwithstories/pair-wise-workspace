package pipeline

import (
	"fmt"
	"math/rand"
	"time"

	"mediaflow/internal/clock"
	"mediaflow/internal/cmaf"
	"mediaflow/internal/inspect"
	"mediaflow/internal/repair"
	"mediaflow/internal/transport"
)

// LayerReport is one representation's processed output. All layers are
// independent; nothing here references another layer's mutable state.
type LayerReport struct {
	Name           string                  `json:"name"`
	Index          int                     `json:"index"`
	TrackID        uint32                  `json:"track_id"`
	Width          uint16                  `json:"width"`
	Height         uint16                  `json:"height"`
	Timescale      uint32                  `json:"timescale"`
	InitTracks     []cmaf.TrackInfo        `json:"init_tracks"`
	Segments       int                     `json:"segments"`
	Fragments      int                     `json:"fragments"`
	Samples        int                     `json:"samples"`
	ScanProblems   []clock.LocatedError    `json:"scan_problems,omitempty"`
	Recovery       repair.Recovery         `json:"-"`
	Layer          repair.LayerResult      `json:"layer"`
	GOP            []inspect.GOPStats      `json:"gop"`
	DamagedRanges  []inspect.DamageRange   `json:"damaged_ranges"`
	KeyframeIssues []inspect.KeyframeIssue `json:"keyframe_issues,omitempty"`
	Transport      transport.Stats         `json:"transport"`
	RTPMissing     clock.Coverage          `json:"rtp_missing,omitempty"`
	ProcessUs      int64                   `json:"process_us"`
	Failed         bool                    `json:"failed"`
	Failure        string                  `json:"failure,omitempty"`
}

// ProcessRepresentation runs the whole per-layer path: scan, inspect, recover
// and transport. It reads nothing from another layer, which is what makes the
// fan-out failure-isolated.
func ProcessRepresentation(src Source, cfg Config) (rep LayerReport) {
	start := time.Now()
	rep = LayerReport{
		Name: src.Name, Index: int(src.TrackID) - 1, TrackID: src.TrackID,
		Width: src.Width, Height: src.Height, Timescale: src.Timescale,
	}
	defer func() { rep.ProcessUs = time.Since(start).Microseconds() }()

	limits := cmaf.DefaultLimits()
	if cfg.MaxSegmentBytes > 0 {
		limits.MaxSegmentBytes = cfg.MaxSegmentBytes
	}
	model := clock.NewErrorModel(cfg.SyncTolMS, cfg.MismatchTolMS)

	tl := clock.NewTimeline()
	sc := cmaf.NewScanner(limits)
	initSeg, err := sc.ParseInit(src.Init, 0)
	if err != nil {
		rep.fail(fmt.Sprintf("init parse: %v", err), src)
		return rep
	}
	sc.ApplyInit(initSeg)
	rep.InitTracks = initSeg.Tracks
	tl.Add(clock.TrackClock{TrackID: src.TrackID, Timescale: src.Timescale})
	rep.ScanProblems = append(rep.ScanProblems, initSeg.Problems...)

	frameUs := nominalFrameUs(cfg.FPS)
	damage := inspect.NewDamageLog(src.TrackID, frameUs, model)
	state := inspect.NewFragmentState(src.TrackID, tl)
	gop := inspect.NewGOPAccumulator(src.TrackID, 1000)

	var segs []cmaf.MediaSegment
	for _, si := range src.Segments {
		ms, err := sc.ParseMedia(si.Bytes, si.Index)
		if err != nil {
			rep.ScanProblems = append(rep.ScanProblems, clock.LocatedError{
				Kind: clock.ErrSegmentTruncated, Segment: si.Index, Detail: err.Error(),
			})
			continue
		}
		rep.ScanProblems = append(rep.ScanProblems, ms.Problems...)
		segs = append(segs, ms)
	}
	rep.Segments = len(segs)

	for _, ms := range segs {
		for _, frag := range ms.Fragments {
			state.ObserveFragment(frag, ms.SegmentIndex, damage)
			for _, sm := range frag.Samples {
				if sm.TrackID != src.TrackID {
					continue
				}
				rep.Samples++
				if !sm.TimeKnown {
					continue
				}
				us, convErr := tl.ToUs(src.TrackID, sm.DecodeTimeTicks)
				if convErr != nil {
					continue
				}
				issue := inspect.DetectKeyframeIssues(src.TrackID, sm)
				if issue != nil {
					rep.KeyframeIssues = append(rep.KeyframeIssues, *issue)
					model.Record(clock.LocatedError{
						Kind: clock.ErrKeyframeConflict, AtUs: us, PositionKnown: true,
						TrackID: src.TrackID, Offset: sm.Offset, Segment: ms.SegmentIndex, Detail: issue.Reason,
					})
				}
				gop.Observe(us, sm.IsKeyframe, issue == nil)
				damage.ObserveSample(us, us+int64(sm.DurationTicks)*1_000_000/int64(maxTS(src.Timescale)), true)
			}
		}
	}
	rep.GOP = []inspect.GOPStats{gop.Result()}

	opts := repair.DefaultOptions()
	opts.MinGapUs = cfg.MinGapUs
	rec := repair.RecoverTrack(tl, src.TrackID, segs, opts)
	rep.Recovery = rec
	rep.Fragments = len(rec.Fragments)
	rep.DamagedRanges = damage.Ranges()
	rep.Layer = repair.LayerSummary(src.Name, rep.Index, rec)

	rep.Transport, rep.RTPMissing = simulateTransport(cfg, src, rec)
	return rep
}

func (r *LayerReport) fail(reason string, src Source) {
	r.Failed = true
	r.Failure = reason
	r.Layer = repair.LayerSummary(src.Name, r.Index, repair.Recovery{TrackID: src.TrackID})
	r.Layer.Failed = true
	r.Layer.Error = reason
}

func maxTS(ts uint32) uint32 {
	if ts == 0 {
		return 1
	}
	return ts
}

func nominalFrameUs(fps int) int64 {
	if fps <= 0 {
		fps = 30
	}
	return 1_000_000 / int64(fps)
}

// simulateTransport packs this layer's recovered samples, applies the
// configured impairments with a stream seeded from (seed, track), and returns
// the receiver's statistics plus the unrecoverable media windows.
func simulateTransport(cfg Config, src Source, rec repair.Recovery) (transport.Stats, clock.Coverage) {
	pc := transport.DefaultPacketConfig()
	pc.SSRC = 0x52500000 + src.TrackID
	pack := transport.NewPackager(pc)
	var envs []transport.Envelope
	frameUs := nominalFrameUs(cfg.FPS)
	for _, plan := range rec.Fragments {
		if plan.Status == repair.StatusDuplicate || plan.Status == repair.StatusMissing {
			continue
		}
		if plan.Samples <= 0 || plan.EndUs <= plan.StartUs {
			continue
		}
		// The packet's media time is the sample start, not the fragment
		// start: fragments are one second long, so using the fragment start
		// would put every packet at the same timestamp and defeat sync checks.
		step := frameUs
		if plan.Samples > 1 && plan.EndUs-plan.StartUs > 0 {
			if per := (plan.EndUs - plan.StartUs) / int64(plan.Samples); per > 0 {
				step = per
			}
		}
		for i := 0; i < plan.Samples; i++ {
			payload := make([]byte, 220)
			for j := range payload {
				payload[j] = byte(j + int(plan.Sequence)*131 + i)
			}
			mediaUs := plan.StartUs + int64(i)*step
			got, err := pack.Pack(transport.SentSample{
				TrackID: src.TrackID, Index: i, MediaUs: mediaUs,
				DurationUs: step, Payload: payload,
			})
			if err != nil {
				continue
			}
			envs = append(envs, got...)
		}
	}
	if len(envs) == 0 {
		return transport.Stats{RecoveryRate: 1, WindowSize: cfg.ReorderWindow}, nil
	}
	sim := transport.Simulation{
		LossFraction:        cfg.Loss,
		ReorderFraction:     cfg.ReorderFraction,
		MaxReorder:          cfg.ReorderWindow / 4,
		DuplicateFraction:   cfg.DuplicateFrac,
		OutOfWindowFraction: 0,
		BaseDelayUs:         cfg.BaseDelayUs,
	}
	rng := rand.New(rand.NewSource(cfg.Seed*31_337 + int64(src.TrackID)*7_919))
	res := transport.Simulate(envs, cfg.ReorderWindow, int64(src.Timescale), sim, rng)
	missing := transport.MissingWindows(envs, res.DeliveredSet, nominalFrameUs(cfg.FPS))
	return res.Stats, missing
}
