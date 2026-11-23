package manifest

import (
	"errors"
	"strings"
	"testing"

	"mediarun/internal/clock"
)

func parseHLSString(t *testing.T, s string) (*Media, error) {
	t.Helper()
	return ParseHLSBytes([]byte(s), DefaultLimits())
}

func TestHLSValidPlaylist(t *testing.T) {
	const src = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:100
#EXTINF:4.0,
seg/000100.m4s
#EXT-X-MEDIA-HASH:` + hashA + `
#EXTINF:3.5,
seg/000101.m4s
#EXTINF:4.0,
seg/000102.m4s
`
	m, err := parseHLSString(t, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The hash directive precedes its URI line, not follows it: our fixture
	// above put it after the first segment, so it applies to segment 2.
	if len(m.Segments) != 3 {
		t.Fatalf("segments = %d", len(m.Segments))
	}
	if m.Segments[0].Seq != 100 || m.Segments[2].Seq != 102 {
		t.Fatalf("sequence numbers wrong: %+v", m.Segments)
	}
	if m.Segments[0].StartNS != 0 {
		t.Fatalf("first start = %d", m.Segments[0].StartNS)
	}
	if m.Segments[1].StartNS != 4_000_000_000 {
		t.Fatalf("second start = %d", m.Segments[1].StartNS)
	}
	if m.Segments[1].DurNS != 3_500_000_000 {
		t.Fatalf("second dur = %d", m.Segments[1].DurNS)
	}
	if m.Segments[2].StartNS != 7_500_000_000 {
		t.Fatalf("third start = %d", m.Segments[2].StartNS)
	}
	if m.TargetDurNS != 4_000_000_000 {
		t.Fatalf("target = %d", m.TargetDurNS)
	}
}

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestHLSHashAttachedToFollowingURI(t *testing.T) {
	src := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n" +
		"#EXT-X-MEDIA-HASH:" + hashA + "\n#EXTINF:4,\na.m4s\n" +
		"#EXT-X-MEDIA-HASH:" + hashA + "\n#EXTINF:4,\nb.m4s\n"
	m, err := parseHLSString(t, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Segments[0].Hash != hashA || m.Segments[1].Hash != hashA {
		t.Fatalf("hashes not attached: %+v", m.Segments)
	}
}

func TestHLSDuplicateSequenceDifferentHashRejected(t *testing.T) {
	// Identical sequence re-declared with the same content commitment is
	// tolerated (discontinuity restarts the axis).
	same := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:5\n" +
		"#EXT-X-MEDIA-HASH:" + hashA + "\n#EXTINF:4,\na.m4s\n" +
		"#EXT-X-MEDIA-SEQUENCE:5\n#EXT-X-MEDIA-HASH:" + hashA + "\n#EXTINF:4,\na.m4s\n"
	if _, err := parseHLSString(t, same); err != nil {
		t.Fatalf("matching commitment should be tolerated: %v", err)
	}

	// Same sequence, different hash: ambiguous, must be rejected.
	conflict := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:5\n" +
		"#EXT-X-MEDIA-HASH:" + hashA + "\n#EXTINF:4,\na.m4s\n" +
		"#EXT-X-MEDIA-SEQUENCE:5\n#EXT-X-MEDIA-HASH:" + hashB + "\n#EXTINF:4,\na.m4s\n"
	_, err := parseHLSString(t, conflict)
	var e *clock.Error
	if !errors.As(err, &e) || e.Code != clock.CodeDuplicateSeq {
		t.Fatalf("want duplicate-sequence rejection, got %v", err)
	}
}

func TestHLSPathEscapeRejected(t *testing.T) {
	cases := []string{
		"../escape.m4s",
		"seg/../../escape.m4s",
		"/abs.m4s",
		"http://example.test/a.m4s",
		"seg//a.m4s",
		"seg/a.m4s?x=1",
		"seg\\a.m4s",
	}
	for _, uri := range cases {
		src := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\n" + uri + "\n"
		_, err := parseHLSString(t, src)
		if err == nil {
			t.Fatalf("%q: expected rejection", uri)
		}
		if e := (*clock.Error)(nil); !errors.As(err, &e) || e.Code != clock.CodePathEscape {
			t.Fatalf("%q: wrong error %v", uri, err)
		}
	}
}

func TestHLSInvalidDurations(t *testing.T) {
	for _, dur := range []string{"0", "-1", "abc", "9999"} {
		src := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:" + dur + ",\na.m4s\n"
		_, err := parseHLSString(t, src)
		if err == nil {
			t.Fatalf("duration %q accepted", dur)
		}
		if !clock.IsKind(err, clock.KindInvalid) {
			t.Fatalf("duration %q: kind %v", dur, err)
		}
	}
}

func TestHLSEntryLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
	for i := 0; i < 11; i++ {
		b.WriteString("#EXTINF:4,\nseg/a.m4s\n")
	}
	_, err := ParseHLSBytes([]byte(b.String()), Limits{MaxEntries: 10, MaxSegmentBytes: 1 << 20})
	if err == nil || !clock.IsKind(err, clock.KindInvalid) {
		t.Fatalf("want entry-limit rejection, got %v", err)
	}
}

func TestHLSRejectsMasterPlaylist(t *testing.T) {
	src := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nlow.m3u8\n"
	_, err := parseHLSString(t, src)
	if err == nil {
		t.Fatalf("master playlist accepted")
	}
}

func TestHLSMissingHeader(t *testing.T) {
	_, err := parseHLSString(t, "#EXTINF:4,\na.m4s\n")
	if err == nil {
		t.Fatalf("missing header accepted")
	}
}
