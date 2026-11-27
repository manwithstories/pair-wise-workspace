package runner

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fieldcut/internal/gen"
	"fieldcut/internal/media"
)

func generateSmall(t *testing.T) (inputDir string, cfg gen.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg = gen.Config{Seed: 3, Frames: 20, Tracks: 3, WrapAt: 4294967200,
		DriftPPM: 120, DropEvery: 7, CorruptEvery: 9, Width: 64, Height: 36, Chunk: 4}
	if _, err := gen.Generate(dir, cfg); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return dir, cfg
}

func TestRunVerifyRoundTrip(t *testing.T) {
	input, cfg := generateSmall(t)
	out := t.TempDir()
	res, err := Run(Options{OutDir: out, InputDir: input, ConfigHash: cfg.ManifestHash(), Workers: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Frames != cfg.Frames || res.Root == "" {
		t.Fatalf("result = %+v", res)
	}
	v, err := Verify(out, input)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if v.RootOnDisk != res.Root {
		t.Fatalf("verify root %s != run root %s", v.RootOnDisk, res.Root)
	}
	if v.Eligible != res.Eligible || v.Recovered != res.Recovered {
		t.Fatalf("recovery mismatch: verify=%d/%d run=%d/%d", v.Recovered, v.Eligible, res.Recovered, res.Eligible)
	}
}

func TestRunInterruptedThenResumeEqualsFull(t *testing.T) {
	input, cfg := generateSmall(t)
	resumed := t.TempDir()
	partial, err := Run(Options{OutDir: resumed, InputDir: input, ConfigHash: cfg.ManifestHash(), Workers: 1, Limit: 5})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("limit run err = %v, want ErrInterrupted", err)
	}
	if partial == nil || !partial.Interrupted {
		t.Fatalf("partial = %+v", partial)
	}
	final, err := Run(Options{OutDir: resumed, InputDir: input, ConfigHash: cfg.ManifestHash(), Workers: 2})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	clean := t.TempDir()
	ref, err := Run(Options{OutDir: clean, InputDir: input, ConfigHash: cfg.ManifestHash(), Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	if final.Root != ref.Root {
		t.Fatalf("resume root %s != full root %s", final.Root, ref.Root)
	}
}

func TestTamperedOutFailsVerify(t *testing.T) {
	input, cfg := generateSmall(t)
	out := t.TempDir()
	if _, err := Run(Options{OutDir: out, InputDir: input, ConfigHash: cfg.ManifestHash(), Workers: 1}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(out, "frames", "00007.png")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Verify(out, input)
	if err == nil {
		t.Fatal("tampered frame verified")
	}
	var me *media.MediaError
	if !errors.As(err, &me) || me.Kind != media.KindConflict {
		t.Fatalf("kind = %v", err)
	}
}

func TestBadInputFailsBeforeOutput(t *testing.T) {
	input, cfg := generateSmall(t)
	if err := os.WriteFile(filepath.Join(input, "manifest.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	_, err := Run(Options{OutDir: out, InputDir: input, ConfigHash: cfg.ManifestHash(), Workers: 1})
	var me *media.MediaError
	if !errors.As(err, &me) || me.Kind != media.KindInput {
		t.Fatalf("kind = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "frames")); !os.IsNotExist(statErr) {
		t.Fatal("output written before input validation")
	}
}
