package metadata

import (
	"strings"
	"testing"

	"mediad/internal/clock"
)

func TestNormalizeRotation(t *testing.T) {
	cases := []struct {
		in   float64
		want int
	}{
		{0, 0}, {44, 0}, {46, 90}, {90, 90}, {135, 180}, {181, 180},
		{269, 270}, {270, 270}, {315, 0}, {-90, 270}, {450, 90},
	}
	for _, c := range cases {
		got, raw := NormalizeRotation(c.in)
		if got != c.want {
			t.Fatalf("NormalizeRotation(%v)=%d want %d", c.in, got, c.want)
		}
		if raw < 0 || raw >= 360 {
			t.Fatalf("raw %v not normalized to [0,360)", raw)
		}
	}
}

func TestMatrixRotation(t *testing.T) {
	// Identity.
	if got := MatrixRotation([9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}); got != 0 {
		t.Fatalf("identity = %v", got)
	}
	// 90 degrees clockwise display transform.
	if got := MatrixRotation([9]int32{0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000}); got != 90 {
		t.Fatalf("90 = %v", got)
	}
	// 180 degrees.
	if got := MatrixRotation([9]int32{-0x10000, 0, 0, 0, -0x10000, 0, 0, 0, 0x40000000}); got != 180 {
		t.Fatalf("180 = %v", got)
	}
}

func TestTrackOutputDimensions(t *testing.T) {
	tr := Track{CodedWidth: 1920, CodedHeight: 1080}
	if tr.OutputWidth() != 1920 || tr.OutputHeight() != 1080 {
		t.Fatal("unrotated")
	}
	tr.Rotation = 90
	if tr.OutputWidth() != 1080 || tr.OutputHeight() != 1920 {
		t.Fatal("rotated 90")
	}
	tr.Crop = &Crop{X: 0, Y: 0, W: 1440, H: 1080}
	if tr.OutputWidth() != 1080 || tr.OutputHeight() != 1440 {
		t.Fatal("cropped+rotated")
	}
}

func TestValidateCropReportsTrackAndInterval(t *testing.T) {
	tr := Track{Key: "v1", CodedWidth: 640, CodedHeight: 360,
		Timebase: clock.Video90k, Start: 1800, Duration: 900,
		Crop: &Crop{X: 100, Y: 0, W: 600, H: 360}}
	err := ValidateCrop(tr)
	if err == nil {
		t.Fatal("expected crop error")
	}
	if err.Track != "v1" || err.Interval == nil || err.Interval.Start != 1800 {
		t.Fatalf("missing provenance: %+v", err)
	}
	tr.Crop = &Crop{X: 0, Y: 0, W: 640, H: 360}
	if err := ValidateCrop(tr); err != nil {
		t.Fatalf("valid crop rejected: %v", err)
	}
}

func TestResolveColorPriorityAndConflicts(t *testing.T) {
	tracks := []Track{
		{Key: "v0", Kind: KindVideo, Color: NewColor(PrimariesBT709, TransferBT709, MatrixBT709, false, SourceDefault)},
		{Key: "v1", Kind: KindVideo, Color: NewColor(PrimariesBT2020, TransferPQ, MatrixBT2020, true, SourceColr)},
		{Key: "v2", Kind: KindVideo, Color: NewColor(PrimariesP3, TransferSRGB, MatrixBT709, false, SourceColr)},
	}
	got := ResolveColor(tracks)
	if got.Primaries != PrimariesBT2020 || !got.FullRange {
		t.Fatalf("wrong priority winner: %+v", got)
	}
	if len(got.Conflicts) == 0 || !strings.Contains(strings.Join(got.Conflicts, ";"), "v2") {
		t.Fatalf("conflicts not recorded: %+v", got.Conflicts)
	}
}

func TestNewColorRecordsDefaults(t *testing.T) {
	c := NewColor(PrimariesBT709, 0, 0, false, SourceColr)
	if !c.Defaulted || c.Transfer != TransferBT709 || c.Matrix != MatrixBT709 {
		t.Fatalf("default not recorded: %+v", c)
	}
	if c.Source != SourceColr {
		t.Fatal("source lost")
	}
}

func TestFrameTicks(t *testing.T) {
	tr := Track{Timebase: clock.MustNew(1, 30000)}
	if got := tr.FrameTicks(30000, 1001); got != 1001 {
		t.Fatalf("29.97 frame ticks = %d", got)
	}
	tr.Timebase = clock.MustNew(1, 90000)
	if got := tr.FrameTicks(25, 1); got != 3600 {
		t.Fatalf("25fps ticks = %d", got)
	}
}
