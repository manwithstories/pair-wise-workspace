package txn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "work"))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m, dir
}

func TestStageCommitAndDeterminism(t *testing.T) {
	m, _ := openManager(t)
	defer m.Close()
	sum1, err := m.Stage("stream", "index.bin", []byte("hello-avpk"))
	if err != nil {
		t.Fatal(err)
	}
	sum2, err := m.Stage("normalize", "audio.bin", []byte("second-stage"))
	if err != nil {
		t.Fatal(err)
	}
	if sum1 == sum2 {
		t.Fatal("distinct content must produce distinct checksums")
	}
	man := m.Manifest()
	if man.LastStage != "normalize" || man.LastChecksum != sum2 {
		t.Fatalf("manifest last = %s/%s", man.LastStage, man.LastChecksum)
	}
	if man.RollbackChecksum != sum1 {
		t.Fatal("rollback checksum must retain the previous committed value")
	}
	// Artifact exists at the committed relative path.
	data, err := os.ReadFile(filepath.Join(m.Root(), man.Stages["stream"].RelPath))
	if err != nil || string(data) != "hello-avpk" {
		t.Fatalf("committed artifact missing or wrong: %v", err)
	}
	// Temp dir is clean after a full commit.
	entries, _ := os.ReadDir(filepath.Join(m.Root(), tmpDir))
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %d", len(entries))
	}
}

func TestRecoverAfterCrash(t *testing.T) {
	m, dir := openManager(t)
	sum, err := m.Stage("stream", "index.bin", []byte("durable"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: drop an uncommitted temp file and a partial WAL line.
	tmp := filepath.Join(m.Root(), tmpDir, "s99-orphan")
	if err := os.WriteFile(tmp, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(m.Root(), walName)
	f, _ := os.OpenFile(walPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if _, err := f.WriteString(`{"op":"prepare","stage":"normalize","seq":99,"rel_path":"x"` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	m.Close()

	rec, rep, err := Recover(filepath.Join(dir, "work"))
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	defer rec.Close()
	if !rec.Committed("stream") {
		t.Fatal("committed stage lost during recovery")
	}
	if rep.LastChecksum != sum {
		t.Fatalf("recovered checksum = %s, want %s", rep.LastChecksum, sum)
	}
	if len(rep.DiscardedTemp) == 0 {
		t.Fatal("orphan temp file was not discarded")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("orphan temp survived recovery")
	}
}

func TestRecoverDropsCorruptArtifact(t *testing.T) {
	m, dir := openManager(t)
	if _, err := m.Stage("stream", "index.bin", []byte("original")); err != nil {
		t.Fatal(err)
	}
	m.Close()
	// Corrupt the committed artifact on disk; checksum verification must drop it.
	man := filepath.Join(dir, "work", manifestName)
	_ = man
	stageFile, _ := filepath.Glob(filepath.Join(dir, "work", stagesDir, "stream", "*"))
	if len(stageFile) != 1 {
		t.Fatalf("stage files = %v", stageFile)
	}
	if err := os.WriteFile(stageFile[0], []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, rep, err := Recover(filepath.Join(dir, "work"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if len(rep.BrokenStages) != 1 || rec.Committed("stream") {
		t.Fatalf("corrupt stage not quarantined: %+v", rep)
	}
}

func TestRollbackPreservesLastChecksum(t *testing.T) {
	m, _ := openManager(t)
	defer m.Close()
	first, _ := m.Stage("stream", "a.bin", []byte("a"))
	second, _ := m.Stage("normalize", "b.bin", []byte("b"))
	kept, err := m.Rollback("normalize")
	if err != nil {
		t.Fatal(err)
	}
	if kept != first {
		t.Fatalf("rollback kept %s, want previous %s", kept, first)
	}
	if m.Committed("normalize") {
		t.Fatal("rolled-back stage still committed")
	}
	if m.Manifest().LastChecksum != first {
		t.Fatal("manifest must point at the preserved earlier checksum")
	}
	if second == first {
		t.Fatal("test setup produced equal checksums")
	}
}

func TestSafeJoinRejectsEscape(t *testing.T) {
	m, _ := openManager(t)
	defer m.Close()
	for _, bad := range []string{"../escape", "../../etc/passwd", "/abs/path", ""} {
		if _, err := m.SafeJoin(bad); err == nil {
			t.Fatalf("unsafe path accepted: %q", bad)
		}
	}
	good, err := m.SafeJoin(filepath.Join(stagesDir, "x", "f.bin"))
	if err != nil || !strings.HasPrefix(good, m.Root()) {
		t.Fatalf("safe path rejected: %v", err)
	}
}

func TestPayloadLimit(t *testing.T) {
	m, _ := openManager(t)
	defer m.Close()
	if _, err := m.Stage("x", "big.bin", make([]byte, MaxStagePayload+1)); err == nil {
		t.Fatal("oversized stage payload accepted")
	}
}
