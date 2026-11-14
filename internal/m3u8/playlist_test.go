package m3u8

import (
	"strings"
	"testing"
)

func TestMediaRoundTrip(t *testing.T) {
	m := &Media{
		Version:        7,
		TargetDuration: 3,
		MediaSequence:  0,
		PlaylistType:   "VOD",
		EndList:        true,
		Segments: []Segment{
			{DurationMs: 2500, URI: "seg_000000.ts", CRC: 0x1a2b3c4d, HasCRC: true},
			{DurationMs: 2000, URI: "gap_000001.ts", Gap: true},
			{DurationMs: 1400, URI: "seg_000002.ts", CRC: 0xdeadbeef, HasCRC: true},
		},
	}
	doc := m.Render()
	got, err := ParseMedia(doc)
	if err != nil {
		t.Fatalf("ParseMedia: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.TargetDuration != 3 || len(got.Segments) != 3 {
		t.Fatalf("got target=%d segments=%d", got.TargetDuration, len(got.Segments))
	}
	if got.Segments[0].CRC != 0x1a2b3c4d || !got.Segments[0].HasCRC {
		t.Fatalf("crc lost: %+v", got.Segments[0])
	}
	if !got.Segments[1].Gap {
		t.Fatalf("gap flag lost: %+v", got.Segments[1])
	}
	if got.Segments[2].DurationMs != 1400 {
		t.Fatalf("duration lost: %+v", got.Segments[2])
	}
	if again := got.Render(); again != doc {
		t.Fatalf("render not stable:\n%s\n---\n%s", doc, again)
	}
}

func TestMediaValidateExtinf(t *testing.T) {
	m := &Media{TargetDuration: 2, Segments: []Segment{{DurationMs: 2001, URI: "a.ts"}}}
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want EXTINF violation, got %v", err)
	}
	m.Segments[0].DurationMs = -1
	if err := m.Validate(); err == nil {
		t.Fatalf("want negative EXTINF violation")
	}
}

func TestUnsafeURI(t *testing.T) {
	for _, u := range []string{"../x.ts", "/abs.ts", "a/../../b.ts", `..\x.ts`} {
		if err := CheckURI(u); err == nil {
			t.Fatalf("uri %q must be rejected", u)
		}
	}
	if err := CheckURI("seg_000001.ts"); err != nil {
		t.Fatalf("safe uri rejected: %v", err)
	}
}

func TestMasterRoundTrip(t *testing.T) {
	m := &Master{Version: 7, Variants: []Variant{
		{Name: "1080p", Bandwidth: 5800000, Width: 1920, Height: 1080, URI: "1080p/index.m3u8"},
		{Name: "240p", Bandwidth: 400000, Width: 426, Height: 240, URI: "240p/index.m3u8"},
	}}
	got, err := ParseMaster(m.Render())
	if err != nil {
		t.Fatalf("ParseMaster: %v", err)
	}
	if len(got.Variants) != 2 || got.Variants[0].Name != "1080p" ||
		got.Variants[0].Bandwidth != 5800000 || got.Variants[1].Height != 240 {
		t.Fatalf("master mismatch: %+v", got.Variants)
	}
}
