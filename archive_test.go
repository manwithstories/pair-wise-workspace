package archivecheck_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaarchive/internal/app"
	"mediaarchive/internal/archive"
	"mediaarchive/internal/fault"
	"mediaarchive/internal/timeline"
)

func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := app.Run(context.Background(), args, &buf)
	out := buf.String()
	t.Logf("args=%v code=%d\n%s", args, code, out)
	return code, out
}

func TestCheckpointAtomicSaveLoadAndTamperDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "checkpoint.json")
	st := timeline.NewState()
	st.Cursor = 7
	cp, err := archive.NewCheckpoint(7, "params-1", st, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.SaveCheckpoint(path, cp); err != nil {
		t.Fatal(err)
	}
	loaded, ferr := archive.LoadCheckpoint(path, "params-1")
	if ferr != nil {
		t.Fatalf("load: %v", ferr)
	}
	if loaded.InputCursor != 7 || loaded.StateHash != cp.StateHash {
		t.Fatalf("bad checkpoint: %+v", loaded)
	}
	if _, ferr := archive.LoadCheckpoint(path, "other-params"); ferr == nil || ferr.Kind != fault.KindIntegrity {
		t.Fatalf("expected params integrity error, got %v", ferr)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(b, []byte(`"cursor": 7`), []byte(`"cursor": 8`), 1)
	if bytes.Equal(b, tampered) {
		t.Fatal("test setup failed: cursor field not found")
	}
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ferr := archive.LoadCheckpoint(path, "params-1"); ferr == nil || ferr.Kind != fault.KindIntegrity {
		t.Fatalf("expected state hash integrity error, got %v", ferr)
	}
}

func TestRecoveryCheckpointResumeCompareAndIdempotency(t *testing.T) {
	root := t.TempDir()
	clean := filepath.Join(root, "clean")
	resume := filepath.Join(root, "resume")

	code, out := runCLI(t, "--scenario", "clean", "--seed", "41", "--events", "30", "--damage-at", "1", "--out", clean)
	if code != 0 {
		t.Fatalf("clean code=%d", code)
	}
	for _, want := range []string{"tracks=3", "events=30", "located_packet=1", "offset_error=0", "frames_sha256=", "manifest_sha256="} {
		if !strings.Contains(out, want) {
			t.Fatalf("clean output missing %q:\n%s", want, out)
		}
	}

	code, out = runCLI(t, "--scenario", "recovery", "--seed", "41", "--events", "30", "--damage-at", "1", "--interrupt-after", "10", "--out", resume)
	if code != fault.ExitCheckpoint {
		t.Fatalf("interrupt code=%d want 75", code)
	}
	if !strings.Contains(out, "checkpoint_events=10") {
		t.Fatalf("missing checkpoint_events=10:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(resume, "checkpoint.json")); err != nil {
		t.Fatalf("checkpoint not saved: %v", err)
	}

	code, out = runCLI(t, "--scenario", "recovery", "--seed", "41", "--events", "30", "--damage-at", "1", "--resume", "--out", resume, "--compare", clean)
	if code != 0 {
		t.Fatalf("resume code=%d", code)
	}
	for _, want := range []string{"checkpoint_events=10", "resumed_events=20", "compare=match", "frames_sha256=", "manifest_sha256="} {
		if !strings.Contains(out, want) {
			t.Fatalf("resume output missing %q:\n%s", want, out)
		}
	}
	firstHashes := readTestHashes(t, resume)

	// 重复执行已完成的 resume：不得产生重复包，哈希保持一致。
	code, out = runCLI(t, "--scenario", "recovery", "--seed", "41", "--events", "30", "--damage-at", "1", "--resume", "--out", resume, "--compare", clean)
	if code != 0 {
		t.Fatalf("repeat resume code=%d", code)
	}
	secondHashes := readTestHashes(t, resume)
	if firstHashes != secondHashes {
		t.Fatalf("repeat resume changed hashes: %s vs %s", firstHashes, secondHashes)
	}
}

func TestCLIExitCodesForUsageFatalAndHashMismatch(t *testing.T) {
	root := t.TempDir()
	if code, _ := runCLI(t, "--scenario", "clean", "--events", "30", "--damage-at", "1"); code != 2 {
		t.Fatalf("missing --out code=%d want 2", code)
	}
	if code, _ := runCLI(t, "--scenario", "bogus", "--out", filepath.Join(root, "usage")); code != 2 {
		t.Fatalf("bad scenario code=%d want 2", code)
	}

	fatalDir := filepath.Join(root, "fatal")
	if code, out := runCLI(t, "--scenario", "fatal", "--seed", "41", "--events", "30", "--damage-at", "1", "--out", fatalDir); code != 3 {
		t.Fatalf("fatal code=%d want 3\n%s", code, out)
	}

	clean := filepath.Join(root, "clean")
	other := filepath.Join(root, "other")
	if code, _ := runCLI(t, "--scenario", "clean", "--seed", "41", "--events", "30", "--damage-at", "1", "--out", clean); code != 0 {
		t.Fatal("clean setup failed")
	}
	code, out := runCLI(t, "--scenario", "clean", "--seed", "42", "--events", "30", "--damage-at", "1", "--out", other, "--compare", clean)
	if code != 4 {
		t.Fatalf("hash mismatch code=%d want 4\n%s", code, out)
	}
	if !strings.Contains(out, "error_kind=integrity") {
		t.Fatalf("expected integrity error kind:\n%s", out)
	}
}

func readTestHashes(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "hashes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]string
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	return h["frames_sha256"] + "|" + h["manifest_sha256"] + "|" + h["state_sha256"]
}
