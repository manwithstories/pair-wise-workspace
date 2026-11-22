package fixture

import (
	"os"
	"testing"

	"mediacheck/internal/probe"
)

// TestFixturesAreProbeClean guards the fixture generator itself: every
// generated file must describe the tracks and samples its spec promises.
func TestFixturesAreProbeClean(t *testing.T) {
	dir := t.TempDir()
	results, err := GenerateAll(dir)
	if err != nil {
		t.Fatalf("GenerateAll: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("results=%d want 5", len(results))
	}
	for _, r := range results {
		r := r
		t.Run(string(r.Kind), func(t *testing.T) {
			if _, err := os.Stat(r.Path); err != nil {
				t.Fatalf("missing fixture: %v", err)
			}
			if len(r.SHA256) != 64 {
				t.Fatalf("bad digest %q", r.SHA256)
			}
			f, err := probe.Analyze(r.Path, probe.DefaultConfig())
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if r.Kind == Truncated {
				if !f.Truncated {
					t.Fatal("truncated fixture not flagged")
				}
				return
			}
			if len(f.Tracks) != 2 {
				t.Fatalf("tracks=%d want 2", len(f.Tracks))
			}
			var v, a *probe.Track
			for _, tr := range f.Tracks {
				switch tr.Kind {
				case probe.KindVideo:
					v = tr
				case probe.KindAudio:
					a = tr
				}
			}
			if v == nil || a == nil {
				t.Fatal("missing video or audio track")
			}
			if v.Walked != uint64(r.Samples) {
				t.Fatalf("video walked=%d want %d", v.Walked, r.Samples)
			}
		})
	}
}

// TestFixtureDeterminism verifies byte-for-byte reproducibility.
func TestFixtureDeterminism(t *testing.T) {
	a, err := Generate(t.TempDir(), DefaultSpec(Valid))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(t.TempDir(), DefaultSpec(Valid))
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != b.SHA256 {
		t.Fatalf("digests differ: %s vs %s", a.SHA256, b.SHA256)
	}
}

// TestEmptyFixture checks the zero-byte input.
func TestEmptyFixture(t *testing.T) {
	r, err := Generate(t.TempDir(), DefaultSpec(Empty))
	if err != nil {
		t.Fatal(err)
	}
	if r.Bytes != 0 {
		t.Fatalf("empty fixture has %d bytes", r.Bytes)
	}
	f, err := probe.Analyze(r.Path, probe.DefaultConfig())
	if err != nil {
		t.Fatalf("probe empty: %v", err)
	}
	if len(f.Tracks) != 0 {
		t.Fatalf("empty file produced %d tracks", len(f.Tracks))
	}
}

// TestCorruptFixtureActuallyDamages confirms the corrupt variant differs from
// the valid one only in the damaged sample's payload.
func TestCorruptFixtureActuallyDamages(t *testing.T) {
	dir := t.TempDir()
	valid, err := Generate(dir, DefaultSpec(Valid))
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := Generate(dir, DefaultSpec(Corrupt))
	if err != nil {
		t.Fatal(err)
	}
	v, err := os.ReadFile(valid.Path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := os.ReadFile(corrupt.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != len(c) {
		t.Fatalf("corrupt fixture length changed: %d vs %d", len(v), len(c))
	}
	diff := 0
	for i := range v {
		if v[i] != c[i] {
			diff++
		}
	}
	if diff == 0 {
		t.Fatal("corrupt fixture is byte-identical to valid")
	}
	if corrupt.DamageOffset <= 0 {
		t.Fatalf("damage offset=%d", corrupt.DamageOffset)
	}
}
