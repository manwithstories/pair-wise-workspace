package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mp4probe/internal/seek"
)

func TestParseTimecode(t *testing.T) {
	cases := map[string]int64{
		"00:00:00.000": 0,
		"00:07:31.240": 451240,
		"01:00:00.000": 3600000,
		"00:00:10.5":   10500,
		"13:59:59.999": 50399999,
	}
	for in, want := range cases {
		got, err := ParseTimecode(in)
		if err != nil || got != want {
			t.Fatalf("ParseTimecode(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1:2", "00:61:00", "00:00:60", "aa:00:00", "00:00:01.1234"} {
		if _, err := ParseTimecode(bad); err == nil {
			t.Fatalf("ParseTimecode(%q) should fail", bad)
		}
	}
}

func TestValidatePath(t *testing.T) {
	for _, ok := range []string{"work/fixtures", "a/b/c.mp4", "./x.mp4", "work/../work/x"} {
		if err := ValidatePath(ok); err != nil {
			t.Fatalf("ValidatePath(%q) rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../escape", "a/../../b", "/tmp/x"} {
		if err := ValidatePath(bad); err == nil {
			t.Fatalf("ValidatePath(%q) should fail", bad)
		}
	}
}

// TestEndToEnd runs the full pipeline in a temp working directory:
// fixture -> index -> seek -> export -> verify.
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	if err := RunFixture([]string{"--out", "work/fixtures", "--cases", "normal,editlist", "--seed", "7"}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := RunIndex([]string{"--in", "work/fixtures/normal.mp4", "--out", "work/index.json"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := RunSeek([]string{"--in", "work/fixtures/editlist.mp4", "--at", "00:07:31.240", "--track", "1", "--frames", "8"}); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if err := RunExport([]string{"--in", "work/fixtures/normal.mp4", "--from", "00:00:10.000", "--to", "00:00:12.000", "--track", "1", "--out", "work/samples.json"}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if err := RunVerify([]string{"--in", "work/fixtures/normal.mp4", "--index", "work/index.json", "--expect", "work/samples.json"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestLandingAccuracy checks the exact landing numbers of the
// acceptance scenario against the editlist fixture.
func TestLandingAccuracy(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if err := RunFixture([]string{"--out", "f", "--cases", "editlist", "--seed", "20261115"}); err != nil {
		t.Fatal(err)
	}
	idx, f, err := loadIndex(filepath.Join("f", "editlist.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := idx.Track(1)
	loc, err := seek.NewLocator(idx, 1, trackMapper(tr), f)
	if err != nil {
		t.Fatal(err)
	}
	res, err := loc.Locate(451240*90000/1000, 8)
	if err != nil {
		t.Fatal(err)
	}
	if res.LandingPts != 40611600 {
		t.Fatalf("landing_pts=%d, want 40611600", res.LandingPts)
	}
	if res.FrameType != "sync" || res.DecodePathLength != 1 {
		t.Fatalf("type=%s path=%d", res.FrameType, res.DecodePathLength)
	}
	if len(res.Frames) != 8 || len(res.SHA256) != 64 {
		t.Fatalf("frames=%d sha=%s", len(res.Frames), res.SHA256)
	}
}

func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if err := RunFixture([]string{"--out", "f", "--cases", "normal", "--seed", "1"}); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join("f", "normal.mp4")
	// Missing parameter -> 2.
	if err := RunSeek([]string{"--in", in}); ExitCode(err) != ExitUsage {
		t.Fatalf("missing param code=%d", ExitCode(err))
	}
	// Bad timecode -> 2.
	if err := RunSeek([]string{"--in", in, "--at", "7:31"}); ExitCode(err) != ExitUsage {
		t.Fatalf("bad timecode code=%d", ExitCode(err))
	}
	// Unknown track -> 2.
	if err := RunSeek([]string{"--in", in, "--at", "00:00:01.000", "--track", "99"}); ExitCode(err) != ExitUsage {
		t.Fatalf("bad track code=%d", ExitCode(err))
	}
	// Beyond duration -> 5.
	if err := RunSeek([]string{"--in", in, "--at", "09:00:00.000"}); ExitCode(err) != ExitLocate {
		t.Fatalf("beyond duration code=%d err=%v", ExitCode(err), err)
	}
	// Missing file -> 7.
	if err := RunIndex([]string{"--in", "f/nope.mp4"}); ExitCode(err) != ExitIO {
		t.Fatalf("missing file code=%d", ExitCode(err))
	}
	// Absolute path rejected -> 2.
	if err := RunIndex([]string{"--in", "/etc/hosts"}); ExitCode(err) != ExitUsage {
		t.Fatalf("absolute path code=%d", ExitCode(err))
	}
	// Corrupt index -> 4.
	if err := os.MkdirAll("work", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("work/bad.json", []byte(`{"version":99,"tracks":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("work/empty.json", []byte(`{"samples":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := RunVerify([]string{"--in", in, "--index", "work/bad.json", "--expect", "work/empty.json"})
	if ExitCode(err) != ExitConsistency {
		t.Fatalf("corrupt index code=%d", ExitCode(err))
	}
	var ee *ExitError
	if !errors.As(err, &ee) && err == nil {
		t.Fatal("expected error")
	}
}
