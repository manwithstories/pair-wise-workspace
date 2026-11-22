package model

import (
	"strings"
	"testing"

	"mediad/internal/clock"
)

func TestManifestRoundtripPreservesFields(t *testing.T) {
	m := &Manifest{
		Version: 1,
		InputID: "in-42",
		Sources: []Source{{
			ID: "src-1", Kind: KindMP4, Path: "a/b.mp4",
			Digest: "aa", RTPClock: clock.MustNew(90000, 1),
		}},
		Tracks: []Track{{
			ID: "tr-1", SourceID: "src-1", Handler: "vide",
			Timebase: clock.MustNew(24000, 1001), Timescale: 24000,
		}},
		Samples: []Sample{{
			TrackID: "tr-1", Index: 1, PTS: -120, Duration: 100,
			Offset: 4096, Size: 512, Keyframe: true, Complete: true,
		}},
		Segments: []Segment{{
			ID: "seg-1", TrackID: "tr-1", StartTick: -120, EndTick: 880,
			Refs: []SourceRef{{
				SourceID: "src-1", TrackID: "tr-1",
				FirstSample: 1, LastSample: 10, StartTick: -120, EndTick: 880,
			}},
			Status: StatusIncomplete, ByteLen: 4096,
			ContentDigest: "cafe", SourceDigest: "beef",
		}},
	}
	out, err := m.Roundtrip()
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	if out.InputID != m.InputID || out.Version != m.Version {
		t.Fatal("scalar fields changed")
	}
	if !out.Tracks[0].Timebase.Equal(m.Tracks[0].Timebase) {
		t.Fatalf("timebase changed: %+v", out.Tracks[0].Timebase)
	}
	if out.Samples[0].PTS != -120 || !out.Samples[0].Keyframe {
		t.Fatalf("sample fields changed: %+v", out.Samples[0])
	}
	if out.Segments[0].Refs[0].LastSample != 10 || out.Segments[0].Status != StatusIncomplete {
		t.Fatalf("segment/ref fields changed: %+v", out.Segments[0])
	}
	if got := out.SegmentByID("seg-1"); got == nil || got.ContentDigest != "cafe" {
		t.Fatal("SegmentByID lookup failed")
	}
	if got := out.SegmentByID("missing"); got != nil {
		t.Fatal("missing lookup must return nil")
	}
	if got := out.TrackByID("tr-1"); got == nil || got.Timescale != 24000 {
		t.Fatal("TrackByID lookup failed")
	}
}

func TestValidateSegmentBytes(t *testing.T) {
	if err := ValidateSegmentBytes(MaxSegmentBytes); err != nil {
		t.Fatalf("boundary must be accepted: %v", err)
	}
	err := ValidateSegmentBytes(MaxSegmentBytes + 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize not rejected: %v", err)
	}
	if err := ValidateSegmentBytes(-1); err == nil {
		t.Fatal("negative size must be rejected")
	}
}

func TestSegmentComplete(t *testing.T) {
	for _, st := range []string{StatusIncomplete, StatusCorrupt, StatusTruncated} {
		s := Segment{Status: st}
		if s.Complete() {
			t.Fatalf("%s segment must not be complete", st)
		}
	}
	ok := Segment{Status: StatusOK}
	if !ok.Complete() {
		t.Fatal("ok segment must be complete")
	}
}
