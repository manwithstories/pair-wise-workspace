package fixture

import (
	"bytes"
	"testing"

	"mediaroute/internal/manifest"
)

func TestGenerateDeterministic(t *testing.T) {
	a, err := Generate(41, "default")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(41, "default")
	if err != nil {
		t.Fatal(err)
	}
	if a.HLS != b.HLS || !bytes.Equal(a.MPD, b.MPD) || !bytes.Equal(a.Init, b.Init) {
		t.Fatal("manifests not deterministic")
	}
	for i := range a.Segments {
		if a.Segments[i] != b.Segments[i] {
			t.Fatalf("segment %d differs", i)
		}
	}
	if !bytes.Equal(a.SegmentBytes(0), b.SegmentBytes(0)) {
		t.Fatal("packet stream not deterministic")
	}
}

func TestGenerateAnomalies(t *testing.T) {
	fx, err := Generate(41, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.Segments) != SegmentCount {
		t.Fatalf("segments %d", len(fx.Segments))
	}
	if !fx.Segments[7].Missing {
		t.Fatal("segment 7 should be missing")
	}
	if !fx.Segments[9].TruncatedFirst {
		t.Fatal("segment 9 should have a truncated keyframe")
	}
	if fx.Segments[3].DeclaredStart >= 3*SegmentDurationTicks {
		t.Fatal("segment 3 should regress")
	}
	// Indexed payload sums to exactly 2280 kbps over 20 s.
	var bytes, ticks int64
	for _, s := range fx.Segments {
		if !s.Missing && !s.TruncatedFirst {
			bytes += s.PayloadBytes
			ticks += SegmentDurationTicks
		}
	}
	kbps := bytes * 8 * 90000 / (ticks * 1000)
	if kbps != 2280 {
		t.Fatalf("actual bitrate %d want 2280", kbps)
	}
}

func TestGenerateTruncatedCase(t *testing.T) {
	fx, err := Generate(41, "truncated")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range fx.Segments {
		if s.TruncatedFirst {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("truncated segments %d want 4", n)
	}
	if fx.Segments[6].DeclaredStart != 6*SegmentDurationTicks+7200 {
		t.Fatalf("segment 6 misalignment wrong: %d", fx.Segments[6].DeclaredStart)
	}
	if _, err := Generate(41, "bogus"); err == nil {
		t.Fatal("unknown case accepted")
	}
}

func TestGeneratedManifestsParse(t *testing.T) {
	fx, err := Generate(41, "default")
	if err != nil {
		t.Fatal(err)
	}
	pl, err := manifest.ParseHLS(fx.HLS)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Segments) != SegmentCount {
		t.Fatalf("hls segments %d", len(pl.Segments))
	}
	mpd, err := manifest.ParseDASH(fx.MPD)
	if err != nil {
		t.Fatal(err)
	}
	if mpd.SegmentCount != SegmentCount || len(mpd.Tracks) != 2 {
		t.Fatalf("mpd %+v", mpd)
	}
	ts, err := manifest.ProbeTimescale(bytes.NewReader(fx.Init))
	if err != nil || ts != 90000 {
		t.Fatalf("timescale %d err %v", ts, err)
	}
	if _, err := manifest.Merge(mpd, manifest.AlignVideo(pl)); err != nil {
		t.Fatal(err)
	}
}
