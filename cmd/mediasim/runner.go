package main

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
	"mediakernel/internal/mp4clip"
	"mediakernel/internal/pipeline"
	"mediakernel/internal/rtp"
	"mediakernel/internal/sim"
	"mediakernel/internal/wav"
)

// threshold encodes the per-scenario acceptance gates. A negative value means
// the corresponding check is not applied for that scenario.
type threshold struct {
	maxAVSyncMS  int64
	maxLatencyMS int64
	minRecovery  float64
	requireLoss  bool
}

// thresholdsByScenario is a package-level table so tests can exercise the
// exit-code-2 path with an intentionally unreachable gate.
var thresholdsByScenario = map[string]threshold{
	sim.ScenarioNominal:  {maxAVSyncMS: 20, maxLatencyMS: 180, minRecovery: 1},
	sim.ScenarioCorrupt:  {maxAVSyncMS: -1, maxLatencyMS: -1, requireLoss: true},
	sim.ScenarioRecovery: {maxAVSyncMS: -1, maxLatencyMS: -1, minRecovery: 0.8},
	sim.ScenarioBurst:    {maxAVSyncMS: -1, maxLatencyMS: -1}, // no fixed gates
}

func scenarioThresholds(scenario string) threshold {
	return thresholdsByScenario[scenario]
}

// generateInput builds the complete deterministic pipeline input for one
// (scenario, seed) using the WAV decoder and RTP parser exactly as an
// external producer would, with the sim trace deciding network fate.
func generateInput(scenario string, seed int64) (pipeline.Input, *sim.Trace, error) {
	params := sim.DefaultParams()
	trace, err := sim.Build(scenario, seed, params)
	if err != nil {
		return pipeline.Input{}, nil, err
	}
	video := sim.BuildVideo(scenario, seed, sim.DefaultVideoParams())

	// 1. Synthesize the capture-side multichannel PCM WAV stream.
	frames := wav.SineFrames(uint32(clock.AudioClockRate), 2,
		params.Packets*params.FramesPerPacket, 440)
	wavBytes, err := wav.WriteBytes(uint32(clock.AudioClockRate), 2, frames)
	if err != nil {
		return pipeline.Input{}, nil, err
	}

	// 2. Decode it back through the real WAV decoder and packetize.
	dec, err := wav.NewDecoder(bytes.NewReader(wavBytes))
	if err != nil {
		return pipeline.Input{}, nil, err
	}
	packets := make(map[uint16][]byte, params.Packets)
	var seq uint16
	for {
		block, derr := dec.NextBlock(params.FramesPerPacket)
		if derr != nil {
			return pipeline.Input{}, nil, derr
		}
		rtpTS := uint32(clock.ToRTP(clock.Ticks(int64(seq))*params.BaseInterval, clock.AudioClockRate))
		wire, merr := rtp.Marshal(params.AudioSSRC, seq, rtpTS, block)
		if merr != nil {
			return pipeline.Input{}, nil, merr
		}
		packets[seq] = wire
		seq++
		if int(seq) >= params.Packets {
			break
		}
	}

	// 3. Replay the trace on the wire: drops disappear, duplicates resend the
	// identical bytes, everything is parsed through the real RTP parser.
	events := make([]*model.Event, 0, len(trace.Actions))
	for _, ac := range trace.Actions {
		if ac.Kind == sim.ActDrop {
			continue
		}
		wire := packets[ac.Seq]
		ev, perr := rtp.Parse(wire, ac.Arrival, rtp.ParseOptions{
			ClockRate: clock.AudioClockRate,
			Kind:      model.KindAudio,
		})
		if perr != nil {
			return pipeline.Input{}, nil, perr
		}
		events = append(events, ev)
	}

	bw := make([]pipeline.BandwidthPoint, len(trace.Bandwidth))
	for i, b := range trace.Bandwidth {
		bw[i] = pipeline.BandwidthPoint{At: b.At, BytesPerSec: b.BytesPerSec}
	}
	return pipeline.Input{
		AudioEvents: events,
		VideoFrames: video.Frames,
		Bandwidth:   bw,
	}, trace, nil
}

// verifyVideoClip serializes the generated frame schedule as an MP4 short
// clip and reads its timing tables back, proving timestamp integrity through
// a real container round trip.
func verifyVideoClip(frames []model.VideoFrame) error {
	samples := make([]mp4clip.Sample, len(frames))
	for i, f := range frames {
		samples[i] = mp4clip.Sample{
			Delta: uint32(clock.VideoClockRate) / 30,
			Data:  framePlaceholder(f),
		}
	}
	clip := mp4clip.Clip{
		TrackID:   2,
		Timescale: clock.VideoClockRate,
		IsVideo:   true,
		Samples:   samples,
		Width:     320,
		Height:    180,
	}
	buf := mp4clip.NewMemBuffer()
	if err := mp4clip.WriteClip(buf, clip); err != nil {
		return err
	}
	timing, err := mp4clip.ReadTiming(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	deltas := make([]uint32, len(samples))
	sizes := make([]uint32, len(samples))
	for i, s := range samples {
		deltas[i], sizes[i] = s.Delta, uint32(len(s.Data))
	}
	return timing.VerifyTiming(deltas, sizes)
}

func framePlaceholder(f model.VideoFrame) []byte {
	out := make([]byte, 16)
	binary.BigEndian.PutUint64(out, f.ContentHash)
	binary.BigEndian.PutUint32(out[8:], f.FrameID)
	binary.BigEndian.PutUint32(out[12:], uint32(f.PTS))
	return out
}

// evaluate applies scenario thresholds and returns a non-nil message when a
// gate fails, which the CLI maps to exit code 2.
func evaluate(scenario string, m model.Metrics, th threshold) string {
	if th.maxAVSyncMS >= 0 && m.AVSyncMSP95 > th.maxAVSyncMS {
		return fmt.Sprintf("av_sync_ms_p95 %d exceeds scenario limit %d", m.AVSyncMSP95, th.maxAVSyncMS)
	}
	if th.maxLatencyMS >= 0 && m.LatencyMSP95 > th.maxLatencyMS {
		return fmt.Sprintf("latency_ms_p95 %d exceeds scenario limit %d", m.LatencyMSP95, th.maxLatencyMS)
	}
	if th.minRecovery > 0 && m.LossRecoveryRate < th.minRecovery {
		return fmt.Sprintf("loss_recovery_rate %.4f below scenario minimum %.2f", m.LossRecoveryRate, th.minRecovery)
	}
	if th.requireLoss && m.RecoveredSamples == 0 && m.UnrecoverableSamples == 0 {
		return "corrupt scenario reported zero loss samples"
	}
	return ""
}
