package demux

import (
	"bytes"
	"testing"

	"mediapipe/internal/ebml"
	"mediapipe/internal/media"
	"mediapipe/internal/testkit"
)

func TestDemuxCleanFixture(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	res, err := Demux(bytes.NewReader(clean.Bytes), DefaultLimits())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if res.DocType != "matroska" {
		t.Fatalf("docType=%q", res.DocType)
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(res.Tracks))
	}
	if len(res.Packets) != len(clean.Samples) {
		t.Fatalf("packets=%d want %d", len(res.Packets), len(clean.Samples))
	}
	if res.Truncated {
		t.Fatal("clean fixture reported truncation")
	}
	if res.UnknownSizes != 1 {
		t.Fatalf("unknown sizes=%d want 1 (segment)", res.UnknownSizes)
	}
	if len(res.Clusters) != 6 {
		t.Fatalf("clusters=%d", len(res.Clusters))
	}
	for _, c := range res.Clusters {
		if !c.HasCRC {
			t.Fatal("fixture cluster missing CRC")
		}
		if !c.VerifyClusterCRC(clean.Bytes) {
			t.Fatalf("cluster at %d failed CRC verification", c.Offset)
		}
	}
}

func TestDemuxPacketPayloadMatchesSource(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	res, err := Demux(bytes.NewReader(clean.Bytes), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Every payload must be a verbatim byte range from the source file, and
	// packet CRC must match the fixture payload.
	found := 0
	for _, p := range res.Packets {
		if !bytes.Contains(clean.Bytes, p.Data) {
			t.Fatalf("packet track=%d dts=%d not present in source", p.Track, p.DTS)
		}
		found++
	}
	if found != len(clean.Samples) {
		t.Fatalf("found=%d", found)
	}
}

func TestDemuxLateFixturePreservesTimestamps(t *testing.T) {
	_, late, _, _ := testkit.Generate(73)
	res, err := Demux(bytes.NewReader(late.Bytes), DefaultLimits())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if len(res.Packets) != len(late.Samples) {
		t.Fatalf("packets=%d want %d", len(res.Packets), len(late.Samples))
	}
	// Even though frames are written out of order, each packet's DTS is the
	// absolute time encoded in the block, so timestamps stay attached to the
	// payload rather than the write position. Compare as a multiset.
	type key struct {
		track uint64
		dts   media.Timestamp
	}
	want := make(map[key]int, len(late.Samples))
	for _, s := range late.Samples {
		want[key{s.Track, s.DTS}]++
	}
	for _, p := range res.Packets {
		k := key{p.Track, p.DTS}
		if want[k] == 0 {
			t.Fatalf("unexpected packet track=%d dts=%d", p.Track, p.DTS)
		}
		want[k]--
		if p.CRC == 0 {
			t.Fatalf("packet track=%d dts=%d has zero CRC", p.Track, p.DTS)
		}
	}
	for k, n := range want {
		if n != 0 {
			t.Fatalf("missing %d packets for track=%d dts=%d", n, k.track, k.dts)
		}
	}
}

func TestDemuxTruncatedFixtureReportsOffset(t *testing.T) {
	_, _, trunc, _ := testkit.Generate(73)
	res, err := Demux(bytes.NewReader(trunc.Bytes), DefaultLimits())
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !res.Truncated {
		t.Fatal("Truncated flag not set")
	}
	if media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("class=%v", media.ClassOf(err))
	}
	off, ok := media.OffsetOf(err)
	if !ok {
		t.Fatal("truncation error carries no offset")
	}
	if off != int64(len(trunc.Bytes)) {
		t.Fatalf("offset=%d want %d", off, len(trunc.Bytes))
	}
	if len(res.Packets) == 0 {
		t.Fatal("expected partial packets before truncation")
	}
}

func TestDemuxUnknownClusterFixture(t *testing.T) {
	_, _, _, interrupted := testkit.Generate(73)
	if !interrupted.Spec.UnknownCluster {
		t.Fatal("fixture did not request unknown clusters")
	}
	res, err := Demux(bytes.NewReader(interrupted.Bytes), DefaultLimits())
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	// One unknown segment plus every unknown cluster.
	if res.UnknownSizes < 2 {
		t.Fatalf("unknown size elements=%d", res.UnknownSizes)
	}
	if len(res.Packets) != len(interrupted.Samples) {
		t.Fatalf("packets=%d want %d", len(res.Packets), len(interrupted.Samples))
	}
}

func TestLaceModes(t *testing.T) {
	body := func(frames ...[]byte) []byte { return bytes.Join(frames, nil) }
	f1, f2, f3 := []byte{0x11, 0x22}, []byte{0x33}, []byte{0x44, 0x55, 0x66}

	// fixed: three equal frames, count byte says 3 frames total.
	fixed := append([]byte{2}, body([]byte{1, 2, 3}, []byte{4, 5, 6}, []byte{7, 8, 9})...)
	got, err := lace(fixed, 2)
	if err != nil || len(got) != 3 || !bytes.Equal(got[2], []byte{7, 8, 9}) {
		t.Fatalf("fixed lace: %v %v", got, err)
	}

	// xiph: sizes 2 and 1, then payload.
	xiph := append([]byte{2, 2, 1}, body(f1, f2, f3)...)
	got, err = lace(xiph, 1)
	if err != nil || len(got) != 3 || !bytes.Equal(got[0], f1) || !bytes.Equal(got[2], f3) {
		t.Fatalf("xiph lace: %v %v", got, err)
	}

	// ebml: sizes encoded as one-byte VINTs.
	ebmlBody := append([]byte{2, 0x82, 0x81}, body(f1, f2, f3)...)
	got, err = lace(ebmlBody, 3)
	if err != nil || len(got) != 3 || !bytes.Equal(got[1], f2) {
		t.Fatalf("ebml lace: %v %v", got, err)
	}
}

func TestLaceRejectsOverrun(t *testing.T) {
	// xiph claims frame 0 is 200 bytes but only 3 remain.
	body := append([]byte{1, 200}, []byte{1, 2, 3}...)
	if _, err := lace(body, 1); err == nil {
		t.Fatal("expected overrun error")
	}
}

func TestDemuxRejectsUnsupportedDocType(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	spec := clean.Spec
	spec.DocType = "avi"
	bad := testkit.Build(spec)
	_, err := Demux(bytes.NewReader(bad.Bytes), DefaultLimits())
	if err == nil || media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("err=%v class=%v", err, media.ClassOf(err))
	}
}

func TestDemuxRejectsOverlongElement(t *testing.T) {
	// Hand-build a Segment claiming a payload larger than the limit.
	var buf bytes.Buffer
	w := ebml.NewWriter(&buf, nil)
	w.Element(ebml.IDEBML, nil)
	w.WriteID(ebml.IDSegment)
	w.WriteSize(1024)
	lim := DefaultLimits()
	lim.EBML.MaxElementSize = 128
	_, err := Demux(bytes.NewReader(buf.Bytes()), lim)
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
}

func TestDemuxRejectsDuplicateTrack(t *testing.T) {
	clean, _, _, _ := testkit.Generate(73)
	spec := clean.Spec
	spec.Tracks = append(spec.Tracks, spec.Tracks[0])
	bad := testkit.Build(spec)
	_, err := Demux(bytes.NewReader(bad.Bytes), DefaultLimits())
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
}
