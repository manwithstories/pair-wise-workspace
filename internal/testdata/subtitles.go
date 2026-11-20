package testdata

import "fmt"

// SubtitleFixture is one named, self-contained subtitle input.
type SubtitleFixture struct {
	Name   string
	Source string
	Track  string
	Data   string
	// ExpectParseErrors is true when the fixture intentionally violates the
	// grammar and must produce recoverable, offset-anchored errors.
	ExpectParseErrors bool
	// Corrupt marks inputs whose cues should still flow after recovery.
	Corrupt bool
}

// cue renders one SRT block. startMs/endMs are inclusive timing bounds.
func cue(seq, startMs, endMs int, text string) string {
	return fmt.Sprintf("%d\n%s --> %s\n%s\n\n", seq, srtStamp(startMs), srtStamp(endMs), text)
}

func srtStamp(ms int) string {
	hh := ms / 3600000
	ms %= 3600000
	mm := ms / 60000
	ms %= 60000
	ss := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hh, mm, ss, ms)
}

// goodSRT spans 15 seconds with cues every 1.5 s, so a 3 s segment window
// yields five windows; a second track overlaps it to exercise priorities.
func goodSRT() string {
	var b string
	for i := 0; i < 10; i++ {
		start := i * 1500
		b += cue(i+1, start, start+1200, fmt.Sprintf("cue-%02d", i+1))
	}
	return b
}

func goodVTT() string {
	b := "WEBVTT\n\n"
	for i := 0; i < 6; i++ {
		start := i * 2000
		b += fmt.Sprintf("%d\n%02d:%02d.%03d --> %02d:%02d.%03d\nvtt-%d\n\n",
			i+1, start/60000, (start%60000)/1000, 0,
			(start+1500)/60000, ((start+1500)%60000)/1000, 0, i+1)
	}
	return b
}

// boundarySRT exercises the edge of every clock range: a cue at time zero,
// a zero-duration cue from a missing end time, an end-before-start cue and a
// cue that overflows the pipeline's 60 s merge horizon (rejected outright).
func boundarySRT() string {
	return "1\n00:00:00,000 --> 00:00:00,500\nat-midnight\n\n" +
		"2\n00:00:02,000 --> \nmissing-end\n\n" +
		"3\n00:00:05,000 --> 00:00:04,000\nend-before-start\n\n" +
		"4\n00:00:06,000 --> 00:00:07,000\nrecovered-cue\n\n" +
		"5\n00:01:01,000 --> 00:01:02,000\nbeyond-horizon\n"
}

// truncatedSRT cuts a cue mid-line and includes invalid UTF-8 bytes plus a
// block that never receives its blank-line terminator before EOF.
func truncatedSRT() string {
	return "1\n00:00:01,000 --> 00:00:02,000\nok-before-damage\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nabc\xff\xfedef\n\n" +
		"3\n00:00:05,000 --> 00:00:06,000\nunterminated"
}

// overlappingTrack returns cues that deliberately overlap goodSRT on a
// higher-priority lane.
func overlappingTrack() string {
	var b string
	for i := 0; i < 8; i++ {
		start := i*1500 + 400
		b += cue(i+1, start, start+1000, fmt.Sprintf("overlay-%02d", i+1))
	}
	return b
}

// SubtitleFixtures returns the full deterministic subtitle set.
func SubtitleFixtures() []SubtitleFixture {
	return []SubtitleFixture{
		{Name: "good-srt", Source: "good.srt", Track: "track-a", Data: goodSRT()},
		{Name: "good-vtt", Source: "good.vtt", Track: "track-b", Data: goodVTT()},
		{Name: "overlay", Source: "overlay.srt", Track: "track-priority", Data: overlappingTrack()},
		{Name: "empty", Source: "empty.srt", Track: "track-empty", Data: ""},
		{Name: "boundary", Source: "boundary.srt", Track: "track-boundary",
			Data: boundarySRT(), ExpectParseErrors: true, Corrupt: true},
		{Name: "truncated", Source: "truncated.srt", Track: "track-damaged",
			Data: truncatedSRT(), ExpectParseErrors: true, Corrupt: true},
	}
}
