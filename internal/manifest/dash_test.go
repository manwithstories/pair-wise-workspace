package manifest

import (
	"errors"
	"strings"
	"testing"

	"mediarun/internal/clock"
)

const dashSample = `<?xml version="1.0" encoding="utf-8"?>
<MPD type="static" mediaPresentationDuration="PT12S">
  <BaseURL>cam0/</BaseURL>
  <Period id="p0">
    <AdaptationSet id="v" mimeType="video/mp4" timescale="90000">
      <Representation id="r0" bandwidth="1200000">
        <SegmentTemplate media="$Number%05d$.m4s" startNumber="1" timescale="90000"/>
        <SegmentTimeline>
          <S t="0" d="180000" r="2"/>
          <S d="90000"/>
        </SegmentTimeline>
      </Representation>
    </AdaptationSet>
    <AdaptationSet id="a" contentType="audio" timescale="48000">
      <SegmentTimeline>
        <S t="0" d="192000" r="2"/>
        <S d="96000"/>
      </SegmentTimeline>
    </AdaptationSet>
  </Period>
</MPD>`

func TestDASHExpandsTimeline(t *testing.T) {
	m, err := ParseDASH(strings.NewReader(dashSample), DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var video, audio []Segment
	for _, s := range m.Segments {
		switch s.Track {
		case "video":
			video = append(video, s)
		case "audio":
			audio = append(audio, s)
		}
	}
	if len(video) != 4 {
		t.Fatalf("video segments = %d, want 4", len(video))
	}
	if len(audio) != 4 {
		t.Fatalf("audio segments = %d, want 4", len(audio))
	}
	// 180000 ticks at 90kHz = 2s.
	if video[0].DurNS != 2_000_000_000 {
		t.Fatalf("video dur = %d", video[0].DurNS)
	}
	if video[3].StartNS != 6_000_000_000 || video[3].DurNS != 1_000_000_000 {
		t.Fatalf("last video seg start=%d dur=%d", video[3].StartNS, video[3].DurNS)
	}
	if video[0].URI != "cam0/00001.m4s" {
		t.Fatalf("expanded uri = %q", video[0].URI)
	}
	if video[3].URI != "cam0/00004.m4s" {
		t.Fatalf("last uri = %q", video[3].URI)
	}
	if audio[0].DurNS != 4_000_000_000 {
		t.Fatalf("audio dur = %d", audio[0].DurNS)
	}
	if m.Timescale != 48000 {
		t.Fatalf("media timescale = %d", m.Timescale)
	}
}

func TestDASHRejectsZeroDuration(t *testing.T) {
	src := strings.Replace(dashSample, `d="180000"`, `d="0"`, 1)
	_, err := ParseDASH(strings.NewReader(src), DefaultLimits())
	if err == nil || !clock.IsKind(err, clock.KindInvalid) {
		t.Fatalf("want invalid, got %v", err)
	}
}

func TestDASHRejectsBackwardsT(t *testing.T) {
	src := strings.Replace(dashSample, `<S d="90000"/>`, `<S t="100"/>`, 1)
	_, err := ParseDASH(strings.NewReader(src), DefaultLimits())
	var e *clock.Error
	if !errors.As(err, &e) || e.Code != clock.CodeInvalidDuration {
		t.Fatalf("want invalid-duration rejection, got %v", err)
	}
}

func TestDASHRejectsPathEscape(t *testing.T) {
	src := strings.Replace(dashSample, "cam0/", "../outside/", 1)
	_, err := ParseDASH(strings.NewReader(src), DefaultLimits())
	var e *clock.Error
	if !errors.As(err, &e) || e.Code != clock.CodePathEscape {
		t.Fatalf("want path-escape rejection, got %v", err)
	}
}

func TestDASHRejectsEntryLimit(t *testing.T) {
	src := strings.Replace(dashSample, `r="2"`, `r="999"`, 1)
	_, err := ParseDASH(strings.NewReader(src), Limits{MaxEntries: 10, MaxSegmentBytes: 1 << 20})
	if err == nil || !clock.IsKind(err, clock.KindInvalid) {
		t.Fatalf("want entry-limit rejection, got %v", err)
	}
}

func TestDASHRejectsUnknownTemplate(t *testing.T) {
	src := strings.Replace(dashSample, "$Number%05d$", "$Time%05d$", 1)
	_, err := ParseDASH(strings.NewReader(src), DefaultLimits())
	if err == nil {
		t.Fatalf("unknown template identifier accepted")
	}
}
