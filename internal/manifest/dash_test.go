package manifest

import (
	"strings"
	"testing"

	"mediac/internal/core"
)

const dashCleanMPD = `<?xml version="1.0"?>
<MPD type="static">
  <Period id="p0">
    <AdaptationSet contentType="video" mimeType="video/mp4">
      <Representation id="v-400" codecs="avc1.640028" width="640" height="360" bandwidth="400000">
        <SegmentTemplate timescale="1000" initialization="v-400/init.mp4"
          media="v-400/seg-$Number$.m4s" startNumber="1" duration="1000"/>
      </Representation>
      <Representation id="v-800" codecs="avc1.640028" width="1280" height="720" bandwidth="800000">
        <SegmentTemplate timescale="90000" initialization="v-800/init.mp4"
          media="v-800/seg-$Number$.m4s" startNumber="1" duration="90000"/>
      </Representation>
      <Representation id="v-1600" codecs="hvc1.1.6.L120" width="1920" height="1080" bandwidth="1600000">
        <SegmentTemplate timescale="1000" initialization="v-1600/init.mp4"
          media="v-1600/seg-$Number$.m4s" startNumber="1" duration="1000"/>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHClean(t *testing.T) {
	root := t.TempDir()
	m, err := ParseDASH(root, "manifest.mpd", strings.NewReader(dashCleanMPD), DASHScope{StartIndex: 1, EndIndex: -1})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Representations) != 3 {
		t.Fatalf("reps=%d want 3", len(m.Representations))
	}
	for _, rep := range m.Representations {
		if len(rep.Segments) != DefaultSegmentCount {
			t.Fatalf("rep %s segments=%d want %d", rep.ID, len(rep.Segments), DefaultSegmentCount)
		}
		if rep.InitPath == "" {
			t.Fatalf("rep %s missing init path", rep.ID)
		}
		last := rep.Segments[len(rep.Segments)-1]
		if last.Index != DefaultSegmentCount || last.StartMs != int64(DefaultSegmentCount-1)*1000 {
			t.Fatalf("unified clock wrong: %+v", last)
		}
	}
	// 90000 timescale must project to the same millisecond window.
	r800 := m.Representations[1]
	if r800.Timescale != 90000 || r800.Segments[1].DurationMs != 1000 {
		t.Fatalf("timescale projection failed: %+v", r800.Segments[1])
	}
	if got := m.SegmentCount(); got != 3*DefaultSegmentCount {
		t.Fatalf("segment count=%d", got)
	}
}

func TestParseDASHScope(t *testing.T) {
	root := t.TempDir()
	m, err := ParseDASH(root, "m.mpd", strings.NewReader(dashCleanMPD),
		DASHScope{StartIndex: 5, EndIndex: 8})
	if err != nil {
		t.Fatal(err)
	}
	segs := m.Representations[0].Segments
	if len(segs) != 4 || segs[0].Index != 5 || segs[3].Index != 8 {
		t.Fatalf("scope window wrong: %+v", segs)
	}
	// Clock must continue across the skipped prefix.
	if segs[0].StartMs != 4000 {
		t.Fatalf("start clock=%d want 4000", segs[0].StartMs)
	}
}

func TestParseDASHSegmentTimeline(t *testing.T) {
	mpd := `<?xml version="1.0"?>
<MPD><Period><AdaptationSet><Representation id="v0" bandwidth="500000">
  <SegmentTemplate timescale="1000" initialization="v0/init.mp4" media="v0/$Time$.m4s">
    <SegmentTimeline>
      <S t="0" d="1000" r="2"/>
      <S t="3000" d="2000"/>
    </SegmentTimeline>
  </SegmentTemplate>
</Representation></AdaptationSet></Period></MPD>`
	m, err := ParseDASH(t.TempDir(), "m.mpd", strings.NewReader(mpd),
		DASHScope{StartIndex: 1, EndIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	segs := m.Representations[0].Segments
	want := []struct {
		no        int
		start, du int64
	}{{1, 0, 1000}, {2, 1000, 1000}, {3, 2000, 1000}, {4, 3000, 2000}}
	if len(segs) != len(want) {
		t.Fatalf("segments=%d", len(segs))
	}
	for i, w := range want {
		if segs[i].Index != w.no || segs[i].StartMs != w.start || segs[i].DurationMs != w.du {
			t.Fatalf("seg %d = %+v want %+v", i, segs[i], w)
		}
	}
}

func TestParseDASHRejects(t *testing.T) {
	root := t.TempDir()
	t.Run("path escape", func(t *testing.T) {
		mpd := strings.Replace(dashCleanMPD, "v-400/init.mp4", "../outside.mp4", 1)
		_, err := ParseDASH(root, "m.mpd", strings.NewReader(mpd), DASHScope{StartIndex: 1, EndIndex: -1})
		if err == nil {
			t.Fatal("escape must error")
		}
		if f, ok := core.AsFault(err); !ok || f.Kind != core.FaultPathEscape {
			t.Fatalf("fault=%v", err)
		}
	})
	t.Run("missing init", func(t *testing.T) {
		mpd := `<?xml version="1.0"?><MPD><Period><AdaptationSet><Representation id="x" bandwidth="1">
		  <SegmentTemplate media="x/$Number$.m4s" duration="1000" timescale="1000"/>
		</Representation></AdaptationSet></Period></MPD>`
		_, err := ParseDASH(root, "m.mpd", strings.NewReader(mpd), DASHScope{StartIndex: 1, EndIndex: -1})
		if f, ok := core.AsFault(err); !ok || f.Kind != core.FaultMissingInit {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("backward timeline", func(t *testing.T) {
		mpd := `<?xml version="1.0"?>
<MPD>
  <Period>
    <AdaptationSet>
      <Representation id="x" bandwidth="1">
        <SegmentTemplate timescale="1000" initialization="x/i.mp4" media="x/$Time$.m4s">
          <SegmentTimeline>
            <S t="0" d="1000"/>
            <S t="500" d="1000"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`
		_, err := ParseDASH(root, "m.mpd", strings.NewReader(mpd), DASHScope{StartIndex: 1, EndIndex: -1})
		f, ok := core.AsFault(err)
		if !ok || f.Kind != core.FaultTimestampBackward {
			t.Fatalf("err=%v", err)
		}
		if f.Source.Line != 9 { // line of the offending second <S>
			t.Fatalf("fault line=%d want 9: %+v", f.Source.Line, f)
		}
	})
}
