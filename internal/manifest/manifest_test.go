package manifest_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediascan/internal/core"
	"mediascan/internal/fixture"
	"mediascan/internal/manifest"
)

func generateClean(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := fixture.Generate(dir, fixture.FaultNone); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return dir
}

func readFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

func TestRoundTripCleanCorpus(t *testing.T) {
	root := generateClean(t)
	if _, err := manifest.RoundTripHLSMaster("master", readFile(t, root, "hls/master.m3u8")); err != nil {
		t.Fatalf("master roundtrip: %v", err)
	}
	media, err := manifest.RoundTripHLSMedia("media", readFile(t, root, "hls/media.m3u8"))
	if err != nil {
		t.Fatalf("media roundtrip: %v", err)
	}
	if len(media.Segments) != fixture.HLSSegments {
		t.Fatalf("media segments = %d, want %d", len(media.Segments), fixture.HLSSegments)
	}
	disc := 0
	for _, s := range media.Segments {
		if s.Discontinuity {
			disc++
		}
		if s.Duration != core.NewRat(1, 2) {
			t.Fatalf("segment %s duration = %s, want 1/2", s.URI, s.Duration)
		}
		if !s.HasPDT {
			t.Fatalf("segment %s missing program-date-time", s.URI)
		}
	}
	if disc != 2 {
		t.Fatalf("discontinuities = %d, want 2", disc)
	}
	mpd, err := manifest.RoundTripDASH("mpd", readFile(t, root, "dash/manifest.mpd"))
	if err != nil {
		t.Fatalf("dash roundtrip: %v", err)
	}
	if len(mpd.Representations) != 2 {
		t.Fatalf("representations = %d, want 2", len(mpd.Representations))
	}
	for _, rep := range mpd.Representations {
		if len(rep.Segments) != fixture.DashSegments {
			t.Fatalf("rep %s segments = %d, want %d", rep.ID, len(rep.Segments), fixture.DashSegments)
		}
	}
}

func TestRoundTripReportsFirstField(t *testing.T) {
	root := generateClean(t)
	media, err := manifest.RoundTripHLSMedia("media", readFile(t, root, "hls/media.m3u8"))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	// Mutate one normalized field at a time: the diff must name the
	// first differing field in index order.
	other := *media
	other.Segments = append([]manifest.HLSSegment(nil), media.Segments...)
	other.Segments[2].Duration = core.NewRat(3, 4)
	if f := manifest.DiffMedia(media, &other); f != "segments[2].duration" {
		t.Fatalf("diff field = %q, want segments[2].duration", f)
	}
	other = *media
	other.Segments = append([]manifest.HLSSegment(nil), media.Segments...)
	other.Segments[0].URI = "renamed.m4s"
	other.Segments[1].Discontinuity = !other.Segments[1].Discontinuity
	if f := manifest.DiffMedia(media, &other); f != "segments[0].uri" {
		t.Fatalf("diff field = %q, want segments[0].uri (first field wins)", f)
	}
	if f := manifest.DiffMedia(media, media); f != "" {
		t.Fatalf("self diff = %q, want empty", f)
	}

	// A self-consistent but edited playlist still round-trips cleanly.
	data := readFile(t, root, "hls/media.m3u8")
	edited := strings.Replace(string(data), "#EXTINF:0.500000,\nseg0.m4s",
		"#EXTINF:0.750000,\nseg0.m4s", 1)
	m2, err := manifest.RoundTripHLSMedia("media", []byte(edited))
	if err != nil {
		t.Fatalf("edited playlist should round-trip: %v", err)
	}
	if m2.Segments[0].Duration != core.NewRat(3, 4) {
		t.Fatalf("edited duration = %s, want 3/4", m2.Segments[0].Duration)
	}
}

func TestDASHPTOMapping(t *testing.T) {
	root := generateClean(t)
	mpd, err := manifest.RoundTripDASH("mpd", readFile(t, root, "dash/manifest.mpd"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var repB *manifest.MPDRepresentation
	for i := range mpd.Representations {
		if mpd.Representations[i].ID == "b" {
			repB = &mpd.Representations[i]
		}
	}
	if repB == nil {
		t.Fatal("representation b not found")
	}
	if repB.PTO != 90000 {
		t.Fatalf("rep b PTO = %d, want 90000", repB.PTO)
	}
	// (t - pto) / timescale must map rep b onto the same unified
	// timeline as rep a: 0, 1/2, 1, ...
	for i, s := range repB.Segments {
		want := core.NewRat(int64(i), 2)
		if got := repB.StartSeconds(s); got != want {
			t.Fatalf("rep b seg%d start = %s, want %s", i, got, want)
		}
		if got := repB.DurationSeconds(s); got != core.NewRat(1, 2) {
			t.Fatalf("rep b seg%d duration = %s, want 1/2", i, got)
		}
		if url := repB.SegmentURL(i); url != "b/seg"+string(rune('0'+i))+".m4s" {
			t.Fatalf("rep b seg%d url = %s", i, url)
		}
	}
}

func TestInvalidDurationRejected(t *testing.T) {
	dir := t.TempDir()
	if err := fixture.Generate(dir, fixture.FaultInvalidDuration); err != nil {
		t.Fatalf("generate: %v", err)
	}
	_, err := manifest.ParseHLSMedia(readFile(t, dir, "hls/media.m3u8"))
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("want invalid-duration error, got %v", err)
	}
}

func TestMismatchErrorField(t *testing.T) {
	var me *manifest.MismatchError
	err := error(&manifest.MismatchError{Manifest: "m", Field: "segments[0].duration"})
	if !errors.As(err, &me) {
		t.Fatal("MismatchError should unwrap")
	}
	if me.Field != "segments[0].duration" {
		t.Fatalf("field = %s", me.Field)
	}
}
