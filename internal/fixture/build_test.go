package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildIsDeterministic(t *testing.T) {
	spec, err := PresetSpec("normal")
	if err != nil {
		t.Fatal(err)
	}
	a, manA, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, manB, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("lengths differ %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
	if len(manA.Tracks) != 3 || len(manB.Tracks) != 3 {
		t.Fatalf("manifests %d vs %d tracks", len(manA.Tracks), len(manB.Tracks))
	}
	if manA.VideoFrameCount() != Cam0Frames+Cam1Frames {
		t.Fatalf("video frames=%d", manA.VideoFrameCount())
	}
	if manA.TotalTicks != TotalFrames*VideoFrameTicks {
		t.Fatalf("total ticks=%d", manA.TotalTicks)
	}
}

func TestBuildSeedChangesBytes(t *testing.T) {
	spec, _ := PresetSpec("normal")
	a, _, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.Seed = 99
	b, _, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Fatal("changing the seed must change the sample body")
	}
}

func TestFaults(t *testing.T) {
	trunc, _ := PresetSpec("truncated")
	norm, _ := PresetSpec("normal")
	tb, _, err := Build(trunc)
	if err != nil {
		t.Fatal(err)
	}
	nb, _, err := Build(norm)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(tb)) != trunc.Fault.TruncateKeepBytes {
		t.Fatalf("truncated size=%d want %d", len(tb), trunc.Fault.TruncateKeepBytes)
	}
	if len(tb) >= len(nb) {
		t.Fatal("truncated fixture is not smaller")
	}

	corrupt, _ := PresetSpec("corrupt")
	cb, _, err := Build(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if len(cb) != len(nb) {
		t.Fatalf("corruption must not change length: %d vs %d", len(cb), len(nb))
	}
	// Exactly one contiguous byte range per corrupted sample differs.
	if string(cb) == string(nb) {
		t.Fatal("corrupt fixture equals the normal fixture")
	}

	mi, _ := PresetSpec("missing-index")
	mb, _, err := Build(mi)
	if err != nil {
		t.Fatal(err)
	}
	if len(mb) >= len(nb) {
		t.Fatal("missing-index fixture should be smaller than normal")
	}
}

func TestEnsureWritesOnce(t *testing.T) {
	dir := t.TempDir()
	p1, man1, err := Ensure(dir, "truncated")
	if err != nil {
		t.Fatal(err)
	}
	st1, err := os.Stat(p1)
	if err != nil {
		t.Fatal(err)
	}
	// Re-Ensuring reuses the exact file.
	p2, man2, err := Ensure(dir, "truncated")
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 || man1.Name != man2.Name {
		t.Fatalf("paths %q %q", p1, p2)
	}
	st2, _ := os.Stat(p2)
	if st1.Size() != st2.Size() {
		t.Fatal("file was overwritten with different data")
	}
	if filepath.Base(p1) != "truncated.mp4" {
		t.Fatalf("name=%s", filepath.Base(p1))
	}
	if _, _, err := Ensure(dir, "bogus"); err == nil {
		t.Fatal("unknown preset accepted")
	}
}
