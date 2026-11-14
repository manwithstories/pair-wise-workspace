package license

import (
	"errors"
	"testing"
	"time"
)

func TestVerifyWindowAndBinding(t *testing.T) {
	mk := make([]byte, 32)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lic, err := Issue(mk, Claim{
		Device: "dev-A", FirstSeg: 0, LastSeg: 3,
		NotBefore: base, NotAfter: base.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lic.VerifyAt(mk, "dev-A", base.Add(5*time.Minute), 60); err != nil {
		t.Fatalf("valid window: %v", err)
	}
	if err := lic.VerifyAt(mk, "dev-B", base.Add(5*time.Minute), 60); !errors.Is(err, ErrDeviceMismatch) {
		t.Fatalf("want device mismatch got %v", err)
	}
	if err := lic.VerifyAt(mk, "dev-A", base.Add(-time.Minute), 0); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("want not yet valid got %v", err)
	}
	if err := lic.VerifyAt(mk, "dev-A", base.Add(11*time.Minute), 0); !errors.Is(err, ErrExpired) {
		t.Fatalf("want expired got %v", err)
	}
	// 偏移容忍：过期 30s，skew=60 放行
	if err := lic.VerifyAt(mk, "dev-A", base.Add(10*time.Minute+30*time.Second), 60); err != nil {
		t.Fatalf("skew should tolerate: %v", err)
	}
	if !lic.Covers(3) || lic.Covers(4) {
		t.Fatal("range cover wrong")
	}
}
