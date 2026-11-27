package clock

import (
	"math"
	"testing"
)

func TestDriftEstimatorNominal(t *testing.T) {
	// 90kHz video: 90 ticks per master millisecond.
	d := NewDriftEstimator(90000, 1000)
	master := int64(0)
	ticks := uint64(0)
	for i := 0; i < 6; i++ {
		master += 100
		ticks += 9000 // exactly nominal
		conf, err := d.Observe(master, ticks)
		if err != nil || conf == Rejected {
			t.Fatalf("nominal observe %d: conf=%v err=%v", i, conf, err)
		}
	}
	if math.Abs(d.DriftPPM) > 1.0 {
		t.Fatalf("nominal drift = %f want ~0", d.DriftPPM)
	}
}

func TestDriftEstimatorExceedsBound(t *testing.T) {
	d := NewDriftEstimator(90000, 1000)
	d.Observe(0, 0)
	// 5% fast = 50000 ppm, far over the 250 ppm limit.
	conf, err := d.Observe(1000, 94500)
	if err == nil || conf != Rejected {
		t.Fatalf("excess drift must reject: conf=%v err=%v", conf, err)
	}
	if !HasKind(err, ErrDriftExceeded) {
		t.Fatalf("want ErrDriftExceeded, got %v", err)
	}
}

func TestDriftEstimatorRollback(t *testing.T) {
	d := NewDriftEstimator(48000, 1000)
	d.Observe(0, 0)
	d.Observe(100, 4800)
	conf, err := d.Observe(200, 4000) // slave ticks decrease
	if err == nil || conf != Rejected {
		t.Fatalf("rollback must reject: conf=%v err=%v", conf, err)
	}
	if !HasKind(err, ErrTfdtRollback) {
		t.Fatalf("want ErrTfdtRollback, got %v", err)
	}
}

func TestDriftEstimatorSmallDriftIsUncertain(t *testing.T) {
	d := NewDriftEstimator(90000, 1000)
	d.Observe(0, 0)
	// 50 ppm fast stays inside the 250 ppm bound but is no longer ideal:
	// 900000 nominal ticks + 45 extra over 10000 master ms.
	conf, err := d.Observe(10000, 900000+45)
	if err != nil || conf == Rejected {
		t.Fatalf("in-bound drift: conf=%v err=%v", conf, err)
	}
}
