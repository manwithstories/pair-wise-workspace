package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	errOut, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer errOut.Close()
	code := run(args, out, errOut)
	stdout, _ := os.ReadFile(out.Name())
	stderr, _ := os.ReadFile(errOut.Name())
	return code, string(stdout), string(stderr)
}

func TestCLIEndToEndAndExitCodes(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := runCLI(t, "fixtures", "gen", "--out", dir, "--count", "128", "--seed", "7")
	if code != statusOK {
		t.Fatalf("fixtures exit=%d stderr=%s", code, stderr)
	}
	probeJSON := filepath.Join(dir, "probe.json")
	code, _, stderr = runCLI(t, "probe", "--in", filepath.Join(dir, "mixed_gb18030.srt"), "--json", probeJSON)
	if code != statusOK {
		t.Fatalf("probe exit=%d stderr=%s", code, stderr)
	}
	var probe map[string]any
	readJSON(t, probeJSON, &probe)
	if int(probe["cues_total"].(float64)) != 128 || probe["overlap_groups"].(float64) < 1 {
		t.Fatalf("probe report: %#v", probe)
	}
	encoding := probe["encoding"].(map[string]any)
	if encoding["name"] != "GB18030" || encoding["confidence"].(float64) < 0.7 {
		t.Fatalf("encoding report: %#v", encoding)
	}

	frameJSON := filepath.Join(dir, "frames.json")
	code, _, stderr = runCLI(t, "frame", "--in", filepath.Join(dir, "ep01.ass"), "--fps", "24000/1001", "--json", frameJSON)
	if code != statusOK {
		t.Fatalf("frame exit=%d stderr=%s", code, stderr)
	}
	var frames struct {
		Frames []struct {
			Index    int   `json:"index"`
			InMS     int64 `json:"in_ms"`
			OutMS    int64 `json:"out_ms"`
			InFrame  int64 `json:"in_frame"`
			OutFrame int64 `json:"out_frame"`
		} `json:"frames"`
	}
	readJSON(t, frameJSON, &frames)
	first := frames.Frames[0]
	if first.Index != 1 || first.InMS != 1250 || first.OutMS != 3750 || first.InFrame != 30 || first.OutFrame != 90 {
		t.Fatalf("first frame row: %#v", first)
	}

	output := filepath.Join(dir, "ep01.vtt")
	code, _, stderr = runCLI(t, "convert", "--in", filepath.Join(dir, "ep01.ass"), "--out", output,
		"--in-fps", "24000/1001", "--out-fps", "30000/1001")
	if code != statusOK {
		t.Fatalf("convert exit=%d stderr=%s", code, stderr)
	}
	firstBytes, _ := os.ReadFile(output)
	code, _, stderr = runCLI(t, "convert", "--in", filepath.Join(dir, "ep01.ass"), "--out", output,
		"--in-fps", "24000/1001", "--out-fps", "30000/1001")
	secondBytes, _ := os.ReadFile(output)
	if code != statusOK || string(firstBytes) != string(secondBytes) {
		t.Fatalf("convert is not idempotent: exit=%d stderr=%s", code, stderr)
	}

	rtJSON := filepath.Join(dir, "roundtrip.json")
	code, _, stderr = runCLI(t, "roundtrip", "--in", filepath.Join(dir, "stress_50k.srt"), "--via", "vtt",
		"--in-fps", "25", "--out-fps", "30000/1001", "--tolerance-ms", "0", "--json", rtJSON)
	if code != statusOK {
		t.Fatalf("roundtrip exit=%d stderr=%s", code, stderr)
	}
	var roundtrip map[string]any
	readJSON(t, rtJSON, &roundtrip)
	if roundtrip["max_abs_drift_ms"].(float64) != 0 || roundtrip["cue_count_match"] != true {
		t.Fatalf("roundtrip report: %#v", roundtrip)
	}

	code, _, _ = runCLI(t, "roundtrip", "--in", filepath.Join(dir, "stress_50k.srt"), "--via", "ass",
		"--in-fps", "25", "--out-fps", "30000/1001", "--tolerance-ms", "0", "--json", rtJSON)
	if code != statusOK {
		t.Fatalf("ASS roundtrip exit=%d", code)
	}
	readJSON(t, rtJSON, &roundtrip)
	if roundtrip["quantized_max_ms"].(float64) > 5 {
		t.Fatalf("ASS quantization too large: %#v", roundtrip)
	}

	code, _, _ = runCLI(t, "probe", "--in", filepath.Join(dir, "invalid.srt"),
		"--json", filepath.Join(dir, "invalid.json"))
	if code != statusOK {
		t.Fatalf("invalid probe exit=%d", code)
	}
	readJSON(t, filepath.Join(dir, "invalid.json"), &probe)
	if probe["invalid_cues"].(float64) != 2 {
		t.Fatalf("invalid cue count: %#v", probe)
	}
	code, _, _ = runCLI(t, "probe", "--in", filepath.Join(dir, "low_confidence.srt"))
	if code != statusLowEnc {
		t.Fatalf("low confidence exit=%d", code)
	}
	errorJSON := filepath.Join(dir, "error.json")
	code, _, _ = runCLI(t, "probe", "--in", filepath.Join(dir, "truncated.srt"), "--json", errorJSON)
	if code != statusInput {
		t.Fatalf("truncated exit=%d", code)
	}
	var envelope errorEnvelope
	readJSON(t, errorJSON, &envelope)
	if envelope.Error.Code != "file_truncated" || envelope.Error.ParsedCues != 1 {
		t.Fatalf("error report: %#v", envelope)
	}
	code, _, _ = runCLI(t, "frame", "--in", "x")
	if code != statusArgs {
		t.Fatalf("argument error exit=%d", code)
	}
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
