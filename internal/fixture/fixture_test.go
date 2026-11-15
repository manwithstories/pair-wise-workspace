package fixture

import "testing"

func TestGenerate(t *testing.T) {
	f, err := Generate(Options{Periods: 2, GOPMS: 2002, DurationMS: 120000, Timescale: 90000})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Reps) != 7 {
		t.Fatalf("reps=%d want 7", len(f.Reps))
	}
	total := 0
	for _, r := range f.Reps {
		if r.PTOTicks != 3003 {
			t.Fatalf("rep %s pto=%d want 3003", r.ID, r.PTOTicks)
		}
		for _, p := range r.Periods {
			total += len(p.Segments)
		}
	}
	if total != 420 {
		t.Fatalf("segments_total=%d want 420", total)
	}
	p0 := f.Reps[0].Periods[0].Segments
	if p0[0].T != 3003 || p0[0].D != 180180 {
		t.Fatalf("preroll first segment: %+v", p0[0])
	}
	last := p0[len(p0)-1]
	if last.D >= 180180 {
		t.Fatalf("tail should be short of a GOP: %+v", last)
	}
	if f.Reps[0].Periods[1].StartMS != 60000 {
		t.Fatalf("period 1 start=%d", f.Reps[0].Periods[1].StartMS)
	}
}

func TestAnomalies(t *testing.T) {
	gen := func(a string) *File {
		f, err := Generate(Options{Periods: 2, GOPMS: 2002, DurationMS: 120000, Timescale: 90000, Anomaly: a})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	base := gen("")
	if got := len(gen("missing").Reps[0].Periods[0].Segments); got != len(base.Reps[0].Periods[0].Segments)-1 {
		t.Fatal("missing anomaly did not drop a segment")
	}
	z := gen("zerodur").Reps[0].Periods[0].Segments
	zero := false
	for _, s := range z {
		if s.D == 0 {
			zero = true
		}
	}
	if !zero {
		t.Fatal("zerodur anomaly missing")
	}
	po := gen("periodorder").Reps[0].Periods
	if po[0].StartMS <= po[1].StartMS {
		t.Fatal("periodorder anomaly missing")
	}
}
