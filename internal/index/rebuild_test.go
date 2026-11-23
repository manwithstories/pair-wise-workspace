package index

import (
	"testing"

	"mediad/internal/clock"
	"mediad/internal/stream"
)

func fixture(n int) ([]stream.AccessUnit, []clock.FixedSample) {
	var units []stream.AccessUnit
	var fixed []clock.FixedSample
	for i := 0; i < n; i++ {
		units = append(units, stream.AccessUnit{
			Index: i, Offset: int64(i * 100), Size: 100, Keyframe: i%5 == 0,
		})
		fixed = append(fixed, clock.FixedSample{
			Seq: uint64(i), Time: int64(i * 3600), Keyframe: i%5 == 0,
		})
	}
	return units, fixed
}

func TestRebuildAndValidate(t *testing.T) {
	units, fixed := fixture(20)
	idx, mism := Rebuild(units, fixed)
	if len(mism) != 0 {
		t.Fatalf("mismatches=%v", mism)
	}
	if len(idx.Entries) != 4 {
		t.Fatalf("entries=%d, want 4", len(idx.Entries))
	}
	if m := Validate(idx, units); len(m) != 0 {
		t.Fatalf("validate=%v", m)
	}
	for i := 1; i < len(idx.Entries); i++ {
		if idx.Entries[i].Time <= idx.Entries[i-1].Time {
			t.Fatal("entries not time-ordered")
		}
	}
}

func TestRebuildDeterministicDigest(t *testing.T) {
	units, fixed := fixture(50)
	a, _ := Rebuild(units, fixed)
	b, _ := Rebuild(units, fixed)
	if a.Digest() != b.Digest() {
		t.Fatalf("digests differ:\n%s\n%s", a.Digest(), b.Digest())
	}
	// A different stream must produce a different digest.
	units2, fixed2 := fixture(51)
	c, _ := Rebuild(units2, fixed2)
	if a.Digest() == c.Digest() {
		t.Fatal("different inputs produced equal digests")
	}
}

func TestRebuildSkipsIsolatedAndCorrupted(t *testing.T) {
	units, fixed := fixture(10)
	units[5].Corrupted = true
	fixed[5] = clock.FixedSample{Seq: 5, Time: -1, Isolated: true, Keyframe: true}
	idx, _ := Rebuild(units, fixed)
	if len(idx.Entries) != 1 || idx.Skipped != 1 {
		t.Fatalf("entries=%d skipped=%d", len(idx.Entries), idx.Skipped)
	}
}

func TestValidateDetectsTampering(t *testing.T) {
	units, fixed := fixture(10)
	idx, _ := Rebuild(units, fixed)
	idx.Entries[0].Offset += 1
	if m := Validate(idx, units); len(m) != 1 {
		t.Fatalf("validate=%v, want 1 mismatch", m)
	}
}
