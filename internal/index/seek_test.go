package index

import (
	"os"
	"path/filepath"
	"testing"

	"avrepair/internal/media"
)

func seg(id string, ptsMS int64, quarantine string) *media.Segment {
	s := &media.Segment{
		ID: id, Source: "hls", URI: id + ".ts", Seq: 0,
		Duration: media.NewRational(1000, 1000),
		Samples:  []media.Sample{{PTS: media.NewRational(ptsMS, 1000), Sync: true}},
	}
	if quarantine != "" {
		s.Quarantine = quarantine
	}
	return s
}

func timeline(segs ...*media.Segment) *media.Timeline {
	return &media.Timeline{Tracks: []*media.Track{{
		ID: "video", Kind: media.TrackVideo, Segments: segs,
	}}}
}

func opts() BuildOptions {
	return BuildOptions{
		InputHash:    "in",
		OutputOf:     func(s *media.Segment) (string, bool) { return s.ID + ".out", s.Quarantine == "" },
		SourceHashOf: func(s *media.Segment) string { return "sha-" + s.ID },
	}
}

func TestBuildSkipsQuarantinedAndKeepsMonotonic(t *testing.T) {
	tl := timeline(seg("a", 0, ""), seg("b", 1000, media.ReasonTruncated), seg("c", 2000, ""))
	idx, err := Build(tl, opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 2 || idx.Entries[1].Segment != "c" {
		t.Fatalf("entries: %+v", idx.Entries)
	}
	if idx.Tracks[0].Quarantine != 1 || idx.Tracks[0].Entries != 2 {
		t.Fatalf("summary: %+v", idx.Tracks[0])
	}
	for _, e := range idx.Entries {
		if e.Output == "b.out" {
			t.Fatal("index references a quarantined segment")
		}
		if e.SourceURI == "" || e.SourceSHA == "" {
			t.Fatalf("entry missing provenance: %+v", e)
		}
	}
}

func TestBuildRejectsNonMonotonic(t *testing.T) {
	tl := timeline(seg("a", 1000, ""), seg("b", 1000, ""))
	if _, err := Build(tl, opts()); err == nil {
		t.Fatal("duplicate PTS accepted")
	}
	tl = timeline(seg("a", 2000, ""), seg("b", 1000, ""))
	if _, err := Build(tl, opts()); err == nil {
		t.Fatal("decreasing PTS accepted")
	}
}

func TestSeek(t *testing.T) {
	idx, err := Build(timeline(seg("a", 0, ""), seg("b", 1000, ""), seg("c", 2000, "")), opts())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := idx.Seek("video", media.NewRational(1500, 1000))
	if !ok || e.Segment != "b" {
		t.Fatalf("seek: %+v %v", e, ok)
	}
	if _, ok := idx.Seek("video", media.NewRational(-1, 1000)); ok {
		t.Fatal("seek before start should fail")
	}
	if _, ok := idx.Seek("audio", media.NewRational(1000, 1000)); ok {
		t.Fatal("seek on unknown track should fail")
	}
}

func TestAtomicWriteVerifiesAndCommits(t *testing.T) {
	out := t.TempDir()
	for _, id := range []string{"a.out", "b.out"} {
		if err := os.WriteFile(filepath.Join(out, id), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := Build(timeline(seg("a", 0, ""), seg("b", 1000, "")), opts())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(out, "index.json")
	if _, err := WriteAtomic(path, idx, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil || len(decoded.Entries) != 2 {
		t.Fatalf("decode: %v %+v", err, decoded)
	}
}

func TestValidateRejectsMissingOutput(t *testing.T) {
	out := t.TempDir()
	idx, err := Build(timeline(seg("a", 0, "")), opts())
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Validate(out); err == nil {
		t.Fatal("missing output accepted")
	}
}

func TestVerifyTempDetectsCorruption(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "a.out"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := Build(timeline(seg("a", 0, "")), opts())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := idx.Encode()
	tmp, sum, err := WriteTemp(filepath.Join(out, "index.json"), data)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTemp(tmp, sum, out); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTemp(tmp, "deadbeef", out); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	os.Remove(tmp)
}
