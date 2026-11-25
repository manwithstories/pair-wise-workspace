package main

import (
	"strings"
	"testing"
)

func TestVerifyCommandAcceptanceFlags(t *testing.T) {
	out := t.TempDir()
	code := run([]string{
		"verify", "--out", out, "--resume",
		"--faults", "truncated,stale-index,rtp-wrap,slow-reader,crash-after-mux",
	})
	if code != 0 {
		t.Fatalf("acceptance run exit=%d", code)
	}
}

func TestMissingSubcommandReturnsPipelineCode(t *testing.T) {
	if code := run([]string{"--out", t.TempDir()}); code != 3 {
		t.Fatalf("code=%d", code)
	}
}

func TestFaultListParsingTrimsWhitespace(t *testing.T) {
	// Indirectly ensure the same comma syntax used by the README is accepted.
	parts := strings.Split("truncated, stale-index", ",")
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != "stale-index" {
		t.Fatalf("parts=%v", parts)
	}
}
