package report

import (
	"bytes"
	"path/filepath"
	"testing"

	"turncode/internal/config"
	"turncode/internal/quality"
	"turncode/internal/state"
)

func testReport(meanShift float64) Report {
	acc := quality.NewAccumulator(10)
	for i := 0; i < 20; i++ {
		acc.Add(i, 40+meanShift+float64(i%3))
	}
	spec := config.Spec{TargetBPS: 1200000, FPS: 30, Frames: 20}
	return Build(Input{Spec: spec, BitstreamBytes: 100000, Checkpoint: state.Checkpoint{Quality: *acc}})
}

func TestBuildWriteLoadCompare(t *testing.T) {
	base := testReport(0)
	cur := testReport(-1)
	path := filepath.Join(t.TempDir(), "report.json")
	if err := WriteJSON(path, base); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	reg := ApplyBaseline(&cur, loaded)
	if !reg.Regression || len(cur.DegradedSegments) == 0 {
		t.Fatalf("expected regression: %+v", reg)
	}
	var text bytes.Buffer
	if err := WriteText(&text, cur); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(text.Bytes(), []byte("regression: true")) {
		t.Fatal("text report missing regression")
	}
}
