package manifest

import (
	"errors"
	"strings"
	"testing"

	"mediatime/internal/clock"
)

const validHLS = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:10
#EXT-X-DISCONTINUITY
#EXTINF:4.0,
seg10.m4s
#EXTINF:4.0,
seg11.m4s
#EXTINF:3.5,
seg12.m4s
#EXT-X-ENDLIST
`

func TestParseHLSBasic(t *testing.T) {
	m, err := ParseHLS([]byte(validHLS))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if m.Protocol != ProtoHLS || len(m.Fragments) != 3 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if m.MediaSequence != 10 || m.Fragments[0].Sequence != 0 {
		t.Fatal("sequence mapping wrong")
	}
	if !m.Fragments[0].Discontinuity || m.Fragments[1].Discontinuity {
		t.Fatal("discontinuity flag wrong")
	}
	if m.Fragments[0].DurationTicks != 4*int64(clock.TickRate) {
		t.Fatal("duration conversion wrong")
	}
	if m.Fragments[2].StartTicks != 8*int64(clock.TickRate) {
		t.Fatalf("start ticks wrong: %d", m.Fragments[2].StartTicks)
	}
}

func TestParseHLSRejectsDuplicateConflict(t *testing.T) {
	// MEDIA-SEQUENCE 回退使第二片与第一片落在同一序号但 URI 不同 → 冲突拒绝。
	bad := `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:10
#EXTINF:4.0,
seg10.m4s
#EXT-X-MEDIA-SEQUENCE:10
#EXTINF:5.0,
seg10-different.m4s
#EXT-X-ENDLIST
`
	_, err := ParseHLS([]byte(bad))
	if !errors.Is(err, ErrDuplicateSequence) {
		t.Fatalf("want ErrDuplicateSequence, got %v", err)
	}
}

func TestParseHLSRejectsPathEscape(t *testing.T) {
	cases := []string{
		"#EXTINF:1,\n../escape.m4s\n",
		"#EXTINF:1,\n/abs/path.m4s\n",
		"#EXTINF:1,\nhttp://evil/x.m4s\n",
		"#EXTINF:1,\nC:\\win\\x.m4s\n",
	}
	for _, c := range cases {
		doc := "#EXTM3U\n#EXT-X-TARGETDURATION:1\n" + c + "#EXT-X-ENDLIST\n"
		if _, err := ParseHLS([]byte(doc)); !errors.Is(err, ErrPathOutOfScope) {
			t.Fatalf("case %q: want ErrPathOutOfScope, got %v", c, err)
		}
	}
}

func TestParseHLSRejectsOversizedAndInvalid(t *testing.T) {
	if _, err := ParseHLS([]byte("not a playlist")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("want ErrInvalidManifest, got %v", err)
	}
	big := make([]byte, MaxManifestBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := ParseHLS(big); !errors.Is(err, ErrManifestTooLarge) {
		t.Fatalf("want ErrManifestTooLarge, got %v", err)
	}
}

func TestParseHLSTooManyFragments(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n")
	for i := 0; i < MaxFragments+1; i++ {
		sb.WriteString("#EXTINF:1.0,\nseg.m4s\n")
	}
	if _, err := ParseHLS([]byte(sb.String())); !errors.Is(err, ErrTooManyFragments) {
		t.Fatalf("want ErrTooManyFragments, got %v", err)
	}
}

const validDASH = `<?xml version="1.0"?>
<MPD type="static">
  <Period>
    <AdaptationSet contentType="video">
      <SegmentTemplate timescale="90000" media="chunk-$Number$.m4s" startNumber="1">
        <SegmentTimeline>
          <S t="0" d="180000" r="2"/>
          <S d="90000"/>
        </SegmentTimeline>
      </SegmentTemplate>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHSegmentTimeline(t *testing.T) {
	m, err := ParseDASH([]byte(validDASH))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if m.Protocol != ProtoDASH || len(m.Fragments) != 4 {
		t.Fatalf("fragment count wrong: %+v", m.Fragments)
	}
	if m.Fragments[0].URI != "chunk-1.m4s" || m.Fragments[3].URI != "chunk-4.m4s" {
		t.Fatalf("template expansion wrong: %s %s", m.Fragments[0].URI, m.Fragments[3].URI)
	}
	// r=2 展开为 3 片，每片 2 秒；第 4 片 1 秒，起点应在 6 秒处。
	if m.Fragments[3].StartTicks != 6*int64(clock.TickRate) {
		t.Fatalf("4th start = %d", m.Fragments[3].StartTicks)
	}
	if m.Fragments[0].DurationTicks != 2*int64(clock.TickRate) {
		t.Fatal("timescale scaling wrong")
	}
}

func TestParseDASHRepresentationTemplate(t *testing.T) {
	doc := `<?xml version="1.0"?>
<MPD><Period><AdaptationSet>
  <Representation id="v0">
    <SegmentTemplate timescale="1000" media="$RepresentationID$/f-$Number$.m4s">
      <SegmentTimeline><S t="0" d="1000"/><S d="1000"/></SegmentTimeline>
    </SegmentTemplate>
  </Representation>
</AdaptationSet></Period></MPD>`
	m, err := ParseDASH([]byte(doc))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if m.Fragments[0].URI != "v0/f-1.m4s" {
		t.Fatalf("RepresentationID expansion wrong: %s", m.Fragments[0].URI)
	}
	// 1000 timescale 下 1000 单位 = 1 秒 = 90000 Tick。
	if m.Fragments[0].DurationTicks != int64(clock.TickRate) {
		t.Fatalf("timescale conversion = %d", m.Fragments[0].DurationTicks)
	}
}

func TestParseDASHRejectsBadXMLAndPath(t *testing.T) {
	if _, err := ParseDASH([]byte("<MPD><Period></MPD>")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("want ErrInvalidManifest, got %v", err)
	}
	doc := strings.Replace(validDASH, "chunk-$Number$.m4s", "../$Number$.m4s", 1)
	if _, err := ParseDASH([]byte(doc)); !errors.Is(err, ErrPathOutOfScope) {
		t.Fatalf("want ErrPathOutOfScope, got %v", err)
	}
}

func TestValidatePathUnit(t *testing.T) {
	good := []string{"a.m4s", "dir/a.m4s", "./a.m4s", "a/b/c.m4s"}
	for _, p := range good {
		if err := validatePath(p); err != nil {
			t.Fatalf("path %q wrongly rejected: %v", p, err)
		}
	}
	bad := []string{"", "../a", "a/../../b", "/abs", "scheme://x", "C:/a"}
	for _, p := range bad {
		if err := validatePath(p); err == nil {
			t.Fatalf("path %q should be rejected", p)
		}
	}
}
