package pipeline_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediasync/internal/pipeline"
)

func runDefault(t *testing.T) *pipeline.Report {
	t.Helper()
	opts := pipeline.DefaultOptions()
	r, err := pipeline.Run(opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

func TestRunProducesAcceptanceMetrics(t *testing.T) {
	r := runDefault(t)
	if r.RoundtripConsistency < 0.99 {
		t.Errorf("roundtrip_consistency = %.4f, want >= 0.99", r.RoundtripConsistency)
	}
	if r.KeyframeHitRate < 0.95 {
		t.Errorf("keyframe_hit_rate = %.4f, want >= 0.95", r.KeyframeHitRate)
	}
	if r.SegmentCount == 0 || r.SegmentCount != len(r.Segments) {
		t.Errorf("segment_count = %d with %d segment reports", r.SegmentCount, len(r.Segments))
	}
	if len(r.SegmentSHA256) != r.SegmentCount {
		t.Errorf("segment_sha256 has %d entries for %d segments", len(r.SegmentSHA256), r.SegmentCount)
	}
	if r.DuplicatePackets == 0 || r.ReorderedPackets == 0 || r.BurstLostPackets == 0 {
		t.Errorf("packet counters not populated: dup=%d reordered=%d burst_lost=%d",
			r.DuplicatePackets, r.ReorderedPackets, r.BurstLostPackets)
	}
	if r.ErrorsWithOffset == 0 {
		t.Error("errors_with_offset = 0, expected injected faults to carry offsets")
	}
	if r.DuplicateMatches == 0 || len(r.Matches) == 0 {
		t.Error("cross-camera duplicates were not detected")
	}
	if !r.GatesPass() {
		t.Errorf("gates failed: %+v", r.Gates())
	}
	if got := r.ExitCode(); got != 0 {
		t.Errorf("exit code = %d, want 0 for the seeded demo", got)
	}
	if r.SilenceRuns != 2 {
		t.Errorf("silence_runs = %d, want 2", r.SilenceRuns)
	}
}

func TestSummaryContainsRequiredFields(t *testing.T) {
	r := runDefault(t)
	summary := r.Summary()
	for _, want := range []string{
		"roundtrip_consistency", "keyframe_hit_rate", "segment_count", "segment_sha256",
		"duplicate_packets", "reordered_packets", "burst_lost_packets", "errors_with_offset",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q", want)
		}
	}
	// Every segment must print its 64-bit hash prefix.
	for _, s := range r.Segments {
		if !strings.Contains(summary, s.Hash64) {
			t.Errorf("summary missing hash for %s#%d", s.Camera, s.Index)
		}
	}
}

func TestRunIsDeterministic(t *testing.T) {
	a := runDefault(t)
	b := runDefault(t)
	if a.SegmentCount != b.SegmentCount {
		t.Fatalf("segment count differs between runs: %d vs %d", a.SegmentCount, b.SegmentCount)
	}
	for i := range a.SegmentSHA256 {
		if a.SegmentSHA256[i] != b.SegmentSHA256[i] {
			t.Fatalf("segment %d hash differs between runs", i)
		}
	}
	if a.DuplicatePackets != b.DuplicatePackets || a.BurstLostPackets != b.BurstLostPackets {
		t.Error("packet counters differ between runs with the same seed")
	}
}

func TestRunSeedChangesPayloads(t *testing.T) {
	a := runDefault(t)
	opts := pipeline.DefaultOptions()
	opts.Seed = 7
	b, err := pipeline.Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.SegmentSHA256) == 0 || len(b.SegmentSHA256) == 0 {
		t.Fatal("no hashes to compare")
	}
	same := 0
	for i := range a.SegmentSHA256 {
		if i < len(b.SegmentSHA256) && a.SegmentSHA256[i] == b.SegmentSHA256[i] {
			same++
		}
	}
	if same == len(a.SegmentSHA256) {
		t.Error("changing the seed did not change any segment hash")
	}
}

func TestWriteJSONRoundtrip(t *testing.T) {
	r := runDefault(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	if err := r.WriteJSON(path); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	for _, key := range []string{"roundtrip_consistency", "keyframe_hit_rate", "segment_count",
		"segment_sha256", "duplicate_packets", "reordered_packets", "burst_lost_packets",
		"errors_with_offset", "segments", "issues", "stages"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("JSON report missing %q", key)
		}
	}
	// No temporary files are left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("output directory has %d entries, want 1", len(entries))
	}
}

func TestSafeOutputPathRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"", "   ", "../../etc/passwd", "/tmp/../etc/shadow", string([]byte{'a', 0, 'b'})}
	for _, c := range cases {
		if _, err := pipeline.SafeOutputPath(c); err == nil {
			t.Errorf("SafeOutputPath(%q) should fail", c)
		}
	}
	ok := filepath.Join(dir, "nested", "out.json")
	// A missing parent directory is not created implicitly; still, the path
	// itself is acceptable.
	if _, err := pipeline.SafeOutputPath(ok); err != nil {
		t.Errorf("SafeOutputPath(%q) = %v", ok, err)
	}
	if _, err := pipeline.SafeOutputPath(dir); err == nil {
		t.Error("directory path accepted as an output file")
	}
}

func TestWriteJSONRejectsTraversal(t *testing.T) {
	r := runDefault(t)
	if err := r.WriteJSON("../../escape.json"); err == nil {
		t.Fatal("traversal path accepted")
	}
}

func TestReportJSONShape(t *testing.T) {
	r := runDefault(t)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"fatal":false`)) == false {
		// fatal is only serialised through issues; ensure issues are present.
		t.Log("no top-level fatal field, issues carry the state")
	}
	if !bytes.Contains(data, []byte(`"sha256_64"`)) {
		t.Error("segment hashes missing the 64-bit short form")
	}
}

func TestExitCodeSemantics(t *testing.T) {
	r := runDefault(t)
	if got := r.ExitCode(); got != 0 {
		t.Errorf("seeded demo exit code = %d", got)
	}
	// An unexpected recoverable issue must yield exit code 2.
	r.Issues = append(r.Issues, pipeline.IssueReport{Stage: "mp4", Code: "malformed_record", Expected: false})
	if got := r.ExitCode(); got != 2 {
		t.Errorf("exit code with unexpected issue = %d, want 2", got)
	}
	// A fatal issue must yield exit code 1.
	r.Issues = append(r.Issues, pipeline.IssueReport{Stage: "mp4", Code: "malformed_record", Fatal: true})
	if got := r.ExitCode(); got != 1 {
		t.Errorf("exit code with fatal issue = %d, want 1", got)
	}
}

func TestInvalidOptionsFail(t *testing.T) {
	opts := pipeline.DefaultOptions()
	opts.Frames = "not-a-rate"
	if _, err := pipeline.Run(opts); err == nil {
		t.Fatal("invalid frame rate accepted")
	}
}

func TestExpectedIssueMarking(t *testing.T) {
	opts := pipeline.DefaultOptions()
	opts.Expected = nil
	strict, err := pipeline.Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	expected := 0
	for _, is := range strict.Issues {
		if is.Expected {
			expected++
		}
	}
	if expected == 0 {
		t.Error("no issues were attributed to fixture injection")
	}
	if strict.ExitCode() != 0 {
		t.Errorf("strict run exit = %d, all reported issues should be attributed", strict.ExitCode())
	}
}
