package ingest

import (
	"errors"
	"testing"

	"mediacq/internal/testdata"
)

func TestParseCleanFixture(t *testing.T) {
	f, err := testdata.Build(testdata.DefaultParams(17))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(f.MP4)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(c.Tracks))
	}
	video := c.Tracks[0]
	if video.Kind != "video" || len(video.Samples) != f.Params.VideoFrames {
		t.Fatalf("video track wrong: %+v", video.Kind)
	}
	if len(video.KeyframeIndices) != len(f.ExpectedKeyframes) {
		t.Fatalf("keyframes = %d, want %d",
			len(video.KeyframeIndices), len(f.ExpectedKeyframes))
	}
	// Sample timelines must be strictly monotonic and on the unified axis.
	var prev int64 = -1
	for _, s := range video.Samples {
		if s.Start.Nanoseconds() <= prev {
			t.Fatal("non-monotonic sample timeline")
		}
		if s.Duration <= 0 {
			t.Fatal("non-positive sample duration")
		}
		prev = s.Start.Nanoseconds()
	}
	// Chunk offsets must point inside the declared mdat payload.
	for _, s := range video.Samples {
		if s.FileOffset <= 0 || int64(s.FileOffset)+int64(s.Size) > int64(len(f.MP4)) {
			t.Fatalf("sample offset %d outside container", s.FileOffset)
		}
	}
}

func TestParseVFRPreservesDeltas(t *testing.T) {
	p := testdata.DefaultParams(17)
	p.VFR = true
	f, err := testdata.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(f.MP4)
	if err != nil {
		t.Fatal(err)
	}
	distinct := map[int64]bool{}
	for _, s := range c.Tracks[0].Samples {
		distinct[s.Duration.Nanoseconds()] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("VFR collapsed to %d duration(s)", len(distinct))
	}
}

func TestParseCorruptBoxRejected(t *testing.T) {
	p := testdata.DefaultParams(17)
	p.CorruptBox = true
	f, err := testdata.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(f.MP4)
	if err == nil {
		t.Fatal("corrupt box must be rejected")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %T %v", err, err)
	}
	var be *BoxError
	if !errors.As(err, &be) {
		t.Fatalf("error must be *BoxError: %T", err)
	}
	if be.Offset < 0 || be.Box == "" {
		t.Fatalf("error must locate box/offset: %+v", be)
	}
}

func TestParseTruncatedRejected(t *testing.T) {
	p := testdata.DefaultParams(17)
	p.Truncate = true
	f, err := testdata.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(f.MP4); err == nil {
		t.Fatal("truncated mdat must fail parsing, no fabricated samples")
	}
}

func TestParseMissingKeyframeRejected(t *testing.T) {
	p := testdata.DefaultParams(17)
	p.MissingKey = true
	f, err := testdata.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(f.MP4); !errors.Is(err, ErrMissingKeyframe) {
		t.Fatalf("want ErrMissingKeyframe, got %v", err)
	}
}

func TestMaxKeyframeGap(t *testing.T) {
	f, _ := testdata.Build(testdata.DefaultParams(17))
	c, err := Parse(f.MP4)
	if err != nil {
		t.Fatal(err)
	}
	if g := c.Tracks[0].MaxKeyframeGap(); g != f.Params.KeyframeEvery {
		t.Fatalf("max keyframe gap = %d, want %d", g, f.Params.KeyframeEvery)
	}
}
