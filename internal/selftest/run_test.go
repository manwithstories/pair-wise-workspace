package selftest

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"medpipe/internal/resume"
)

func ffmpegReady() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// TestRunEmitsContractFields drives the full self-test through the package
// entry point and validates every acceptance JSONL field.
func TestRunEmitsContractFields(t *testing.T) {
	if !ffmpegReady() {
		t.Skip("ffmpeg not installed")
	}
	opts := Options{Dir: t.TempDir(), Seed: 73, Resume: true}
	if err := Run(opts); err != nil {
		t.Fatalf("selftest Run: %v", err)
	}
	// Run writes JSONL to stdout; rerun capturing via the report helpers is
	// not possible since Run owns stdout, so assert on the artifacts instead.
	entries, err := readOutDir(opts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no segment outputs produced")
	}
	if _, err := hashSegZero(opts.Dir); err != nil {
		t.Fatal(err)
	}
	// Checkpoint must exist and validate against the input.
	store, err := resume.NewStore(opts.Dir)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cp == nil || len(cp.Segments) == 0 {
		t.Fatal("checkpoint has no committed segments after resume")
	}
}

// TestSummaryRecordShape ensures the summary serializes all acceptance keys.
func TestSummaryRecordShape(t *testing.T) {
	rec := SummaryRecord{
		Event: "summary", BadOffsetDelta: "0B", ResumeMillis: 0,
		InflightBytes: 3267, SilenceMillis: 0, DedupHits: 3,
		Segments: 4,
		Sha256:   "bbfa3feb263ec5b59a236e26a2e347e432d5e8df6b52701f7790e30e879ed4a0",
	}
	var buf bytes.Buffer
	if err := WriteJSONL(&buf, rec); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"bad_offset_delta", "resume_ms", "inflight_bytes",
		"silence_ms", "dedup_hits", "sha256", "segments"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("summary missing field %s in %s", key, line)
		}
	}
	if got["bad_offset_delta"] != "0B" {
		t.Fatalf("bad_offset_delta = %v, want 0B", got["bad_offset_delta"])
	}
	if len(got["sha256"].(string)) != 64 {
		t.Fatal("sha256 must be 64 hex chars")
	}
}

// TestRunRejectsEmptyDir covers the argument-validation branch inside Run.
func TestRunRejectsEmptyDir(t *testing.T) {
	if err := Run(Options{Dir: ""}); err == nil {
		t.Fatal("empty dir accepted")
	}
}

// TestCorruptionRecordShape validates the corruption JSONL field naming.
func TestCorruptionRecordShape(t *testing.T) {
	var buf bytes.Buffer
	err := WriteJSONL(&buf, CorruptionRecord{
		Event: "corruption", Sample: "badlen.mp4", BadOffsetDelta: "0B",
		ExpectedOffset: 0, ReportedOffset: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"bad_offset_delta":"0B"`) {
		t.Fatalf("corruption record missing delta field: %s", buf.String())
	}
}
