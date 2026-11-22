package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"mediapack/internal/clock"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Exists() {
		t.Fatal("fresh dir must not contain a checkpoint")
	}
	want := State{
		SourceFP:     "abc123",
		ParseOffset:  4096,
		WatermarkNS:  5_000_000_000,
		ManifestSHA:  "deadbeef",
		SegmentCount: 12,
		QueueBytes:   700,
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if !s.Exists() {
		t.Fatal("checkpoint must exist after save")
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != CurrentVersion || got.SourceFP != want.SourceFP ||
		got.ParseOffset != want.ParseOffset || got.WatermarkNS != want.WatermarkNS ||
		got.ManifestSHA != want.ManifestSHA || got.SegmentCount != want.SegmentCount {
		t.Fatalf("roundtrip mismatch:\nwant %+v\ngot  %+v", want, got)
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	for i := 0; i < 5; i++ {
		if err := s.Save(State{SourceFP: "fp", ParseOffset: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := filepath.Glob(filepath.Join(dir, ".checkpoint-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}

func TestSaveRequiresFingerprint(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if err := s.Save(State{ParseOffset: 10}); err == nil {
		t.Fatal("save without source fingerprint must fail")
	}
}

func TestVerifyAcceptsMatching(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	st := State{SourceFP: "fp", ManifestSHA: "sha", ParseOffset: 1, WatermarkNS: 1}
	if err := s.Verify(st, "fp", "sha"); err != nil {
		t.Fatalf("matching state must verify: %v", err)
	}
	// Empty stored manifest digest means none written yet: allowed.
	st2 := State{SourceFP: "fp", ManifestSHA: ""}
	if err := s.Verify(st2, "fp", "whatever"); err != nil {
		t.Fatalf("pre-first-manifest verify should pass: %v", err)
	}
}

func TestVerifyRejectsSourceMismatch(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	st := State{SourceFP: "original", ManifestSHA: "sha"}
	err := s.Verify(st, "changed", "sha")
	mm, ok := err.(*Mismatch)
	if !ok {
		t.Fatalf("want *Mismatch, got %T %v", err, err)
	}
	if mm.Field != "source_fingerprint" {
		t.Fatalf("mismatch field=%s", mm.Field)
	}
	me, ok := clock.AsMediaError(err)
	if !ok || me.Category != clock.CatCheckpoint {
		t.Fatalf("underlying error must be CatCheckpoint, got %v", err)
	}
}

func TestVerifyRejectsManifestMismatch(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	st := State{SourceFP: "fp", ManifestSHA: "old-digest"}
	err := s.Verify(st, "fp", "new-digest")
	mm, ok := err.(*Mismatch)
	if !ok || mm.Field != "manifest_sha256" {
		t.Fatalf("want manifest mismatch, got %v", err)
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if _, err := s.Load(); err == nil {
		t.Fatal("load on missing file must error")
	}
	// Corrupt JSON.
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("corrupt json must be rejected")
	}
	// Bad version.
	bad := `{"version":99,"source_fingerprint":"fp","parse_offset":1,"watermark_ns":1}`
	if err := os.WriteFile(s.Path(), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("unsupported version must be rejected")
	}
}

func TestRemove(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if err := s.Save(State{SourceFP: "fp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if s.Exists() {
		t.Fatal("checkpoint must be gone after remove")
	}
	if err := s.Remove(); err != nil {
		t.Fatalf("remove on absent file must be a clean no-op: %v", err)
	}
}
