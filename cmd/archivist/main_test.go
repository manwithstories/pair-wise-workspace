package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"archivearchivist/internal/model"
	"archivearchivist/internal/testkit"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestGenerateInspectRecoverFlow(t *testing.T) {
	dir := t.TempDir()
	code, out, errs := runCLI(t, "generate", "--out", dir)
	if code != 0 {
		t.Fatalf("generate exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "timestamp wraps 1") {
		t.Fatalf("generate output missing wrap count: %s", out)
	}
	timeline := filepath.Join(dir, "timeline.json")
	capture := filepath.Join(dir, "stream.rtp")

	code, out, errs = runCLI(t, "inspect", "--input", timeline, "--capture", capture)
	if code != 0 {
		t.Fatalf("inspect exit %d: %s", code, errs)
	}
	for _, want := range []string{
		"tracks=4", "corrupt bytes=[", "wraps=1", "constant_fps=false",
		"sync_error_p95_ms=", "packet_recovery_rate=",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("inspect output missing %q:\n%s", want, out)
		}
	}

	jsonPath := filepath.Join(dir, "result.json")
	code, out, errs = runCLI(t, "recover", "--input", timeline, "--capture", capture,
		"--iterations", "30", "--json", jsonPath)
	if code != 0 {
		t.Fatalf("recover exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "acceptance latency_p95_ms=") {
		t.Fatalf("recover output missing latency acceptance line:\n%s", out)
	}
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metrics struct {
			Tracks             int   `json:"tracks"`
			CorruptRanges      int   `json:"corrupt_ranges"`
			Wraps              int   `json:"wraps"`
			ConstantFPS        bool  `json:"constant_fps"`
			SyncErrorP95US     int64 `json:"sync_error_p95_us"`
			LatencyP95US       int64 `json:"latency_p95_us"`
			PacketRecoveryRate struct {
				Num int64 `json:"num"`
				Den int64 `json:"den"`
			} `json:"packet_recovery_rate"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result json: %v", err)
	}
	m := doc.Metrics
	// The recover document carries the full analysis; its presence is checked
	// by TestRecoverJSONCarriesAnalysis.
	if m.Tracks != 4 || m.CorruptRanges < 1 || m.Wraps < 1 {
		t.Fatalf("unexpected result document: %+v", doc)
	}
	if m.ConstantFPS {
		t.Fatalf("constant_fps = true")
	}
	if m.SyncErrorP95US > 20_000 {
		t.Fatalf("sync p95 = %d", m.SyncErrorP95US)
	}
	if m.LatencyP95US <= 0 || m.LatencyP95US > 250_000 {
		t.Fatalf("latency p95 = %d", m.LatencyP95US)
	}
	if m.PacketRecoveryRate.Num*10 < m.PacketRecoveryRate.Den*9 {
		t.Fatalf("recovery rate = %d/%d", m.PacketRecoveryRate.Num, m.PacketRecoveryRate.Den)
	}
}

func TestRecoverJSONCarriesAnalysis(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir, VideoFrames: 90})
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, "analysis.json")
	code, _, errs := runCLI(t, "recover", "--input", fx.Timeline, "--capture", fx.Capture,
		"--iterations", "30", "--json", jsonPath)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"timeline", "scans", "capture", "clocks", "plan", "metrics", "faults"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("analysis JSON missing %q", key)
		}
	}
	var clocks map[string]struct {
		Points []struct {
			Orig int64 `json:"original"`
			Norm int64 `json:"normalized"`
		} `json:"timestamp_points"`
	}
	if err := json.Unmarshal(doc["clocks"], &clocks); err != nil {
		t.Fatal(err)
	}
	v1, ok := clocks["V1"]
	if !ok || len(v1.Points) == 0 {
		t.Fatalf("no retained timestamps for V1: %+v", clocks)
	}
	// Original and normalized timestamps must both survive, and at least one
	// pair must straddle the 33-bit boundary.
	wrapped := false
	for _, p := range v1.Points {
		if p.Norm >= 1<<33 && p.Orig < 1<<32 {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatalf("no wrapped timestamp pair retained")
	}
}

func TestExitCodeInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"nonsense"},
		{"inspect"},
		{"inspect", "--input", "/definitely/not/here.json"},
		{"generate"},
	} {
		code, _, _ := runCLI(t, args...)
		if code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestExitCodeInvalidTimeline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"tracks":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runCLI(t, "inspect", "--input", path)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (stderr %s)", code, errs)
	}
}

func TestExitCodeUnrecoverable(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir, VideoFrames: 240})
	if err != nil {
		t.Fatal(err)
	}
	// Destroy every sample of the video track so no keyframe can bracket it.
	video := filepath.Join(dir, "video.mp4")
	b, err := os.ReadFile(video)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b {
		if i > 2048 {
			b[i] = 0
		}
	}
	if err := os.WriteFile(video, b, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "recover", "--input", fx.Timeline, "--capture", fx.Capture)
	if code != 3 {
		t.Fatalf("exit %d, want 3:\n%s", code, out)
	}
	if !strings.Contains(out, "unrecoverable") {
		t.Fatalf("output missing unrecoverable verdict:\n%s", out)
	}
}

func TestCycleInputStillReportsIndependently(t *testing.T) {
	dir := t.TempDir()
	fx, err := testkit.Generate(testkit.Config{OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	tl, err := model.LoadTimeline(fx.Timeline)
	if err != nil {
		t.Fatal(err)
	}
	for i := range tl.Tracks {
		switch tl.Tracks[i].ID {
		case "A1":
			tl.Tracks[i].DependsOn = []string{"V1", "A2"}
		case "A2":
			tl.Tracks[i].DependsOn = []string{"A1"}
		}
	}
	b, _ := json.MarshalIndent(tl, "", "  ")
	cyc := filepath.Join(dir, "cycle.json")
	if err := os.WriteFile(cyc, b, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "recover", "--input", cyc, "--capture", fx.Capture)
	if code != 3 {
		t.Fatalf("exit %d, want 3 for a cycle:\n%s", code, out)
	}
	if !strings.Contains(out, "dependency_cycle") {
		t.Fatalf("cycle not named in output:\n%s", out)
	}
	if strings.Contains(out, "boundaries recovered=0 ") {
		t.Fatalf("independent branches were dropped:\n%s", out)
	}
}
