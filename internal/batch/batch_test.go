package batch

import (
	"os"
	"path/filepath"
	"testing"

	"loudcheck/internal/wavio"
)

func writeWAV(t *testing.T, path string, hdr wavio.WriteHeader, planar [][]float64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := wavio.WriteWAV(path, hdr, planar); err != nil {
		t.Fatal(err)
	}
}

func TestBatchStableOrderAndIsolation(t *testing.T) {
	dir := t.TempDir()
	header := wavio.WriteHeader{SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	writeWAV(t, filepath.Join(dir, "b_good.wav"), header, wavio.TonePlanar(48000, 6, 2, 1000, 0.07, 0.1))
	writeWAV(t, filepath.Join(dir, "a_good.wav"), header, wavio.TonePlanar(48000, 6, 2, 1000, 0.07, 0.2))
	if err := os.WriteFile(filepath.Join(dir, "c_broken.wav"), []byte("not a wav"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, workers := range []int{1, 4, 8} {
		results, summary, err := Run(dir, workers, EBUProfile())
		if err != nil {
			t.Fatal(err)
		}
		if results[0].Path != filepath.Join(dir, "a_good.wav") ||
			results[1].Path != filepath.Join(dir, "b_good.wav") ||
			results[2].Path != filepath.Join(dir, "c_broken.wav") {
			t.Fatalf("workers %d: not lexicographic: %v", workers, paths(results))
		}
		if summary.Total != 3 || summary.ReadError != 1 || summary.Pass+summary.Fail != 2 {
			t.Fatalf("workers %d bad summary %+v", workers, summary)
		}
		if results[2].Status != StatusReadError {
			t.Fatalf("broken file must be isolated as read error, got %s", results[2].Status)
		}
	}
}

func TestProfileVerdictBoundaries(t *testing.T) {
	profile := EBUProfile()
	loud := func(integrated, peak float64) (Status, string) {
		return profile.Evaluate(fakeResult(integrated, peak, true))
	}
	if status, _ := loud(-23, -2); status != StatusPass {
		t.Fatal("in-window should pass")
	}
	if status, _ := loud(-22.4, -2); status != StatusFail {
		t.Fatal("above +0.5 must fail")
	}
	if status, _ := loud(-23, -0.5); status != StatusFail {
		t.Fatal("true peak above -1 must fail")
	}
	if status, _ := profile.Evaluate(fakeResult(-23, -2, false)); status != StatusFail {
		t.Fatal("undefined loudness must fail")
	}
}

func paths(results []FileResult) []string {
	out := make([]string, len(results))
	for i, result := range results {
		out[i] = result.Path
	}
	return out
}
