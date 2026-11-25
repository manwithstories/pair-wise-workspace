package relay

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCommand executes a command against a fresh output directory and returns
// the exit code and stdout.
func runCommand(t *testing.T, command string, opts Options) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := Run(context.Background(), command, opts, &buf)
	return code, buf.String()
}

func TestSelfcheckPasses(t *testing.T) {
	dir := t.TempDir()
	code, out := runCommand(t, "selfcheck", Options{
		Seed: 41, Workers: 4, Inflight: 12, Out: filepath.Join(dir, "out"),
	})
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitOK, out)
	}
	// Every acceptance field must be present with its contract value.
	want := []string{
		"roundtrip_rate=1.000",
		"keyframe_hits=48/48",
		"frames=120",
		"segments=6",
		"sha256_match=6/6",
		"wraps=2",
		"vfr_intervals=3",
		"recovered=2/2",
		"cancelled=1",
		"exit=0",
	}
	for _, w := range want {
		if !containsLine(out, w) {
			t.Errorf("missing %q in output:\n%s", w, out)
		}
	}
	// PNG shard hashes are 64-hex.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "png_shard=") {
			hash := line[strings.Index(line, ".")+1:]
			if len(hash) != 64 {
				t.Errorf("shard hash %q is not 64 hex chars", hash)
			}
		}
	}
	// A manifest must have been written with one line per frame.
	manifest, err := os.ReadFile(filepath.Join(dir, "out", "SHA256SUMS"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if n := strings.Count(string(manifest), "\n"); n != 120 {
		t.Fatalf("manifest lines = %d, want 120", n)
	}
}

func TestDemoIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	opts := Options{Seed: 41, Workers: 4, Inflight: 12}
	optsA := opts
	optsA.Out = a
	optsB := opts
	optsB.Out = b
	if code, out := runCommand(t, "demo", optsA); code != ExitOK {
		t.Fatalf("demo a exit = %d\n%s", code, out)
	}
	if code, out := runCommand(t, "demo", optsB); code != ExitOK {
		t.Fatalf("demo b exit = %d\n%s", code, out)
	}
	ma, err := os.ReadFile(filepath.Join(a, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read a: %v", err)
	}
	mb, err := os.ReadFile(filepath.Join(b, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read b: %v", err)
	}
	if !bytes.Equal(ma, mb) {
		t.Fatal("two demo runs produced different SHA256SUMS")
	}
}

func TestSeedChangesOutput(t *testing.T) {
	dir := t.TempDir()
	optsA := Options{Seed: 41, Workers: 2, Inflight: 4, Out: filepath.Join(dir, "a")}
	optsB := Options{Seed: 42, Workers: 2, Inflight: 4, Out: filepath.Join(dir, "b")}
	runCommand(t, "demo", optsA)
	runCommand(t, "demo", optsB)
	ma, _ := os.ReadFile(filepath.Join(dir, "a", "SHA256SUMS"))
	mb, _ := os.ReadFile(filepath.Join(dir, "b", "SHA256SUMS"))
	if bytes.Equal(ma, mb) {
		t.Fatal("different seeds produced identical manifests")
	}
}

func TestInvalidOptionsExitBadInput(t *testing.T) {
	dir := t.TempDir()
	for _, opts := range []Options{
		{Seed: 1, Workers: 0, Inflight: 1, Out: filepath.Join(dir, "a")},
		{Seed: 1, Workers: 1, Inflight: 0, Out: filepath.Join(dir, "b")},
		{Seed: 1, Workers: 1, Inflight: 1, Out: ""},
	} {
		if code, _ := runCommand(t, "selfcheck", opts); code != ExitBadInput {
			t.Fatalf("exit = %d, want %d for %+v", code, ExitBadInput, opts)
		}
	}
}

func TestOutputDirectoryGuard(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// A foreign file must be refused so we never clobber unrelated output.
	if err := os.WriteFile(filepath.Join(out, "precious.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := runCommand(t, "selfcheck", Options{
		Seed: 1, Workers: 1, Inflight: 1, Out: out,
	}); code != ExitBadInput {
		t.Fatalf("exit = %d, want %d with foreign file present", code, ExitBadInput)
	}
	if _, err := os.Stat(filepath.Join(out, "precious.txt")); err != nil {
		t.Fatal("existing file was removed")
	}
}

func TestCancelPathExitsThree(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	code, stdout := runCommand(t, "selfcheck", Options{
		Seed: 41, Workers: 2, Inflight: 3, Out: out, CancelAfter: 5,
	})
	if code != ExitCancelled {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitCancelled, stdout)
	}
	if _, err := os.Stat(filepath.Join(out, "checkpoint.json")); err != nil {
		t.Fatalf("checkpoint not written: %v", err)
	}
	if !containsLine(stdout, "cancelled=1") {
		t.Fatalf("cancelled field not reported:\n%s", stdout)
	}
}

func containsLine(haystack, needle string) bool {
	for _, line := range strings.Split(haystack, "\n") {
		if line == needle {
			return true
		}
	}
	return false
}
