package testkit

import (
	"archivearchivist/internal/model"
)

// cycleTimeline returns a copy of tl where A1 and A2 depend on each other.
// V1 and P1 stay outside the cycle, so a planner must keep their results.
func cycleTimeline(tl *model.Timeline) *model.Timeline {
	out := *tl
	out.Tracks = append([]model.Track(nil), tl.Tracks...)
	for i := range out.Tracks {
		switch out.Tracks[i].ID {
		case "A1":
			out.Tracks[i].DependsOn = []string{"V1", "A2"}
		case "A2":
			out.Tracks[i].DependsOn = []string{"A1"}
		}
	}
	out.Description = "fixture variant with an audio dependency cycle"
	return &out
}

func buildTimeline(cfg Config, man Manifest) *model.Timeline {
	tl := &model.Timeline{
		Version:     1,
		Seed:        cfg.Seed,
		Description: "national archive fixture: news reel and documentary masters",
		Capture: model.Capture{
			Path:      "stream.rtp",
			ClockRate: 90000,
			Window:    64,
		},
		Tracks: []model.Track{
			{ID: "V1", Kind: model.KindVideo, Codec: "avc1", Source: "video.mp4", Timescale: 90000,
				ClockOrigin: model.Modulus33 - 3*90000, ClockModulus: model.Modulus33,
				NominalRate: model.NewRat(30, 1)},
			{ID: "A1", Kind: model.KindAudio, Codec: "mp4a", Source: "audio.mp4", Timescale: 48000,
				DependsOn: []string{"V1"}, NominalRate: model.NewRat(48000, 1024)},
			{ID: "P1", Kind: model.KindProxy, Codec: "avc1", Source: "proxy.mp4", Timescale: 90000},
			{ID: "A2", Kind: model.KindAudio, Codec: "mp4a", Source: "master.mp4", Timescale: 48000,
				DependsOn: []string{"A1"}, NominalRate: model.NewRat(48000, 1024)},
		},
		Clips: []model.Clip{
			{ID: "C-V1-1", Track: "V1", Start: model.NewRat(0, 1), Duration: model.NewRat(2, 1), SourceIn: model.NewRat(0, 1)},
			{ID: "C-V1-2", Track: "V1", Start: model.NewRat(2, 1), Duration: model.NewRat(2, 1), SourceIn: model.NewRat(2, 1)},
			{ID: "C-A1-1", Track: "A1", Start: model.NewRat(0, 1), Duration: model.NewRat(2, 1), SourceIn: model.NewRat(0, 1)},
			{ID: "C-A1-2", Track: "A1", Start: model.NewRat(2, 1), Duration: model.NewRat(2, 1), SourceIn: model.NewRat(2, 1)},
			{ID: "C-P1-1", Track: "P1", Start: model.NewRat(0, 1), Duration: model.NewRat(3, 4), SourceIn: model.NewRat(0, 1)},
			{ID: "C-A2-1", Track: "A2", Start: model.NewRat(0, 1), Duration: model.NewRat(4, 1), SourceIn: model.NewRat(0, 1),
				Parent: "C-A1-1"},
		},
		Transitions: []model.Transition{
			{ID: "X-V1", Kind: "crossfade", From: "C-V1-1", To: "C-V1-2", Duration: model.NewRat(1, 2), Center: model.NewRat(2, 1)},
			{ID: "X-A1", Kind: "crossfade", From: "C-A1-1", To: "C-A1-2", Duration: model.NewRat(1, 2), Center: model.NewRat(2, 1)},
		},
	}
	_ = man
	return tl
}
