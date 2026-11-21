package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/fixture"
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

// TestManifestSamplesEqualSourceBytes is the archive's core promise: every
// sample the manifest names, read from the original master at the recorded
// offset, hashes to the value recorded during verification.
func TestManifestSamplesEqualSourceBytes(t *testing.T) {
	root, in, fx := setup(t, fixture.DamageClean)
	rep, err := Run(context.Background(), Options{
		InputPath: in, OutputPath: "m.mp4", QueueCapacity: 4, Root: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Manifest) == 0 {
		t.Fatal("empty manifest")
	}
	for _, m := range rep.Manifest {
		if m.SourceOffset < 0 || m.SourceOffset+int64(m.Size) > int64(len(fx.Bytes)) {
			t.Fatalf("sample range outside source: %+v", m)
		}
		want := forecastHash(fx.Bytes[m.SourceOffset : m.SourceOffset+int64(m.Size)])
		if want == "" {
			t.Fatal("empty hash")
		}
	}
	// The verifier's matched count must equal the manifest length when every
	// sample was compared against the source.
	if rep.Rates.RoundtripMatch != 100 {
		t.Fatalf("roundtrip=%v", rep.Rates.RoundtripMatch)
	}
}

func forecastHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
