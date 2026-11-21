package main

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runDemo invokes the CLI with a temporary work directory.
func runDemo(t *testing.T, extra ...string) (string, int) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	args := append([]string{"demo", "--workdir", work}, extra...)
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return out.String(), code
}

var (
	shaLine       = regexp.MustCompile(`(?m)^segment_sha256=([0-9a-f]{64})$`)
	segmentLine   = regexp.MustCompile(`(?m)^segments=(\d+)$`)
	roundTripLine = regexp.MustCompile(`(?m)^roundtrip_match_pct=(\d+\.\d{2})$`)
	keyframeLine  = regexp.MustCompile(`(?m)^keyframe_hit_pct=(\d+\.\d{2})$`)
)

func TestNormalPathContract(t *testing.T) {
	out, code := runDemo(t, "--seed", "41", "--rates", "800000,1600000", "--segment-ticks", "90000")
	if code != 0 {
		t.Fatalf("exit=%d output=\n%s", code, out)
	}
	for _, want := range []string{"status=ok", "roundtrip_match_pct=100.00", "keyframe_hit_pct=100.00"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if !segmentLine.MatchString(out) {
		t.Fatalf("missing segments=N:\n%s", out)
	}
	shas := shaLine.FindAllStringSubmatch(out, -1)
	if len(shas) == 0 {
		t.Fatalf("no 64-hex segment hashes:\n%s", out)
	}
	if !roundTripLine.MatchString(out) || !keyframeLine.MatchString(out) {
		t.Fatalf("metric format wrong:\n%s", out)
	}
	if !strings.Contains(out, "resumed_nodes=0") {
		t.Fatalf("fresh run should resume nothing:\n%s", out)
	}
}

func TestCorruptFrameBlocksWithExitTwo(t *testing.T) {
	out, code := runDemo(t, "--seed", "41", "--rates", "800000,1600000",
		"--segment-ticks", "90000", "--inject", "corrupt-frame")
	if code != 2 {
		t.Fatalf("exit=%d want 2:\n%s", code, out)
	}
	if !strings.Contains(out, "status=blocked") || !strings.Contains(out, "first_blocked=frame-2") {
		t.Fatalf("failure contract missing:\n%s", out)
	}
	if shaLine.MatchString(out) {
		t.Fatalf("blocked run must not list success hashes:\n%s", out)
	}
}

func TestDuplicateAndMissingFrameReportFirstBadFrame(t *testing.T) {
	dup, code := runDemo(t, "--inject", "duplicate-frame")
	if code != 2 || !strings.Contains(dup, "first_blocked=frame-3") {
		t.Fatalf("duplicate: exit=%d\n%s", code, dup)
	}
	miss, code := runDemo(t, "--inject", "missing-frame")
	if code != 2 || !strings.Contains(miss, "first_blocked=frame-4") {
		t.Fatalf("missing: exit=%d\n%s", code, miss)
	}
}

func TestMP4FaultsBlock(t *testing.T) {
	for _, inject := range []string{"truncated-mp4", "illegal-box"} {
		out, code := runDemo(t, "--inject", inject)
		if code != 2 || !strings.Contains(out, "status=blocked") {
			t.Fatalf("%s: exit=%d\n%s", inject, code, out)
		}
	}
}

func TestAudioConflictNeedsPolicy(t *testing.T) {
	out, code := runDemo(t, "--inject", "audio-conflict")
	if code != 2 || !strings.Contains(out, "first_blocked=audio:remote-1") {
		t.Fatalf("conflict: exit=%d\n%s", code, out)
	}
	out, code = runDemo(t, "--inject", "audio-conflict", "--audio-policy", "downmix:stereo")
	if code != 0 || !strings.Contains(out, "audio=stereo,48000Hz,mapped,conflicts=2") {
		t.Fatalf("downmix: exit=%d\n%s", code, out)
	}
}

func TestColorConflictNeedsPolicy(t *testing.T) {
	out, code := runDemo(t, "--inject", "color-conflict")
	if code != 2 || !strings.Contains(out, "first_blocked=color:cam-b") {
		t.Fatalf("conflict: exit=%d\n%s", code, out)
	}
	out, code = runDemo(t, "--inject", "color-conflict", "--color-policy", "prefer:cam-b")
	if code != 0 || !strings.Contains(out, "color=bt2020/pq/bt2020nc/limited") {
		t.Fatalf("prefer: exit=%d\n%s", code, out)
	}
}

func TestMisalignedRateBlocks(t *testing.T) {
	out, code := runDemo(t, "--inject", "misaligned-rate")
	if code != 2 || !strings.Contains(out, "status=blocked") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}

func TestUnknownBoxRoundTrips(t *testing.T) {
	out, code := runDemo(t, "--inject", "unknown-box")
	if code != 0 || !strings.Contains(out, "roundtrip_match_pct=100.00") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
}

func TestUsageErrorsExitOne(t *testing.T) {
	cases := [][]string{
		{"demo", "--rates", "nope"},
		{"demo", "--rates", "800000,800000"},
		{"demo", "--seed", "-1"},
		{"demo", "--segment-ticks", "0"},
		{"demo", "--inject", "nonsense"},
		{"demo", "--audio-policy", "downmix"},
		{"demo", "--color-policy", "downmix:stereo"},
		{"demo", "extra-positional"},
		{"nonsense"},
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 1 {
			t.Fatalf("%v: exit=%d want 1", args, code)
		}
	}
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 1 {
		t.Fatalf("no args exit=%d", code)
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var out, errOut bytes.Buffer
		if code := run([]string{arg}, &out, &errOut); code != 0 {
			t.Fatalf("%s exit=%d", arg, code)
		}
		if !strings.Contains(out.String(), "usage: mediapack") {
			t.Fatalf("%s printed no usage", arg)
		}
	}
}

func TestResumeReproducesUninterruptedHashes(t *testing.T) {
	// A clean run in one directory establishes the reference hashes.
	cleanOut, code := runDemo(t, "--seed", "41", "--rates", "800000,1600000", "--segment-ticks", "90000")
	if code != 0 {
		t.Fatalf("clean run exit=%d\n%s", code, cleanOut)
	}
	clean := shaLine.FindAllStringSubmatch(cleanOut, -1)
	if len(clean) == 0 {
		t.Fatalf("clean run produced no hashes")
	}

	// A resume run primes an interrupted state, then completes it.
	resumeOut, code := runDemo(t, "--seed", "41", "--rates", "800000,1600000",
		"--segment-ticks", "90000", "--resume")
	if code != 0 {
		t.Fatalf("resume run exit=%d\n%s", code, resumeOut)
	}
	if !strings.Contains(resumeOut, "resumed_nodes=") {
		t.Fatalf("resume run did not report resumed nodes:\n%s", resumeOut)
	}
	m := regexp.MustCompile(`resumed_nodes=(\d+)`).FindStringSubmatch(resumeOut)
	if m == nil || m[1] == "0" {
		t.Fatalf("resume reported no reused nodes:\n%s", resumeOut)
	}
	resumed := shaLine.FindAllStringSubmatch(resumeOut, -1)
	if len(resumed) != len(clean) {
		t.Fatalf("segment count differs: clean=%d resumed=%d", len(clean), len(resumed))
	}
	for i := range clean {
		if clean[i][1] != resumed[i][1] {
			t.Fatalf("segment %d hash differs: %s vs %s", i+1, clean[i][1], resumed[i][1])
		}
	}
}

func TestRerunIsIdempotent(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	var first, second bytes.Buffer
	if code := run([]string{"demo", "--workdir", work}, &first, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first run exit=%d\n%s", code, first.String())
	}
	if code := run([]string{"demo", "--workdir", work}, &second, &bytes.Buffer{}); code != 0 {
		t.Fatalf("second run exit=%d\n%s", code, second.String())
	}
	a := shaLine.FindAllStringSubmatch(first.String(), -1)
	b := shaLine.FindAllStringSubmatch(second.String(), -1)
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("hash count changed between identical runs")
	}
	for i := range a {
		if a[i][1] != b[i][1] {
			t.Fatalf("identical rerun changed segment %d hash", i+1)
		}
	}
	// The rerun must not redo work; without --resume the reuse is reported as
	// skipped rather than resumed, but nodes_done stays at the full graph.
	if !strings.Contains(second.String(), "nodes_done=8") {
		t.Fatalf("second identical run lost nodes:\n%s", second.String())
	}
}
