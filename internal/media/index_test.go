package media

import (
	"os"
	"path/filepath"
	"testing"

	"review/internal/clock"
	"review/internal/fixture"
	"review/internal/issues"
)

func gen(t *testing.T, damage string) (string, int64) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.mp4")
	off, err := fixture.GenMP4(p, 100, clock.Rate2997, 0, damage)
	if err != nil {
		t.Fatal(err)
	}
	return p, off
}

func TestInspectClean(t *testing.T) {
	p, _ := gen(t, "")
	rep, iss := Inspect(p)
	if len(iss) != 0 || rep.DamageOffset != -1 {
		t.Fatalf("clean file flagged: %+v %v", rep, iss)
	}
	if rep.Samples != 100 || rep.LastGoodSample != 99 || rep.Delta != 1001 {
		t.Fatalf("bad report: %+v", rep)
	}
}

func TestInspectTruncated(t *testing.T) {
	p, want := gen(t, "truncated")
	rep, _ := Inspect(p)
	if rep.Kind != issues.MP4Truncated {
		t.Fatalf("kind %q", rep.Kind)
	}
	if rep.DamageOffset != want {
		t.Fatalf("damage offset %d, want exact truncation point %d", rep.DamageOffset, want)
	}
	st, _ := os.Stat(p)
	if rep.DamageOffset != st.Size() {
		t.Fatalf("damage offset %d != file size %d", rep.DamageOffset, st.Size())
	}
	if rep.LastGoodSample != 69 { // 70% of 100 samples survive
		t.Fatalf("last good %d", rep.LastGoodSample)
	}
}

func TestInspectIncompleteIndex(t *testing.T) {
	p, _ := gen(t, "incomplete-index")
	rep, iss := Inspect(p)
	if rep.Kind != issues.MP4IncompleteIdx {
		t.Fatalf("kind %q", rep.Kind)
	}
	if rep.DamageOffset < 0 || rep.LastGoodSample != 96 { // 97 declared chunks
		t.Fatalf("bad report: %+v", rep)
	}
	if len(iss) != 1 || iss[0].ByteOffset != rep.DamageOffset {
		t.Fatalf("issues: %v", iss)
	}
}

func TestInspectGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.mp4")
	os.WriteFile(p, []byte("not an mp4 at all"), 0o644)
	rep, iss := Inspect(p)
	if len(iss) == 0 || rep.DamageOffset < 0 {
		t.Fatalf("garbage not located: %+v %v", rep, iss)
	}
}
