package verify

import (
	"encoding/json"
	"strings"
	"testing"

	"bq/internal/clock"
	"bq/internal/manifest"
	"bq/internal/model"
	"bq/internal/timeline"
	"bq/internal/transport"
)

func baseManifest() *manifest.Manifest {
	return &manifest.Manifest{
		Version: "bq/1", DurationS: 10, MasterHz: 90000,
		Streams: []manifest.Stream{
			{ID: 0, Kind: manifest.KindVideo, Codec: "h264", Container: "mp4",
				File: "v.mp4", Width: 8, Height: 8, TimebaseNum: 90000, KeyframeEvery: 1},
			{ID: 1, Kind: manifest.KindAudio, Codec: "aac", Container: "mp4",
				File: "a.m4a", TimebaseNum: 48000, KeyframeEvery: 1},
		},
	}
}

// alignedTimeline builds video at each whole second and audio offset by
// offsetMs milliseconds to exercise A/V gap measurement.
func alignedTimeline(offsetMs int64) *timeline.Timeline {
	tl := &timeline.Timeline{MasterHz: 90000, Streams: map[int][]timeline.Item{}}
	for s := int64(0); s < 10; s++ {
		tl.Streams[0] = append(tl.Streams[0], timeline.Item{
			StreamID: 0, Seq: s, MasterPTS: s * clock.MasterHz, Keyframe: true,
		})
		offset, _ := clock.MsToMaster(offsetMs)
		tl.Streams[1] = append(tl.Streams[1], timeline.Item{
			StreamID: 1, Seq: s, MasterPTS: s*clock.MasterHz + offset,
		})
	}
	return tl
}

func TestAVSyncMeasurement(t *testing.T) {
	in := Input{
		Manifest: baseManifest(),
		Timeline: alignedTimeline(12), // 12 ms audio offset
		Transport: &transport.Result{Stats: map[int]*transport.Stats{}},
	}
	m, _ := Compute(in)
	if m.AVSyncMaxMs != 12 {
		t.Fatalf("want 12ms av gap, got %d", m.AVSyncMaxMs)
	}
}

func TestAVSyncThresholdGate(t *testing.T) {
	in := Input{
		Manifest: baseManifest(),
		Timeline: alignedTimeline(30), // beyond 20ms gate
		Transport: &transport.Result{Stats: map[int]*transport.Stats{}},
	}
	m, _ := Compute(in)
	failed := m.Evaluate(DefaultThresholds())
	if len(failed) != 1 || failed[0] != "av_sync_max_ms" {
		t.Fatalf("want av gate failure, got %v", failed)
	}
}

func TestRecoveryPercentage(t *testing.T) {
	res := &transport.Result{Stats: map[int]*transport.Stats{
		0: {StreamID: 0, RecoveredPackets: 9, UnrecoveredPackets: 1},
	}}
	if got := measureRecovery(res); got != 90 {
		t.Fatalf("want 90%% recovery, got %v", got)
	}
	if got := measureRecovery(&transport.Result{Stats: map[int]*transport.Stats{}}); got != 100 {
		t.Fatalf("no loss must report 100%%, got %v", got)
	}
}

func TestRecoveryGate(t *testing.T) {
	in := Input{
		Manifest:  baseManifest(),
		Timeline:  alignedTimeline(0),
		Transport: &transport.Result{Stats: map[int]*transport.Stats{0: {RecoveredPackets: 8, UnrecoveredPackets: 2}}},
	}
	m, _ := Compute(in)
	if m.LossRecoveryPct != 80 {
		t.Fatalf("want 80%%, got %v", m.LossRecoveryPct)
	}
	failed := m.Evaluate(DefaultThresholds())
	if len(failed) != 1 || failed[0] != "loss_recovery_pct" {
		t.Fatalf("want recovery gate failure, got %v", failed)
	}
}

func TestPercentile95(t *testing.T) {
	lat := make([]StageLatency, 20)
	for i := range lat {
		lat[i] = StageLatency{Stage: "s", Ms: int64(i + 1)} // 1..20 ms
	}
	if got := percentile95(lat); got != 19 {
		t.Fatalf("nearest-rank p95 want 19, got %d", got)
	}
	if got := percentile95(nil); got != 0 {
		t.Fatalf("empty p95 want 0, got %d", got)
	}
	if got := percentile95([]StageLatency{{Ms: 42}}); got != 42 {
		t.Fatalf("single p95 want 42, got %d", got)
	}
}

func TestComputeAggregatesAndFirstOffset(t *testing.T) {
	res := &transport.Result{
		Stats: map[int]*transport.Stats{
			0: {RecoveredPackets: 3, UnrecoveredPackets: 1, DuplicatePackets: 1},
		},
		Errors: []model.Error{
			model.NewError(model.StageTransport, 0, model.ErrPacketUnrecovered, model.SeverityError,
				"x", 500, 9, true),
			model.NewError(model.StageTransport, 0, model.ErrPacketLoss, model.SeverityRecovered,
				"y", 100, 5, true),
		},
	}
	in := Input{
		Manifest: baseManifest(), Timeline: alignedTimeline(0),
		Transport: res, FrameHash: "abc", FrameCount: 5,
		Latencies: []StageLatency{{Ms: 100}, {Ms: 200}, {Ms: 300}},
	}
	m, errs := Compute(in)
	if m.RecoveredPackets != 3 || m.UnrecoveredPackets != 1 {
		t.Fatalf("packet totals wrong: %+v", m)
	}
	if m.FirstSourceOffset != 100 {
		t.Fatalf("first source offset must be the sorted minimum, got %d", m.FirstSourceOffset)
	}
	// Errors must be deterministically sorted (offset 100 before 500).
	if errs[0].SourceOffset != 100 {
		t.Fatal("errors not sorted")
	}
	if m.FrameSHA256 != "abc" || m.Frames != 5 || m.ErrorCount != 2 {
		t.Fatalf("metric aggregation wrong: %+v", m)
	}
}

func TestPropagationAndExitCodes(t *testing.T) {
	errs := []model.Error{
		model.NewError(model.StageClock, 0, model.ErrPTSWrap, model.SeverityRecovered, "w", 1, 1, true),
		model.NewError(model.StageTransport, 0, model.ErrPacketLoss, model.SeverityRecovered, "l", 2, 2, true),
		model.NewError(model.StageTransport, 0, model.ErrDuplicatePacket, model.SeverityWarning, "d", 3, 3, true),
		model.NewError(model.StageTransport, 0, model.ErrTailTruncation, model.SeverityError, "t", -1, 0, false),
		model.NewError(model.StageTimeline, 0, model.ErrAnchorConflict, model.SeverityError, "a", -1, 4, true),
	}
	res := &transport.Result{Stats: map[int]*transport.Stats{0: {RecoveredPackets: 1}}}
	in := Input{Manifest: baseManifest(), Timeline: alignedTimeline(0), Transport: res}
	m, sorted := Compute(in)
	rep := BuildReport(m, append(sorted, errs...), DefaultThresholds(), 1, 1)
	if !rep.Propagation.WrapObserved || !rep.Propagation.LossObserved ||
		!rep.Propagation.DuplicateObserved || !rep.Propagation.TruncationObserved ||
		!rep.Propagation.AnchorConflicts {
		t.Fatalf("anomaly not propagated: %+v", rep.Propagation)
	}
	if rep.WrapEvents != 1 || rep.UnrecoverableRanges != 1 {
		t.Fatal("wrap/range counts wrong")
	}
	if rep.ExitCode() != 0 {
		t.Fatalf("recovered anomalies with healthy metrics must pass, got %d", rep.ExitCode())
	}
}

func TestExitCodeFatalAndThreshold(t *testing.T) {
	fatal := []model.Error{model.NewError(model.StageManifest, 0,
		model.ErrCorruptManifest, model.SeverityFatal, "bad", 7, 0, false)}
	rep := BuildReport(Metrics{}, fatal, DefaultThresholds(), 0, 0)
	if rep.ExitCode() != 2 || rep.Status != "unrecoverable_input" {
		t.Fatalf("fatal must exit 2, got %d/%s", rep.ExitCode(), rep.Status)
	}

	bad := Metrics{AVSyncMaxMs: 50}
	rep = BuildReport(bad, nil, DefaultThresholds(), 0, 0)
	if rep.ExitCode() != 1 || rep.Status != "threshold_exceeded" {
		t.Fatalf("threshold must exit 1, got %d/%s", rep.ExitCode(), rep.Status)
	}
}

func TestReportJSONStableAndComplete(t *testing.T) {
	m := Metrics{AVSyncMaxMs: 5, LossRecoveryPct: 95, LatencyP95Ms: 450,
		Frames: 5, FrameSHA256: "deadbeef", ErrorCount: 0, FirstSourceOffset: -1}
	rep := BuildReport(m, nil, DefaultThresholds(), 0, 0)
	data, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("report not valid json: %v", err)
	}
	for _, key := range []string{"av_sync_max_ms", "loss_recovery_pct", "latency_p95_ms",
		"frames", "frame_sha256", "error_count", "first_source_offset",
		"recovered_packets", "unrecovered_packets"} {
		if _, ok := round["metrics"].(map[string]any)[key]; !ok {
			t.Fatalf("json missing metrics.%s", key)
		}
	}
	if !strings.Contains(string(data), "\"status\": \"pass\"") {
		t.Fatalf("status missing: %s", data)
	}
	// Re-marshal is byte-identical (deterministic struct order).
	data2, _ := rep.JSON()
	if string(data) != string(data2) {
		t.Fatal("report JSON not stable across marshals")
	}
}
