package manifest

import (
	"strings"
	"testing"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

const goodM3U8 = `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-MAP:URI="init.mp4"
#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z
#EXTINF:4.0,
seg0.ts
#EXTINF:4.0,
seg1.ts
#EXT-X-DISCONTINUITY
#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:20Z
#EXTINF:2.0,
seg2.ts
#EXTINF:2.0,
seg3.ts
#EXT-X-ENDLIST
`

func TestParseHLSWithDiscontinuity(t *testing.T) {
	p, err := ParseHLS("live.m3u8", strings.NewReader(goodM3U8))
	if err != nil {
		t.Fatal(err)
	}
	segs := p.Representations[0].Segments
	if len(segs) != 4 {
		t.Fatalf("segs=%d", len(segs))
	}
	// 前两个连续：0-4s, 4-8s；discontinuity 后显式重置到 20s
	want := []clock.Ticks{0, 4 * clock.Second, 20 * clock.Second, 22 * clock.Second}
	for i, s := range segs {
		if s.Start != want[i] {
			t.Fatalf("seg %d start=%d want=%d", i, s.Start, want[i])
		}
	}
	if p.Representations[0].InitURI != "init.mp4" {
		t.Fatalf("init=%s", p.Representations[0].InitURI)
	}
}

func TestParseHLSAmbiguousDiscontinuity(t *testing.T) {
	bad := strings.Replace(goodM3U8, "#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:20Z\n", "", 1)
	_, err := ParseHLS("bad.m3u8", strings.NewReader(bad))
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindManifest {
		t.Fatalf("err=%v", err)
	}
	if fe.Line == 0 || !strings.Contains(fe.Msg, "ambiguous") {
		t.Fatalf("fe=%+v", fe)
	}
}

func TestParseHLSMissingInit(t *testing.T) {
	bad := strings.Replace(goodM3U8, "#EXT-X-MAP:URI=\"init.mp4\"\n", "", 1)
	_, err := ParseHLS("bad.m3u8", strings.NewReader(bad))
	fe, ok := fault.As(err)
	if !ok || !strings.Contains(fe.Msg, "init segment") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseHLSBadEXTINF(t *testing.T) {
	bad := strings.Replace(goodM3U8, "#EXTINF:4.0,", "#EXTINF:-1,", 1)
	_, err := ParseHLS("bad.m3u8", strings.NewReader(bad))
	fe, ok := fault.As(err)
	if !ok || fe.Line != 6 {
		t.Fatalf("err=%v", err)
	}
}

func TestParseHLSPDTJumpWithoutDiscontinuity(t *testing.T) {
	jump := strings.Replace(goodM3U8, "#EXT-X-DISCONTINUITY\n", "", 1)
	_, err := ParseHLS("jump.m3u8", strings.NewReader(jump))
	fe, ok := fault.As(err)
	if !ok || !strings.Contains(fe.Msg, "without discontinuity") {
		t.Fatalf("err=%v", err)
	}
}
