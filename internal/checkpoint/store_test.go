package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty load err = %v", err)
	}
	st := State{
		InputOffset:   42,
		ConfigDigest:  ConfigDigest("seed=42", "bitrates=64k,128k"),
		Committed:     map[string]int{"64k": 3},
		CommittedSHA:  map[string]string{"64k/3": "abc"},
		ManifestChain: "deadbeef",
		ManifestCount: 7,
		LedgerDigest:  "feed",
	}
	if err := s.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.InputOffset != 42 || got.ManifestCount != 7 || got.Committed["64k"] != 3 || got.Schema != Schema {
		t.Fatalf("state = %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt not stamped")
	}
	// No temp files are left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestConfigDigestSensitive(t *testing.T) {
	a := ConfigDigest("seed=42", "dur=120s")
	b := ConfigDigest("seed=43", "dur=120s")
	if a == b || len(a) != 64 {
		t.Fatalf("digest %s vs %s", a, b)
	}
	// Order and field boundaries matter.
	if ConfigDigest("ab", "c") == ConfigDigest("a", "bc") {
		t.Fatal("digest boundaries are ambiguous")
	}
}

func TestSaveCancelled(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Save(ctx, State{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRejectCorruptAndBadSchema(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("corrupt state accepted")
	}
	if err := os.WriteFile(s.Path(), []byte(`{"schema":99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("future schema accepted")
	}
	if _, err := NewStore(""); err == nil {
		t.Fatal("empty dir accepted")
	}
}

func TestLastTracksNewest(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	s.Save(context.Background(), State{InputOffset: 1})
	s.Save(context.Background(), State{InputOffset: 2})
	if s.Last().InputOffset != 2 {
		t.Fatalf("last = %+v", s.Last())
	}
}
