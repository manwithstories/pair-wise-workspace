package lab

import (
	"context"
	"reflect"
	"testing"
)

func TestRepeatedRunsIdentical(t *testing.T) {
	cfg := Config{
		Seed: 17, Workers: 4, Queue: 64, Packets: 4096, Repeat: 3,
		Thresholds: DefaultThresholds,
	}
	m1, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m1, m2) {
		t.Fatalf("metrics differ across runs:\n%+v\n%+v", m1, m2)
	}
	if !m1.RepeatHashEqual {
		t.Fatal("repeat hash equality lost")
	}
}

func TestDifferentSeedsChangeDriftButStayValid(t *testing.T) {
	base := Config{
		Workers: 4, Queue: 64, Packets: 4096, Repeat: 2,
		Thresholds: DefaultThresholds,
	}
	a, err := Run(context.Background(), withSeed(base, 17))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(context.Background(), withSeed(base, 99))
	if err != nil {
		t.Fatal(err)
	}
	if a.AVDriftP95MS == b.AVDriftP95MS {
		t.Fatal("different seeds produced identical drift quantiles")
	}
	if len(a.Failures) != 0 || len(b.Failures) != 0 {
		t.Fatalf("threshold failures: %v / %v", a.Failures, b.Failures)
	}
}

func withSeed(c Config, seed uint64) Config {
	c.Seed = seed
	return c
}
