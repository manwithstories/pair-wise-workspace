package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/index"
	"mediad/internal/model"
)

func TestGenerateAllFixtureClasses(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	want := []string{NameNormal, NameCorrupt, NameTruncated, NameWrap, NameInterrupt, "audio.m4a", LossProfileName}
	for _, name := range want {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("fixture %s missing: %v", name, err)
		}
	}
}

func TestNormalFixtureScansClean(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	res, err := index.Scan(filepath.Join(dir, NameNormal))
	if err != nil {
		t.Fatalf("normal fixture must scan clean: %v", err)
	}
	if len(res.Index.Faults) != 0 {
		t.Fatalf("normal fixture has faults: %+v", res.Index.Faults)
	}
	if len(res.Index.Tracks) != 1 || len(res.Index.Tracks[0].Samples) == 0 {
		t.Fatal("normal fixture must contain one populated track")
	}
}

func TestTruncatedFixtureReportsExactOffset(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, NameTruncated)
	st, _ := os.Stat(path)
	res, err := index.Scan(path)
	if err == nil {
		t.Fatal("truncated fixture must produce a hard scan error")
	}
	if res.Index.FirstBadByte != st.Size() {
		t.Fatalf("first_bad_offset = %d, want truncation point %d",
			res.Index.FirstBadByte, st.Size())
	}
}

func TestWraparoundFixtureDetected(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	res, _ := index.Scan(filepath.Join(dir, NameWrap))
	kinds := map[model.FaultKind]bool{}
	for _, f := range res.Index.Faults {
		kinds[f.Kind] = true
	}
	if !kinds[model.FaultPTSWrap] {
		t.Fatalf("wraparound fixture must yield pts_wraparound, got %+v", res.Index.Faults)
	}
}

func TestCorruptFixtureStillIndexed(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	res, _ := index.Scan(filepath.Join(dir, NameCorrupt))
	if res == nil || res.Index == nil || res.Index.SHA256 == "" {
		t.Fatal("corrupt fixture must still return a content-addressed index")
	}
}

func TestLossProfileIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	p, err := LoadProfileForTest(filepath.Join(dir, LossProfileName))
	if err != nil {
		t.Fatal(err)
	}
	if p.Seed != 42 || len(p.DropSeqs) != 3 {
		t.Fatalf("loss profile mismatch: %+v", p)
	}
}
