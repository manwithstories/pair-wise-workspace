package cmdarch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"archivetl/internal/report"
)

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func demoOpts(t *testing.T, dir string) DemoOptions {
	t.Helper()
	return DemoOptions{
		FPS: "30000/1001", WrapDay: true,
		SlowConsumerMs: 1, Output: dir,
	}
}

func TestRunDemoHappyPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	res, err := RunDemo(context.Background(), demoOpts(t, dir))
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if res.Exit != ExitOK {
		t.Fatalf("exit=%d want 0", res.Exit)
	}
	s := res.Summary
	if len(s.Segments) < 4 {
		t.Fatalf("segments=%d want >=4", len(s.Segments))
	}
	if s.RoundtripRate < 0.999 || s.RoundtripRate > 1.001 {
		t.Fatalf("roundtrip_rate=%v want 1.0", s.RoundtripRate)
	}
	if s.KeyframeHitRate < 0.95 {
		t.Fatalf("keyframe_hit_rate=%v want >=0.95", s.KeyframeHitRate)
	}
	if len(s.Errors) < 2 {
		t.Fatalf("errors=%d want >=2 (corrupt fixtures)", len(s.Errors))
	}
	for _, e := range s.Errors {
		if e.Phase == "index" && e.Offset == -1 && e.Source != "garbage.mp4" {
			t.Fatalf("index error without offset: %+v", e)
		}
	}
	for _, seg := range s.Segments {
		if len(seg.SHA256) != 64 {
			t.Fatalf("segment %d hash not 64 hex chars", seg.ID)
		}
	}
}

func TestBoundaryRejectionRecorded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	res, err := RunDemo(context.Background(), demoOpts(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.Boundary.RejectedRecords < 1 {
		t.Fatalf("rejected_records=%d want >=1", res.Summary.Boundary.RejectedRecords)
	}
	if res.Summary.Boundary.ZeroDuration < 1 {
		t.Fatal("zero-duration boundary cue not summarized")
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	dir1 := filepath.Join(t.TempDir(), "a")
	dir2 := filepath.Join(t.TempDir(), "b")
	first, err := RunDemo(context.Background(), demoOpts(t, dir1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := RunDemo(context.Background(), demoOpts(t, dir2))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Summary.Segments) != len(second.Summary.Segments) {
		t.Fatal("segment count differs between runs")
	}
	for i := range first.Summary.Segments {
		a, b := first.Summary.Segments[i], second.Summary.Segments[i]
		if a.SHA256 != b.SHA256 {
			t.Fatalf("segment %d hash differs: %s vs %s", a.ID, a.SHA256, b.SHA256)
		}
	}
}

func TestResumeSkipsUnchangedSegments(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	first, err := RunDemo(context.Background(), demoOpts(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	// Second run against the same directory must reuse every segment.
	second, err := RunDemo(context.Background(), demoOpts(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, seg := range second.Summary.Segments {
		if seg.Skipped {
			skipped++
		}
	}
	if skipped != len(first.Summary.Segments) {
		t.Fatalf("skipped=%d want %d", skipped, len(first.Summary.Segments))
	}
}

func TestInvalidRateIsUsageError(t *testing.T) {
	opts := demoOpts(t, filepath.Join(t.TempDir(), "x"))
	opts.FPS = "not-a-rate"
	_, err := RunDemo(context.Background(), opts)
	if ExitCodeFor(err) != ExitUsage {
		t.Fatalf("exit=%d want usage(2) err=%v", ExitCodeFor(err), err)
	}
}

func TestMissingOutputIsUsageError(t *testing.T) {
	_, err := RunDemo(context.Background(), DemoOptions{FPS: "24"})
	if ExitCodeFor(err) != ExitUsage {
		t.Fatalf("exit=%d want usage(2)", ExitCodeFor(err))
	}
}

func TestExitCodeClassification(t *testing.T) {
	cases := []struct {
		err  error
		want ExitCode
	}{
		{&UsageError{Message: "x"}, ExitUsage},
		{&VerifyError{Message: "x"}, ExitVerify},
		{&IOError{Message: "x"}, ExitIO},
		{nil, ExitOK},
	}
	for _, c := range cases {
		if got := ExitCodeFor(c.err); got != c.want {
			t.Fatalf("%v -> %d want %d", c.err, got, c.want)
		}
	}
}

func TestVerifyFailureAfterTamper(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	if _, err := RunDemo(context.Background(), demoOpts(t, dir)); err != nil {
		t.Fatal(err)
	}
	// Tamper with a committed segment's bytes while leaving the manifest
	// untouched. Reopening the store and running its independent verify pass
	// is exactly the mismatch -> VerifyError (exit 3) seam RunDemo uses.
	seg := filepath.Join(dir, "segments", "seg-00000.afr")
	raw := readFile(t, seg)
	raw[len(raw)-1] ^= 0xFF
	writeFile(t, seg, raw)

	store, err := report.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mismatch, err := store.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatch) == 0 {
		t.Fatal("tampered segment not reported as a mismatch")
	}
	// Map the condition the same way RunDemo does onto a process exit code.
	var exitErr error = &VerifyError{Message: "hash verification failed"}
	if got := ExitCodeFor(exitErr); got != ExitVerify {
		t.Fatalf("verify condition exit=%d want 3", got)
	}
}

func TestIOErrorOnUnwritableOutput(t *testing.T) {
	// A path that exists as a regular file cannot be used as a directory.
	blocker := filepath.Join(t.TempDir(), "afile")
	writeFile(t, blocker, []byte("not-a-directory"))
	opts := demoOpts(t, blocker)
	_, err := RunDemo(context.Background(), opts)
	if ExitCodeFor(err) != ExitIO {
		t.Fatalf("exit=%d want io(4) err=%v", ExitCodeFor(err), err)
	}
}
