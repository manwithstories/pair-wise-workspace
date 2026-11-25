package clock

import (
	"errors"
	"testing"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

func meta(id uint32, ts uint32, drift int32, wrap uint32, start uint64) demux.TrackMeta {
	return demux.TrackMeta{ID: id, Kind: demux.KindVideo, Timescale: ts,
		DriftPPM: drift, WrapUnits: wrap, StartTime: start}
}

func sample(id uint32, seq uint32, dts uint64) demux.Sample {
	return demux.Sample{TrackID: id, Seq: seq, DTS: dts, Duration: 1000, Size: 10}
}

func TestDriftCorrection(t *testing.T) {
	// A corrector built directly has a zero origin. StartTime matches the
	// first DTS so relative time begins at zero.
	cr, err := NewCorrector(meta(1, 1000, 250, 0, 1000), model.Rational{})
	if err != nil {
		t.Fatalf("corrector: %v", err)
	}
	// Zero origin: absolute corrected time = 2000/1000 s * 0.99975
	// = 1.9995 s = 3999/2000.
	got, err := cr.Apply(sample(1, 1, 2000))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := model.NewRational(3999, 2000)
	if got.DTSNorm.Cmp(want) != 0 {
		t.Fatalf("drift: got %s want %s", got.DTSNorm, want)
	}
}

func TestDriftRejectedBeyondThreshold(t *testing.T) {
	cases := []int32{251, -251, 1000}
	for _, drift := range cases {
		if _, err := NewCorrector(meta(1, 1000, drift, 0, 0), model.Rational{}); err == nil {
			t.Fatalf("drift %d ppm must be rejected", drift)
		}
	}
	// Boundary is inclusive: exactly +/-250 ppm is acceptable.
	for _, drift := range []int32{250, -250} {
		if _, err := NewCorrector(meta(1, 1000, drift, 0, 0), model.Rational{}); err != nil {
			t.Fatalf("drift %d ppm at boundary rejected: %v", drift, err)
		}
	}
}

func TestWraparoundAccepted(t *testing.T) {
	cr, _ := NewCorrector(meta(1, 1000, 0, 1000, 0), model.Rational{})
	// Upper-half (900 >= 500) then lower-half (50 < 500) backward jump is an
	// unambiguous single wrap.
	first, err := cr.Apply(sample(1, 1, 900))
	if err != nil {
		t.Fatal(err)
	}
	second, err := cr.Apply(sample(1, 2, 50))
	if err != nil {
		t.Fatalf("wrap rejected: %v", err)
	}
	if second.WrapCount != 1 {
		t.Fatalf("wrap count = %d, want 1", second.WrapCount)
	}
	// Unwrapped time must keep advancing past the period.
	if second.DTSNorm.Cmp(first.DTSNorm) <= 0 {
		t.Fatalf("time did not advance after wrap: %s <= %s",
			second.DTSNorm, first.DTSNorm)
	}
}

func TestAmbiguousWrapRejected(t *testing.T) {
	cr, _ := NewCorrector(meta(1, 1000, 0, 1000, 0), model.Rational{})
	if _, err := cr.Apply(sample(1, 1, 200)); err != nil {
		t.Fatal(err)
	}
	// Jumping from the lower half to a still-low value cannot be a clean wrap.
	_, err := cr.Apply(sample(1, 2, 100))
	var me *model.Error
	if !errors.As(err, &me) || me.Stage != model.StageClock {
		t.Fatalf("ambiguous wrap must yield clock error, got %v", err)
	}
}

func TestBackwardWithoutWrapPeriodRejected(t *testing.T) {
	cr, _ := NewCorrector(meta(1, 1000, 0, 0, 0), model.Rational{})
	if _, err := cr.Apply(sample(1, 1, 500)); err != nil {
		t.Fatal(err)
	}
	if _, err := cr.Apply(sample(1, 2, 400)); err == nil {
		t.Fatal("backward timestamp without wrap period must fail")
	}
}

func TestSharedOriginAlignsNonZeroStarts(t *testing.T) {
	tracks := []demux.TrackMeta{
		{ID: 1, Kind: demux.KindVideo, Timescale: 1000, StartTime: 100},
		{ID: 2, Kind: demux.KindAudio, Timescale: 1000, StartTime: 200},
	}
	n, err := NewNormalizer(tracks, []uint32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	a, err := n.Apply(sample(1, 1, 100))
	if err != nil {
		t.Fatal(err)
	}
	// Earliest start (track 1, 0.1s) is the origin, so its first DTS is 0.
	if !a.DTSNorm.IsZero() {
		t.Fatalf("origin alignment: got %s want 0", a.DTSNorm)
	}
	b, err := n.Apply(sample(2, 1, 200))
	if err != nil {
		t.Fatal(err)
	}
	if b.DTSNorm.Cmp(model.NewRational(1, 10)) != 0 {
		t.Fatalf("track 2 start offset: got %s want 0.1", b.DTSNorm)
	}
}

func TestVFRDurationsCorrectedIndependently(t *testing.T) {
	cr, _ := NewCorrector(meta(1, 1000, 0, 0, 0), model.Rational{})
	s := sample(1, 1, 0)
	s.Duration = 2500 // 2.5 seconds, independent of DTS
	got, err := cr.Apply(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.DurNorm.Cmp(model.NewRational(5, 2)) != 0 {
		t.Fatalf("vfr duration: got %s want 5/2", got.DurNorm)
	}
}
