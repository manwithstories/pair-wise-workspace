package batch_test

import (
	"sort"
	"testing"

	"loudness/internal/batch"
	"loudness/internal/gen"
	"loudness/internal/report"
)

func TestBatchContinuesAfterErrorAndKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	if _, err := gen.WriteSuite(dir); err != nil {
		t.Fatal(err)
	}
	result, err := batch.Run(batch.Options{
		Dir: dir, Jobs: 4, Target: -23, Ceiling: -1, Tolerance: report.DefaultToleranceLU,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := report.Summary{Total: 8, Pass: 5, Fail: 2, Error: 1}
	if result.Summary != want {
		t.Fatalf("summary = %+v, want %+v", result.Summary, want)
	}
	names := make([]string, len(result.Rows))
	for i, row := range result.Rows {
		names[i] = row.File
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("rows not sorted: %v", names)
	}
	foundError := false
	for _, row := range result.Rows {
		if row.File == "corrupt.wav" {
			foundError = row.Err != "" && row.Verdict == "ERROR"
		}
	}
	if !foundError {
		t.Fatal("corrupt.wav error row missing")
	}
}

func TestBatchEmptyDirectory(t *testing.T) {
	if _, err := batch.Run(batch.Options{Dir: t.TempDir(), Jobs: 1, Target: -23, Ceiling: -1}); err == nil {
		t.Fatal("expected no-wav error")
	}
}
