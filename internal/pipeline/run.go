package pipeline

import (
	"context"

	"avpipe/internal/fault"
	"avpipe/internal/report"
	"avpipe/internal/track"
)

// runTrack exercises the dependency graph over the real stages and returns
// the per-track status plus the first root cause, if any.
func runTrack(ctx context.Context, opts Options, haveMP4 bool) (*track.Outcome, error) {
	g := track.New()
	g.MustAdd(track.Node{Name: "audio", Kind: track.KindAudio})
	g.MustAdd(track.Node{Name: "rtp", Kind: track.KindDerived, Deps: []string{"audio"}})
	g.MustAdd(track.Node{Name: "video", Kind: track.KindVideo, Deps: []string{"audio"}})
	g.MustAdd(track.Node{Name: "sync", Kind: track.KindDerived, Deps: []string{"audio", "video"}})
	g.MustAdd(track.Node{Name: "report", Kind: track.KindDerived, Deps: []string{"rtp", "sync"}})
	return g.Run(ctx, func(ctx context.Context, name string) error {
		if name == "video" && !haveMP4 {
			return fault.New(fault.CodeInternal, "pipeline.video", "no MP4 clip available")
		}
		return ctx.Err()
	})
}

// toTrackStatus renders the outcome in execution order.
func toTrackStatus(out *track.Outcome) []report.TrackStatus {
	if out == nil {
		return nil
	}
	res := make([]report.TrackStatus, 0, len(out.Order))
	for _, name := range out.Order {
		ts := report.TrackStatus{Name: name, Status: string(out.Status[name])}
		if err := out.Failures[name]; err != nil {
			ts.Error = fault.Root(err).Error()
		}
		res = append(res, ts)
	}
	return res
}
