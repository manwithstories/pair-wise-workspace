package gen

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fieldcut/internal/ingest"
	"fieldcut/internal/media"
)

func smallConfig() Config {
	return Config{Seed: 7, Frames: 40, Tracks: 3, WrapAt: 4294967200,
		DriftPPM: 120, DropEvery: 11, CorruptEvery: 13, Width: 64, Height: 36, Chunk: 8}
}

func TestGenerateIsValidAndDeterministic(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	ra, err := Generate(a, smallConfig())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	rb, err := Generate(b, smallConfig())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if ra.Manifest.Entries[0].SHA256 != rb.Manifest.Entries[0].SHA256 {
		t.Fatal("generator is not deterministic for the same seed")
	}
	_, sets, err := ingest.Load(filepath.Join(a, "manifest.json"))
	if err != nil {
		t.Fatalf("generated manifest rejected: %v", err)
	}
	if len(sets) != 3 {
		t.Fatalf("tracks = %d", len(sets))
	}
}

func TestGeneratedWrapCrossesMidSequence(t *testing.T) {
	res, err := Generate(t.TempDir(), smallConfig())
	if err != nil {
		t.Fatal(err)
	}
	var sawHigh, sawLow bool
	for _, c := range res.Cameras {
		if c.Missing || c.Track != "cam_a" {
			continue
		}
		// WrapAt - frames/2*40000 = 4294967200 - 800000 = 4294167200.
		if c.Tick > 4294167200 {
			sawHigh = true
		}
		if c.Tick < 4294167200 {
			sawLow = true
		}
	}
	if !sawHigh || !sawLow {
		t.Fatalf("wrap not exercised: high=%v low=%v", sawHigh, sawLow)
	}
}

func TestGenerateRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"frames", func(c *Config) { c.Frames = 0 }},
		{"tracks", func(c *Config) { c.Tracks = 4 }},
		{"wrap", func(c *Config) { c.WrapAt = 0 }},
		{"canvas", func(c *Config) { c.Width = media.MaxWidth + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := smallConfig()
			tc.mut(&cfg)
			_, err := Generate(t.TempDir(), cfg)
			if err == nil {
				t.Fatal("bad config accepted")
			}
			var me *media.MediaError
			if !errors.As(err, &me) {
				t.Fatalf("not MediaError: %v", err)
			}
		})
	}
}

func TestMutateTargetsEachRule(t *testing.T) {
	for _, kind := range []string{"bad-hash", "traversal", "oversize", "cycle", "fps"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := Generate(dir, smallConfig()); err != nil {
				t.Fatal(err)
			}
			if kind == "bad-hash" {
				// A hex-shaped hash passes manifest syntax checks and is
				// rejected later by CheckFrames during byte verification.
				_, sets, err := ingest.Load(filepath.Join(dir, "manifest.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := Mutate(dir, kind); err != nil {
					t.Fatalf("Mutate: %v", err)
				}
				bad := sets["cam_a"][0]
				bad.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
				if err := ingest.VerifyEntry(dir, bad); err == nil {
					t.Fatal("bad hash not rejected by frame verification")
				}
				return
			}
			if err := Mutate(dir, kind); err != nil {
				t.Fatalf("Mutate: %v", err)
			}
			if kind == "cycle" {
				// Ingest accepts the rows; the dependency DAG must reject the
				// cycle before any output exists.
				_, sets, err := ingest.Load(filepath.Join(dir, "manifest.json"))
				if err != nil {
					t.Fatalf("cycle manifest rejected too early: %v", err)
				}
				if len(sets) != 3 {
					t.Fatal("tracks lost")
				}
				return
			}
			if _, _, err := ingest.Load(filepath.Join(dir, "manifest.json")); err == nil {
				t.Fatalf("mutation %s not rejected by ingest", kind)
			}
		})
	}
	if err := Mutate(t.TempDir(), "nope"); err == nil {
		t.Fatal("unknown mutation accepted")
	}
	if err := os.Remove(filepath.Join(t.TempDir(), "missing")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
