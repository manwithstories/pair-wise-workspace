package transport

import "testing"

func TestPacerStepMappingAndTransitions(t *testing.T) {
	p, f := NewPacer([]int{12000, 4000, 9000}, 120)
	if f != nil {
		t.Fatal(f)
	}
	if r := p.RateKbps(0); r != 12000 {
		t.Fatalf("seg0 rate=%d", r)
	}
	if r := p.RateKbps(60); r != 4000 {
		t.Fatalf("seg60 rate=%d want 4000", r)
	}
	if r := p.RateKbps(119); r != 9000 {
		t.Fatalf("seg119 rate=%d want 9000", r)
	}
	tr := p.Transitions()
	if len(tr) != 2 {
		t.Fatalf("transitions=%+v want 2", tr)
	}
}

func TestPaceInverseToBandwidth(t *testing.T) {
	p, _ := NewPacer([]int{12000, 4000, 9000}, 120)
	fast := p.Pace(0, 1200)
	slow := p.Pace(60, 1200)
	if slow <= fast {
		t.Fatalf("low bandwidth must pace slower: fast=%v slow=%v", fast, slow)
	}
	if p.MediaBitrateKbps(0) >= 12000 || p.MediaBitrateKbps(60) >= 4000 {
		t.Fatal("media bitrate should apply safety factor")
	}
}

func TestPacerValidation(t *testing.T) {
	if _, f := NewPacer(nil, 120); f == nil {
		t.Fatal("empty steps must fail")
	}
	if _, f := NewPacer([]int{1000, 0}, 120); f == nil {
		t.Fatal("zero bandwidth must fail")
	}
	if _, f := NewPacer([]int{1000}, 0); f == nil {
		t.Fatal("zero segments must fail")
	}
	p, _ := NewPacer([]int{1000}, 10)
	if f := p.SetSafetyFactor(120); f == nil {
		t.Fatal("bad safety factor must fail")
	}
}
