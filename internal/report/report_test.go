package report

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loudcheck/internal/batch"
	"loudcheck/internal/loudness"
)

func TestNaNSerializesAsNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	result := batch.FileResult{
		Path:   "silence.wav",
		Status: batch.StatusFail,
		Result: loudness.EngineResult{
			GateResult: loudness.GateResult{
				IntegratedLUFS:   math.NaN(),
				RelativeGateLUFS: math.NaN(),
				ShortTerm:        loudness.ShortTerm{MaxLUFS: math.NaN()},
			},
			TruePeakDBTP: math.Inf(-1),
		},
	}
	if err := WriteJSON(path, "ebu-r128", []batch.FileResult{result}, batch.Summary{Total: 1}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "NaN") {
		t.Fatalf("NaN leaked into JSON: %s", text)
	}
	if !strings.Contains(text, `"integrated_lufs": null`) {
		t.Fatalf("expected null integrated_lufs: %s", text)
	}
}

func TestSafeFloat(t *testing.T) {
	if data, _ := SafeFloat(math.NaN()).MarshalJSON(); string(data) != "null" {
		t.Fatalf("NaN must marshal to null, got %s", data)
	}
	if data, _ := SafeFloat(-23).MarshalJSON(); string(data) != "-23" {
		t.Fatalf("normal float mismatch: %s", data)
	}
}
