package manifest

import (
	"errors"
	"testing"

	"github.com/clearliu/fieldcut/internal/clock"
)

const dashMPD = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">
  <Period>
    <AdaptationSet contentType="video">
      <Representation id="v0">
        <SegmentTemplate timescale="90000" media="v/$Number$.m4s"
          initialization="v/init.mp4" startNumber="1">
          <SegmentTimeline>
            <S t="0" d="360000" r="2"/>
            <S d="180000"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet contentType="audio">
      <Representation id="a0">
        <SegmentTemplate timescale="48000" media="a/$Time$.m4s"
          initialization="a/init.mp4" startNumber="0">
          <SegmentTimeline>
            <S t="0" d="96000"/>
            <S t="96000" d="96000"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHBasic(t *testing.T) {
	evs, err := ParseDASH([]byte(dashMPD), Options{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 2 init + 4 video + 2 audio.
	if len(evs) != 8 {
		t.Fatalf("events = %d, want 8: %+v", len(evs), evs)
	}
	video := evs[:5]
	if video[0].Kind != KindInit || video[0].URI != "v/init.mp4" || video[0].TrackID != 0 {
		t.Fatalf("video init wrong: %+v", video[0])
	}
	wantSeq := []int{1, 2, 3, 4}
	for i, ev := range video[1:] {
		if ev.Sequence != wantSeq[i] || ev.URI != "v/"+itoa(wantSeq[i])+".m4s" {
			t.Fatalf("video seg %d = %+v", i, ev)
		}
		if ev.Duration != clock.Ticks(4*clock.Rate) && !(i == 3 && ev.Duration == clock.Ticks(2*clock.Rate)) {
			t.Fatalf("video dur %d = %d", i, ev.Duration)
		}
	}
	// R expansion: 3x 4s then 1x 2s; starts 0, 4s, 8s, 12s.
	wantStart := []clock.Ticks{0, clock.Ticks(4 * clock.Rate), clock.Ticks(8 * clock.Rate), clock.Ticks(12 * clock.Rate)}
	for i, ev := range video[1:] {
		if ev.Start != wantStart[i] {
			t.Fatalf("video start %d = %d, want %d", i, ev.Start, wantStart[i])
		}
	}
	audio := evs[5:]
	if audio[0].TrackID != 1 || audio[0].URI != "a/init.mp4" {
		t.Fatalf("audio init wrong: %+v", audio[0])
	}
	// 48 kHz -> 90 kHz: 96000 ticks == 2 s == 180000 ticks.
	if audio[1].Duration != clock.Ticks(2*clock.Rate) || audio[1].Start != 0 {
		t.Fatalf("audio scaling wrong: %+v", audio[1])
	}
	if audio[1].URI != "a/0.m4s" || audio[2].URI != "a/96000.m4s" {
		t.Fatalf("$Time$ expansion wrong: %q %q", audio[1].URI, audio[2].URI)
	}
	if audio[2].Start != clock.Ticks(2*clock.Rate) {
		t.Fatalf("audio second start = %d", audio[2].Start)
	}
}

func TestParseDASHRejects(t *testing.T) {
	t.Run("malformed xml", func(t *testing.T) {
		if _, err := ParseDASH([]byte("<MPD><Period>"), Options{}); !errors.Is(err, clock.ErrInvalidArgument) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("remote template", func(t *testing.T) {
		mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet>
		  <Representation id="x"><SegmentTemplate media="https://h/$Number$.m4s">
		  <SegmentTimeline><S t="0" d="1000"/></SegmentTimeline>
		  </SegmentTemplate></Representation></AdaptationSet></Period></MPD>`
		if _, err := ParseDASH([]byte(mpd), Options{}); !errors.Is(err, clock.ErrPathTraversal) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("negative repeat", func(t *testing.T) {
		mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet>
		  <Representation id="x"><SegmentTemplate media="s/$Number$.m4s">
		  <SegmentTimeline><S t="0" d="1000" r="-1"/></SegmentTimeline>
		  </SegmentTemplate></Representation></AdaptationSet></Period></MPD>`
		if _, err := ParseDASH([]byte(mpd), Options{}); err == nil {
			t.Fatal("expected error for negative r")
		}
	})
	t.Run("traversal in expansion", func(t *testing.T) {
		mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet>
		  <Representation id="x"><SegmentTemplate media="../$Number$.m4s">
		  <SegmentTimeline><S t="0" d="1000"/></SegmentTimeline>
		  </SegmentTemplate></Representation></AdaptationSet></Period></MPD>`
		if _, err := ParseDASH([]byte(mpd), Options{}); !errors.Is(err, clock.ErrPathTraversal) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("empty mpd", func(t *testing.T) {
		mpd := `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"></MPD>`
		if _, err := ParseDASH([]byte(mpd), Options{}); err == nil {
			t.Fatal("expected error for empty MPD")
		}
	})
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
