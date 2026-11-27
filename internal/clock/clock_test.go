package clock

import (
	"errors"
	"math"
	"testing"

	"fieldcut/internal/media"
)

const fullRange = 1 << 32

func TestUnwrapSingleWrap(t *testing.T) {
	in := []Sample{
		{Seq: 0, Tick: 4294967200, Nanos: 0},
		{Seq: 1, Tick: 400, Nanos: 40_000},
		{Seq: 2, Tick: 800, Nanos: 80_000},
	}
	out, err := Unwrap("A", in, fullRange)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	want := []int64{4294967200, 400, 800}
	if len(out) != len(want) {
		t.Fatalf("got %d samples", len(out))
	}
	if out[1].Unwrapped != 400+fullRange || out[2].Unwrapped != 800+fullRange {
		t.Fatalf("unwrapped = %d, %d", out[1].Unwrapped, out[2].Unwrapped)
	}
}

func TestUnwrapRejectsAmbiguity(t *testing.T) {
	cases := []struct {
		name string
		in   []Sample
	}{
		{"forward-jump", []Sample{{Seq: 0, Tick: 100}, {Seq: 1, Tick: 100 + fullRange/2 + 10}}},
		{"backward-drop", []Sample{{Seq: 0, Tick: 2000}, {Seq: 1, Tick: 100}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Unwrap("cam", tc.in, fullRange)
			if err == nil {
				t.Fatalf("expected ambiguity error")
			}
			var me *media.MediaError
			if !errors.As(err, &me) {
				t.Fatalf("not MediaError: %v", err)
			}
			if me.Kind != media.KindClock || me.Track != "cam" || !me.HasSeq || me.Seq != 1 {
				t.Fatalf("error lacks track/seq: %v", err)
			}
		})
	}
}

func TestUnwrapRejectsRepeatedTick(t *testing.T) {
	in := []Sample{{Seq: 0, Tick: 50}, {Seq: 1, Tick: 50}}
	if _, err := Unwrap("A", in, fullRange); err == nil {
		t.Fatalf("repeated tick accepted")
	}
}

func TestFitDriftRecoversPPM(t *testing.T) {
	nominal := media.TickBase
	const ppm = 120.0
	slope := nominal.Float() * (1 + ppm*1e-6)
	var anchors []Marker
	for i := 0; i < 50; i++ {
		tick := int64(i) * 40_000
		nanos := int64(math.Round(float64(tick) * slope))
		anchors = append(anchors, Marker{Track: "A", Seq: int64(i), Tick: tick, Nanos: nanos})
	}
	f, err := FitDrift(anchors, nominal)
	if err != nil {
		t.Fatalf("FitDrift: %v", err)
	}
	if math.Abs(f.PPM-ppm) > 1.0 {
		t.Fatalf("ppm = %.3f, want ≈ %.1f", f.PPM, ppm)
	}
	if f.MaxErrorMs > 0.001 {
		t.Fatalf("max error %.4f ms too large", f.MaxErrorMs)
	}
}

func TestFitDriftMapsExactly(t *testing.T) {
	nominal := media.TickBase
	f, err := FitDrift([]Marker{
		{Track: "A", Seq: 0, Tick: 0, Nanos: 0},
		{Track: "A", Seq: 1, Tick: 10_000_000, Nanos: 10_000_000},
	}, nominal)
	if err != nil {
		t.Fatal(err)
	}
	got := f.MapWallNanos(50_000_000)
	if got != 50_000_000 {
		t.Fatalf("map = %d, want 50000000", got)
	}
}

func TestFitDriftNeedsTwoAnchors(t *testing.T) {
	_, err := FitDrift([]Marker{{Track: "A", Seq: 0, Tick: 1, Nanos: 1}}, media.TickBase)
	if err == nil {
		t.Fatal("expected error for one anchor")
	}
}
