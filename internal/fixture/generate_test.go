package fixture

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"archive/internal/clock"
	"archive/internal/demux"
	"archive/internal/manifest"
)

func segmentTracks(t *testing.T, root string, lay Layout, idx int) ([]demux.Track, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, lay.MediaDir, "seg-"+fmtName(idx)+".m4s"))
	if err != nil {
		return nil, err
	}
	return demux.Demux(data, "seg")
}

func fmtName(i int) string {
	return fmt.Sprintf("%05d", i)
}

func TestGenerateAndParseHLS(t *testing.T) {
	root := t.TempDir()
	lay, err := Generate(root, Options{Segments: 5})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, lay.HLS))
	if err != nil {
		t.Fatal(err)
	}
	segs, err := manifest.ParseHLS(raw, lay.HLS, lay.MediaDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 5 {
		t.Fatalf("got %d segments", len(segs))
	}
	if segs[4].Start.Float64() != 8 {
		t.Fatalf("segment 4 start = %v", segs[4].Start)
	}
	for _, s := range segs {
		if _, err := os.Stat(filepath.Join(root, s.URI)); err != nil {
			t.Fatalf("segment %s missing: %v", s.URI, err)
		}
	}
}

func TestGeneratedSegmentDemuxes(t *testing.T) {
	root := t.TempDir()
	lay, err := Generate(root, Options{Segments: 2})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, lay.MediaDir, "seg-00001.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := demux.Demux(data, "seg-00001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks", len(tracks))
	}
	var video, audio *demux.Track
	for i := range tracks {
		switch tracks[i].Handler {
		case "vide":
			video = &tracks[i]
		case "soun":
			audio = &tracks[i]
		}
	}
	if video == nil || audio == nil {
		t.Fatalf("missing tracks: %+v", tracks)
	}
	if video.Codec != "avc1" || video.Timescale != VideoTimesc || len(video.Samples) != videoFrames {
		t.Fatalf("bad video track: %+v", *video)
	}
	if video.Entry == nil || len(video.Entry) < 8 {
		t.Fatal("video sample entry not captured")
	}
	if audio.Codec != "mp4a" || audio.Channels != 2 || len(audio.Samples) != audioFrames {
		t.Fatalf("bad audio track: %+v", *audio)
	}
	if audio.Lang != "eng" {
		t.Fatalf("language = %q", audio.Lang)
	}
	// Sample zero decodes to the segment base (2s for segment 1).
	if got := len(video.Samples[0].Data); got != 64 {
		t.Fatalf("video sample size = %d", got)
	}
	if want := uint64(2 * VideoTimesc); video.Samples[0].DTS != want {
		t.Fatalf("video DTS = %d, want %d", video.Samples[0].DTS, want)
	}
}

func TestWrapCounters(t *testing.T) {
	root := t.TempDir()
	lay, err := Generate(root, Options{Segments: 45, Wrap: true})
	if err != nil {
		t.Fatal(err)
	}
	// Segment 39 ends just before the 33-bit boundary; segment 40
	// begins exactly at the wrapped zero.
	before, err := segmentTracks(t, root, lay, 39)
	if err != nil {
		t.Fatal(err)
	}
	after, err := segmentTracks(t, root, lay, 40)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range before {
		if tr.Handler != "vide" {
			continue
		}
		if tr.Samples[0].DTS < uint64(1<<33)-uint64(SegDurSecs)*VideoTimesc {
			t.Fatalf("segment 39 should sit near the wrap boundary, got %d", tr.Samples[0].DTS)
		}
	}
	for _, tr := range after {
		if tr.Handler != "vide" {
			continue
		}
		if tr.Samples[0].DTS != 0 {
			t.Fatalf("segment 40 should wrap to zero, got %d", tr.Samples[0].DTS)
		}
		for _, s := range tr.Samples {
			if s.DTS >= 1<<33 {
				t.Fatalf("raw DTS must stay modulo the wrap period, got %d", s.DTS)
			}
		}
	}
	// Unwrapping both segments in order must produce a monotonic
	// timeline spanning the boundary.
	u := clock.Unwrap{Period: 1 << 33}
	var last int64
	for _, trs := range [][]demux.Track{before, after} {
		for _, tr := range trs {
			if tr.Handler != "vide" {
				continue
			}
			for _, s := range tr.Samples {
				got := u.Next(s.DTS)
				if got <= last {
					t.Fatalf("unwrapped timeline not increasing: %d after %d", got, last)
				}
				last = got
			}
		}
	}
}

func TestMissingAndTruncated(t *testing.T) {
	root := t.TempDir()
	lay, err := Generate(root, Options{Segments: 4, MissingAt: 2, TruncateAt: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, lay.MediaDir, "seg-00002.m4s")); !os.IsNotExist(err) {
		t.Fatal("missing segment should be absent")
	}
	root2 := t.TempDir()
	lay2, err := Generate(root2, Options{Segments: 4, MissingAt: -1, TruncateAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root2, lay2.MediaDir, "seg-00001.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(filepath.Join(root2, lay2.MediaDir, "seg-00000.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) >= len(full) {
		t.Fatalf("truncated segment (%d) should be shorter than intact (%d)", len(data), len(full))
	}
}
