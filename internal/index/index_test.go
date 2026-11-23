package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/fault"
	"mediaroute/internal/fixture"
	"mediaroute/internal/index"
	"mediaroute/internal/probe"
)

func buildReport(t *testing.T) (*probe.ContainerReport, string) {
	t.Helper()
	spec := fixture.DefaultSpec(41)
	raw, err := spec.BuildMP4()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(input, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := probe.ScanMP4(input)
	if err != nil {
		t.Fatal(err)
	}
	return rep, input
}

func TestIndexSaveLoadReuse(t *testing.T) {
	rep, input := buildReport(t)
	store := index.NewStore(t.TempDir(), index.DefaultParams())
	if err := store.Save(input, rep); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, fp, err := store.Load(input, rep.InputSHA256)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.TotalSamples() != rep.TotalSamples() {
		t.Fatal("reused index has different sample count")
	}
	if loaded.InputSHA256 != rep.InputSHA256 {
		t.Fatal("reused index digest mismatch")
	}
	if fp.Scanner != probe.ScannerVersion {
		t.Fatal("scanner version not carried in fingerprint")
	}
}

func TestIndexRejectsTamperedAndTruncated(t *testing.T) {
	rep, input := buildReport(t)
	dir := t.TempDir()
	store := index.NewStore(dir, index.DefaultParams())
	if err := store.Save(input, rep); err != nil {
		t.Fatal(err)
	}
	idxPath := filepath.Join(dir, filepath.Base(input)+".midx")

	t.Run("truncated", func(t *testing.T) {
		good, _ := os.ReadFile(idxPath)
		bad := append([]byte(nil), good[:len(good)/2]...)
		os.WriteFile(idxPath, bad, 0o600)
		if _, _, err := store.Load(input, rep.InputSHA256); err == nil {
			t.Fatal("truncated index was accepted")
		} else if fault.AsError(err).Category != fault.CatIndexInvalid {
			t.Fatalf("category=%s", fault.AsError(err).Category)
		}
	})

	t.Run("checksum_corruption", func(t *testing.T) {
		store.Save(input, rep)
		good, _ := os.ReadFile(idxPath)
		bad := append([]byte(nil), good...)
		// Flip a digit inside the report body (the size_bytes field sits
		// well past the header and its checksum), guaranteeing a real data
		// change rather than whitespace.
		marker := []byte("\"SizeBytes\"")
		pos := -1
		for i := 0; i+len(marker) <= len(bad); i++ {
			if string(bad[i:i+len(marker)]) == string(marker) {
				pos = i + len(marker) + 1
				break
			}
		}
		if pos < 0 {
			t.Fatal("size_bytes marker not found in index")
		}
		bad[pos] = bad[pos] + 1
		os.WriteFile(idxPath, bad, 0o600)
		if _, _, err := store.Load(input, rep.InputSHA256); err == nil {
			t.Fatal("corrupted index was accepted")
		} else if fault.AsError(err).Category != fault.CatIndexInvalid {
			t.Fatalf("category=%s", fault.AsError(err).Category)
		}
	})

	t.Run("fingerprint_change", func(t *testing.T) {
		store.Save(input, rep)
		other := index.DefaultParams()
		other.MaxSamples = 1
		store2 := index.NewStore(dir, other)
		if _, _, err := store2.Load(input, rep.InputSHA256); err == nil {
			t.Fatal("parameter change did not invalidate index")
		}
		if _, _, err := store.Load(input, "deadbeef"); err == nil {
			t.Fatal("input digest change did not invalidate index")
		}
	})
}

func TestIndexSafeRescanAfterInvalidation(t *testing.T) {
	rep, input := buildReport(t)
	dir := t.TempDir()
	store := index.NewStore(dir, index.DefaultParams())
	idxPath := filepath.Join(dir, filepath.Base(input)+".midx")
	store.Save(input, rep)
	// Corrupt then invalidate and rebuild.
	good, _ := os.ReadFile(idxPath)
	os.WriteFile(idxPath, good[:10], 0o600)
	if err := store.Invalidate(input); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := os.Stat(idxPath); !os.IsNotExist(err) {
		t.Fatal("invalidated index still present")
	}
	if err := store.Save(input, rep); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	if _, _, err := store.Load(input, rep.InputSHA256); err != nil {
		t.Fatalf("rebuilt index not reusable: %v", err)
	}
}

func TestIndexDeterministic(t *testing.T) {
	rep, input := buildReport(t)
	dir := t.TempDir()
	store := index.NewStore(dir, index.DefaultParams())
	store.Save(input, rep)
	first, _ := os.ReadFile(filepath.Join(dir, filepath.Base(input)+".midx"))
	store.Save(input, rep)
	second, _ := os.ReadFile(filepath.Join(dir, filepath.Base(input)+".midx"))
	if string(first) != string(second) {
		t.Fatal("index serialization is not deterministic across saves")
	}
}
