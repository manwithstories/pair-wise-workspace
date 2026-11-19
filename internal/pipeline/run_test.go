package pipeline

import (
	"encoding/json"
	"testing"

	"mediamix/internal/core"
	"mediamix/internal/fixture"
)

// TestAcceptanceHealthyRun asserts the exact targets from the demo contract:
// sync <= 20 ms, recovery >= 90%, P95 <= 500 ms.
func TestAcceptanceHealthyRun(t *testing.T) {
	rep, err := Run(Options{Seed: 41, LossBurst: 7, BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SyncErrorMS > 20 {
		t.Errorf("sync error %.3f ms exceeds 20 ms", rep.SyncErrorMS)
	}
	if rep.RecoveryRate < 0.90 {
		t.Errorf("recovery rate %.3f below 90%%", rep.RecoveryRate)
	}
	if rep.LatencyP95MS > 500 {
		t.Errorf("p95 latency %.1f ms exceeds 500 ms", rep.LatencyP95MS)
	}
	if len(rep.FailedTracks) != 0 {
		t.Errorf("no track should fail: %v", rep.FailedTracks)
	}
	if rep.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", rep.ExitCode)
	}
	if !rep.Healthy() {
		t.Error("report should be healthy")
	}
}

// TestContainerExcludesFailedTracks asserts the deliverable never contains a
// track the pipeline rejected, while still carrying every healthy one.
func TestContainerExcludesFailedTracks(t *testing.T) {
	healthy, err := Run(Options{Seed: 41, BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if healthy.Container == nil {
		t.Fatal("healthy run must produce a verified container")
	}
	baseTracks := healthy.Container.Tracks
	if baseTracks != 6 {
		t.Fatalf("container tracks = %d, want 6", baseTracks)
	}
	if healthy.Container.Samples != healthy.Container.Chunks {
		t.Fatalf("one sample per chunk expected: %d vs %d",
			healthy.Container.Samples, healthy.Container.Chunks)
	}

	corrupt, err := Run(Options{Seed: 41, CorruptIndex: true, BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if corrupt.Container == nil {
		t.Fatal("container must still be produced for the healthy tracks")
	}
	if corrupt.Container.Tracks != baseTracks-1 {
		t.Fatalf("failed track must be excluded: got %d tracks, want %d",
			corrupt.Container.Tracks, baseTracks-1)
	}
	for _, typ := range corrupt.Container.TrackTypes {
		if typ != "vide" && typ != "soun" {
			t.Fatalf("unexpected handler type %q", typ)
		}
	}
}

// TestCorruptIndexIsolated asserts that a bad index fails only its own track
// and leaves every other track's results intact.
func TestCorruptIndexIsolated(t *testing.T) {
	rep, err := Run(Options{Seed: 41, CorruptIndex: true, TruncateTrack: "audio-2", BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.FailedTracks) != 1 || rep.FailedTracks[0] != "cam-a" {
		t.Fatalf("failed tracks = %v, want [cam-a]", rep.FailedTracks)
	}
	// Top-level validation error names the index failure and the track.
	if len(rep.ValidationErrors) == 0 {
		t.Fatal("index failure must surface as a validation error")
	}
	ve := rep.ValidationErrors[0]
	if ve.Code != core.CodeBadIndex || ve.Track != "cam-a" {
		t.Fatalf("validation error = %+v", ve)
	}
	// cam-b and proxy must be untouched.
	for _, tr := range rep.Tracks {
		switch tr.Track {
		case "cam-a":
			if tr.Status != StatusFailed {
				t.Errorf("cam-a status = %s, want failed", tr.Status)
			}
		case "cam-b", "proxy-av1":
			if tr.Status != StatusOK {
				t.Errorf("%s status = %s, want ok (must not be polluted)", tr.Track, tr.Status)
			}
			if tr.Index == nil || tr.Index.Count() == 0 {
				t.Errorf("%s lost its index", tr.Track)
			}
		}
	}
	if rep.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", rep.ExitCode)
	}
}

// TestTruncatedTrackIsDegradedNotFatal ensures a truncated audio track reports
// the defect but does not abort the run.
func TestTruncatedTrackIsDegradedNotFatal(t *testing.T) {
	rep, err := Run(Options{Seed: 41, TruncateTrack: "audio-2", BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tr := range rep.Tracks {
		if tr.Track != "audio-2" {
			continue
		}
		found = true
		if tr.Status != StatusDegraded {
			t.Fatalf("audio-2 status = %s, want degraded", tr.Status)
		}
		hasInvalid := false
		for _, a := range tr.Adjustments {
			if a.Code == core.CodeAudioInvalid {
				hasInvalid = true
			}
		}
		if !hasInvalid {
			t.Fatalf("audio-2 missing truncation notice: %+v", tr.Adjustments)
		}
	}
	if !found {
		t.Fatal("audio-2 missing from report")
	}
}

// TestLowBandwidthUnrecoverable asserts that a starved link honestly reports
// unrecoverable tracks while keeping the run structurally sound.
func TestLowBandwidthUnrecoverable(t *testing.T) {
	rep, err := Run(Options{Seed: 41, LossBurst: 7, BandwidthMBps: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unrecoverable) == 0 {
		t.Fatal("starved link must report unrecoverable tracks")
	}
	if rep.RecoveryRate >= 1.0 {
		t.Fatalf("recovery rate %.3f should reflect unrecoverable gaps", rep.RecoveryRate)
	}
	if rep.ExitCode != 4 {
		t.Fatalf("exit code = %d, want 4", rep.ExitCode)
	}
	// Even under loss, sync error must remain bounded: independent tracks each
	// degrade in isolation rather than dragging the timeline.
	if rep.SyncErrorMS > 20 {
		t.Fatalf("sync error %.3f ms exceeds 20 ms under loss", rep.SyncErrorMS)
	}
}

// TestBandwidthShrinksBuffer asserts the adaptive bound moves with bandwidth.
func TestBandwidthShrinksBuffer(t *testing.T) {
	low, err := Run(Options{Seed: 41, BandwidthMBps: 1})
	if err != nil {
		t.Fatal(err)
	}
	high, err := Run(Options{Seed: 41, BandwidthMBps: 10})
	if err != nil {
		t.Fatal(err)
	}
	if low.BufferBound >= high.BufferBound {
		t.Fatalf("low-bandwidth bound %d must be below high %d", low.BufferBound, high.BufferBound)
	}
	if low.LatencyP95MS > 500 {
		t.Fatalf("latency unbounded: %.1f ms", low.LatencyP95MS)
	}
}

// TestJSONContract locks the exact field names the CLI promises.
func TestJSONContract(t *testing.T) {
	rep, err := Run(Options{Seed: 41, BandwidthMBps: 3})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"sync_error_ms", "recovery_rate", "latency_p95_ms",
		"reordered_packets", "duplicate_packets", "failed_tracks",
	} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing JSON field %q", key)
		}
	}
	// failed_tracks must be an array, not null, when no tracks fail.
	var failed []string
	if err := json.Unmarshal(m["failed_tracks"], &failed); err != nil {
		t.Fatalf("failed_tracks is not an array: %s", m["failed_tracks"])
	}
	if failed == nil {
		t.Fatal("failed_tracks must serialize as [], not null")
	}
}

// TestDeterministicFixtures asserts identical options give identical reports.
func TestDeterministicFixtures(t *testing.T) {
	a, err := Run(Options{Seed: 7, LossBurst: 3, BandwidthMBps: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(Options{Seed: 7, LossBurst: 3, BandwidthMBps: 2})
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatal("same seed must produce identical reports")
	}
}

// TestRejectsBadArguments checks parameter validation.
func TestRejectsBadArguments(t *testing.T) {
	if _, err := Run(Options{Seed: 1, BandwidthMBps: -1}); err == nil {
		t.Fatal("negative bandwidth must be rejected")
	}
	if _, err := Run(Options{Seed: 1, BandwidthMBps: 1}); err != nil {
		t.Fatalf("default limits must be accepted: %v", err)
	}
}

// TestRejectsZeroLimits asserts that a nonsensical fixture limit is refused
// instead of silently generating an empty scenario.
func TestRejectsZeroLimits(t *testing.T) {
	rep, err := Run(Options{Seed: 1, BandwidthMBps: 1, Limits: zeroLimits})
	if err == nil {
		t.Fatal("zero track limit must be rejected")
	}
	if rep != nil {
		t.Fatalf("rejected run must not return a report: %+v", rep)
	}
	if core.CodeOf(err) != core.CodeOverLimit {
		t.Fatalf("code = %s, want %s", core.CodeOf(err), core.CodeOverLimit)
	}
}

// zeroLimits requests zero tracks, which the fixture layer must reject rather
// than generating an empty scenario.
var zeroLimits = fixture.Limits{MaxTracks: 0, MaxPackets: 100, MaxSeconds: 10, MaxPayload: 100}
