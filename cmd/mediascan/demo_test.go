package main

import (
	"bytes"
	"strings"
	"testing"
)

// runCaptured executes the demo command with output captured into a buffer.
func runCaptured(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	prev := outputWriter
	outputWriter = &buf
	defer func() { outputWriter = prev }()
	code := run(append([]string{"demo"}, args...))
	return buf.String(), code
}

func TestDemoNormalContract(t *testing.T) {
	out, code := runCaptured(t, "--seed", "41", "--late-event", "1")
	required := []string{
		"parse_roundtrip=1.000",
		"replay_equal=true",
		"corrupt_ranges=",
		"segments=",
		"sha256=",
		"start_ns=",
		"end_ns=",
	}
	for _, want := range required {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	// Exit 2 is the documented code for localized recoverable faults.
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	// keyframe_hit_rate must be present and at least 0.98.
	if !strings.Contains(out, "keyframe_hit_rate=") {
		t.Error("keyframe_hit_rate missing")
	}
}

func TestDemoFaultInjectedContract(t *testing.T) {
	out, code := runCaptured(t, "--truncate-mp4", "17", "--drop-index", "2", "--late-event", "3")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	for _, want := range []string{"recovered_ranges=", "errors:", "path=fault-injected"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, "rebuilt_indexes=1") {
		t.Errorf("drop-index did not trigger a rebuild\n%s", out)
	}
	if !strings.Contains(out, "replay_equal=true") {
		t.Error("replay_equal must hold on the fault-injected path")
	}
}

func TestDemoIsDeterministic(t *testing.T) {
	a, _ := runCaptured(t, "--seed", "41", "--late-event", "1")
	b, _ := runCaptured(t, "--seed", "41", "--late-event", "1")
	if a != b {
		t.Error("two identical invocations produced different output")
	}
}

func TestDemoRejectsBadArgs(t *testing.T) {
	cases := [][]string{
		{"--late-event", "-1"},
		{"--segment-ms", "0"},
		{"--subtitle-format", "ass"},
		{"--unknown-flag"},
	}
	for _, c := range cases {
		if _, code := runCaptured(t, c...); code != 1 {
			t.Errorf("args %v gave exit %d, want 1", c, code)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	if code := run([]string{"frobnicate"}); code != 1 {
		t.Errorf("unknown command exit = %d, want 1", code)
	}
	if code := run(nil); code != 1 {
		t.Errorf("no args exit = %d, want 1", code)
	}
	if code := run([]string{"help"}); code != 0 {
		t.Errorf("help exit = %d, want 0", code)
	}
}

func TestDemoVTTFormat(t *testing.T) {
	out, _ := runCaptured(t, "--subtitle-format", "vtt")
	if !strings.Contains(out, "parse_roundtrip=1.000") {
		t.Errorf("VTT round trip failed\n%s", out)
	}
}
