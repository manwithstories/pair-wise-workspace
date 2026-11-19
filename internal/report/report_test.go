package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"review/internal/issues"
)

func TestExitCodes(t *testing.T) {
	clean := Summary{}
	if clean.ExitCode() != 0 {
		t.Fatal("clean run must exit 0")
	}
	withIssues := Summary{Issues: []issues.Issue{{Kind: issues.Overlap}}}
	if withIssues.ExitCode() != 2 {
		t.Fatal("located errors must exit 2")
	}
	failed := Summary{VerificationFailed: true}
	if failed.ExitCode() != 3 {
		t.Fatal("verification failure must exit 3")
	}
}

func TestJSONKeys(t *testing.T) {
	s := Summary{Status: "complete", DamageOffsetBytes: 50844, RecoveredBlocks: 3,
		ResumeMs: 12, FramesExported: 3840, SHA256: strings.Repeat("a", 64),
		AlignmentErrorFrames: 2}
	var buf bytes.Buffer
	s.PrintJSON(&buf)
	var m map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"damage_offset_bytes", "recovered_blocks", "resume_ms",
		"frames_exported", "sha256", "alignment_error_frames"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %s", k, buf.String())
		}
	}
}
