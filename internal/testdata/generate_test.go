package testdata

import (
	"bytes"
	"testing"
)

func TestSubtitleFixtureSet(t *testing.T) {
	fixtures := SubtitleFixtures()
	seen := map[string]bool{}
	var empty, damaged int
	for _, fx := range fixtures {
		if seen[fx.Name] {
			t.Fatalf("duplicate fixture %q", fx.Name)
		}
		seen[fx.Name] = true
		if fx.Data == "" {
			empty++
		}
		if fx.Corrupt {
			damaged++
			if !fx.ExpectParseErrors {
				t.Fatalf("corrupt fixture %q not marked for errors", fx.Name)
			}
		}
	}
	if empty != 1 {
		t.Fatalf("empty fixtures=%d want 1", empty)
	}
	if damaged < 2 {
		t.Fatalf("damaged fixtures=%d want >=2", damaged)
	}
}

func TestMP4FixtureDeterministic(t *testing.T) {
	spec := MP4Spec{
		Timescale: 24000, SampleCount: 48, Delta: 1000,
		SampleSize: 8, KeyEvery: 8, Chunks: 4,
	}
	a, err := BuildMP4(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildMP4(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("MP4 fixture is not deterministic")
	}
	corrupt := BuildCorruptMP4(a, spec)
	if len(corrupt) < 3 {
		t.Fatalf("corrupt variants=%d want >=3", len(corrupt))
	}
	names := map[string]bool{}
	for _, c := range corrupt {
		if names[c.Name] {
			t.Fatalf("duplicate corrupt name %q", c.Name)
		}
		names[c.Name] = true
		if bytes.Equal(c.Data, a) {
			t.Fatalf("corrupt fixture %q identical to valid input", c.Name)
		}
	}
}
