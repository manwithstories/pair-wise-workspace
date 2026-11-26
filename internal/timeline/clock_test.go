package timeline

import "testing"

func clock90k(t *testing.T) Clock {
	t.Helper()
	c, err := New(Rational{Num: 1, Den: 90000})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSecondsAndStep(t *testing.T) {
	c := clock90k(t)
	if got := c.Seconds(90000); got != 1.0 {
		t.Fatalf("Seconds(90000)=%v", got)
	}
	step, err := c.StepPTS(Rational{Num: 24, Den: 1})
	if err != nil || step != 3750 {
		t.Fatalf("step=%d err=%v", step, err)
	}
	if _, err := New(Rational{Num: 0, Den: 1}); err == nil {
		t.Fatal("expected error for zero timebase")
	}
	if _, err := c.StepPTS(Rational{Num: 7, Den: 3}); err == nil {
		t.Fatal("expected non-integer step error")
	}
}

func TestCheckMonotonicGapRegression(t *testing.T) {
	c := clock90k(t)
	refs := []FrameRef{{Seq: 0, PTS: 0}, {Seq: 1, PTS: 3750}, {Seq: 3, PTS: 11250}}
	d := c.Check(refs, 3750)
	if len(d) != 1 || d[0].Regression || d[0].Missing != 1 {
		t.Fatalf("gap: %+v", d)
	}
	if d[0].Range.Start != 0.08333333333333333*1.0 && d[0].Range.Start <= 0 {
		t.Fatalf("bad range: %+v", d[0].Range)
	}
	// regression: pts goes backwards
	refs = []FrameRef{{Seq: 0, PTS: 0}, {Seq: 1, PTS: 3750}, {Seq: 2, PTS: 3000}}
	d = c.Check(refs, 3750)
	if len(d) != 1 || !d[0].Regression || d[0].Seq != 2 {
		t.Fatalf("regression: %+v", d)
	}
	if !(d[0].Range.Start < d[0].Range.End) {
		t.Fatalf("range not half-open ordered: %+v", d[0].Range)
	}
	// after a regression the next good frame must not raise a false gap
	refs = []FrameRef{{Seq: 0, PTS: 0}, {Seq: 1, PTS: 3750}, {Seq: 2, PTS: 3000}, {Seq: 3, PTS: 11250}}
	if d = c.Check(refs, 3750); len(d) != 1 {
		t.Fatalf("false gap after regression: %+v", d)
	}
	// clean sequence yields nothing
	refs = []FrameRef{{Seq: 0, PTS: 0}, {Seq: 1, PTS: 3750}, {Seq: 2, PTS: 7500}}
	if d := c.Check(refs, 3750); len(d) != 0 {
		t.Fatalf("clean: %+v", d)
	}
}
