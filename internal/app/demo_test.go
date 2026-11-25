package app

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediaproc/internal/sample"
)

func testOptions(dir, fault string, recover bool) Options {
	return Options{
		Seed:         41,
		Out:          dir,
		MaxMemory:    8 << 20,
		Queue:        4,
		SlowConsumer: 20 * time.Millisecond,
		Fault:        fault,
		Recover:      recover,
	}
}

func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func assertSameTreeHashes(t *testing.T, a, b string) {
	t.Helper()
	ha, hb := hashTree(t, a), hashTree(t, b)
	if len(ha) != len(hb) {
		t.Fatalf("file count mismatch: %d vs %d", len(ha), len(hb))
	}
	for name, hash := range ha {
		if hb[name] != hash {
			t.Fatalf("artifact %s hash differs:\n%s\n%s", name, hash, hb[name])
		}
	}
}

func TestNormalDemoMetricsAndReproducibleArtifacts(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	r1, err := RunDemo(testOptions(dir1, "", false))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := RunDemo(testOptions(dir2, "", false))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Report{r1, r2} {
		if r.Segments != 6 || r.HashOK != 6 || r.HashTotal != 6 {
			t.Fatalf("segment/hash counts wrong: %+v", r)
		}
		if r.RoundtripPercent != "100.00%" || r.KeyframePercent != "100.00%" {
			t.Fatalf("rates wrong: %+v", r)
		}
		if r.Silence != 2 || r.Dedup != 1 {
			t.Fatalf("silence/dedup wrong: %+v", r)
		}
		if r.PeakQueue != 4 {
			t.Fatalf("peak queue=%d want 4", r.PeakQueue)
		}
		if !r.MemoryOK() || r.PeakMemory > r.MemoryLimit {
			t.Fatalf("memory invariant failed: %+v", r)
		}
	}
	assertSameTreeHashes(t, dir1, dir2)
}

func TestTruncatedRecoveryOffsetsAndReproducibleArtifacts(t *testing.T) {
	fx := sample.Generate(sample.Config{Seed: 41, Fault: "truncated-block"})
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	opts := testOptions(dir1, "truncated-block", true)
	opts.SlowConsumer = 0
	r1, err := RunDemo(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.Out = dir2
	r2, err := RunDemo(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*Report{r1, r2} {
		if r.Recoveries != 1 {
			t.Fatalf("recoveries=%d want 1", r.Recoveries)
		}
		if r.FirstErrorOffset != fx.FaultOffset {
			t.Fatalf("offset=%d want fixture offset %d", r.FirstErrorOffset, fx.FaultOffset)
		}
		if len(r.RecoveryOffsets) != 1 || r.RecoveryOffsets[0] <= r.FirstErrorOffset {
			t.Fatalf("recovery offsets=%v", r.RecoveryOffsets)
		}
		if r.Segments != 5 || r.HashOK != 5 || r.HashTotal != 5 {
			t.Fatalf("recovered counts wrong: %+v", r)
		}
	}
	if r1.RecoveryOffsets[0] != r2.RecoveryOffsets[0] {
		t.Fatalf("recovery boundaries differ: %v vs %v", r1.RecoveryOffsets, r2.RecoveryOffsets)
	}
	assertSameTreeHashes(t, dir1, dir2)
}

func TestInvalidVINTRecovery(t *testing.T) {
	fx := sample.Generate(sample.Config{Seed: 41, Fault: "invalid-vint"})
	dir := t.TempDir()
	opts := testOptions(dir, "invalid-vint", true)
	opts.SlowConsumer = 0
	r, err := RunDemo(opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Recoveries != 1 || r.FirstErrorOffset != fx.FaultOffset || r.Segments != 5 {
		t.Fatalf("invalid VINT recovery report=%+v fixtureOffset=%d", r, fx.FaultOffset)
	}
}
