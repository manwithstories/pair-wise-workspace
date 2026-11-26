package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestArgumentErrorsExitTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"unknown"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown command code=%d", code)
	}
	if code := run([]string{"demo", "--case", "bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("invalid case code=%d", code)
	}
	if code := run([]string{"demo", "--bad-flag"}, &out, &errOut); code != 2 {
		t.Fatalf("bad flag code=%d", code)
	}
}

func TestStrictBadSegmentExitsThree(t *testing.T) {
	var out, errOut bytes.Buffer
	dir := filepath.Join(t.TempDir(), "work")
	code := run([]string{"demo", "--case", "damaged", "--seed", "17", "--strict", "--workdir", dir}, &out, &errOut)
	if code != 3 {
		t.Fatalf("strict damaged code=%d stderr=%s stdout=%s", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "damaged_segments=1") {
		t.Fatalf("missing damaged count: %s", out.String())
	}
}

func TestIOFailureExitsFour(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"demo", "--case", "clean", "--workdir", "/dev/null/medcheck"}, &out, &errOut)
	if code != 4 {
		t.Fatalf("I/O failure code=%d stderr=%s", code, errOut.String())
	}
}

func TestDemoReportContainsRequiredFields(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"demo", "--case", "recover", "--seed", "17", "--workdir", t.TempDir()}, &out, &errOut)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"roundtrip=100.00%", "keyframe_hit=12/12", "cues=48/48",
		"recovered=4", "segment_sha256=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q\n%s", want, text)
		}
	}
}
