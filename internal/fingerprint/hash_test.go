package fingerprint

import (
	"testing"
)

func TestComputeDeterministic(t *testing.T) {
	a := Compute([][]byte{{1, 2, 3}, {4, 5}})
	b := Compute([][]byte{{1, 2, 3}, {4, 5}})
	if !Equal(a, b) {
		t.Fatal("identical content must produce equal fingerprints")
	}
	if a.Encode() != b.Encode() || len(a.Encode()) != 16 {
		t.Fatalf("encoding unstable: %q vs %q", a.Encode(), b.Encode())
	}
}

func TestDifferentContentNotEqual(t *testing.T) {
	a := Compute([][]byte{{1, 2, 3}})
	b := Compute([][]byte{{1, 2, 4}})
	if Equal(a, b) {
		t.Fatal("distinct content collapsed into one fingerprint")
	}
}

func TestHashCollisionReverifiedByLengthAndDigest(t *testing.T) {
	// Simulate a hash collision: same 64-bit hash but different length. Equal
	// must reject it even though the fast hash agrees.
	a := Compute([][]byte{{0xab, 0xcd}})
	forged := a
	forged.Length = a.Length + 1
	if Equal(a, forged) {
		t.Fatal("length mismatch must defeat a hash collision")
	}
	// Same hash and length but different digest must also be rejected.
	forged2 := a
	forged2.Digest[0] ^= 0xff
	if Equal(a, forged2) {
		t.Fatal("digest mismatch must defeat a hash collision")
	}
}

func TestDeduperOrderAndCounts(t *testing.T) {
	d := NewDeduper()
	c1 := Compute([][]byte{{1, 1, 1}})
	c2 := Compute([][]byte{{2, 2, 2}})
	c3 := Compute([][]byte{{1, 1, 1}}) // exact duplicate of c1
	c4 := Compute([][]byte{{3, 3, 3}})

	cases := []struct {
		f        Fingerprint
		wantDup  bool
	}{
		{c1, false}, {c2, false}, {c3, true}, {c4, false},
	}
	for i, c := range cases {
		if got := d.Add(c.f); got != c.wantDup {
			t.Fatalf("case %d: dup=%v want %v", i, got, c.wantDup)
		}
	}
	if d.Duplicates() != 1 {
		t.Fatalf("duplicates = %d, want 1", d.Duplicates())
	}
	uniq := d.Unique()
	if len(uniq) != 3 {
		t.Fatalf("unique = %d, want 3", len(uniq))
	}
	// First-seen order must be preserved.
	if !Equal(uniq[0], c1) || !Equal(uniq[1], c2) || !Equal(uniq[2], c4) {
		t.Fatal("unique fingerprint order not preserved")
	}
}

func TestRepeatedRunIdentical(t *testing.T) {
	payload := [][]byte{{9, 8, 7}, {6, 5}}
	runs := make([]Fingerprint, 3)
	for i := range runs {
		runs[i] = Compute(payload)
	}
	for i := 1; i < len(runs); i++ {
		if !Equal(runs[0], runs[i]) {
			t.Fatal("repeated fingerprinting is not deterministic")
		}
	}
}
