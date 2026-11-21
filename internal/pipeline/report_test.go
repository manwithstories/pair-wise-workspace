package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/remux"
)

func TestWriteManifestIsCompleteAndStable(t *testing.T) {
	rep, err := runPipeline(t, "clean", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.Validate(); err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	if err := WriteManifest(&a, rep); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(&b, rep); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Fatal("manifest is not deterministic")
	}
	var doc ManifestDocument
	if err := json.Unmarshal(a.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OutputSize != rep.OutputSize || doc.OutputSHA256 != rep.OutputSHA256 {
		t.Fatalf("output facts missing: %+v", doc)
	}
	if doc.Rates.RoundtripMatch != 100 || doc.Rates.SegmentHashMatch != 100 {
		t.Fatalf("rates=%+v", doc.Rates)
	}
	if doc.Queue.Peak > doc.Queue.Capacity {
		t.Fatalf("queue report: %+v", doc.Queue)
	}
	if len(doc.Samples) != len(rep.Manifest) {
		t.Fatalf("samples=%d manifest=%d", len(doc.Samples), len(rep.Manifest))
	}
	if len(doc.Keyframes) == 0 || len(doc.Tracks) == 0 {
		t.Fatalf("keyframes=%d tracks=%d", len(doc.Keyframes), len(doc.Tracks))
	}
	// Samples must be ordered by track then sample number for diffability.
	for i := 1; i < len(doc.Samples); i++ {
		prev, cur := doc.Samples[i-1], doc.Samples[i]
		if cur.Track < prev.Track || (cur.Track == prev.Track && cur.OutputSample < prev.OutputSample) {
			t.Fatalf("samples not sorted at %d", i)
		}
	}
}

func TestReportValidateRejectsEmptyOutput(t *testing.T) {
	rep := &Report{OutputSize: 0}
	if err := rep.Validate(); !errors.Is(err, fault.ErrOutput) {
		t.Fatalf("got %v", err)
	}
	rep = &Report{OutputSize: 10, Manifest: []remux.ManifestEntry{{Track: 1, Size: 0}}}
	if err := rep.Validate(); !errors.Is(err, fault.ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}
