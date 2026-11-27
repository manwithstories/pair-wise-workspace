package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"mediacheck/internal/inspect"
	"mediacheck/internal/model"
	"mediacheck/internal/pipeline"
	"mediacheck/internal/recover"
)

func result() *pipeline.Result {
	return &pipeline.Result{
		Packets: 2400,
		Bytes:   120000,
		Faults: []model.Fault{
			{Track: model.TrackAudio, Kind: model.FaultLoss},
			{Track: model.TrackAudio, Kind: model.FaultDuplicate},
		},
		Recovery: recover.Outcomes{Slots: 20, FEC: 18, PLC: 1, Silence: 1},
		Queue:    pipeline.QueueStats{PeakItems: 30, MaxItems: 32},
		Latency:  pipeline.LatencyStats{P95: 12 * time.Millisecond},
		Elapsed:  500 * time.Millisecond,
		TrackSpan: map[model.Track]int64{
			model.TrackAudio: 24_000_000_000,
			model.TrackVideo: 23_990_000_000,
		},
	}
}

func TestBuildMapsAllRequiredFields(t *testing.T) {
	r := result()
	var d model.DamageSet
	d.AddRange(0, 20_000_000, model.FaultLoss)
	d.AddRange(100_000_000, 120_000_000, model.FaultTruncatedHeader)
	r.Damage = d
	r.Inspection.Streams = map[model.Track]inspect.StreamStats{
		model.TrackVideo: {Keyframes: 5, GOPs: []int64{1_000_000_000, 1_000_000_000}},
	}
	m := Build(r, "/tmp/out.mkv", "abc123")
	if m.SyncErrorMS != 10 {
		t.Fatalf("sync error = %v, want 10", m.SyncErrorMS)
	}
	if m.LossRecoveryPct != 100 {
		t.Fatalf("recovery = %v, want 100", m.LossRecoveryPct)
	}
	if m.P95MS != 12 {
		t.Fatalf("p95 = %v", m.P95MS)
	}
	if m.QueuePeak != 30 {
		t.Fatalf("queue peak = %d", m.QueuePeak)
	}
	if len(m.DamagedRanges) != 2 {
		t.Fatalf("damaged ranges = %v", m.DamagedRanges)
	}
	if m.OutputSHA256 != "abc123" {
		t.Fatalf("hash = %q", m.OutputSHA256)
	}
	if m.KeyframeInterval != 1000 {
		t.Fatalf("keyframe interval = %v", m.KeyframeInterval)
	}
	if !m.AllTargetsMet() {
		t.Fatalf("targets not met: %+v", m.Targets)
	}
	if len(m.FaultKinds) != 2 || m.FaultKinds[0] != "duplicate" || m.FaultKinds[1] != "loss" {
		t.Fatalf("fault kinds = %v, want deduplicated and sorted", m.FaultKinds)
	}
}

func TestTargetsFailWhenThresholdsBroken(t *testing.T) {
	r := result()
	r.Recovery.Failed = 5
	r.Latency.P95 = 400 * time.Millisecond
	r.Queue = pipeline.QueueStats{PeakItems: 40, MaxItems: 32}
	r.TrackSpan[model.TrackAudio] = 24_500_000_000
	m := Build(r, "", "")
	if m.AllTargetsMet() {
		t.Fatalf("targets unexpectedly met: %+v", m.Targets)
	}
	if m.Targets.SyncErrorWithin20MS {
		t.Fatal("sync target should fail at 500 ms drift")
	}
	if m.Targets.LossRecoveryAtLeast95 || m.Targets.P95Within250MS || m.Targets.QueueWithinBound {
		t.Fatalf("targets = %+v", m.Targets)
	}
}

func TestExitCodes(t *testing.T) {
	clean := Metrics{}
	if got := ExitCode(clean); got != 0 {
		t.Fatalf("clean exit = %d, want 0", got)
	}
	damaged := Metrics{Faults: 1}
	if got := ExitCode(damaged); got != 2 {
		t.Fatalf("damaged exit = %d, want 2", got)
	}
	canceled := Metrics{Canceled: true, Faults: 3}
	if got := ExitCode(canceled); got != 130 {
		t.Fatalf("canceled exit = %d, want 130", got)
	}
}

func TestPrintContainsStableKeys(t *testing.T) {
	r := result()
	m := Build(r, "/tmp/out.mkv", "deadbeef")
	var buf bytes.Buffer
	Print(&buf, m, []string{"note=hello"})
	out := buf.String()
	for _, key := range []string{
		"sync_error_ms=", "loss_recovery_pct=", "p95_ms=", "queue_peak=",
		"damaged_ranges=", "output_sha256=", "exit_code=", "targets_met=", "note=hello",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("output missing %q:\n%s", key, out)
		}
	}
	if strings.Contains(out, "output_sha256=none") {
		t.Fatal("hash present but printed as none")
	}
}

func TestPrintMarksMissingOutput(t *testing.T) {
	var buf bytes.Buffer
	Print(&buf, Build(result(), "", ""), nil)
	if !strings.Contains(buf.String(), "output_sha256=none") {
		t.Fatalf("missing output not marked:\n%s", buf.String())
	}
}

func TestSummaryCapsLongLists(t *testing.T) {
	if got := Summary(nil, 3); got != "none" {
		t.Fatalf("empty summary = %q", got)
	}
	got := Summary([]string{"c", "a", "b", "d", "e"}, 2)
	if !strings.Contains(got, "(+3 more)") || !strings.HasPrefix(got, "a,b") {
		t.Fatalf("capped summary = %q", got)
	}
}

func TestJSONLinesContainsPointersAndTags(t *testing.T) {
	m := Build(result(), "/tmp/out.mkv", "cafe")
	lines := JSONLines(m)
	if len(lines) != 1 {
		t.Fatalf("JSONLines returned %d lines", len(lines))
	}
	for _, key := range []string{`"sync_error_ms"`, `"damaged_ranges"`, `"output_sha256"`, `"targets"`} {
		if !strings.Contains(lines[0], key) {
			t.Fatalf("JSON missing %s: %s", key, lines[0])
		}
	}
}
