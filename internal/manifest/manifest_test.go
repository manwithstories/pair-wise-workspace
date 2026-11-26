package manifest

import (
	"errors"
	"strings"
	"testing"

	"aircheck/internal/media"
)

const masterText = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="main",DEFAULT=YES,URI="audio/media.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=640x360,FRAME-RATE=30,AUDIO="aud"
video/360p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,CODECS="avc1.64001f,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=30,AUDIO="aud"
video/720p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4000000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1920x1080,FRAME-RATE=30,AUDIO="aud"
video/1080p.m3u8
`

const mediaText = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:1
#EXT-X-MEDIA-SEQUENCE:7
#EXT-X-MAP:URI="init.mp4"
#EXTINF:1.000,
video/seg00007.m4s
#EXTINF:1.000,
video/seg00008.m4s
#EXTINF:1.000,
video/seg00009.m4s
#EXT-X-ENDLIST
`

func TestScanLinesOffsets(t *testing.T) {
	lines := ScanLines([]byte("a\nbb\r\n\nccc"))
	want := []struct {
		n   int
		off int64
		txt string
	}{{1, 0, "a"}, {2, 2, "bb"}, {3, 6, ""}, {4, 7, "ccc"}}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines", len(lines))
	}
	for i, w := range want {
		if lines[i].Num != w.n || lines[i].Offset != w.off || lines[i].Text != w.txt {
			t.Fatalf("line %d = %+v, want %+v", i, lines[i], w)
		}
	}
	if LineAt([]byte("a\nbb\n"), 4) != 2 {
		t.Fatalf("LineAt = %d", LineAt([]byte("a\nbb\n"), 4))
	}
	bom := ScanLines([]byte("\xef\xbb\xbf#EXTM3U"))
	if bom[0].Text != "#EXTM3U" {
		t.Fatalf("BOM not stripped: %q", bom[0].Text)
	}
}

func TestValidateURIRejectsEscapes(t *testing.T) {
	bad := []string{
		"http://example.com/x.m3u8", "//cdn/x.m3u8", "/etc/passwd", "../x", "a/../../x",
		"..%2fx", "x\\y", "x\x00y", "", "..", "./", "x#y",
	}
	for _, b := range bad {
		if _, err := ValidateURI("media", b); err == nil {
			t.Fatalf("ValidateURI accepted %q", b)
		} else if !errors.Is(err, media.FaultManifestRange) {
			t.Fatalf("%q wrong kind: %v", b, err)
		}
	}
	for _, g := range []string{"a/b.m3u8", "seg.ts", "video/720p.m3u8"} {
		if _, err := ValidateURI("media", g); err != nil {
			t.Fatalf("ValidateURI rejected %q: %v", g, err)
		}
	}
}

func TestParseHLSMasterLinesAndSelection(t *testing.T) {
	m, err := ParseHLSMaster(Artifact{Name: "master.m3u8", Data: []byte(masterText)}, HSLOptions{Root: "media"})
	if err != nil {
		t.Fatalf("ParseHLSMaster: %v", err)
	}
	if len(m.Variants) != 3 {
		t.Fatalf("variants %d", len(m.Variants))
	}
	if m.Variants[1].URI != "video/720p.m3u8" || m.Variants[1].Line != 8 {
		t.Fatalf("variant 1 = %+v", m.Variants[1])
	}
	// offset of "video/720p.m3u8" must equal the byte position in the text.
	if int(m.Variants[1].Offset) != strings.Index(masterText, "video/720p.m3u8") {
		t.Fatalf("offset %d want %d", m.Variants[1].Offset, strings.Index(masterText, "video/720p.m3u8"))
	}
	v, err := m.SelectVariant(2000000)
	if err != nil || v.Bandwidth != 2000000 {
		t.Fatalf("select: %+v %v", v, err)
	}
	if len(m.Audio) != 1 || m.Audio[0].URI != "audio/media.m3u8" {
		t.Fatalf("audio renditions %+v", m.Audio)
	}
}

func TestParseHLSMasterRejectsRangeAndDup(t *testing.T) {
	bad := strings.Replace(masterText, "video/720p.m3u8", "../escape.m3u8", 1)
	if _, err := ParseHLSMaster(Artifact{Name: "m.m3u8", Data: []byte(bad)}, HSLOptions{Root: "media"}); !errors.Is(err, media.FaultManifestRange) {
		t.Fatalf("escape kind: %v", err)
	}
	dup := strings.Replace(masterText, "video/1080p.m3u8", "video/360p.m3u8", 1)
	_, err := ParseHLSMaster(Artifact{Name: "m.m3u8", Data: []byte(dup)}, HSLOptions{Root: "media"})
	if !errors.Is(err, media.FaultManifestRange) {
		t.Fatalf("duplicate variant kind: %v", err)
	}
	if _, ok := media.AsFault(err); !ok {
		t.Fatalf("not a fault: %v", err)
	}
	if f, _ := media.AsFault(err); f.Pos.Line == 0 {
		t.Fatalf("duplicate fault lost line: %+v", f.Pos)
	}
}

func TestParseHLSMediaExactOffsets(t *testing.T) {
	clk := media.MustClock(1000)
	m, err := ParseHLSMedia(Artifact{Name: "v.m3u8", Data: []byte(mediaText)}, HSLOptions{Root: "media", Clock: clk})
	if err != nil {
		t.Fatalf("ParseHLSMedia: %v", err)
	}
	if len(m.Segments) != 3 || m.SeqStart != 7 || !m.EndList {
		t.Fatalf("media = %+v", m)
	}
	s := m.Segments[1]
	if s.Ref.URI != "video/seg00008.m4s" || s.Ref.Line != 9 {
		t.Fatalf("seg = %+v", s)
	}
	if int(s.Ref.Offset) != strings.Index(mediaText, "video/seg00008.m4s") {
		t.Fatalf("offset %d want %d", s.Ref.Offset, strings.Index(mediaText, "video/seg00008.m4s"))
	}
	if s.Duration.Ticks() != 1000 || s.TotalTicks != 1000 {
		t.Fatalf("duration %d total %d", s.Duration.Ticks(), s.TotalTicks)
	}
	if m.Init == nil || m.Init.URI != "init.mp4" {
		t.Fatalf("init %+v", m.Init)
	}
	if m.EndTick() != 3000 {
		t.Fatalf("end tick %d", m.EndTick())
	}
}

func TestParseHLSMediaRejects(t *testing.T) {
	clk := media.MustClock(90000)
	cases := []struct {
		name string
		text string
		kind media.FaultKind
	}{
		{"escape", strings.Replace(mediaText, "video/seg00007.m4s", "../x.m4s", 1), media.FaultManifestRange},
		{"absolute", strings.Replace(mediaText, "video/seg00007.m4s", "/tmp/x.m4s", 1), media.FaultManifestRange},
		{"zero duration", strings.Replace(mediaText, "#EXTINF:1.000,\nvideo/seg00007.m4s", "#EXTINF:0.000,\nvideo/seg00007.m4s", 1), media.FaultDurationMismatch},
		{"negative duration", strings.Replace(mediaText, "#EXTINF:1.000,\nvideo/seg00007.m4s", "#EXTINF:-2.0,\nvideo/seg00007.m4s", 1), media.FaultDurationMismatch},
		{"no endlist", strings.Replace(mediaText, "#EXT-X-ENDLIST\n", "", 1), media.FaultManifestSyntax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseHLSMedia(Artifact{Name: "v.m3u8", Data: []byte(c.text)}, HSLOptions{Root: "media", Clock: clk})
			if err == nil {
				t.Fatal("expected rejection")
			}
			if media.KindOf(err) != c.kind {
				t.Fatalf("kind %v want %v (%v)", media.KindOf(err), c.kind, err)
			}
		})
	}
}

func TestDurationFromEXTINF(t *testing.T) {
	c := media.MustClock(90000)
	for _, sec := range []float64{1, 2.5, 0.5, 1.002} {
		d, err := DurationFromEXTINF(sec, c)
		if err != nil {
			t.Fatalf("%v: %v", sec, err)
		}
		if d.Ticks() != int64(sec*90000) {
			t.Fatalf("%v -> %d", sec, d.Ticks())
		}
	}
	if _, err := DurationFromEXTINF(1.0/3, c); err == nil {
		// 0.3333333333333333 * 90000 is not integral; must be rejected rather
		// than silently rounded.
		t.Fatal("1/3s must be inexact at 90k")
	}
	d, err := DurationFromEXTINF(0.3333333333333333, c)
	if err == nil {
		t.Fatalf("unexpectedly exact: %d", d.Ticks())
	}
	if _, err := DurationFromEXTINF(0, c); err == nil {
		t.Fatal("zero must fail")
	}
}
