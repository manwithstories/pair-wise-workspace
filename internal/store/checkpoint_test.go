package store

import (
	"errors"
	"testing"
)

func TestFileStore_SaveLoadAndDuplicateReject(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if key, err := s.Load("n1"); err != nil || key != "" {
		t.Fatalf("missing key must be empty, got %q/%v", key, err)
	}
	if err := s.Save("n1", "key-1"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := s.Save("n1", "key-1"); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("identical re-save must fail, got %v", err)
	}
	// Changed key overwrites (invalidation), no error.
	if err := s.Save("n1", "key-2"); err != nil {
		t.Fatalf("invalidation save: %v", err)
	}
	if key, _ := s.Load("n1"); key != "key-2" {
		t.Fatalf("load = %q, want key-2", key)
	}
	e, err := s.LoadEntry("n1")
	if err != nil || e.NodeID != "n1" || e.UpdatedMS == 0 {
		t.Fatalf("entry = %+v err=%v", e, err)
	}
}

func TestFileStore_IDsSortedAndIndependent(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileStore(dir)
	for _, id := range []string{"zeta", "alpha", "mid"} {
		if err := s.Save(id, "k-"+id); err != nil {
			t.Fatal(err)
		}
	}
	// A second store instance over the same directory proves persistence.
	s2, _ := NewFileStore(dir)
	ids, err := s2.IDs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mid", "zeta"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want sorted %v", ids, want)
		}
	}
}

func TestFileStore_RemoveIsIdempotent(t *testing.T) {
	s, _ := NewFileStore(t.TempDir())
	if err := s.Save("n", "k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("n"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := s.Remove("n"); err != nil {
		t.Fatalf("removing absent entry must be nil, got %v", err)
	}
	if key, _ := s.Load("n"); key != "" {
		t.Fatalf("removed entry must be empty, got %q", key)
	}
}

func TestFileStore_CollisionFreePaths(t *testing.T) {
	s, _ := NewFileStore(t.TempDir())
	// Distinct ids must map to distinct files even though names are hashed.
	if err := s.Save("a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("b", "2"); err != nil {
		t.Fatal(err)
	}
	ids, _ := s.IDs()
	if len(ids) != 2 {
		t.Fatalf("two distinct nodes expected, got %v", ids)
	}
}
