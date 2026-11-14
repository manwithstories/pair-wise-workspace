package main

import (
	"fmt"

	"mp4replay/internal/mp4atom"
	"mp4replay/internal/report"
)

func buildProbe(m *mp4atom.Movie) *report.Probe {
	p := &report.Probe{
		Tracks:                 len(m.Tracks),
		MediaTimescale:         m.Movie.Timescale,
		UnknownBoxes:           m.UnknownSkip,
		PresentationDurationMs: 0,
	}
	for _, tr := range m.Tracks {
		kf := 0
		for _, s := range tr.Samples {
			if s.Sync {
				kf++
			}
		}
		tl := tr.Timeline
		var first, last int64
		if len(tl.PresentMs) > 0 {
			first, last = tl.PresentMs[0], tl.PresentMs[len(tl.PresentMs)-1]
		}
		mediaMs := scaleMedia(tr.Table.MediaDur, tr.Table.Timescale)
		summary := report.ProbeTrack{
			TrackID:         tr.Table.TrackID,
			Handler:         tr.Handler(),
			Timescale:       tr.Table.Timescale,
			SampleCount:     len(tr.Samples),
			KeyframeCount:   kf,
			FirstPtsMs:      first,
			LastPtsMs:       last,
			PresentationMs:  tl.DurationMs,
			MediaDurationMs: mediaMs,
			WrapEvents:      tl.WrapEvents,
		}
		p.AllTracks = append(p.AllTracks, summary)
		switch tr.Handler() {
		case "vide":
			cp := summary
			p.Video = &cp
			if tl.DurationMs > p.PresentationDurationMs {
				p.PresentationDurationMs = tl.DurationMs
			}
		case "soun":
			cp := summary
			p.Audio = &cp
		}
	}
	if p.Video == nil && len(p.AllTracks) > 0 {
		cp := p.AllTracks[0]
		p.Video = &cp
		p.PresentationDurationMs = cp.PresentationMs
	}
	return p
}

func scaleMedia(dur uint64, ts uint32) int64 {
	if ts == 0 {
		return 0
	}
	return int64((dur*1000 + uint64(ts)/2) / uint64(ts))
}

func printProbeText(p *report.Probe) {
	fmt.Printf("tracks=%d movie_timescale=%d presentation_ms=%d unknown_skipped=%d\n",
		p.Tracks, p.MediaTimescale, p.PresentationDurationMs, p.UnknownBoxes)
	for _, t := range p.AllTracks {
		fmt.Printf("  trak id=%d handler=%s ts=%d samples=%d keyframes=%d "+
			"pts=[%d..%d] present_ms=%d media_ms=%d wraps=%d\n",
			t.TrackID, t.Handler, t.Timescale, t.SampleCount, t.KeyframeCount,
			t.FirstPtsMs, t.LastPtsMs, t.PresentationMs, t.MediaDurationMs,
			t.WrapEvents)
	}
}
