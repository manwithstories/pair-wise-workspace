package manifest

import (
	"os"
	"strings"
	"testing"

	"github.com/abema/go-mp4"

	"mediaroute/internal/clock"
)

const goodHLS = `#EXTM3U
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
seg0.ts
#EXTINF:2.0,
seg1.ts
`

func TestParseHLS(t *testing.T) {
	p, err := ParseHLS(goodHLS)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Segments) != 2 || p.Segments[1].URI != "seg1.ts" {
		t.Fatalf("segments %+v", p.Segments)
	}
	if p.Segments[0].DurationTicks != 180000 {
		t.Fatalf("duration %d want 180000", p.Segments[0].DurationTicks)
	}
}

func TestParseHLSRejectsTraversal(t *testing.T) {
	for _, uri := range []string{"../etc/passwd", "/abs/path", "a/../../b", "http://x/y"} {
		bad := "#EXTM3U\n#EXTINF:2.0,\n" + uri + "\n"
		if _, err := ParseHLS(bad); err == nil {
			t.Fatalf("uri %q accepted", uri)
		}
	}
}

func TestParseHLSRejectsOverflow(t *testing.T) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for i := 0; i < MaxSegments+1; i++ {
		b.WriteString("#EXTINF:2.0,\nseg.ts\n")
	}
	if _, err := ParseHLS(b.String()); err == nil {
		t.Fatal("segment overflow accepted")
	}
	if _, err := ParseHLS("#EXTM3U\n#EXTINF:99999,\nseg.ts\n"); err == nil {
		t.Fatal("huge duration accepted")
	}
	if _, err := ParseHLS("seg0.ts\n"); err == nil {
		t.Fatal("missing header accepted")
	}
}

const goodMPD = `<MPD><Period>
<AdaptationSet contentType="video"><Representation id="v1" bandwidth="2000000" colorPrimaries="bt709">
<SegmentList timescale="90000" duration="180000" count="12"/></Representation></AdaptationSet>
<AdaptationSet contentType="audio"><Representation id="a1" bandwidth="128000" audioSamplingRate="48000" audioChannels="2">
<SegmentList timescale="90000" duration="180000" count="12"/></Representation></AdaptationSet>
</Period></MPD>`

func TestParseDASH(t *testing.T) {
	m, err := ParseDASH([]byte(goodMPD))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tracks) != 2 || m.Tracks[1].AudioRate != 48000 || m.Tracks[1].AudioChannels != 2 {
		t.Fatalf("tracks %+v", m.Tracks)
	}
	if m.SegmentCount != 12 || m.SegmentDurationTicks != 180000 {
		t.Fatalf("grid %d x %d", m.SegmentCount, m.SegmentDurationTicks)
	}
}

func TestParseDASHRejects(t *testing.T) {
	if _, err := ParseDASH([]byte(`<MPD><Period><AdaptationSet contentType="text"><Representation id="x"/></AdaptationSet></Period></MPD>`)); err == nil {
		t.Fatal("bad kind accepted")
	}
	bad := strings.Replace(goodMPD, `timescale="90000"`, `timescale="44100"`, 1)
	if _, err := ParseDASH([]byte(bad)); err == nil {
		t.Fatal("bad timescale accepted")
	}
}

func TestProbeTimescale(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "init-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := mp4.NewWriter(f)
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeFtyp()}); err != nil {
		t.Fatal(err)
	}
	if _, err := mp4.Marshal(w, &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, MinorVersion: 1,
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}}}}, mp4.Context{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.EndBox(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMoov()}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdhd()}); err != nil {
		t.Fatal(err)
	}
	if _, err := mp4.Marshal(w, &mp4.Mdhd{Timescale: 90000, DurationV0: 180000, Language: [3]byte{'u', 'n', 'd'}}, mp4.Context{}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.EndBox(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.EndBox(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	ts, err := ProbeTimescale(f)
	if err != nil {
		t.Fatal(err)
	}
	if ts != 90000 {
		t.Fatalf("timescale %d want 90000", ts)
	}
}

func TestAlignAndMerge(t *testing.T) {
	p, err := ParseHLS(goodHLS)
	if err != nil {
		t.Fatal(err)
	}
	vid := AlignVideo(p)
	if vid[1].Start != 180000 || vid[1].End != 360000 {
		t.Fatalf("video intervals %+v", vid)
	}
	// 2 s per segment at 48 kHz.
	aud, err := AlignAudio([]AudioSegment{
		{Seq: 0, Samples: 96000, Rate: clock.Rate48000},
		{Seq: 1, Samples: 96000, Rate: clock.Rate48000},
	}, vid)
	if err != nil {
		t.Fatal(err)
	}
	if aud[1].End != 360000 {
		t.Fatalf("audio intervals %+v", aud)
	}
	// 44.1 kHz segment covering the same 2 s.
	if _, err := AlignAudio([]AudioSegment{
		{Seq: 0, Samples: 88200, Rate: clock.Rate44100},
		{Seq: 1, Samples: 88200, Rate: clock.Rate44100},
	}, vid); err != nil {
		t.Fatal(err)
	}
	// Drift beyond one frame fails (99000 samples = 2.0208 s).
	if _, err := AlignAudio([]AudioSegment{
		{Seq: 0, Samples: 96000, Rate: clock.Rate48000},
		{Seq: 1, Samples: 99000, Rate: clock.Rate48000},
	}, vid); err == nil {
		t.Fatal("drift accepted")
	}
	m, err := ParseDASH([]byte(goodMPD))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Merge(m, vid); err == nil {
		t.Fatal("count mismatch accepted")
	}
	m.SegmentCount = 2
	if _, err := Merge(m, vid); err != nil {
		t.Fatal(err)
	}
}
