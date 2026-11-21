package manifest

import (
	"strings"
	"testing"
)

const good = `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXT-X-MEDIA-SEQUENCE:7
#EXTINF:4.000,
seg7.m4s
#EXTINF:2.500,
seg8.m4s
#EXT-X-ENDLIST
`

func TestParseHLSGood(t *testing.T) {
	p := ParseHLS("live.m3u8", []byte(good))
	if p.Truncated || len(p.Faults) != 0 {
		t.Fatalf("unexpected faults: %+v", p.Faults)
	}
	if len(p.Segments) != 2 || p.Segments[0].Index != 7 || p.Segments[1].Index != 8 {
		t.Fatalf("segments: %+v", p.Segments)
	}
	if p.Segments[0].Duration.Millis() != 4000 {
		t.Fatalf("duration: %v", p.Segments[0].Duration)
	}
	if p.Duration.Millis() != 6500 {
		t.Fatalf("total: %v", p.Duration.Millis())
	}
	// 偏移应指向 EXTINF 行
	off := p.Segments[1].Offset
	if !strings.HasPrefix(good[off:], "#EXTINF:2.5") {
		t.Fatalf("offset %d wrong", off)
	}
}

func TestParseHLSTruncated(t *testing.T) {
	trunc := `#EXTM3U
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:4.000,
seg0.m4s
#EXTINF:4.000,
`
	p := ParseHLS("t.m3u8", []byte(trunc))
	if !p.Truncated {
		t.Fatal("expected truncated")
	}
	if len(p.Segments) != 1 {
		t.Fatalf("prefix must be kept: %+v", p.Segments)
	}
	if !p.Faults[0].Recoverable {
		t.Fatal("truncation must be recoverable")
	}
	if p.Faults[0].Offset < 0 {
		t.Fatal("need non-negative offset")
	}
}

func TestParseHLSBadDuration(t *testing.T) {
	bad := "#EXTM3U\n#EXTINF:abc,\nseg0.m4s\n#EXT-X-ENDLIST\n"
	p := ParseHLS("b.m3u8", []byte(bad))
	if len(p.Segments) != 0 || len(p.Faults) == 0 {
		t.Fatalf("bad duration: %+v", p)
	}
}
