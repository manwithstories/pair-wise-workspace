package main

import (
	"context"
	"crypto/sha256"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"archive/internal/errs"
	"archive/internal/index"
	"archive/internal/mp4"
	"archive/internal/testdata"
)

func testCtx() context.Context { return context.Background() }
func testRng() *rand.Rand      { return rand.New(rand.NewSource(41)) }

func genFixtures(t *testing.T) *testdata.Fixtures {
	t.Helper()
	f, err := testdata.Generate(testdata.Config{
		Seed: 41, BurstLoss: 7, Reorder: 12, Bandwidth: []float64{1, 8, 2},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func findAsset(f *testdata.Fixtures, name string) testdata.Asset {
	for _, a := range f.Good {
		if a.Name == name {
			return a
		}
	}
	return testdata.Asset{}
}

// A 32-bit clock that wraps mid-asset must come out monotonic on the
// common timebase, with tracks sharing one presentation origin.
func TestWrapAssetMonotonic(t *testing.T) {
	f := genFixtures(t)
	a := findAsset(f, "wrap-hls")
	if a.Name == "" {
		t.Fatal("wrap-hls fixture missing")
	}
	outDir := t.TempDir()
	o := processAsset(testCtx(), assetInput{a.Name, a.Kind, a.Dir, a.ManifestFile}, outDir, 1<<20, testRng())
	if o.rep.Status != "ok" {
		t.Fatal(o.rep.Error)
	}
	out, err := os.ReadFile(filepath.Join(outDir, "wrap-hls.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := mp4.Demux("wrap-hls", "out.mp4", out)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range tracks {
		if len(tr.Samples) < 2 {
			continue
		}
		for i := 1; i < len(tr.Samples); i++ {
			if tr.Samples[i].DTS <= tr.Samples[i-1].DTS {
				t.Fatalf("track %d DTS not monotonic at %d: %d after %d",
					tr.ID, i, tr.Samples[i].DTS, tr.Samples[i-1].DTS)
			}
		}
		if tr.Samples[0].DTS != 0 {
			t.Fatalf("track %d does not start at the common origin: %d", tr.ID, tr.Samples[0].DTS)
		}
	}
}

// A resumed run must rebuild a byte-identical output after a chunk of
// the committed file is damaged.
func TestResumeRebuildsIdenticalOutput(t *testing.T) {
	f := genFixtures(t)
	a := findAsset(f, "big-dash")
	if a.Name == "" {
		t.Fatal("big-dash fixture missing")
	}
	outDir := t.TempDir()
	const chunk = 1 << 20
	in := assetInput{a.Name, a.Kind, a.Dir, a.ManifestFile}
	if o := processAsset(testCtx(), in, outDir, chunk, testRng()); o.rep.Status != "ok" {
		t.Fatal(o.rep.Error)
	}
	outPath := filepath.Join(outDir, "big-dash.mp4")
	orig, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(orig)
	if len(orig) <= chunk {
		t.Fatalf("fixture too small to exercise chunking: %d bytes", len(orig))
	}
	// damage a byte inside the last committed chunk so the resume must
	// roll back to the previous checkpoint, not to zero
	cp, err := index.Load(outPath + ".index.json")
	if err != nil || cp == nil || len(cp.Chunks) < 2 {
		t.Fatalf("index not usable for resume test: %v %+v", err, cp)
	}
	last := cp.Chunks[len(cp.Chunks)-1]
	start := cp.Chunks[len(cp.Chunks)-2].ByteEnd
	damaged := append([]byte(nil), orig...)
	damaged[start+(last.ByteEnd-start)/2] ^= 0xff
	if err := os.WriteFile(outPath, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	o := processAsset(testCtx(), in, outDir, chunk, testRng())
	if o.rep.Status != "ok" {
		t.Fatal(o.rep.Error)
	}
	if o.resumePos <= 0 {
		t.Fatal("pipeline did not resume from a checkpoint")
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != want {
		t.Fatal("rebuilt output differs from the original")
	}
	// a third run with everything in place must be a no-op
	if o := processAsset(testCtx(), in, outDir, chunk, testRng()); o.rep.Status != "ok" {
		t.Fatalf("complete rerun: %s", o.rep.Error)
	}
	again, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(again) != want {
		t.Fatal("complete rerun changed the output")
	}
}

// Each corrupt fixture must fail with the expected error kind, and the
// message must locate the failure.
func TestCorruptFixturesDetected(t *testing.T) {
	f := genFixtures(t)
	outDir := t.TempDir()
	for _, c := range f.Corrupt {
		in := assetInput{c.Name, c.Kind, c.Dir, c.ManifestFile}
		o := processAsset(testCtx(), in, outDir, 1<<20, testRng())
		if o.rep.Status != "failed" {
			t.Fatalf("%s: expected failure, got %s", c.Name, o.rep.Status)
		}
		if o.errKind != c.Want {
			t.Fatalf("%s: want %s, got %s (%s)", c.Name, c.Want, o.errKind, o.rep.Error)
		}
		if o.rep.Error == "" {
			t.Fatalf("%s: failure has no message", c.Name)
		}
	}
}

// A blocked output path must surface as a locatable IO error.
func TestOutputBlockedIsIOError(t *testing.T) {
	f := genFixtures(t)
	a := f.Good[0]
	blocked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(blocked, a.Name+".mp4"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := processAsset(testCtx(), assetInput{a.Name, a.Kind, a.Dir, a.ManifestFile}, blocked, 1<<20, testRng())
	if o.rep.Status != "failed" || o.errKind != string(errs.KindIO) {
		t.Fatalf("want io failure, got %q (%s)", o.errKind, o.rep.Error)
	}
}
