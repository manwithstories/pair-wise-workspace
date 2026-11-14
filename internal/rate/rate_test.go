package rate

import (
	"math"
	"testing"
)

func TestModelFitAndPredict(t *testing.T) {
	m := NewModel()
	// synthetic: bits = 1e8 * qp^-1.5
	for qp := 10; qp <= 50; qp += 5 {
		bits := int64(1e8 * math.Pow(float64(qp), -1.5))
		m.Add(qp, bits)
	}
	alpha, beta, ok := m.Fit(8)
	if !ok {
		t.Fatal("fit failed on clean power-law data")
	}
	if beta > -1.2 || beta < -1.8 {
		t.Fatalf("beta %.3f not near -1.5", beta)
	}
	got := PredictQP(alpha, beta, int64(1e8*math.Pow(30, -1.5)))
	if math.Abs(got-30) > 2 {
		t.Fatalf("predicted qp %.2f, want ~30", got)
	}
}

func TestModelWindowK(t *testing.T) {
	m := NewModel()
	for i := 0; i < 20; i++ {
		m.Add(10+i, int64(1000*(20-i)))
	}
	if m.Observations() != 20 {
		t.Fatalf("observations: got %d", m.Observations())
	}
	if _, _, ok := m.Fit(5); !ok {
		t.Fatal("fit with window failed")
	}
}

func TestModelRejectsBadData(t *testing.T) {
	m := NewModel()
	if _, _, ok := m.Fit(8); ok {
		t.Fatal("fit should fail with no data")
	}
	m.Add(20, 1000)
	if _, _, ok := m.Fit(8); ok {
		t.Fatal("fit should fail with one point")
	}
	m.Add(20, 1000) // same qp: zero variance
	if _, _, ok := m.Fit(8); ok {
		t.Fatal("fit should fail with constant qp")
	}
}

func TestControllerQPClamps(t *testing.T) {
	c := NewController(1200000, 30, 30)
	qp := c.DecideQP()
	if qp < MinQPTest || qp > MaxQPTest {
		t.Fatalf("qp %d out of range", qp)
	}
	// feed a huge overspend: qp must jump but stay delta-limited
	c.Update(qp, 100*c.TargetBitsPerSegment)
	next := c.DecideQP()
	if next > qp+c.MaxQPDelta {
		t.Fatalf("qp delta not limited: %d -> %d", qp, next)
	}
	if next > 51 || next < 1 {
		t.Fatalf("qp %d out of [1,51]", next)
	}
}

const (
	MinQPTest = 1
	MaxQPTest = 51
)

func TestControllerCarryover(t *testing.T) {
	c := NewController(1200000, 30, 30)
	target := c.TargetBitsPerSegment
	c.Update(30, target/2) // underspend: lend forward
	if c.Carry() <= 0 {
		t.Fatalf("underspend should create positive carry, got %d", c.Carry())
	}
	carry := c.Carry()
	c.Update(30, target*2) // overspend: claw back
	if c.Carry() >= carry {
		t.Fatalf("overspend should reduce carry, got %d after %d", c.Carry(), carry)
	}
}

func TestControllerVBV(t *testing.T) {
	c := NewController(1200000, 30, 30)
	under := c.Update(30, c.VBVSizeBits*10) // absurd drain
	if !under {
		t.Fatal("expected VBV underflow on absurd drain")
	}
	if c.VBVUnderflows() != 1 {
		t.Fatalf("underflow count: got %d", c.VBVUnderflows())
	}
	if c.VBVLevel() != 0 {
		t.Fatalf("level should clamp at 0, got %d", c.VBVLevel())
	}
	// normal operation: fill keeps buffer non-negative
	c2 := NewController(1200000, 30, 30)
	for i := 0; i < 8; i++ {
		qp := c2.DecideQP()
		if c2.Update(qp, c2.TargetBitsPerSegment) {
			t.Fatalf("segment %d: unexpected underflow", i)
		}
	}
}

func TestControllerDeterministic(t *testing.T) {
	run := func() []int {
		c := NewController(1200000, 30, 30)
		qps := []int{}
		bits := int64(1500000)
		for i := 0; i < 8; i++ {
			qp := c.DecideQP()
			qps = append(qps, qp)
			c.Update(qp, bits)
			bits = bits * 9 / 10
		}
		return qps
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("controller not deterministic at %d: %v vs %v", i, a, b)
		}
	}
}
