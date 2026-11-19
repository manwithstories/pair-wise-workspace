// Package pipeline schedules per-track processing in dependency order and
// aggregates the delivery metrics. A failing track never contaminates another.
package pipeline

import (
	"sort"

	"mediamix/internal/audio"
	"mediamix/internal/codec"
	"mediamix/internal/core"
	"mediamix/internal/fixture"
	"mediamix/internal/mp4"
	"mediamix/internal/timeline"
	"mediamix/internal/transport"
)

// Options control a run.
type Options struct {
	Seed          int64
	LossBurst     int
	BandwidthMBps float64
	CorruptIndex  bool
	TruncateTrack string
	Limits        fixture.Limits
	Target        audio.Target
}

// TrackResult is the per-track outcome.
type TrackResult struct {
	Track          string              `json:"track"`
	Kind           timeline.NodeKind   `json:"kind"`
	Status         string              `json:"status"` // ok | degraded | failed
	Error          *core.Error         `json:"error,omitempty"`
	Adjustments    []*core.Error       `json:"adjustments,omitempty"`
	Received       int                 `json:"received"`
	Delivered      int                 `json:"delivered"`
	Duplicates     int                 `json:"duplicate_packets"`
	Reordered      int                 `json:"reordered_packets"`
	Recovered      int                 `json:"recovered_packets"`
	Unrecoverable  int                 `json:"unrecoverable_gaps"`
	SkippedPackets int                 `json:"skipped_packets"`
	Affected       []timeline.Span     `json:"affected_us,omitempty"`
	SyncOffsetUs   timeline.Micros     `json:"sync_offset_us"`
	Audio          *audio.Result       `json:"audio,omitempty"`
	Index          *codec.Index        `json:"index,omitempty"`
	LatencyP95Ms   float64             `json:"latency_p95_ms"`
}

// Report is the machine-readable run summary. The JSON field names are part of
// the CLI contract and are asserted by tests.
type Report struct {
	Seed             int64             `json:"seed"`
	SyncErrorMS      float64           `json:"sync_error_ms"`
	RecoveryRate     float64           `json:"recovery_rate"`
	LatencyP95MS     float64           `json:"latency_p95_ms"`
	ReorderedPackets int               `json:"reordered_packets"`
	DuplicatePackets int               `json:"duplicate_packets"`
	FailedTracks     []string          `json:"failed_tracks"`
	DegradedTracks   []string          `json:"degraded_tracks,omitempty"`
	Unrecoverable    []string          `json:"unrecoverable_tracks,omitempty"`
	ValidationErrors []*core.Error     `json:"validation_errors,omitempty"`
	BufferBound      int                `json:"buffer_bound"`
	BandwidthMBps    float64            `json:"bandwidth_mbps"`
	Container        *mp4.ContainerInfo `json:"container,omitempty"`
	Tracks           []TrackResult     `json:"tracks"`
	// ExitCode is the process status implied by this report.
	ExitCode         int               `json:"exit_code"`
}

const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusFailed   = "failed"
)

// ErrUnrecoverable signals a completed run that still contains failed tracks.
var ErrUnrecoverable = &core.Error{Code: core.CodeUnrecoverable, Detail: "unrecoverable tracks remain", Fatal: false}

// Run executes the full pipeline once.
func Run(opts Options) (*Report, error) {
	if opts.Limits == (fixture.Limits{}) {
		opts.Limits = fixture.DefaultLimits()
	}
	if opts.Target.Rate == 0 {
		opts.Target = audio.DefaultTarget()
	}
	if opts.BandwidthMBps < 0 {
		return nil, &core.Error{Code: core.CodeBadArgument, Detail: "bandwidth must be >= 0", Fatal: true}
	}
	rep := &Report{Seed: opts.Seed, BandwidthMBps: opts.BandwidthMBps}

	fx, err := fixture.Generate(fixture.Params{
		Seed: opts.Seed, LossBurst: opts.LossBurst, BandwidthMBps: opts.BandwidthMBps,
		CorruptIndex: opts.CorruptIndex, TruncateTrack: opts.TruncateTrack, Limits: opts.Limits,
	})
	if err != nil {
		return nil, err
	}
	graph, err := buildGraph(fx)
	if err != nil {
		return nil, err
	}
	if errs := graph.Validate(); len(errs) > 0 {
		rep.ValidationErrors = errs
		rep.finish()
		if len(errs) > 0 {
			return rep, errs[0]
		}
	}
	order, err := graph.Order()
	if err != nil {
		return nil, err
	}

	policy := transport.DefaultRecoveryPolicy()
	policy.BandwidthMBps = opts.BandwidthMBps
	engine := transport.NewRecoveryEngine(policy, newSource(fx, opts))
	rep.BufferBound = engine.BufferBound()

	results := make(map[string]*TrackResult)
	// Stable dependency order: a dependent track is only processed after its
	// parents, but each track's failure is isolated to itself.
	for _, id := range order {
		node, _ := graph.Node(id)
		res := runTrack(fx, node, engine, opts)
		results[id] = res
		rep.Tracks = append(rep.Tracks, *res)
	}
	rep.accumulate(results, engine)
	rep.Container = muxDeliverable(fx, rep.Tracks)
	rep.finish()
	return rep, nil
}

// muxDeliverable writes the healthy tracks into an ISO-BMFF container and
// re-reads it. Failed tracks are excluded: a container must never present
// media the pipeline already rejected, and one bad track must not poison the
// deliverable of the others. A nil result means every track failed or the
// container could not be verified; the caller treats that as "no deliverable"
// rather than a silently broken file.
func muxDeliverable(fx *fixture.Fixture, tracks []TrackResult) *mp4.ContainerInfo {
	var specs []mp4.TrackSamples
	for _, tr := range tracks {
		if tr.Status == StatusFailed {
			continue
		}
		spec := findSpec(fx, tr.Track)
		if spec == nil || spec.Timescale == 0 {
			continue
		}
		ts := mp4.TrackSamples{
			Track: spec.ID, Kind: tr.Kind, Timescale: spec.Timescale,
			TotalSamples: spec.PacketCount, Span: timeline.Span{Start: spec.Origin, End: spec.Origin + spec.DurationUs},
		}
		if tr.Index != nil {
			for _, s := range tr.Index.Sync {
				ts.Keyframes = append(ts.Keyframes, mp4.Sample{PTS: s.PTS, Size: s.Size, Key: s.Key})
			}
		}
		if ts.Span.Empty() && spec.DurationUs == 0 {
			// Zero-length audio (the deliberate empty track) still needs one
			// sample so the container stays well-formed.
			ts.TotalSamples = 1
		}
		specs = append(specs, ts)
	}
	if len(specs) == 0 {
		return nil
	}
	data, err := mp4.Build(specs)
	if err != nil {
		return nil
	}
	info, err := mp4.Inspect(data)
	if err != nil {
		return nil
	}
	return info
}


// newSource builds the retransmit source. It only hands back packets the
// sender actually transmitted, so recovery can never invent media.
func newSource(fx *fixture.Fixture, opts Options) transport.RecoverySource {
	var all []fixture.Delivery
	order := make([]string, 0, len(fx.Sent))
	for id := range fx.Sent {
		order = append(order, id)
	}
	sort.Strings(order)
	for _, id := range order {
		all = append(all, fixture.Impair(fx.Sent[id], fixture.NetworkOptions{
			Seed: opts.Seed,
		})...)
	}
	return &fxSource{
		sender:    fixture.SenderFromDeliveries(all),
		bandwidth: opts.BandwidthMBps,
	}
}

// fxSource adapts the fixture sender to transport.RecoverySource. It also
// implements transport.GapViability: a retransmit burst larger than what the
// current uplink can carry cannot be repaired, which is what produces genuine
// unrecoverable spans on a degraded link.
type fxSource struct {
	sender    *fixture.Sender
	bandwidth float64
}

// Fetch returns a retransmitted copy for a sequence at its original position.
func (s *fxSource) Fetch(track string, ext uint64) (transport.Packet, bool) {
	p, ok := s.sender.Fetch(track, ext)
	if !ok {
		return transport.Packet{}, false
	}
	return transport.Packet{
		Track: track, Seq: p.Seq, Timestamp: p.Timestamp, Marker: p.Marker,
		Keyframe: p.Keyframe, Payload: p.Payload, Retransmit: true,
	}, true
}

// GapViable reports whether a burst of the given size can be retransmitted at
// the current bandwidth. Two packets per Mbps plus a small floor; a 7-packet
// burst needs at least 2.5 Mbps to heal.
func (s *fxSource) GapViable(track string, startExt uint64, count int) bool {
	if s.bandwidth <= 0 {
		return true
	}
	maxBurst := int(s.bandwidth*2) + 2
	return count <= maxBurst
}

// buildGraph derives the explicit track graph. Dependencies are declared, not
// inferred from timestamps: the proxy follows both camera angles, and the audio
// bus follows the proxy so edits land in video-then-audio order.
func buildGraph(fx *fixture.Fixture) (*timeline.Graph, error) {
	g, err := timeline.NewGraph(len(fx.Tracks) + 1)
	if err != nil {
		return nil, err
	}
	deps := map[string][]string{
		"proxy-av1": {"cam-a", "cam-b"},
		"audio-1":   {"proxy-av1"},
	}
	for _, spec := range fx.Tracks {
		span, err := timeline.NewSpan(spec.Origin, spec.DurationUs)
		if err != nil {
			return nil, err
		}
		// No exclusive groups: the two camera angles and the audio sources
		// legitimately cover the same wall-clock interval. Overlap validation
		// is still exercised directly by the graph tests.
		n := timeline.Node{
			Track: spec.ID, Kind: spec.Kind, Span: span,
			DependsOn: deps[spec.ID],
		}
		if err := g.Add(n); err != nil {
			return nil, err
		}
	}
	return g, nil
}
