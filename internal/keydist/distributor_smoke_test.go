package keydist

import (
	"errors"
	"testing"
	"time"

	"kmsctl/internal/keyschedule"
	"kmsctl/internal/license"
)

func TestDistributorTTLAndScope(t *testing.T) {
	mk := make([]byte, 32)
	for i := range mk {
		mk[i] = byte(i + 1)
	}
	schedule, _ := keyschedule.New(mk, 4, 0)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	d := New(schedule, time.Second, func() time.Time { return clock })
	lic, err := license.Issue(mk, license.Claim{
		Device: "dev-A", FirstSeg: 0, LastSeg: 2,
		NotBefore: base.Add(-time.Minute), NotAfter: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	cred := d.Issue(lic)
	if _, _, err := d.Redeem(cred, 0); err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	clock = clock.Add(2 * time.Second)
	if _, _, err := d.Redeem(cred, 1); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("want KEY_EXPIRED, got %v", err)
	}
	clock = base
	cred2 := d.Issue(lic)
	if _, _, err := d.Redeem(cred2, 5); !errors.Is(err, ErrOutOfScope) {
		t.Fatalf("want OUT_OF_SCOPE, got %v", err)
	}
	if _, _, err := d.Redeem(cred2, 0); err != nil {
		t.Fatalf("redeem seg0: %v", err)
	}
	if _, _, err := d.Redeem(cred2, 0); !errors.Is(err, ErrAlreadyUsed) {
		t.Fatalf("want already used, got %v", err)
	}
}
