package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"avpipe/internal/fault"
)

func TestFinalizePassesWhenMetricsPass(t *testing.T) {
	r := &Report{
		Metrics: []Metric{
			Thresholds{SyncP95Ms: 20}.Check("sync", 12, 20, true),
			Thresholds{RecoveryRateMin: 0.9}.Check("recovery", 0.97, 0.9, false),
		},
	}
	r.Finalize()
	if !r.Passed || r.ExitCode != 0 {
		t.Fatalf("passed=%v exit=%d", r.Passed, r.ExitCode)
	}
}

func TestFinalizeFailsOnSyncThreshold(t *testing.T) {
	r := &Report{Metrics: []Metric{
		Thresholds{SyncP95Ms: 20}.Check("sync", 22.5, 20, true),
	}}
	r.Finalize()
	if r.Passed || r.ExitCode != 3 {
		t.Fatalf("passed=%v exit=%d", r.Passed, r.ExitCode)
	}
}

func TestFinalizeFailsOnRecoveryThreshold(t *testing.T) {
	r := &Report{Metrics: []Metric{
		Thresholds{RecoveryRateMin: 0.9}.Check("recovery", 0.85, 0.9, false),
	}}
	r.Finalize()
	if r.Passed || r.ExitCode != 3 {
		t.Fatalf("passed=%v exit=%d", r.Passed, r.ExitCode)
	}
}

func TestRootErrorMapsToExitTwo(t *testing.T) {
	err := fault.New(fault.CodeWAVTruncated, "audio.wav.parse", "short file").At(9)
	r := &Report{RootError: FromError(err)}
	r.Finalize()
	if r.RootError.Offset == nil || *r.RootError.Offset != 9 {
		t.Fatalf("offset lost: %+v", r.RootError)
	}
	if r.RootError.Code != "wav_truncated" {
		t.Fatalf("code=%s", r.RootError.Code)
	}
}

func TestEncodeContainsNoRawAudioFields(t *testing.T) {
	r := &Report{Command: "run", RMSBefore: 0.1, RMSAfter: 0.2}
	buf, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	text := string(buf)
	for _, forbidden := range []string{"samples", "payload", "pcm_data", "wav_bytes"} {
		if strings.Contains(text, `"`+forbidden+`"`) {
			t.Fatalf("report leaks %q field", forbidden)
		}
	}
	var back map[string]any
	if err := json.Unmarshal(buf, &back); err != nil {
		t.Fatal(err)
	}
	if back["rms_before"] != 0.1 {
		t.Fatalf("rms_before=%v", back["rms_before"])
	}
	if _, ok := back["root_error"]; !ok {
		t.Fatal("root_error must always be present (null when clean)")
	}
}

func TestWriteEncodeCreatesDirectoryAndReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "report.json")
	r := &Report{Command: "selftest", PacketRecovery: 0.95}
	if err := r.WriteEncode(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"packet_recovery_rate": 0.95`) {
		t.Fatalf("unexpected content: %s", raw)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	// Overwrite must succeed and leave a single valid JSON document.
	if err := (&Report{Command: "run"}).WriteEncode(path); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("rewritten report invalid: %v", err)
	}
}

func TestThresholdCheckUncheckedWhenZero(t *testing.T) {
	m := Thresholds{}.Check("latency", 500, 0, true)
	if m.Checked || !m.Passed {
		t.Fatalf("zero limit should be unchecked: %+v", m)
	}
}

func TestFromErrorUsesRootCause(t *testing.T) {
	root := fault.New(fault.CodeQueueLimit, "stream.push", "full").WithTrack("audio")
	wrapped := fault.Wrap(fault.CodeTrackFailed, "track.run", root, "canceled")
	info := FromError(wrapped)
	if info.Code != "queue_limit" || info.Track != "audio" {
		t.Fatalf("info=%+v", info)
	}
	if FromError(nil) != nil {
		t.Fatal("nil error produced info")
	}
}
