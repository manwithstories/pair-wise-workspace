package clock

import (
	"errors"
	"testing"

	"avpipe/internal/media"
)

func newTestUnwrapper(t *testing.T, bits int, threshold int64) *Unwrapper {
	t.Helper()
	cfg := WrapConfig{Bits: bits, Threshold: threshold, MaxWraps: 8}
	u, err := NewUnwrapper(cfg, 0, media.Origin{File: "fixture.mxf"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUnwrapForwardsMonotonic(t *testing.T) {
	u := newTestUnwrapper(t, 33, 0)
	for _, raw := range []int64{0, 100, 200, 300, 400} {
		v, err := u.Value(raw)
		if err != nil {
			t.Fatalf("raw %d: %v", raw, err)
		}
		if v != raw {
			t.Fatalf("raw %d: expanded %d", raw, v)
		}
	}
	if u.Wraps() != 0 {
		t.Fatalf("wraps = %d", u.Wraps())
	}
}

func TestUnwrapRejectsMidFieldRegression(t *testing.T) {
	u := newTestUnwrapper(t, 33, 0)
	if _, err := u.Value(300); err != nil {
		t.Fatal(err)
	}
	_, err := u.Value(200)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if ve.Delta != -100 {
		t.Fatalf("delta %d", ve.Delta)
	}
}

func TestUnwrapExpandsGenuineWrap(t *testing.T) {
	u := newTestUnwrapper(t, 33, 0)
	span := int64(1) << 33
	// Walk just below the top then wrap to a small value.
	values := []int64{span - 3, span - 2, span - 1, 0, 1, 2}
	want := []int64{span - 3, span - 2, span - 1, span, span + 1, span + 2}
	for i, raw := range values {
		v, err := u.Value(raw)
		if err != nil {
			t.Fatalf("raw %d: %v", raw, err)
		}
		if v != want[i] {
			t.Fatalf("raw %d: got %d want %d", raw, v, want[i])
		}
	}
	if u.Wraps() != 1 {
		t.Fatalf("wraps = %d", u.Wraps())
	}
}

func TestUnwrapRejectsHugeBackwardsJump(t *testing.T) {
	u := newTestUnwrapper(t, 33, 0)
	if _, err := u.Value(1000); err != nil {
		t.Fatal(err)
	}
	_, err := u.Value(100)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if ve.Delta != -900 {
		t.Fatalf("delta %d", ve.Delta)
	}
	if media.KindOf(err) != media.KindInternal {
		// Raw ValidationError is not a *media.Error, so the mapping is internal;
		// the caller must wrap it. This asserts that default is documented.
		t.Fatalf("kind = %v", media.KindOf(err))
	}
}

func TestUnwrapRejectsOutOfRangeRaw(t *testing.T) {
	u := newTestUnwrapper(t, 33, 0)
	if _, err := u.Value(int64(1) << 34); err == nil {
		t.Fatal("expected out-of-range error")
	}
	if _, err := u.Value(int64(1) << 34); media.KindOf(err) != media.KindCorrupt {
		t.Fatalf("expected corrupt, got %v", media.KindOf(err))
	}
}

func TestUnwrapThresholdBoundary(t *testing.T) {
	cfg := WrapConfig{Bits: 33, Threshold: 500, MaxWraps: 8}
	u, err := NewUnwrapper(cfg, 0, media.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	span := int64(1) << 33
	// Previous raw is exactly 500 below the top of the field: accepted.
	if _, err := u.Value(span - 501); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Value(1); err != nil {
		t.Fatalf("exactly-threshold wrap rejected: %v", err)
	}
	// Previous raw 501 below the top: rejected as mid-field regression.
	u2, _ := NewUnwrapper(cfg, 0, media.Origin{})
	if _, err := u2.Value(span - 502); err != nil {
		t.Fatal(err)
	}
	if _, err := u2.Value(1); err == nil {
		t.Fatal("over-threshold jump accepted")
	}
}

func TestUnwrapRejectsFarWrapDestination(t *testing.T) {
	cfg := WrapConfig{Bits: 33, Threshold: 500, MaxWraps: 8}
	u, err := NewUnwrapper(cfg, 0, media.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	span := int64(1) << 33
	if _, err := u.Value(span - 501); err != nil {
		t.Fatal(err)
	}
	// Lands at 900 after a wrap: too far from zero to be a wrap.
	if _, err := u.Value(900); err == nil {
		t.Fatal("far wrap destination accepted")
	}
}

func TestUnwrapMaxWrapLimit(t *testing.T) {
	cfg := WrapConfig{Bits: 8, MaxWraps: 2, Threshold: 0}
	u, err := NewUnwrapper(cfg, 0, media.Origin{})
	if err != nil {
		t.Fatal(err)
	}
	max := int64(255)
	for i := 0; i < 3; i++ {
		if _, err := u.Value(max); err != nil {
			t.Fatalf("wrap %d: %v", i, err)
		}
		if _, err := u.Value(0); err != nil {
			if i < 2 {
				t.Fatalf("wrap %d: %v", i, err)
			} else if media.KindOf(err) != media.KindPolicy {
				t.Fatalf("third wrap kind: %v", media.KindOf(err))
			}
			return
		}
	}
}

func TestDriftWithinTolerance(t *testing.T) {
	m, err := NewMonitor(DriftConfig{Tolerance: media.MustRat(1, 20), JumpFactor: 0, Window: 8})
	if err != nil {
		t.Fatal(err)
	}
	base := media.MustRat(1, 30)
	for i := int64(0); i < 10; i++ {
		d, err := m.Observe(Observation{
			Reference: media.MustTime(i*1000, base),
			Subject:   media.MustTime(i*1000+5, base), // 5 ticks = 1/6 s offset
		})
		if err != nil {
			t.Fatal(err)
		}
		if d.Class != DriftNone {
			t.Fatalf("unexpected class %v at %d", d.Class, i)
		}
	}
}

func TestDriftExceeded(t *testing.T) {
	m, err := NewMonitorDefault(t)
	if err != nil {
		t.Fatal(err)
	}
	base := media.MustRat(1, 30)
	// Establish baseline at zero offset, then offset grows past tolerance.
	for i := int64(0); i < 4; i++ {
		if _, err := m.Observe(Observation{Reference: media.MustTime(i, base), Subject: media.MustTime(i, base)}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := m.Observe(Observation{Reference: media.MustTime(10, base), Subject: media.MustTime(20, base)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Class != DriftExceeded {
		t.Fatalf("class = %v, deviation %s", d.Class, d.Deviation)
	}
	if m.SegmentsCut() != 1 {
		t.Fatalf("segments cut %d", m.SegmentsCut())
	}
}

func NewMonitorDefault(t *testing.T) (*Monitor, error) {
	t.Helper()
	return NewMonitor(DefaultDriftConfig())
}

func TestDriftJumpDetected(t *testing.T) {
	m, err := NewMonitor(DriftConfig{
		Tolerance:  media.MustRat(1, 20),
		JumpFactor: 8,
		Window:     8,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := media.MustRat(1, 30)
	// Regular cadence of 1 tick per observation.
	for i := int64(0); i < 6; i++ {
		if _, err := m.Observe(Observation{Reference: media.MustTime(i, base), Subject: media.MustTime(i, base)}); err != nil {
			t.Fatal(err)
		}
	}
	// A 1000-tick forward gap is ~1000x the median step.
	d, err := m.Observe(Observation{Reference: media.MustTime(1006, base), Subject: media.MustTime(1006, base)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Class != DriftJump {
		t.Fatalf("class = %v (%s)", d.Class, d.Reason)
	}
}

func TestDriftMedianOfEvenWindow(t *testing.T) {
	base := media.MustRat(1, 30)
	m, err := NewMonitor(DriftConfig{Tolerance: media.MustRat(1, 1), JumpFactor: 8, Window: 8})
	if err != nil {
		t.Fatal(err)
	}
	// Steps: 1,1,1,2 -> median (1+1)/2... but values differ; median of [1,1,1,2]
	// is index 2 (upper middle) by this implementation's convention.
	refs := []int64{0, 1, 2, 3, 5}
	for _, r := range refs {
		if _, err := m.Observe(Observation{Reference: media.MustTime(r, base), Subject: media.MustTime(r, base)}); err != nil {
			t.Fatal(err)
		}
	}
	// 5 -> 6 is a normal step, not a jump.
	d, err := m.Observe(Observation{Reference: media.MustTime(6, base), Subject: media.MustTime(6, base)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Class == DriftJump {
		t.Fatalf("false jump: %s", d.Reason)
	}
}

func TestDriftReset(t *testing.T) {
	m, err := NewMonitorDefault(t)
	if err != nil {
		t.Fatal(err)
	}
	base := media.MustRat(1, 30)
	m.Observe(Observation{Reference: media.MustTime(0, base), Subject: media.MustTime(100, base)})
	m.Reset()
	if _, err := m.Observe(Observation{Reference: media.MustTime(0, base), Subject: media.MustTime(0, base)}); err != nil {
		t.Fatal(err)
	}
	if m.MaxOffset().Num != 0 {
		t.Fatalf("max offset after reset: %s", m.MaxOffset())
	}
}
