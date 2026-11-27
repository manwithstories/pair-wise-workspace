package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"avpipe/internal/media"
)

func pkt(track string, seq, pts int64, data string) media.Packet {
	return media.Packet{
		TrackID: track, Seq: seq, Arrival: seq, PTS: pts, DTS: pts,
		TimeBase: media.MustRat(1, 30), Data: []byte(data),
		Origin: media.Origin{File: "f", TrackID: track, PacketSeq: seq},
	}
}

func TestSegmenterFixedDuration(t *testing.T) {
	// 30fps for 6 seconds = 180 frames; segment every 2s should give 3 groups.
	var pkts []media.Packet
	for i := int64(0); i < 180; i++ {
		pkts = append(pkts, pkt("v0", i, i, "x"))
	}
	segs, groups := Segmenter(media.MustRat(2, 1), pkts)
	if len(segs) != 3 {
		t.Fatalf("segments = %d", len(segs))
	}
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	if total != 180 {
		t.Fatalf("packets lost: %d", total)
	}
}

func TestSegmenterPreservesVFR(t *testing.T) {
	base := media.MustRat(1, 90000)
	pts := []int64{0, 3000, 6000, 9000, 18000, 21000}
	var pkts []media.Packet
	for i, p := range pts {
		pkts = append(pkts, media.Packet{
			TrackID: "v0", Seq: int64(i), PTS: p, DTS: p, TimeBase: base, Data: []byte{byte(i)},
		})
	}
	segs, groups := Segmenter(media.MustRat(2, 1), pkts)
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	if total != len(pkts) {
		t.Fatalf("VFR packets lost: %d/%d", total, len(pkts))
	}
	if len(segs) < 1 {
		t.Fatal("no segments")
	}
}

func TestBuildSegmentRecordsHashStable(t *testing.T) {
	var pkts []media.Packet
	for i := int64(0); i < 60; i++ {
		pkts = append(pkts, pkt("v0", i, i, "a"))
	}
	segs, groups := Segmenter(media.MustRat(2, 1), pkts)
	recs1 := BuildSegmentRecords(segs, groups)
	recs2 := BuildSegmentRecords(segs, groups)
	if len(recs1) != len(recs2) {
		t.Fatal("record count mismatch")
	}
	for i := range recs1 {
		if recs1[i].Hash != recs2[i].Hash {
			t.Fatalf("segment %d hash unstable", i)
		}
	}
}

func TestSegmentHashChangesWithOrder(t *testing.T) {
	a := []media.Packet{pkt("v0", 0, 0, "a"), pkt("v0", 1, 1, "b")}
	b := []media.Packet{pkt("v0", 1, 1, "b"), pkt("v0", 0, 0, "a")}
	if media.ComputeSegmentHash(0, a) == media.ComputeSegmentHash(0, b) {
		t.Fatal("hash must depend on order")
	}
}

func TestManifestCommitAtomicAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	w, err := NewWriter(path, 17)
	if err != nil {
		t.Fatal(err)
	}
	w.AddTrack(media.Track{ID: "v0", Kind: media.TrackVideo, Codec: "h264", TimeBase: media.MustRat(1, 30)})
	w.AddInput("in.mp4")
	w.Manifest().SegmentSecs = media.MustRat(2, 1)
	w.AddSegment(SegmentRecord{Index: 0, Packets: 2, Hash: "deadbeef"})
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Seed != 17 || len(m.Segments) != 1 || len(m.Tracks) != 1 {
		t.Fatalf("manifest roundtrip wrong: %+v", m)
	}
	if m.OutputDigest == "" {
		t.Fatal("output digest not computed")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "manifest.json" {
			t.Fatalf("leftover %q", e.Name())
		}
	}
}

func TestVerifyDetectsTamperedSegment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.json")
	w, _ := NewWriter(path, 17)
	w.Manifest().SegmentSecs = media.MustRat(2, 1)
	w.AddSegment(SegmentRecord{Index: 0, Packets: 2, Hash: "aaaa"})
	w.AddSegment(SegmentRecord{Index: 1, Packets: 2, Hash: "bbbb"})
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	// Tamper with one hash after commit by rewriting the file.
	m, _ := Load(path)
	m.Segments[0].Hash = "cccc"
	data := mustMarshal(t, m)
	os.WriteFile(path, data, 0o644)

	v, err := Verify(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.OutputDigestOK {
		t.Fatal("tampered manifest still passes digest check")
	}
	if len(v.Problems) == 0 {
		t.Fatal("no problem reported")
	}
}

func mustMarshal(t *testing.T, m *Manifest) []byte {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCompareRuns(t *testing.T) {
	a := &Manifest{OutputDigest: "d", Segments: []SegmentRecord{{Index: 0, Hash: "h"}}}
	b := &Manifest{OutputDigest: "d", Segments: []SegmentRecord{{Index: 0, Hash: "h"}}}
	ok, _ := CompareRuns(a, b)
	if !ok {
		t.Fatal("identical runs reported different")
	}
	b.OutputDigest = "different"
	ok, _ = CompareRuns(a, b)
	if ok {
		t.Fatal("different runs reported identical")
	}
}
