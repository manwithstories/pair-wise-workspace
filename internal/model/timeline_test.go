package model

import "testing"

func TestUnwrapperForwardWrap(t *testing.T) {
	var u Unwrapper
	start := uint32(0xFFFFFFF0)
	var prev int64
	for i := 0; i < 40; i++ {
		ts := start + uint32(i)*10 // wraps at 2^32
		got := u.Next(ts)
		if i > 0 && got != prev+10 {
			t.Fatalf("step %d: got %d want %d", i, got, prev+10)
		}
		prev = got
	}
	if prev <= int64(^uint32(0)) {
		t.Fatalf("unwrapped time should pass 2^32, got %d", prev)
	}
}

func TestUnwrapperRegressionSurfaces(t *testing.T) {
	var u Unwrapper
	u.Next(1000)
	u.Next(1010)
	if got := u.Next(500); got >= 1010 {
		t.Fatalf("regression should surface as negative step, got %d", got)
	}
}

func TestTimeRange(t *testing.T) {
	r := TimeRange{Start: 10, End: 20}
	if !r.Valid() || !r.Contains(15) || r.Contains(20) {
		t.Fatal("range semantics broken")
	}
	if (TimeRange{Start: 5, End: 5}).Valid() {
		t.Fatal("empty range must be invalid")
	}
}
