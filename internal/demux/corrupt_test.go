package demux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/fixture"
	"mediamerge/internal/model"
)

// verifyLocator checks that a validator reports the first bad byte within the
// one-byte tolerance required by the acceptance criteria.
func verifyLocator(t *testing.T, kind fixture.Kind, wantCode string) {
	t.Helper()
	dir := t.TempDir()
	p := fixture.DefaultPlan(2, 16)
	p.UniformSizes = false
	base := p.Build()
	damaged, dmg, err := fixture.Corrupt(base, kind)
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	path := filepath.Join(dir, "bad.mp4")
	if err := os.WriteFile(path, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Read(context.Background(), path)
	if err == nil {
		t.Fatalf("%s: expected error", kind)
	}
	pe, ok := err.(*model.Error)
	if !ok {
		t.Fatalf("%s: error type %T", kind, err)
	}
	if wantCode != "" && pe.Code != wantCode {
		t.Fatalf("%s: code=%s want %s (%v)", kind, pe.Code, wantCode, pe)
	}
	if dmg.LocatorTarget >= 0 {
		if delta := pe.Offset - dmg.LocatorTarget; delta < -1 || delta > 1 {
			t.Fatalf("%s: locator %d want %d (delta %d)", kind, pe.Offset, dmg.LocatorTarget, delta)
		}
	}
}

func TestCorruptTruncatedBoxLocator(t *testing.T) {
	verifyLocator(t, fixture.KindTruncatedBox, model.CodeTruncated)
}

func TestCorruptSampleTableLocator(t *testing.T) {
	verifyLocator(t, fixture.KindCorruptStbl, model.CodeSampleTable)
}

func TestTailTruncationLocator(t *testing.T) {
	verifyLocator(t, fixture.KindTailTruncation, model.CodeTruncated)
}

func TestUnknownRequiredBoxLocator(t *testing.T) {
	verifyLocator(t, fixture.KindUnknownBox, model.CodeUnknownBox)
}

func TestZeroTimescaleLocator(t *testing.T) {
	verifyLocator(t, fixture.KindZeroTimescale, model.CodeTimebase)
}
