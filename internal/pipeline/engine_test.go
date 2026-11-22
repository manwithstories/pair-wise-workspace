package pipeline

import (
	"encoding/binary"
	"testing"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
	"mediakernel/internal/sim"
)

// buildEngineInput converts a deterministic trace into model events and video
// frames exactly the way the CLI does, so the pipeline test exercises the
// same data path as the acceptance demonstrations.
func buildEngineInput(t *testing.T, scenario string, seed int64) (Input, *sim.Trace) {
	t.Helper()
	params := sim.DefaultParams()
	trace, err := sim.Build(scenario, seed, params)
	if err != nil {
		t.Fatal(err)
	}
	video := sim.BuildVideo(scenario, seed, sim.DefaultVideoParams())
	events := make([]*model.Event, 0, len(trace.Actions))
	for _, ac := range trace.Actions {
		if ac.Kind == sim.ActDrop {
			continue
		}
		pcm := make([]byte, params.FramesPerPacket*2*2) // stereo 16-bit
		binary.LittleEndian.PutUint16(pcm, uint16(ac.Seq)*7)
		events = append(events, &model.Event{
			Kind:        model.KindAudio,
			Stream:      ac.Stream,
			Seq:         ac.Seq,
			EventTime:   ac.MediaTime,
			Arrival:     ac.Arrival,
			PayloadHash: uint64(ac.Seq)*131 + 17,
			AudioPCM:    pcm,
		})
	}
	bw := make([]BandwidthPoint, len(trace.Bandwidth))
	for i, b := range trace.Bandwidth {
		bw[i] = BandwidthPoint{At: b.At, BytesPerSec: b.BytesPerSec}
	}
	return Input{
		AudioEvents: events,
		VideoFrames: video.Frames,
		Bandwidth:   bw,
	}, trace
}

func TestRunDeterministicAcrossEngines(t *testing.T) {
	for _, sc := range []string{sim.ScenarioNominal, sim.ScenarioCorrupt, sim.ScenarioRecovery, sim.ScenarioBurst} {
		in, _ := buildEngineInput(t, sc, 41)
		out1 := New(DefaultConfig(sc)).Run(in)
		out2 := New(DefaultConfig(sc)).Run(in)
		if out1.Metrics.ResultHash != out2.Metrics.ResultHash {
			t.Fatalf("%s: result hash differs across runs: %s vs %s",
				sc, out1.Metrics.ResultHash, out2.Metrics.ResultHash)
		}
		if len(out1.Frames) != len(out2.Frames) || len(out1.Frames) == 0 {
			t.Fatalf("%s: aligned frame output unstable or empty", sc)
		}
	}
}

func TestNominalMetricsWithinPlayoutGoals(t *testing.T) {
	in, _ := buildEngineInput(t, sim.ScenarioNominal, 41)
	out := New(DefaultConfig(sim.ScenarioNominal)).Run(in)
	m := out.Metrics
	if m.AVSyncMSP95 > 20 {
		t.Fatalf("nominal av sync p95 too high: %d", m.AVSyncMSP95)
	}
	if m.LatencyMSP95 > 180 {
		t.Fatalf("nominal latency p95 too high: %d", m.LatencyMSP95)
	}
	if m.DuplicateCount != 0 || m.ReorderedCount != 0 || m.UnrecoverableSamples != 0 {
		t.Fatalf("nominal run must be clean: %+v", m)
	}
	if m.LossRecoveryRate != 1 {
		t.Fatalf("no loss means recovery rate must be 1: %v", m.LossRecoveryRate)
	}
}

func TestCorruptReportsLossAndFaults(t *testing.T) {
	in, trace := buildEngineInput(t, sim.ScenarioCorrupt, 41)
	out := New(DefaultConfig(sim.ScenarioCorrupt)).Run(in)
	m := out.Metrics
	if m.UnrecoverableSamples == 0 && m.RecoveredSamples == 0 {
		t.Fatal("corrupt scenario must report loss handling")
	}
	if m.DuplicateCount == 0 {
		t.Fatal("corrupt trace injects duplicates but none were counted")
	}
	// Every fault must carry a category and a decision.
	for _, f := range out.Faults {
		if f.Category == "" || f.Decision == "" {
			t.Fatalf("fault missing category/decision: %+v", f)
		}
	}
	if len(trace.LostSeqs) == 0 {
		t.Fatal("fixture lost no sequences")
	}
}

func TestRecoveryMeetsRateThreshold(t *testing.T) {
	in, _ := buildEngineInput(t, sim.ScenarioRecovery, 41)
	out := New(DefaultConfig(sim.ScenarioRecovery)).Run(in)
	if out.Metrics.LossRecoveryRate < 0.8 {
		t.Fatalf("recovery rate below 80%%: %v", out.Metrics.LossRecoveryRate)
	}
	if out.Metrics.RecoveredSamples == 0 {
		t.Fatal("bracketed loss runs must produce recovered samples")
	}
}

func TestWindowBoundedAndReplayClassified(t *testing.T) {
	in, _ := buildEngineInput(t, sim.ScenarioBurst, 41)
	cfg := DefaultConfig(sim.ScenarioBurst)
	cfg.WindowMaxEvents = 32 // force capacity pressure
	out := New(cfg).Run(in)
	if out.Metrics.LateReplayCount == 0 {
		t.Fatal("late reorder packets must trigger replay accounting")
	}
	if out.Metrics.DroppedExpired < 0 {
		t.Fatal("dropped count cannot be negative")
	}
}

func TestClockTickAssumptions(t *testing.T) {
	if clock.TicksPerSecond != 48000 {
		t.Fatal("unified clock assumptions changed")
	}
}
