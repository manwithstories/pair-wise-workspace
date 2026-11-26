package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/manifest"
	"mediad/internal/stream"
)

func generateSmall(t *testing.T) []Generated {
	t.Helper()
	cases, err := GenerateAll(t.TempDir(), 7, 48)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatalf("want 3 cases, got %d", len(cases))
	}
	return cases
}

func load(t *testing.T, dir string) *manifest.Manifest {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := manifest.Decode(f) // strict decode must accept fixtures
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestManifestHashes independently recomputes every frame file digest and
// compares it against the manifest entry.
func TestManifestHashes(t *testing.T) {
	for _, c := range generateSmall(t) {
		m := load(t, c.Dir)
		vt := m.Tracks[0]
		mismatch := map[int64]bool{}
		for _, fr := range vt.Frames {
			data, err := os.ReadFile(filepath.Join(c.Dir, fr.File))
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(data)) != fr.Bytes {
				t.Fatalf("%s frame %d: byte count %d != manifest %d",
					c.Name, fr.Seq, len(data), fr.Bytes)
			}
			if !stream.VerifySHA256(data, fr.SHA256) {
				mismatch[fr.Seq] = true
			}
		}
		switch c.Name {
		case "success", "edge":
			if len(mismatch) != 0 {
				t.Fatalf("%s: unexpected hash mismatches %v", c.Name, mismatch)
			}
		case "corrupt":
			if len(mismatch) != 1 || !mismatch[12] {
				t.Fatalf("corrupt: want hash mismatch at seq 12, got %v", mismatch)
			}
		}
	}
}

// TestExpectations checks the generator's expected counts against the
// manifest structure an independent reader would see.
func TestExpectations(t *testing.T) {
	for _, c := range generateSmall(t) {
		m := load(t, c.Dir)
		total := len(m.Tracks[0].Frames)
		switch c.Name {
		case "success":
			if c.Expect.Frames != total || c.Expect.CorruptRanges != 0 {
				t.Fatalf("success: %+v (total %d)", c.Expect, total)
			}
			if c.Expect.AudioSamples["a1"] != 96000 {
				t.Fatalf("success audio: %+v", c.Expect.AudioSamples)
			}
		case "corrupt":
			if c.Expect.Frames != total-2 || c.Expect.CorruptRanges != 2 ||
				c.Expect.RecoveredFrames != 34 {
				t.Fatalf("corrupt: %+v (total %d)", c.Expect, total)
			}
		case "edge":
			if c.Expect.Frames != total-1 || c.Expect.CorruptRanges != 1 ||
				c.Expect.RecoveredFrames != 11 {
				t.Fatalf("edge: %+v (total %d)", c.Expect, total)
			}
			if len(c.Expect.AudioSamples) != 0 {
				t.Fatalf("edge audio must be absent: %+v", c.Expect.AudioSamples)
			}
		}
	}
}
