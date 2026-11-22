package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI executes the command in a temp working directory so tests never touch
// the repository tree.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	args = append(args, "--work-dir", filepath.Join(dir, "work"))
	var out, errBuf bytes.Buffer
	code = run(args, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

func TestRejectsInvalidArguments(t *testing.T) {
	cases := [][]string{
		{},
		{"bogus"},
		{"demo", "--duration", "abc"},
		{"demo", "--duration", "0s"},
		{"demo", "--segment", "200s", "--duration", "120s"},
		{"demo", "--late-window", "30s", "--segment", "10s"},
		{"demo", "--bitrates", "64k"},
		{"demo", "--bitrates", "64k,64k"},
		{"demo", "--bitrates", "64k,abc"},
		{"demo", "--seed", "0"},
		{"demo", "--damage", "bogus"},
		{"demo", "--interrupt-after", "500s", "--duration", "120s"},
		{"demo", "extra"},
	}
	for _, args := range cases {
		_, stderr, code := runCLI(t, args...)
		if code != 2 {
			t.Fatalf("%v: code = %d stderr=%q", args, code, stderr)
		}
		if !strings.Contains(stderr, "error:") {
			t.Fatalf("%v: no error message: %q", args, stderr)
		}
	}
}

func TestDemoReportsParseableFields(t *testing.T) {
	if testing.Short() {
		t.Skip("demo run skipped in -short")
	}
	stdout, stderr, code := runCLI(t,
		"demo", "--seed", "7", "--duration", "20s", "--segment", "10s",
		"--late-window", "1s", "--bitrates", "64k,128k",
		"--interrupt-after", "10s", "--resume")
	if code != 0 {
		t.Fatalf("code = %d stderr=%s stdout=%s", code, stderr, stdout)
	}
	fields := parseKV(stdout)
	for _, key := range []string{
		"AUDIO_VIDEO_SYNC_P95_MS", "PACKET_LOSS_RECOVERY_RATE", "PIPELINE_P95_MS",
		"REPRESENTATION_BOUNDARY_MAX_DELTA_MS", "RESUME_OUTPUT_SHA256_MATCH", "REPLAY_VERSION_COUNT",
		"OUTPUT_SHA256", "QUARANTINED_PACKETS", "REPLAYED_SEGMENTS", "ACCEPTANCE",
	} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing %s in output:\n%s", key, stdout)
		}
	}
	if fields["ACCEPTANCE"] != "pass" {
		t.Fatalf("acceptance = %s\n%s", fields["ACCEPTANCE"], stdout)
	}
	if fields["RESUME_OUTPUT_SHA256_MATCH"] != "true" {
		t.Fatalf("resume match = %s", fields["RESUME_OUTPUT_SHA256_MATCH"])
	}
	if len(fields["OUTPUT_SHA256"]) != 64 {
		t.Fatalf("output sha = %q", fields["OUTPUT_SHA256"])
	}
}

func TestDemoBurstLossBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("demo run skipped in -short")
	}
	stdout, stderr, code := runCLI(t,
		"demo", "--seed", "3", "--duration", "40s", "--damage", "burst-loss",
		"--late-at-boundary", "--interrupt-after", "15s", "--resume")
	if code != 0 {
		t.Fatalf("code = %d stderr=%s\n%s", code, stderr, stdout)
	}
	fields := parseKV(stdout)
	if fields["ACCEPTANCE"] != "pass" {
		t.Fatalf("acceptance = %s", fields["ACCEPTANCE"])
	}
	// The scenario must actually quarantine late traffic and replay segments.
	if fields["QUARANTINED_PACKETS"] == "0" {
		t.Fatal("no quarantined packets in a boundary scenario")
	}
	if fields["REPLAY_VERSION_COUNT"] == "1" {
		t.Fatal("no replay versions recorded")
	}
}

func TestNoResumeStillMatchesReference(t *testing.T) {
	if testing.Short() {
		t.Skip("demo run skipped in -short")
	}
	stdout, stderr, code := runCLI(t, "demo", "--seed", "11", "--duration", "20s", "--segment", "10s", "--late-window", "1s")
	if code != 0 {
		t.Fatalf("code = %d stderr=%s", code, stderr)
	}
	if parseKV(stdout)["RESUME_OUTPUT_SHA256_MATCH"] != "true" {
		t.Fatalf("uninterrupted run does not match its reference:\n%s", stdout)
	}
}

func parseKV(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.IndexByte(line, '='); i > 0 {
			m[line[:i]] = line[i+1:]
		}
	}
	return m
}
