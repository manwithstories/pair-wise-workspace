package manifest

import (
	"strings"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

const sampleHLS = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:10
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="init.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0xDEAD
#EXTINF:1.96,
seg1.m4s
#EXTINF:2.00,
seg2.m4s
#EXT-X-DISCONTINUITY
#EXTINF:1.5,
seg3.m4s
#EXT-X-ENDLIST
`

func TestHLSParseAndRoundTrip(t *testing.T) {
	m, faults, err := ParseHLS(strings.NewReader(sampleHLS), "playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if faults.Len() != 0 {
		t.Fatalf("unexpected faults: %v", faults.Err())
	}
	if m.SegmentCount() != 3 || m.MediaSequence != 10 || !m.EndList {
		t.Fatalf("bad header: %+v", m)
	}
	segs := m.Segments()
	if segs[0].Start != 0 || segs[1].Start != 176400 || segs[2].Start != 356400 {
		t.Fatalf("timeline ticks wrong: %+v", segs)
	}
	if segs[2].End() != 356400+135000 {
		t.Fatalf("end = %d", segs[2].End())
	}
	if segs[0].Sequence != 10 || segs[2].Sequence != 12 {
		t.Fatalf("sequence numbers = %d..%d", segs[0].Sequence, segs[2].Sequence)
	}
	if !segs[2].Discontinuity || segs[1].Discontinuity {
		t.Fatal("discontinuity flags wrong")
	}
	if segs[0].Key == nil || segs[0].Key.Method != "AES-128" || segs[0].Key.IV != "0xDEAD" {
		t.Fatalf("key = %+v", segs[0].Key)
	}
	if segs[0].MapURI != "init.mp4" || m.Representations[0].InitURI != "init.mp4" {
		t.Fatal("init map not retained")
	}

	out, err := SerializeHLS(m)
	if err != nil {
		t.Fatal(err)
	}
	m2, faults2, err := ParseHLS(strings.NewReader(string(out)), "roundtrip.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if faults2.Len() != 0 {
		t.Fatalf("round trip faults: %v", faults2.Err())
	}
	matched, total := RoundTripScore(m, m2)
	if matched != total || total == 0 {
		t.Fatalf("round trip %d/%d\n%s", matched, total, out)
	}
	if !Equal(m, m2) {
		t.Fatalf("canonical mismatch\n%s", out)
	}
	if d1, _ := m.Digest(); d1 == "" {
		t.Fatal("empty digest")
	}
}

func TestHLSByteRange(t *testing.T) {
	src := `#EXTM3U
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
#EXT-X-BYTERANGE:100@0
seg.m4s
#EXTINF:2.0,
#EXT-X-BYTERANGE:50
seg.m4s
`
	m, f, err := ParseHLS(strings.NewReader(src), "p")
	if err != nil || f.Len() != 0 {
		t.Fatalf("%v %v", err, f.Err())
	}
	segs := m.Segments()
	if segs[0].ByteRange.Offset != 0 || !segs[0].ByteRange.HasOffset {
		t.Fatalf("first range %+v", segs[0].ByteRange)
	}
	if segs[1].ByteRange.Offset != 100 || segs[1].ByteRange.HasOffset {
		t.Fatalf("implicit range %+v", segs[1].ByteRange)
	}
	out, _ := SerializeHLS(m)
	m2, _, err := ParseHLS(strings.NewReader(string(out)), "p2")
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(m, m2) {
		t.Fatalf("byte range round trip differs:\n%s", out)
	}
}

func TestHLSUnknownDurationIsNotFabricated(t *testing.T) {
	src := "#EXTM3U\n#EXT-X-TARGETDURATION:2\nseg.m4s\n"
	m, f, err := ParseHLS(strings.NewReader(src), "p")
	if err != nil {
		t.Fatal(err)
	}
	if f.Len() != 1 || f.All()[0].Kind != fault.KindManifestParse {
		t.Fatalf("expected one parse fault, got %v", f.Err())
	}
	seg := m.Segments()[0]
	if seg.DurationKnown || seg.Duration != 0 {
		t.Fatalf("duration fabricated: %+v", seg)
	}
}

func TestHLSRejectsMissingHeader(t *testing.T) {
	if _, _, err := ParseHLS(strings.NewReader("#EXTINF:2,\nseg.m4s\n"), "p"); err == nil {
		t.Fatal("expected missing header error")
	}
}

func TestTargetDurationRendering(t *testing.T) {
	m, _, err := ParseHLS(strings.NewReader(sampleHLS), "p")
	if err != nil {
		t.Fatal(err)
	}
	if m.TargetDuration != 180000 {
		t.Fatalf("target = %d", m.TargetDuration)
	}
	tb := clock.Default()
	if got := tb.Seconds(m.TargetDuration); got != 2 {
		t.Fatalf("seconds = %v", got)
	}
}
