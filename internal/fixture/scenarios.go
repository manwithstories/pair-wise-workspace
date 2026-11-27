package fixture

import (
	"fmt"
	"strings"

	"avrepair/internal/clock"
	"avrepair/internal/track"
)

// Scenario is one fully generated, self-contained test case.
type Scenario struct {
	Name      string
	Spec      Spec
	Video     []Packet
	Audio     []Packet
	Subtitles []SubtitleCue
	Nodes     []*track.Node
	// TruncateAt, when > 0, means only packets with TS < TruncateAt are
	// considered valid; the remainder simulates a torn tail.
	TruncateAt int64
	// Interrupted marks a run that stops before commit (no index published).
	Interrupted bool
	// ExpectExit is the process exit code the engine should produce.
	ExpectExit int
}

// Build assembles the named scenario with all cross-track metadata wired up.
func Build(name string) (*Scenario, error) {
	spec := DefaultSpec()
	sc := &Scenario{Name: name, Spec: spec, ExpectExit: 0}

	videoDefect, subDefect := "", ""
	switch name {
	case CaseClean:
	case CaseRollback:
		videoDefect, subDefect = CaseRollback, CaseRollback
	case CaseOverlap:
		subDefect = CaseOverlap
	case CaseMetaConflict:
	case CaseTruncated:
		sc.TruncateAt = 6 * clock.Second
		sc.ExpectExit = 0 // recovered verified prefix still completes cleanly
	case CaseInterrupted:
		sc.Interrupted = true
		sc.ExpectExit = 3
	default:
		return nil, fmt.Errorf("unknown scenario %q", name)
	}

	sc.Video = spec.GenerateVideo(videoDefect)
	sc.Audio = spec.GenerateAudio("")
	sc.Subtitles = spec.GenerateSubtitles(subDefect)
	sc.Nodes = buildNodes(name)
	return sc, nil
}

// buildNodes constructs the explicit track dependency graph for a scenario.
func buildNodes(name string) []*track.Node {
	videoColor := track.ColorMeta{Source: "cam-a", Space: "bt709", Range: "tv"}
	audioLayout := "stereo"
	audioRate := 48_000
	switch name {
	case CaseMetaConflict:
		// A second video node upstream carries contradictory color metadata.
		return []*track.Node{
			{ID: "video-src", Kind: track.KindVideo,
				Color: track.ColorMeta{Source: "cam-b", Space: "bt2020", Range: "full"}},
			{ID: "video", Kind: track.KindVideo, DependsOn: []string{"video-src"},
				Color: videoColor},
			{ID: "audio", Kind: track.KindAudio, Layout: audioLayout,
				Channels: 2, SampleRate: audioRate},
			{ID: "sub", Kind: track.KindSub, Layout: "srt",
				DependsOn: []string{"audio", "video"}},
		}
	}
	return []*track.Node{
		{ID: "video", Kind: track.KindVideo, Color: videoColor},
		{ID: "audio", Kind: track.KindAudio, Layout: audioLayout,
			Channels: 2, SampleRate: audioRate},
		{ID: "sub", Kind: track.KindSub, Layout: "srt",
			DependsOn: []string{"audio", "video"}},
	}
}

// SRT renders the generated cues as an SRT document.
func SRT(cues []SubtitleCue) string {
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n",
			i+1, srtTime(c.StartNS), srtTime(c.EndNS), c.Text)
	}
	return b.String()
}

// WebVTT renders the generated cues as a WebVTT document.
func WebVTT(cues []SubtitleCue) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", vttTime(c.StartNS), vttTime(c.EndNS), c.Text)
	}
	return b.String()
}

func srtTime(ns int64) string {
	h := ns / clock.Hour
	ns %= clock.Hour
	m := ns / clock.Minute
	ns %= clock.Minute
	s := ns / clock.Second
	ms := (ns % clock.Second) / clock.Millisecond
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

func vttTime(ns int64) string {
	h := ns / clock.Hour
	ns %= clock.Hour
	m := ns / clock.Minute
	ns %= clock.Minute
	s := ns / clock.Second
	ms := (ns % clock.Second) / clock.Millisecond
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}
