package checkpoint_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediapack/internal/checkpoint"
	"mediapack/internal/fault"
)

func writeGarbage(m *checkpoint.Manager, s string) error {
	return os.WriteFile(filepath.Join(m.Dir(), "checkpoint.json"), []byte(s), 0o644)
}

func TestCommitLoadRoundTrip(t *testing.T) {
	m, err := checkpoint.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.Load(); ok || err != nil {
		t.Fatalf("fresh dir must have no checkpoint: ok=%v err=%v", ok, err)
	}
	st := checkpoint.State{
		SourceID: "src-1", InputSHA256: "abcd", NextAU: 12,
		CompletedFrags: []string{"frag-000.mp4", "frag-001.mp4"},
		OutputSHA256:   "out-hash",
	}
	if err := m.Commit(st); err != nil {
		t.Fatal(err)
	}
	got, ok, err := m.Load()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.NextAU != 12 || !got.Committed || len(got.CompletedFrags) != 2 ||
		got.OutputSHA256 != "out-hash" || got.SourceID != "src-1" {
		t.Fatalf("state mismatch: %+v", got)
	}
}

func TestFragmentWriteAndSkip(t *testing.T) {
	m, _ := checkpoint.NewManager(t.TempDir())
	data := []byte("fragment payload")
	n, err := m.FragmentDurable("frag-007.mp4", data)
	if err != nil || n != int64(len(data)) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if !m.FragmentExists("frag-007.mp4") {
		t.Fatal("fragment should be visible immediately after durable write")
	}
	// Bare-name only: traversal attempts are refused.
	if _, err := m.FragmentDurable("../escape.bin", data); err == nil {
		t.Fatal("traversal fragment name must be rejected")
	}
	if _, err := m.FragmentDurable("a/b.bin", data); err == nil {
		t.Fatal("nested path fragment name must be rejected")
	}
}

func TestCorruptCheckpointReportedAsIO(t *testing.T) {
	dir := t.TempDir()
	m, _ := checkpoint.NewManager(dir)
	var err error
	if err = m.Commit(checkpoint.State{NextAU: 1}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the JSON in place.
	if err := writeGarbage(m, "{not-json"); err != nil {
		t.Fatal(err)
	}
	_, _, err = m.Load()
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindIO {
		t.Fatalf("expected IO fault for corrupt checkpoint, got %v", err)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	if _, err := checkpoint.NewManager("../../tmp/x"); err == nil {
		t.Fatal("traversal dir must be rejected")
	}
}
