package demux

import (
	"bytes"
	"testing"

	"mediacore/internal/fault"
	"mediacore/internal/fixture"
	"mediacore/internal/sample"
)

func openArtifact(t *testing.T, art *fixture.Artifact) *Container {
	t.Helper()
	r, err := NewReader(bytes.NewReader(art.Bytes), DefaultLimits())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	c, err := r.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

func TestOpenBothVariants(t *testing.T) {
	wantCodec := []sample.Codec{sample.CodecH264, sample.CodecAAC, sample.CodecOpus}
	wantScale := []uint32{90000, 44100, 48000}
	for variant := 0; variant < 2; variant++ {
		art, err := fixture.Build(fixture.DefaultSpec(42, variant))
		if err != nil {
			t.Fatal(err)
		}
		c := openArtifact(t, art)
		if len(c.Tracks) != 3 {
			t.Fatalf("variant %d: got %d tracks", variant, len(c.Tracks))
		}
		for i, tm := range c.Tracks {
			if tm.Codec != wantCodec[i] || tm.Timescale != wantScale[i] {
				t.Fatalf("variant %d track %d: %+v", variant, i, tm)
			}
			if tm.Samples != 24 || tm.TrackID != uint32(i+1) {
				t.Fatalf("variant %d track %d: id=%d samples=%d",
					variant, i, tm.TrackID, tm.Samples)
			}
			// Offsets must equal the fixture ground truth exactly.
			for k, want := range art.Tracks[i].SampleOffsets {
				if tm.chunkOff[k] != want {
					t.Fatalf("variant %d track %d sample %d offset %d != truth %d",
						variant, i, k, tm.chunkOff[k], want)
				}
			}
		}
	}
}

func TestReadSamples(t *testing.T) {
	art, _ := fixture.Build(fixture.DefaultSpec(42, 0))
	c := openArtifact(t, art)
	all, faults, err := c.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(faults) != 0 {
		t.Fatalf("clean input must have no faults, got %v", faults)
	}
	for i, ss := range all {
		if len(ss) != 24 {
			t.Fatalf("track %d samples=%d", i, len(ss))
		}
		var prev int64 = -1
		for k, s := range ss {
			if s.PTS < 0 || int64(s.PTS) <= prev {
				t.Fatalf("non-monotonic PTS track %d sample %d", i, k)
			}
			prev = int64(s.PTS)
			if len(s.Data) != art.Tracks[i].SampleSizes[k] {
				t.Fatalf("compressed bytes must be preserved (track %d sample %d): got %d want %d",
					i, k, len(s.Data), art.Tracks[i].SampleSizes[k])
			}
			if s.Offset != art.Tracks[i].SampleOffsets[k] {
				t.Fatalf("sample offset drift")
			}
		}
	}
}

func TestMissingIndexRebuild(t *testing.T) {
	good, _ := fixture.Build(fixture.DefaultSpec(42, 1))
	spec := fixture.DefaultSpec(42, 1)
	spec.Mode = fixture.ModeMissingIndex
	broken, _ := fixture.Build(spec)

	cGood := openArtifact(t, good)
	cBad := openArtifact(t, broken)
	if !cBad.MissingIndex[0] && len(cBad.Tracks) != 3 {
		t.Fatal("rebuilt container must still expose 3 tracks")
	}
	a, _, err := cGood.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	b, f2, err := cBad.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(f2) != 0 {
		t.Fatalf("rebuilt index must not produce sample faults: %v", f2)
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			t.Fatalf("rebuilt sample count mismatch track %d", i)
		}
		for k := range a[i] {
			if a[i][k].PTS != b[i][k].PTS || !bytes.Equal(a[i][k].Data, b[i][k].Data) {
				t.Fatalf("rebuilt samples differ at track %d sample %d", i, k)
			}
		}
	}
}

func TestCorruptSampleOffset(t *testing.T) {
	spec := fixture.DefaultSpec(42, 0)
	spec.Mode = fixture.ModeCorruptSample
	art, _ := fixture.Build(spec)
	c := openArtifact(t, art)
	_, faults, err := c.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	f := fault.First(faults)
	if f == nil || f.Kind != fault.KindCorruptSample || !f.Recoverable() {
		t.Fatalf("want recoverable corrupt fault, got %v", f)
	}
	if diff := f.Offset - art.CorruptOffset; diff < -16 || diff > 16 {
		t.Fatalf("localization error %d bytes exceeds 16 (reported=%d truth=%d)",
			diff, f.Offset, art.CorruptOffset)
	}
}

func TestTruncatedTail(t *testing.T) {
	spec := fixture.DefaultSpec(42, 0)
	spec.Mode = fixture.ModeTruncatedTail
	art, _ := fixture.Build(spec)
	c := openArtifact(t, art)
	_, faults, err := c.ReadAll()
	if err != nil {
		t.Fatalf("tail truncation must stay recoverable: %v", err)
	}
	f := fault.First(faults)
	if f == nil || f.Strategy != fault.SkipFragment {
		t.Fatalf("want skip-fragment fault, got %v", f)
	}
	if f.Offset < 0 || f.Offset > int64(len(art.Bytes)) {
		t.Fatalf("fault offset outside file: %d", f.Offset)
	}
}

func TestIndexOutOfRangeAborts(t *testing.T) {
	spec := fixture.DefaultSpec(42, 0)
	spec.Mode = fixture.ModeIndexOutOfRange
	art, _ := fixture.Build(spec)
	r, _ := NewReader(bytes.NewReader(art.Bytes), DefaultLimits())
	_, err := r.Open()
	f := fault.As(err)
	if f == nil || f.Kind != fault.KindIndexOutOfRange || f.Recoverable() {
		t.Fatalf("want aborting out-of-range fault, got %v", err)
	}
}

func TestUnknownCodecAborts(t *testing.T) {
	spec := fixture.DefaultSpec(42, 0)
	spec.Tracks = append(spec.Tracks, fixture.TrackSpec{
		Codec: "zzzZ", Timescale: 48000, Samples: 4, Delta: 960,
	})
	art, err := fixture.Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := NewReader(bytes.NewReader(art.Bytes), DefaultLimits())
	_, err = r.Open()
	f := fault.As(err)
	if f == nil || f.Kind != fault.KindUnknownCodec || f.Offset < 0 {
		t.Fatalf("want offset-bearing unknown-codec fault, got %v", err)
	}
}

func TestTruncatedHeaderAborts(t *testing.T) {
	spec := fixture.DefaultSpec(42, 0)
	spec.Mode = fixture.ModeTruncatedHeader
	art, _ := fixture.Build(spec)
	r, _ := NewReader(bytes.NewReader(art.Bytes), DefaultLimits())
	_, err := r.Open()
	if f := fault.As(err); f == nil || f.Kind != fault.KindTruncatedHeader {
		t.Fatalf("want truncated-header fault, got %v", err)
	}
}
