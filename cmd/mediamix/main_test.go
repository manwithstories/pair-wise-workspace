package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestDemoAcceptanceExitZero(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"demo", "--seed", "41", "--loss-burst", "7", "--bandwidth-mbps", "3", "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	var rep map[string]any
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out.String())
	}
	if rep["sync_error_ms"].(float64) > 20 {
		t.Errorf("sync_error_ms = %v > 20", rep["sync_error_ms"])
	}
	if rep["recovery_rate"].(float64) < 0.90 {
		t.Errorf("recovery_rate = %v < 0.90", rep["recovery_rate"])
	}
	if rep["latency_p95_ms"].(float64) > 500 {
		t.Errorf("latency_p95_ms = %v > 500", rep["latency_p95_ms"])
	}
}

func TestDemoCorruptIndexExitThree(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"demo", "--seed", "41", "--corrupt-index", "--truncate-track", "audio-2", "--json"}, &out, &errb)
	if code != 3 {
		t.Fatalf("exit = %d, want 3; stderr=%s", code, errb.String())
	}
	var rep struct {
		ValidationErrors []struct {
			Code  string `json:"code"`
			Track string `json:"track"`
		} `json:"validation_errors"`
		FailedTracks []string `json:"failed_tracks"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(rep.ValidationErrors) == 0 || rep.ValidationErrors[0].Code != "bad_index" {
		t.Fatalf("index error missing: %+v", rep.ValidationErrors)
	}
	if len(rep.FailedTracks) != 1 || rep.FailedTracks[0] != "cam-a" {
		t.Fatalf("failed tracks = %v", rep.FailedTracks)
	}
}

func TestDemoBadArgumentsExitTwo(t *testing.T) {
	cases := [][]string{
		{"demo", "--bandwidth-mbps", "0"},
		{"demo", "--loss-burst", "-1"},
		{"demo", "--nope"},
		{"bogus"},
		{},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("args %v exit = %d, want 2", args, code)
		}
	}
}

func TestDemoUnrecoverableExitFour(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"demo", "--seed", "41", "--loss-burst", "7", "--bandwidth-mbps", "0.5", "--json"}, &out, &errb)
	if code != 4 {
		t.Fatalf("exit = %d, want 4; stdout=%s", code, out.String())
	}
}

func TestDemoHumanOutputHasRequiredFields(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"demo", "--seed", "41"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	text := out.String()
	for _, want := range []string{"sync_error_ms=", "recovery_rate=", "latency_p95_ms=", "reordered_packets=", "duplicate_packets=", "failed_tracks="} {
		if !bytes.Contains([]byte(text), []byte(want)) {
			t.Errorf("human output missing %q:\n%s", want, text)
		}
	}
}
