package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediasync/internal/testdata"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectCleanSubtitle(t *testing.T) {
	path := writeTemp(t, "clean.srt", testdata.GenerateSRT(testdata.SubtitleClean, 1))
	code, out, _ := invoke(t, "inspect", "--subtitle", path)
	if code != 0 {
		t.Fatalf("exit = %d, out = %s", code, out)
	}
	if !strings.Contains(out, "cues=5") {
		t.Errorf("output = %s", out)
	}
}

func TestInspectRecoverableDamage(t *testing.T) {
	cases := map[string]testdata.SubtitleKind{
		"bad.srt":     testdata.SubtitleBadTime,
		"trunc.srt":   testdata.SubtitleTruncated,
		"overlap.srt": testdata.SubtitleOverlap,
	}
	for name, kind := range cases {
		path := writeTemp(t, name, testdata.GenerateSRT(kind, 1))
		code, out, _ := invoke(t, "inspect", "--subtitle", path)
		if code != 2 {
			t.Errorf("%s: exit = %d, want 2", name, code)
		}
		if !strings.Contains(out, "@") {
			t.Errorf("%s: issues printed without byte offsets: %s", name, out)
		}
	}
}

func TestInspectMP4Faults(t *testing.T) {
	spec := testdata.DefaultSpec(3)
	spec.Fault = testdata.FaultChunkOutOfFile
	path := writeTemp(t, "bad.mp4", testdata.GenerateMP4(spec))
	code, out, _ := invoke(t, "inspect", "--mp4", path)
	if code != 2 {
		t.Fatalf("exit = %d, want 2; out=%s", code, out)
	}
	if !strings.Contains(out, "out_of_range") {
		t.Errorf("output = %s", out)
	}

	trunc := testdata.DefaultSpec(3)
	trunc.Fault = testdata.FaultTruncatedMoov
	path = writeTemp(t, "trunc.mp4", testdata.GenerateMP4(trunc))
	code, _, _ = invoke(t, "inspect", "--mp4", path)
	if code != 1 {
		t.Errorf("truncated moov exit = %d, want 1", code)
	}
}

func TestInspectRejectsTraversalAndLimits(t *testing.T) {
	if code, _, _ := invoke(t, "inspect", "--subtitle", "../../etc/passwd"); code != 1 {
		t.Errorf("traversal exit = %d, want 1", code)
	}
	big := make([]byte, maxSubtitleFile+1)
	path := writeTemp(t, "big.srt", big)
	if code, _, _ := invoke(t, "inspect", "--subtitle", path); code != 1 {
		t.Errorf("oversized exit = %d, want 1", code)
	}
	if code, _, _ := invoke(t, "inspect"); code != 1 {
		t.Errorf("no input exit = %d, want 1", code)
	}
}
