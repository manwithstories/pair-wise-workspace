package adapter

import (
	"context"
	"errors"
	"io"
	"testing"

	"mediacore/internal/media"
)

var errEOF = io.EOF

func TestGeneratedMP4RoundTrips(t *testing.T) {
	layout, err := BuildMP4(MP4Spec{
		Tracks: 3, Samples: 40, Seed: 7,
		Color: &MediaColor{Primaries: 9, Transfer: 16, Matrix: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&MP4Adapter{}).Parse(context.Background(), NewLogicalSource(layout, "probe.mp4"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(c.Tracks))
	}
	for _, tr := range c.Tracks {
		// Tracks are reported in sorted order; map back through the stable
		// identifier to the generator's ordinal.
		ti := int(tr.ID.Value) - 1
		if tr.SampleCount() != 40 {
			t.Fatalf("track %s samples=%d", tr.ID, tr.SampleCount())
		}
		for _, i := range []int{0, 17, 39} {
			s, err := tr.Index.At(i)
			if err != nil {
				t.Fatal(err)
			}
			if s.Offset != layout.SampleOffsets[ti][i] || s.Size != int64(layout.SampleSizes[ti][i]) {
				t.Fatalf("track %s sample %d off=%d want %d size=%d want %d",
					tr.ID, i, s.Offset, layout.SampleOffsets[ti][i], s.Size, layout.SampleSizes[ti][i])
			}
		}
	}
	for _, tr := range c.Tracks {
		got := tr.Color
		if tr.Type == media.MediaVideo && (!got.Present || got.Primaries != 9 || got.Matrix != 9) {
			t.Fatalf("track %s color=%v", tr.ID, got)
		}
		if tr.Type == media.MediaAudio && got.Present {
			t.Fatalf("audio track %s unexpectedly has colour %v", tr.ID, got)
		}
	}
	if c.Fingerprint == "" {
		t.Fatal("empty fingerprint")
	}
	// The generated sample entry must be copied verbatim for the muxer.
	for _, tr := range c.Tracks {
		if len(tr.SampleEntry) == 0 {
			t.Fatalf("track %s has no sample entry", tr.ID)
		}
	}
}

func TestGeneratedMXFRoundTrips(t *testing.T) {
	layout, err := BuildMXF(MXFGenerator{Tracks: 3, Samples: 25, UnknownKLV: 2})
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&MXFAdapter{}).Parse(context.Background(), NewMixedSource(layout, nil, "probe.mxf"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(c.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(c.Tracks))
	}
	if c.UnknownBoxes != 2 {
		t.Fatalf("unknown=%d", c.UnknownBoxes)
	}
	for _, tr := range c.Tracks {
		if tr.SampleCount() != 25 {
			t.Fatalf("track %s samples=%d", tr.ID, tr.SampleCount())
		}
		s, err := tr.Index.At(3)
		if err != nil {
			t.Fatal(err)
		}
		want := layout.SampleOffsets[tr.ID.Value][3]
		if s.Offset != want || s.Size != int64(layout.SampleSizes[tr.ID.Value][3]) {
			t.Fatalf("track %s sample 3 off=%d want %d size=%d", tr.ID, s.Offset, want, s.Size)
		}
	}
}

// TestMP4TruncationReportsExactOffset covers the required truncation fault:
// the reported offset must be the first byte the input could not supply.
func TestMP4TruncationReportsExactOffset(t *testing.T) {
	layout, err := BuildMP4(MP4Spec{Tracks: 2, Samples: 20, Corrupt: "truncate", Offset: 4096})
	if err != nil {
		t.Fatal(err)
	}
	src := &truncatingSource{layout: layout, size: layout.TruncateAt}
	_, err = (&MP4Adapter{}).Parse(context.Background(), src)
	if err == nil {
		t.Fatal("expected truncation")
	}
	if !errors.Is(err, media.ErrTruncated) {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
	if off := media.OffsetOf(err); off != 4096 {
		t.Fatalf("offset=%d want 4096", off)
	}
}

// TestMP4InvalidSampleLengthReportsTableOffset covers the required illegal
// sample length fault: it must be caught from the table, before the essence
// range is trusted.
func TestMP4InvalidSampleLengthReportsTableOffset(t *testing.T) {
	layout, err := BuildMP4(MP4Spec{Tracks: 2, Samples: 20, Corrupt: "sample_length", Offset: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&MP4Adapter{}).Parse(context.Background(), NewLogicalSource(layout, "bad.mp4"))
	if err == nil {
		t.Fatal("expected invalid length")
	}
	if !errors.Is(err, media.ErrInvalidLength) {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
	if media.OffsetOf(err) <= 0 {
		t.Fatalf("no table offset reported: %v", err)
	}
	e := err.(*media.Error)
	if !e.HasTrack {
		t.Fatal("no track identity reported")
	}
}

func TestMP4UnknownBoxesAreCounted(t *testing.T) {
	layout, err := BuildMP4(MP4Spec{Tracks: 1, Samples: 5, UnknownBoxes: 3})
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&MP4Adapter{}).Parse(context.Background(), NewLogicalSource(layout, "skip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if c.UnknownBoxes != 3 {
		t.Fatalf("unknown=%d", c.UnknownBoxes)
	}
}

func TestMP4RejectsUnsupportedCodec(t *testing.T) {
	layout, err := BuildMP4(MP4Spec{Tracks: 1, Samples: 5, Codec: "alac"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&MP4Adapter{}).Parse(context.Background(), NewLogicalSource(layout, "alac.mp4"))
	if !errors.Is(err, media.ErrUnsupportedCodec) {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
}

func TestRegistryDetectsAndPinsVersions(t *testing.T) {
	r := DefaultRegistry()
	mp4Layout, err := BuildMP4(MP4Spec{Tracks: 1, Samples: 2})
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.Detect(NewLogicalSource(mp4Layout, "x.mp4"))
	if err != nil || a.Format() != "mp4" {
		t.Fatalf("detect mp4: %v %v", a, err)
	}
	mxfLayout, err := BuildMXF(MXFGenerator{Tracks: 1, Samples: 2})
	if err != nil {
		t.Fatal(err)
	}
	a, err = r.Detect(NewMixedSource(mxfLayout, nil, "x.mxf"))
	if err != nil || a.Format() != "mxf" {
		t.Fatalf("detect mxf: %v %v", a, err)
	}
	if _, err := r.Select("mp4", "9.9.9"); err == nil {
		t.Fatal("unknown version should not resolve")
	}
	if err := r.Register(&MP4Adapter{}); err == nil {
		t.Fatal("duplicate registration should fail")
	}
}

// TestBuildPlanIsDeterministicAndRejectsDuplicates covers the mapping rules.
func TestBuildPlanIsDeterministicAndRejectsDuplicates(t *testing.T) {
	r := DefaultRegistry()
	ad, _ := r.Select("mp4", "")
	mk := func() *media.Container {
		layout, err := BuildMP4(MP4Spec{Tracks: 3, Samples: 4})
		if err != nil {
			t.Fatal(err)
		}
		c, err := (&MP4Adapter{}).Parse(context.Background(), NewLogicalSource(layout, "same.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first, err := BuildPlan([]*media.Container{mk()}, map[string]Adapter{"mp4": ad})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPlan([]*media.Container{mk()}, map[string]Adapter{"mp4": ad})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sign() != second.Sign() {
		t.Fatalf("plan order is not reproducible:\n%s\n%s", first.Sign(), second.Sign())
	}

	dup := mk()
	dup.Tracks = append(dup.Tracks, dup.Tracks[0])
	_, err = BuildPlan([]*media.Container{dup}, map[string]Adapter{"mp4": ad})
	if !errors.Is(err, media.ErrDuplicateTrackID) {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
}

func TestMXFAmbiguousWrapReportsOffset(t *testing.T) {
	layout, err := BuildMXF(MXFGenerator{Tracks: 1, Samples: 10, AmbiguousWrap: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&MXFAdapter{}).Parse(context.Background(), NewMixedSource(layout, nil, "amb.mxf"))
	if !errors.Is(err, media.ErrTimestampAmbiguous) {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
	if media.OffsetOf(err) <= 0 {
		t.Fatalf("no offset reported: %v", err)
	}
}

func TestMXFWrapExpandsMonotonically(t *testing.T) {
	layout, err := BuildMXF(MXFGenerator{Tracks: 1, Samples: 8, TimestampWrap: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := (&MXFAdapter{}).Parse(context.Background(), NewMixedSource(layout, nil, "wrap.mxf"))
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	var prev media.Rat
	for i := 0; i < tr.SampleCount(); i++ {
		s, err := tr.Index.At(i)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && s.DTS.Cmp(prev) < 0 {
			t.Fatalf("timestamp went backwards at %d", i)
		}
		prev = s.DTS
	}
}

func TestMXFTruncatedKLVReportsOffset(t *testing.T) {
	layout, err := BuildMXF(MXFGenerator{Tracks: 1, Samples: 4, TruncateAt: 200})
	if err != nil {
		t.Fatal(err)
	}
	src := &truncatingSource{size: layout.TruncateAt, mxf: layout}
	_, err = (&MXFAdapter{}).Parse(context.Background(), src)
	if !errors.Is(err, media.ErrTruncated) {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
	if media.OffsetOf(err) != 200 {
		t.Fatalf("offset=%d want 200", media.OffsetOf(err))
	}
}

// truncatingSource reports the given size and serves only those bytes.
type truncatingSource struct {
	layout *MP4Layout
	mxf    *MXFLayout
	size   int64
	inner  *LogicalSource
}

func (s *truncatingSource) ensure() {
	if s.inner == nil {
		s.inner = NewMixedSource(s.mxf, s.layout, "truncated")
	}
}
func (s *truncatingSource) ReadAt(p []byte, off int64) (int, error) {
	s.ensure()
	if off >= s.size {
		return 0, errEOF
	}
	if off+int64(len(p)) > s.size {
		p = p[:s.size-off]
	}
	return s.inner.ReadAt(p, off)
}
func (s *truncatingSource) Size() int64  { return s.size }
func (s *truncatingSource) Name() string { return "truncated" }
